package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("store: record not found")

type Store struct {
	db *sql.DB
}

type SecurityConfig struct {
	Salt           []byte
	VerifierNonce  []byte
	VerifierCipher []byte
}

// Application describes a service managed by MiniDock. Commands are stored as
// configuration only; this package never executes them.
type Application struct {
	ID                   int64
	Name                 string
	Repository           string
	Branch               string
	WorkDir              string
	Type                 string
	BuildCommand         string
	RunCommand           string
	InternalPort         int
	Domain               string
	Runtime              string
	DeployOnPush         bool
	RequireConfirmation  bool
	AutoRollback         bool
	GitHubInstallationID int64
	CreatedAt            time.Time
}

// AutomationSettings are host-wide because retention operates on all managed
// applications and their shared image/log storage.
type AutomationSettings struct{ CleanupSchedule string }

type Deployment struct {
	ID            int64
	ApplicationID int64
	Status        string
	Image         string
	LogPath       string
	StartedAt     time.Time
	FinishedAt    time.Time
	Action        string
}

// SecretMetadata deliberately excludes the encrypted value. It is safe to
// render in the administration panel and use for audit history.
type SecretMetadata struct {
	Name        string
	Environment string
	Target      string
	UpdatedAt   time.Time
}

type SecretAudit struct {
	Action      string
	Name        string
	Environment string
	Target      string
	CreatedAt   time.Time
}

// Configuration is intentionally stored without encryption. It is for values
// safe to expose to an application's build or runtime, never credentials.
type Configuration struct {
	Name        string
	Value       string
	Environment string
	Target      string
	UpdatedAt   time.Time
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}

	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)

	store := &Store{db: database}
	if err := store.migrate(context.Background()); err != nil {
		database.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) IsInitialized(ctx context.Context) (bool, error) {
	var count int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM settings WHERE key = 'security.salt'").Scan(&count)
	return count == 1, err
}

func (s *Store) InitializeSecurity(ctx context.Context, config SecurityConfig) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for key, value := range map[string][]byte{
		"security.salt":            config.Salt,
		"security.verifier_nonce":  config.VerifierNonce,
		"security.verifier_cipher": config.VerifierCipher,
	} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?)", key, value); err != nil {
			return fmt.Errorf("store %s: %w", key, err)
		}
	}
	return tx.Commit()
}

func (s *Store) SecurityConfig(ctx context.Context) (SecurityConfig, error) {
	values := make(map[string][]byte, 3)
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings WHERE key IN ('security.salt', 'security.verifier_nonce', 'security.verifier_cipher')")
	if err != nil {
		return SecurityConfig{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var key string
		var value []byte
		if err := rows.Scan(&key, &value); err != nil {
			return SecurityConfig{}, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return SecurityConfig{}, err
	}
	if len(values) != 3 {
		return SecurityConfig{}, ErrNotFound
	}
	return SecurityConfig{
		Salt:           values["security.salt"],
		VerifierNonce:  values["security.verifier_nonce"],
		VerifierCipher: values["security.verifier_cipher"],
	}, nil
}

func (s *Store) PutSecret(ctx context.Context, scope, name string, nonce, ciphertext []byte) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO secrets(scope, name, nonce, ciphertext, updated_at)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(scope, name) DO UPDATE SET nonce = excluded.nonce, ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		scope, name, nonce, ciphertext, time.Now().UTC())
	return err
}

func (s *Store) Secret(ctx context.Context, scope, name string) (nonce, ciphertext []byte, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT nonce, ciphertext FROM secrets WHERE scope = ? AND name = ?", scope, name).Scan(&nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return nonce, ciphertext, err
}

func applicationSecretScope(applicationID int64, environment, target string) string {
	return fmt.Sprintf("application:%d:%s:%s", applicationID, environment, target)
}

func validEnvironmentAndTarget(environment, target string) bool {
	return (environment == "production" || environment == "staging") && (target == "runtime" || target == "build")

}

func (s *Store) PutApplicationSecret(ctx context.Context, applicationID int64, environment, target, name string, nonce, ciphertext []byte) error {
	if !validEnvironmentAndTarget(environment, target) {
		return errors.New("invalid secret environment or target")
	}
	scope := applicationSecretScope(applicationID, environment, target)
	var configurationExists bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM application_configuration WHERE application_id = ? AND environment = ? AND target = ? AND name = ?)", applicationID, environment, target, name).Scan(&configurationExists); err != nil {
		return err
	}
	if configurationExists {
		return errors.New("a public configuration already uses this name")
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM secrets WHERE scope = ? AND name = ?)", scope, name).Scan(&exists)
	if err != nil {
		return err
	}
	if err = s.PutSecret(ctx, scope, name, nonce, ciphertext); err != nil {
		return err
	}
	action := "created"
	if exists {
		action = "rotated"
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO secret_audit(application_id, environment, target, name, action, created_at) VALUES(?, ?, ?, ?, ?, ?)", applicationID, environment, target, name, action, time.Now().UTC())
	return err
}

func (s *Store) ApplicationSecrets(ctx context.Context, applicationID int64, environment, target string) ([]SecretMetadata, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, updated_at FROM secrets WHERE scope = ? ORDER BY name", applicationSecretScope(applicationID, environment, target))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	secrets := []SecretMetadata{}
	for rows.Next() {
		var secret SecretMetadata
		if err := rows.Scan(&secret.Name, &secret.UpdatedAt); err != nil {
			return nil, err
		}
		secret.Environment = environment
		secret.Target = target
		secrets = append(secrets, secret)
	}
	return secrets, rows.Err()
}

func (s *Store) ApplicationSecret(ctx context.Context, applicationID int64, environment, target, name string) (nonce, ciphertext []byte, err error) {
	return s.Secret(ctx, applicationSecretScope(applicationID, environment, target), name)
}

func (s *Store) DeleteApplicationSecret(ctx context.Context, applicationID int64, environment, target, name string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM secrets WHERE scope = ? AND name = ?", applicationSecretScope(applicationID, environment, target), name)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		if err == nil {
			return ErrNotFound
		}
		return err
	}
	_, err = s.db.ExecContext(ctx, "INSERT INTO secret_audit(application_id, environment, target, name, action, created_at) VALUES(?, ?, ?, ?, 'deleted', ?)", applicationID, environment, target, name, time.Now().UTC())
	return err
}

func (s *Store) RecordSecretUse(ctx context.Context, applicationID int64, environment, target string, names []string) error {
	for _, name := range names {
		if _, err := s.db.ExecContext(ctx, "INSERT INTO secret_audit(application_id, environment, target, name, action, created_at) VALUES(?, ?, ?, ?, 'used', ?)", applicationID, environment, target, name, time.Now().UTC()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) SecretAudit(ctx context.Context, applicationID int64) ([]SecretAudit, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT action, name, environment, target, created_at FROM secret_audit WHERE application_id = ? ORDER BY id DESC LIMIT 30", applicationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	audit := []SecretAudit{}
	for rows.Next() {
		var event SecretAudit
		if err := rows.Scan(&event.Action, &event.Name, &event.Environment, &event.Target, &event.CreatedAt); err != nil {
			return nil, err
		}
		audit = append(audit, event)
	}
	return audit, rows.Err()
}

func (s *Store) PutApplicationConfiguration(ctx context.Context, applicationID int64, environment, target, name, value string) error {
	if !validEnvironmentAndTarget(environment, target) {
		return errors.New("invalid configuration environment or target")
	}
	var secretExists bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM secrets WHERE scope = ? AND name = ?)", applicationSecretScope(applicationID, environment, target), name).Scan(&secretExists); err != nil {
		return err
	}
	if secretExists {
		return errors.New("a secret already uses this name")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO application_configuration(application_id, environment, target, name, value, updated_at)
		VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(application_id, environment, target, name) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, applicationID, environment, target, name, value, time.Now().UTC())
	return err
}

func (s *Store) ApplicationConfiguration(ctx context.Context, applicationID int64, environment, target string) ([]Configuration, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name, value, updated_at FROM application_configuration WHERE application_id = ? AND environment = ? AND target = ? ORDER BY name", applicationID, environment, target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Configuration{}
	for rows.Next() {
		var value Configuration
		if err := rows.Scan(&value.Name, &value.Value, &value.UpdatedAt); err != nil {
			return nil, err
		}
		value.Environment, value.Target = environment, target
		result = append(result, value)
	}
	return result, rows.Err()
}

func (s *Store) DeleteApplicationConfiguration(ctx context.Context, applicationID int64, environment, target, name string) error {
	result, err := s.db.ExecContext(ctx, "DELETE FROM application_configuration WHERE application_id = ? AND environment = ? AND target = ? AND name = ?", applicationID, environment, target, name)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count == 0 {
		if err != nil {
			return err
		}
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateApplication(ctx context.Context, application Application) (Application, error) {
	// CI/CD historically deployed the configured branch on push. Preserve that
	// safe migration path; callers can switch to manual mode immediately after
	// creation through UpdateApplicationAutomation.
	if !application.DeployOnPush {
		application.DeployOnPush = true
	}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO applications(name, repository, branch, work_dir, type, build_command, run_command, internal_port, domain, runtime, deploy_on_push, require_confirmation, auto_rollback, github_installation_id, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		application.Name, application.Repository, application.Branch, application.WorkDir,
		application.Type, application.BuildCommand, application.RunCommand,
		application.InternalPort, application.Domain, application.Runtime, application.DeployOnPush, application.RequireConfirmation, application.AutoRollback, application.GitHubInstallationID, time.Now().UTC())
	if err != nil {
		return Application{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Application{}, err
	}
	application.ID = id
	return application, nil
}

func (s *Store) Applications(ctx context.Context) ([]Application, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, repository, branch, work_dir, type, build_command, run_command, internal_port, domain, runtime, deploy_on_push, require_confirmation, auto_rollback, github_installation_id, created_at
		FROM applications ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	applications := []Application{}
	for rows.Next() {
		var application Application
		if err := rows.Scan(&application.ID, &application.Name, &application.Repository, &application.Branch,
			&application.WorkDir, &application.Type, &application.BuildCommand, &application.RunCommand,
			&application.InternalPort, &application.Domain, &application.Runtime, &application.DeployOnPush, &application.RequireConfirmation, &application.AutoRollback, &application.GitHubInstallationID, &application.CreatedAt); err != nil {
			return nil, err
		}
		applications = append(applications, application)
	}
	return applications, rows.Err()
}

func (s *Store) Application(ctx context.Context, id int64) (Application, error) {
	var application Application
	err := s.db.QueryRowContext(ctx, `SELECT id, name, repository, branch, work_dir, type, build_command, run_command, internal_port, domain, runtime, deploy_on_push, require_confirmation, auto_rollback, github_installation_id, created_at FROM applications WHERE id = ?`, id).Scan(
		&application.ID, &application.Name, &application.Repository, &application.Branch, &application.WorkDir, &application.Type, &application.BuildCommand, &application.RunCommand, &application.InternalPort, &application.Domain, &application.Runtime, &application.DeployOnPush, &application.RequireConfirmation, &application.AutoRollback, &application.GitHubInstallationID, &application.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	return application, err
}

func (s *Store) StartDeployment(ctx context.Context, applicationID int64, image, logPath string) (Deployment, error) {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO deployments(application_id, status, image, log_path, started_at) VALUES(?, 'running', ?, ?, ?)`, applicationID, image, logPath, now)
	if err != nil {
		return Deployment{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{ID: id, ApplicationID: applicationID, Status: "running", Image: image, LogPath: logPath, StartedAt: now}, nil
}

// QueueDeployment persists work before it is executed. The partial unique index
// installed by migrate guarantees that an application has at most one active job.
func (s *Store) QueueDeployment(ctx context.Context, applicationID int64, action, logPath string) (Deployment, error) {
	if action != "deploy" && action != "rollback" && action != "auto_rollback" {
		return Deployment{}, errors.New("invalid deployment action")
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `INSERT INTO deployments(application_id, status, image, log_path, started_at, action) VALUES(?, 'queued', '', ?, ?, ?)`, applicationID, logPath, now, action)
	if err != nil {
		return Deployment{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{ID: id, ApplicationID: applicationID, Status: "queued", LogPath: logPath, StartedAt: now, Action: action}, nil
}

// NextQueuedDeployment atomically reserves the oldest queued job. A single
// process may run several workers without running one application twice.
func (s *Store) NextQueuedDeployment(ctx context.Context) (Deployment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Deployment{}, err
	}
	defer tx.Rollback()
	var d Deployment
	var finished sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT id, application_id, status, image, log_path, started_at, finished_at, action FROM deployments WHERE status = 'queued' ORDER BY id LIMIT 1`).Scan(&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.LogPath, &d.StartedAt, &finished, &d.Action)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE deployments SET status = 'running', started_at = ? WHERE id = ? AND status = 'queued'`, time.Now().UTC(), d.ID)
	if err != nil {
		return Deployment{}, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return Deployment{}, ErrNotFound
	}
	d.Status = "running"
	if finished.Valid {
		d.FinishedAt = finished.Time
	}
	if err = tx.Commit(); err != nil {
		return Deployment{}, err
	}
	return d, nil
}

// PreviousSuccessfulDeployment returns the successful deployment before the
// currently running successful deployment. It is the rollback target, not the
// current image itself.
func (s *Store) PreviousSuccessfulDeployment(ctx context.Context, applicationID int64) (Deployment, error) {
	var d Deployment
	var finished sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id, application_id, status, image, log_path, started_at, finished_at, action FROM deployments WHERE application_id = ? AND status = 'successful' AND image <> '' ORDER BY id DESC LIMIT 1 OFFSET 1`, applicationID).Scan(&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.LogPath, &d.StartedAt, &finished, &d.Action)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if finished.Valid {
		d.FinishedAt = finished.Time
	}
	return d, err
}

// LatestSuccessfulDeployment is used to restore a known-good image after a
// failed health check. Unlike a user-requested rollback it deliberately picks
// the most recent successful deployment.
func (s *Store) LatestSuccessfulDeployment(ctx context.Context, applicationID int64) (Deployment, error) {
	var d Deployment
	var finished sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id, application_id, status, image, log_path, started_at, finished_at, action FROM deployments WHERE application_id = ? AND status = 'successful' AND image <> '' ORDER BY id DESC LIMIT 1`, applicationID).Scan(&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.LogPath, &d.StartedAt, &finished, &d.Action)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if finished.Valid {
		d.FinishedAt = finished.Time
	}
	return d, err
}

func (s *Store) UpdateApplicationAutomation(ctx context.Context, applicationID int64, deployOnPush, requireConfirmation, autoRollback bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE applications SET deploy_on_push = ?, require_confirmation = ?, auto_rollback = ? WHERE id = ?`, deployOnPush, requireConfirmation, autoRollback, applicationID)
	if err != nil {
		return err
	}
	if count, err := result.RowsAffected(); err != nil || count == 0 {
		if err != nil {
			return err
		}
		return ErrNotFound
	}
	return nil
}

func (s *Store) AutomationSettings(ctx context.Context) (AutomationSettings, error) {
	var settings AutomationSettings
	err := s.db.QueryRowContext(ctx, `SELECT cleanup_schedule FROM automation_settings WHERE id = 1`).Scan(&settings.CleanupSchedule)
	if errors.Is(err, sql.ErrNoRows) {
		return AutomationSettings{CleanupSchedule: "manual"}, nil
	}
	return settings, err
}

func (s *Store) UpdateCleanupSchedule(ctx context.Context, schedule string) error {
	if schedule != "manual" && schedule != "daily" && schedule != "weekly" {
		return errors.New("invalid cleanup schedule")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO automation_settings(id, cleanup_schedule) VALUES(1, ?) ON CONFLICT(id) DO UPDATE SET cleanup_schedule = excluded.cleanup_schedule`, schedule)
	return err
}

func (s *Store) FinishDeployment(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET status = ?, finished_at = ? WHERE id = ?`, status, time.Now().UTC(), id)
	return err
}

func (s *Store) SetDeploymentImage(ctx context.Context, id int64, image string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET image = ? WHERE id = ?`, image, id)
	return err
}

func (s *Store) Deployments(ctx context.Context, applicationID int64) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, application_id, status, image, log_path, started_at, finished_at, action FROM deployments WHERE application_id = ? ORDER BY id DESC`, applicationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Deployment{}
	for rows.Next() {
		var d Deployment
		var finished sql.NullTime
		if err := rows.Scan(&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.LogPath, &d.StartedAt, &finished, &d.Action); err != nil {
			return nil, err
		}
		if finished.Valid {
			d.FinishedAt = finished.Time
		}
		result = append(result, d)
	}
	return result, rows.Err()
}

// Deployment returns a deployment only when it belongs to applicationID. This
// prevents callers from using a deployment identifier to access another
// application's operational data.
func (s *Store) Deployment(ctx context.Context, applicationID, id int64) (Deployment, error) {
	var d Deployment
	var finished sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id, application_id, status, image, log_path, started_at, finished_at, action FROM deployments WHERE application_id = ? AND id = ?`, applicationID, id).Scan(
		&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.LogPath, &d.StartedAt, &finished, &d.Action)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if finished.Valid {
		d.FinishedAt = finished.Time
	}
	return d, err
}

// RetentionCandidates returns finished work older than before. The most recent
// successful deployments are retained independently of age so rollback images
// remain referenced by MiniDock.
func (s *Store) RetentionCandidates(ctx context.Context, applicationID int64, before time.Time, retainSuccessful int) ([]Deployment, error) {
	deployments, err := s.Deployments(ctx, applicationID)
	if err != nil {
		return nil, err
	}
	candidates := make([]Deployment, 0)
	successful := 0
	for _, deployment := range deployments {
		if deployment.Status == "successful" {
			successful++
		}
		if deployment.Status == "queued" || deployment.Status == "running" || deployment.FinishedAt.IsZero() || !deployment.FinishedAt.Before(before) {
			continue
		}
		if deployment.Status == "successful" {
			if successful <= retainSuccessful {
				continue
			}
		}
		candidates = append(candidates, deployment)
	}
	return candidates, nil
}

func (s *Store) DeleteDeployments(ctx context.Context, applicationID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+1)
	args = append(args, applicationID)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM deployments WHERE application_id = ? AND id IN ("+placeholders+")", args...)
	return err
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		PRAGMA foreign_keys = ON;
		CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value BLOB NOT NULL
		);
		CREATE TABLE IF NOT EXISTS secrets (
			scope TEXT NOT NULL,
			name TEXT NOT NULL,
			nonce BLOB NOT NULL,
			ciphertext BLOB NOT NULL,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (scope, name)
		);
		CREATE TABLE IF NOT EXISTS secret_audit (
			id INTEGER PRIMARY KEY,
			application_id INTEGER NOT NULL REFERENCES applications(id),
			environment TEXT NOT NULL,
			target TEXT NOT NULL DEFAULT 'runtime',
			name TEXT NOT NULL,
			action TEXT NOT NULL,
			created_at DATETIME NOT NULL
		);
		CREATE TABLE IF NOT EXISTS application_configuration (
			application_id INTEGER NOT NULL REFERENCES applications(id),
			environment TEXT NOT NULL,
			target TEXT NOT NULL,
			name TEXT NOT NULL,
			value TEXT NOT NULL,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (application_id, environment, target, name)
		);
		CREATE TABLE IF NOT EXISTS applications (
			id INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			repository TEXT NOT NULL,
			branch TEXT NOT NULL,
			work_dir TEXT NOT NULL,
			type TEXT NOT NULL,
			build_command TEXT NOT NULL,
			run_command TEXT NOT NULL,
			internal_port INTEGER NOT NULL,
			domain TEXT NOT NULL UNIQUE,
			runtime TEXT NOT NULL DEFAULT 'auto',
			deploy_on_push BOOLEAN NOT NULL DEFAULT 1,
			require_confirmation BOOLEAN NOT NULL DEFAULT 1,
			auto_rollback BOOLEAN NOT NULL DEFAULT 0,
			github_installation_id INTEGER NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL
		);
		CREATE TABLE IF NOT EXISTS automation_settings (
			id INTEGER PRIMARY KEY CHECK(id = 1),
			cleanup_schedule TEXT NOT NULL DEFAULT 'manual'
		);
		CREATE TABLE IF NOT EXISTS deployments (
			id INTEGER PRIMARY KEY,
			application_id INTEGER NOT NULL REFERENCES applications(id),
			status TEXT NOT NULL,
			image TEXT NOT NULL,
			log_path TEXT NOT NULL,
			started_at DATETIME NOT NULL,
			finished_at DATETIME,
			action TEXT NOT NULL DEFAULT 'deploy'
		);`)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE applications ADD COLUMN runtime TEXT NOT NULL DEFAULT 'auto'`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	for _, statement := range []string{
		`ALTER TABLE applications ADD COLUMN deploy_on_push BOOLEAN NOT NULL DEFAULT 1`,
		`ALTER TABLE applications ADD COLUMN require_confirmation BOOLEAN NOT NULL DEFAULT 1`,
		`ALTER TABLE applications ADD COLUMN auto_rollback BOOLEAN NOT NULL DEFAULT 0`,
		`ALTER TABLE applications ADD COLUMN github_installation_id INTEGER NOT NULL DEFAULT 0`,
	} {
		_, err = s.db.ExecContext(ctx, statement)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE secret_audit ADD COLUMN target TEXT NOT NULL DEFAULT 'runtime'`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	_, err = s.db.ExecContext(ctx, `ALTER TABLE deployments ADD COLUMN action TEXT NOT NULL DEFAULT 'deploy'`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}
	_, err = s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS one_active_deployment_per_application ON deployments(application_id) WHERE status IN ('queued', 'running')`)
	return nil
}
