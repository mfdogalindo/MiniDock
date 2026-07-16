package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrNotFound            = errors.New("store: record not found")
	ErrDeploymentCancelled = errors.New("store: deployment cancellation requested")
)

type Store struct {
	db *sql.DB
}

// Node is the observed identity and liveness data of a remote MiniDock agent.
// Its certificate fingerprint is an identity binding, never an authorization
// token and never a secret.
type Node struct {
	ID                     string
	Name, Version          string
	Capabilities           []string
	CertificateFingerprint string
	CreatedAt, LastSeenAt  time.Time
}

type SecurityConfig struct {
	Salt           []byte
	VerifierNonce  []byte
	VerifierCipher []byte
}

// Role names are deliberately fixed in code. Bindings only reference one of
// these roles, preventing a database edit from inventing elevated permissions.
const (
	RoleViewer        = "viewer"
	RoleOperator      = "operator"
	RoleDeployer      = "deployer"
	RoleAdmin         = "admin"
	RoleSecurityAdmin = "security_admin"
)

type IdentityProvider struct {
	ID, Issuer, ClientID string
	Enabled              bool
	CreatedAt            time.Time
}

type FederatedIdentity struct {
	ProviderID, Subject, Email, DisplayName string
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
	HealthEndpoint       string
	CreatedAt            time.Time
}

// AutomationSettings are host-wide because retention operates on all managed
// applications and their shared image/log storage.
type AutomationSettings struct{ CleanupSchedule string }

type CloudflareConfig struct {
	Mode            string    `json:"mode"`
	AccountID       string    `json:"account_id"`
	TunnelID        string    `json:"tunnel_id"`
	TunnelName      string    `json:"tunnel_name"`
	Status          string    `json:"status"`
	LastHealthCheck time.Time `json:"last_health_check"`
	ErrorMessage    string    `json:"error_message"`
	UpdatedAt       time.Time `json:"updated_at"`
}

type Deployment struct {
	ID                      int64
	ApplicationID           int64
	Status                  string
	Image                   string
	RequestedRef            string
	SourceRevision          string
	SourceFingerprint       string
	ArtifactDigest          string
	Runtime                 string
	InternalPort            int
	HealthEndpoint          string
	Manifest                string
	ConfigurationDigest     string
	FailureStage            string
	FailureCode             string
	FailureDetail           string
	LogPath                 string
	StartedAt               time.Time
	FinishedAt              time.Time
	Action                  string
	Attempt                 int
	LeaseExpiresAt          time.Time
	HeartbeatAt             time.Time
	CancellationRequestedAt time.Time
	QueueDurationMs         int64
	SourceDurationMs        int64
	BuildDurationMs         int64
	StartDurationMs         int64
	HealthDurationMs        int64
	RouteDurationMs         int64
	CurrentStage            string
}

type DeploymentEvent struct {
	ID           int64
	DeploymentID int64
	Stage        string
	EventType    string
	Message      string
	CreatedAt    time.Time
}

type Alert struct {
	ID            int64
	Severity      string
	Message       string
	ApplicationID *int64
	DeploymentID  *int64
	Resolved      bool
	CreatedAt     time.Time
	ResolvedAt    time.Time
}

// HealthEvidence keeps the independently observed release signals. Container
// health, route application and the external HTTP probe must not be collapsed
// into one inferred status.
type HealthEvidence struct {
	ApplicationID                                        int64
	InternalStatus, RouteStatus, ExternalStatus          string
	InternalCheckedAt, RouteCheckedAt, ExternalCheckedAt time.Time
	ExternalObserver, ExternalDetail                     string
	ExternalHTTPStatus                                   int
}

// DeploymentLeaseDuration bounds how long a running job may go without
// proving that its worker is still alive. It is intentionally persisted with
// the job so recovery has evidence rather than relying on process memory.
const DeploymentLeaseDuration = 30 * time.Second

// ReleaseMetadata is provider-neutral evidence captured for a deployment.
// Manifest and ConfigurationDigest must never contain secret values.
type ReleaseMetadata struct {
	RequestedRef, SourceRevision, SourceFingerprint, ArtifactDigest string
	Runtime                                                         string
	InternalPort                                                    int
	HealthEndpoint, Manifest, ConfigurationDigest                   string
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

func (s *Store) UpdateSecurityConfig(ctx context.Context, config SecurityConfig) error {
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
		if _, err := tx.ExecContext(ctx, "UPDATE settings SET value = ? WHERE key = ?", value, key); err != nil {
			return fmt.Errorf("update %s: %w", key, err)
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

func (s *Store) SetSettingString(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, []byte(value))
	return err
}

func (s *Store) SettingString(ctx context.Context, key string) (string, error) {
	var value []byte
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	return string(value), nil
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

func (s *Store) HasSecrets(ctx context.Context, applicationID int64) (bool, error) {
	var exists bool
	pattern := fmt.Sprintf("application:%d:%%", applicationID)
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM secrets WHERE scope LIKE ?)", pattern).Scan(&exists)
	return exists, err
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
	if application.DeployOnPush && application.RequireConfirmation {
		return Application{}, errors.New("push deployments require production confirmation to be disabled")
	}
	// Manual deployments are the safe default. CI/CD must be enabled explicitly
	// after the owner has chosen to disable interactive confirmation.
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO applications(name, repository, branch, work_dir, type, build_command, run_command, internal_port, domain, runtime, deploy_on_push, require_confirmation, auto_rollback, github_installation_id, health_endpoint, created_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		application.Name, application.Repository, application.Branch, application.WorkDir,
		application.Type, application.BuildCommand, application.RunCommand,
		application.InternalPort, application.Domain, application.Runtime, application.DeployOnPush, application.RequireConfirmation, application.AutoRollback, application.GitHubInstallationID, application.HealthEndpoint, time.Now().UTC())
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
		SELECT id, name, repository, branch, work_dir, type, build_command, run_command, internal_port, domain, runtime, deploy_on_push, require_confirmation, auto_rollback, github_installation_id, health_endpoint, created_at
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
			&application.InternalPort, &application.Domain, &application.Runtime, &application.DeployOnPush, &application.RequireConfirmation, &application.AutoRollback, &application.GitHubInstallationID, &application.HealthEndpoint, &application.CreatedAt); err != nil {
			return nil, err
		}
		applications = append(applications, application)
	}
	return applications, rows.Err()
}

func (s *Store) Application(ctx context.Context, id int64) (Application, error) {
	var application Application
	err := s.db.QueryRowContext(ctx, `SELECT id, name, repository, branch, work_dir, type, build_command, run_command, internal_port, domain, runtime, deploy_on_push, require_confirmation, auto_rollback, github_installation_id, health_endpoint, created_at FROM applications WHERE id = ?`, id).Scan(
		&application.ID, &application.Name, &application.Repository, &application.Branch, &application.WorkDir, &application.Type, &application.BuildCommand, &application.RunCommand, &application.InternalPort, &application.Domain, &application.Runtime, &application.DeployOnPush, &application.RequireConfirmation, &application.AutoRollback, &application.GitHubInstallationID, &application.HealthEndpoint, &application.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	return application, err
}

func (s *Store) StartDeployment(ctx context.Context, applicationID int64, image, logPath string) (Deployment, error) {
	now := time.Now().UTC()
	leaseExpiresAt := now.Add(DeploymentLeaseDuration)
	result, err := s.db.ExecContext(ctx, `INSERT INTO deployments(application_id, status, image, log_path, started_at, attempt, heartbeat_at, lease_expires_at) VALUES(?, 'running', ?, ?, ?, 1, ?, ?)`, applicationID, image, logPath, now, now, leaseExpiresAt)
	if err != nil {
		return Deployment{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{ID: id, ApplicationID: applicationID, Status: "running", Image: image, LogPath: logPath, StartedAt: now, Attempt: 1, HeartbeatAt: now, LeaseExpiresAt: leaseExpiresAt}, nil
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
	var finished, heartbeat, lease sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT id, application_id, status, image, requested_ref, source_revision, source_fingerprint, artifact_digest, runtime, internal_port, health_endpoint, manifest, configuration_digest, failure_stage, failure_code, failure_detail, log_path, started_at, finished_at, action, attempt, heartbeat_at, lease_expires_at, queue_duration_ms, source_duration_ms, build_duration_ms, start_duration_ms, health_duration_ms, route_duration_ms, current_stage FROM deployments WHERE status = 'queued' ORDER BY id LIMIT 1`).Scan(&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.RequestedRef, &d.SourceRevision, &d.SourceFingerprint, &d.ArtifactDigest, &d.Runtime, &d.InternalPort, &d.HealthEndpoint, &d.Manifest, &d.ConfigurationDigest, &d.FailureStage, &d.FailureCode, &d.FailureDetail, &d.LogPath, &d.StartedAt, &finished, &d.Action, &d.Attempt, &heartbeat, &lease, &d.QueueDurationMs, &d.SourceDurationMs, &d.BuildDurationMs, &d.StartDurationMs, &d.HealthDurationMs, &d.RouteDurationMs, &d.CurrentStage)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	now := time.Now().UTC()
	leaseExpiresAt := now.Add(DeploymentLeaseDuration)
	queueDuration := now.Sub(d.StartedAt)
	result, err := tx.ExecContext(ctx, `UPDATE deployments SET status = 'running', started_at = ?, attempt = attempt + 1, heartbeat_at = ?, lease_expires_at = ?, queue_duration_ms = ?, current_stage = 'source' WHERE id = ? AND status = 'queued'`, now, now, leaseExpiresAt, queueDuration.Milliseconds(), d.ID)
	if err != nil {
		return Deployment{}, err
	}
	count, err := result.RowsAffected()
	if err != nil || count != 1 {
		return Deployment{}, ErrNotFound
	}
	d.Status = "running"
	d.StartedAt = now
	d.Attempt++
	d.HeartbeatAt = now
	d.LeaseExpiresAt = leaseExpiresAt
	d.QueueDurationMs = queueDuration.Milliseconds()
	d.CurrentStage = "source"
	if finished.Valid {
		d.FinishedAt = finished.Time
	}
	if err = tx.Commit(); err != nil {
		return Deployment{}, err
	}
	return d, nil
}

// HeartbeatDeployment renews an active job's lease. Returning ErrNotFound
// means the job was no longer running and its worker must stop treating it as
// owned work.
func (s *Store) HeartbeatDeployment(ctx context.Context, id int64) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE deployments SET heartbeat_at = ?, lease_expires_at = ? WHERE id = ? AND status = 'running' AND cancellation_requested_at IS NULL`, now, now.Add(DeploymentLeaseDuration), id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		cancelled, cancellationErr := s.DeploymentCancellationRequested(ctx, id)
		if cancellationErr == nil && cancelled {
			return ErrDeploymentCancelled
		}
		return ErrNotFound
	}
	return nil
}

// CancelDeployment cancels queued work immediately. A running command gets a
// durable cancellation request; its heartbeat observes the request and stops
// the command before the final state is recorded as cancelled.
func (s *Store) CancelDeployment(ctx context.Context, applicationID, id int64) error {
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE deployments
		SET status = 'cancelled', finished_at = ?, lease_expires_at = NULL
		WHERE id = ? AND application_id = ? AND status = 'queued'`, now, id, applicationID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 1 {
		return nil
	}
	result, err = s.db.ExecContext(ctx, `UPDATE deployments SET cancellation_requested_at = ?
		WHERE id = ? AND application_id = ? AND status = 'running' AND cancellation_requested_at IS NULL`, now, id, applicationID)
	if err != nil {
		return err
	}
	count, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DeploymentCancellationRequested(ctx context.Context, id int64) (bool, error) {
	var requested sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT cancellation_requested_at FROM deployments WHERE id = ?`, id).Scan(&requested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}
	return requested.Valid, err
}

// RecoverInterruptedDeployments closes work that was running when the worker
// process disappeared. A CLI command cannot safely be resumed from an
// arbitrary point, so reporting a normalized failure is preferable to leaving
// an eternal running job that blocks all future releases for the application.
// Queued work is intentionally untouched and will be claimed normally.
func (s *Store) RecoverInterruptedDeployments(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE deployments
		SET status = 'failed', finished_at = ?, failure_stage = 'start',
			failure_code = 'worker_restarted',
			failure_detail = 'MiniDock restarted before this job completed',
			lease_expires_at = NULL
		WHERE status = 'running'`, time.Now().UTC())
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// PreviousSuccessfulDeployment returns the successful deployment before the
// currently running successful deployment. It is the rollback target, not the
// current image itself.
func (s *Store) PreviousSuccessfulDeployment(ctx context.Context, applicationID int64) (Deployment, error) {
	var d Deployment
	var finished sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id, application_id, status, image, requested_ref, source_revision, source_fingerprint, artifact_digest, runtime, internal_port, health_endpoint, manifest, configuration_digest, failure_stage, failure_code, failure_detail, log_path, started_at, finished_at, action, queue_duration_ms, source_duration_ms, build_duration_ms, start_duration_ms, health_duration_ms, route_duration_ms, current_stage FROM deployments WHERE application_id = ? AND status = 'successful' AND image <> '' ORDER BY id DESC LIMIT 1 OFFSET 1`, applicationID).Scan(&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.RequestedRef, &d.SourceRevision, &d.SourceFingerprint, &d.ArtifactDigest, &d.Runtime, &d.InternalPort, &d.HealthEndpoint, &d.Manifest, &d.ConfigurationDigest, &d.FailureStage, &d.FailureCode, &d.FailureDetail, &d.LogPath, &d.StartedAt, &finished, &d.Action, &d.QueueDurationMs, &d.SourceDurationMs, &d.BuildDurationMs, &d.StartDurationMs, &d.HealthDurationMs, &d.RouteDurationMs, &d.CurrentStage)
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
	err := s.db.QueryRowContext(ctx, `SELECT id, application_id, status, image, requested_ref, source_revision, source_fingerprint, artifact_digest, runtime, internal_port, health_endpoint, manifest, configuration_digest, failure_stage, failure_code, failure_detail, log_path, started_at, finished_at, action, queue_duration_ms, source_duration_ms, build_duration_ms, start_duration_ms, health_duration_ms, route_duration_ms, current_stage FROM deployments WHERE application_id = ? AND status = 'successful' AND image <> '' ORDER BY id DESC LIMIT 1`, applicationID).Scan(&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.RequestedRef, &d.SourceRevision, &d.SourceFingerprint, &d.ArtifactDigest, &d.Runtime, &d.InternalPort, &d.HealthEndpoint, &d.Manifest, &d.ConfigurationDigest, &d.FailureStage, &d.FailureCode, &d.FailureDetail, &d.LogPath, &d.StartedAt, &finished, &d.Action, &d.QueueDurationMs, &d.SourceDurationMs, &d.BuildDurationMs, &d.StartDurationMs, &d.HealthDurationMs, &d.RouteDurationMs, &d.CurrentStage)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if finished.Valid {
		d.FinishedAt = finished.Time
	}
	return d, err
}

func (s *Store) UpdateApplicationAutomation(ctx context.Context, applicationID int64, deployOnPush, requireConfirmation, autoRollback bool) error {
	if deployOnPush && requireConfirmation {
		return errors.New("push deployments require production confirmation to be disabled")
	}
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

func (s *Store) UpdateApplicationDomain(ctx context.Context, applicationID int64, domain string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE applications SET domain = ? WHERE id = ?`, domain, applicationID)
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
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET status = CASE WHEN cancellation_requested_at IS NOT NULL THEN 'cancelled' ELSE ? END, finished_at = ?, lease_expires_at = NULL WHERE id = ?`, status, time.Now().UTC(), id)
	return err
}

func (s *Store) SetDeploymentImage(ctx context.Context, id int64, image string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET image = ? WHERE id = ?`, image, id)
	return err
}

// SetDeploymentReleaseMetadata records reproducibility data as it becomes
// known. This intentionally permits partial metadata: source resolution and
// artifact creation happen in different stages and may fail independently.
func (s *Store) SetDeploymentReleaseMetadata(ctx context.Context, id int64, metadata ReleaseMetadata) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET requested_ref = ?, source_revision = ?, source_fingerprint = ?, artifact_digest = ?, runtime = ?, internal_port = ?, health_endpoint = ?, manifest = ?, configuration_digest = ? WHERE id = ?`,
		metadata.RequestedRef, metadata.SourceRevision, metadata.SourceFingerprint, metadata.ArtifactDigest, metadata.Runtime, metadata.InternalPort, metadata.HealthEndpoint, metadata.Manifest, metadata.ConfigurationDigest, id)
	return err
}

// SetDeploymentFailure records a normalized cause separately from the human
// log, so callers never have to infer the failed phase from free-form text.
func (s *Store) SetDeploymentFailure(ctx context.Context, id int64, stage, code, detail string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET failure_stage = ?, failure_code = ?, failure_detail = ? WHERE id = ?`, stage, code, detail, id)
	return err
}

func (s *Store) Deployments(ctx context.Context, applicationID int64) ([]Deployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, application_id, status, image, requested_ref, source_revision, source_fingerprint, artifact_digest, runtime, internal_port, health_endpoint, manifest, configuration_digest, failure_stage, failure_code, failure_detail, log_path, started_at, finished_at, action, attempt, heartbeat_at, lease_expires_at, cancellation_requested_at, queue_duration_ms, source_duration_ms, build_duration_ms, start_duration_ms, health_duration_ms, route_duration_ms, current_stage FROM deployments WHERE application_id = ? ORDER BY id DESC`, applicationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Deployment{}
	for rows.Next() {
		var d Deployment
		var finished, heartbeat, lease, cancellation sql.NullTime
		if err := rows.Scan(&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.RequestedRef, &d.SourceRevision, &d.SourceFingerprint, &d.ArtifactDigest, &d.Runtime, &d.InternalPort, &d.HealthEndpoint, &d.Manifest, &d.ConfigurationDigest, &d.FailureStage, &d.FailureCode, &d.FailureDetail, &d.LogPath, &d.StartedAt, &finished, &d.Action, &d.Attempt, &heartbeat, &lease, &cancellation, &d.QueueDurationMs, &d.SourceDurationMs, &d.BuildDurationMs, &d.StartDurationMs, &d.HealthDurationMs, &d.RouteDurationMs, &d.CurrentStage); err != nil {
			return nil, err
		}
		if finished.Valid {
			d.FinishedAt = finished.Time
		}
		if heartbeat.Valid {
			d.HeartbeatAt = heartbeat.Time
		}
		if lease.Valid {
			d.LeaseExpiresAt = lease.Time
		}
		if cancellation.Valid {
			d.CancellationRequestedAt = cancellation.Time
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
	var finished, heartbeat, lease, cancellation sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id, application_id, status, image, requested_ref, source_revision, source_fingerprint, artifact_digest, runtime, internal_port, health_endpoint, manifest, configuration_digest, failure_stage, failure_code, failure_detail, log_path, started_at, finished_at, action, attempt, heartbeat_at, lease_expires_at, cancellation_requested_at, queue_duration_ms, source_duration_ms, build_duration_ms, start_duration_ms, health_duration_ms, route_duration_ms, current_stage FROM deployments WHERE application_id = ? AND id = ?`, applicationID, id).Scan(
		&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.RequestedRef, &d.SourceRevision, &d.SourceFingerprint, &d.ArtifactDigest, &d.Runtime, &d.InternalPort, &d.HealthEndpoint, &d.Manifest, &d.ConfigurationDigest, &d.FailureStage, &d.FailureCode, &d.FailureDetail, &d.LogPath, &d.StartedAt, &finished, &d.Action, &d.Attempt, &heartbeat, &lease, &cancellation, &d.QueueDurationMs, &d.SourceDurationMs, &d.BuildDurationMs, &d.StartDurationMs, &d.HealthDurationMs, &d.RouteDurationMs, &d.CurrentStage)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if finished.Valid {
		d.FinishedAt = finished.Time
	}
	if heartbeat.Valid {
		d.HeartbeatAt = heartbeat.Time
	}
	if lease.Valid {
		d.LeaseExpiresAt = lease.Time
	}
	if cancellation.Valid {
		d.CancellationRequestedAt = cancellation.Time
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
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON;"); err != nil {
		return err
	}

	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}

	// If version is 0, check if settings table exists to determine if we are migrating an existing unversioned database
	if version == 0 {
		var count int
		err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='settings'").Scan(&count)
		if err != nil {
			return err
		}
		if count > 0 {
			// This is an existing database that was running the unversioned migrations.
			// Run all the migration steps (catching duplicate errors) to make sure they are up-to-date,
			// then set the current user_version.
			if err := s.applyMigrationV1(ctx); err != nil {
				return err
			}
			if err := s.applyMigrationV2(ctx); err != nil {
				return err
			}
			if err := s.applyMigrationV3(ctx); err != nil {
				return err
			}
			if err := s.applyMigrationV4(ctx); err != nil {
				return err
			}
			if err := s.applyMigrationV5(ctx); err != nil {
				return err
			}
			if err := s.applyMigrationV6(ctx); err != nil {
				return err
			}
			if err := s.applyMigrationV7(ctx); err != nil {
				return err
			}
			if err := s.applyMigrationV8(ctx); err != nil {
				return err
			}
			if err := s.applyMigrationV9(ctx); err != nil {
				return err
			}
			if err := s.applyMigrationV10(ctx); err != nil {
				return err
			}
			if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 10;"); err != nil {
				return err
			}
			return nil
		}
	}

	// Sequential migrations
	if version < 1 {
		if err := s.applyMigrationV1(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 1;"); err != nil {
			return err
		}
	}
	if version < 2 {
		if err := s.applyMigrationV2(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 2;"); err != nil {
			return err
		}
	}
	if version < 3 {
		if err := s.applyMigrationV3(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 3;"); err != nil {
			return err
		}
	}
	if version < 4 {
		if err := s.applyMigrationV4(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 4;"); err != nil {
			return err
		}
	}
	if version < 5 {
		if err := s.applyMigrationV5(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 5;"); err != nil {
			return err
		}
	}
	if version < 6 {
		if err := s.applyMigrationV6(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 6;"); err != nil {
			return err
		}
	}
	if version < 7 {
		if err := s.applyMigrationV7(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 7;"); err != nil {
			return err
		}
	}
	if version < 8 {
		if err := s.applyMigrationV8(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 8;"); err != nil {
			return err
		}
	}
	if version < 9 {
		if err := s.applyMigrationV9(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 9;"); err != nil {
			return err
		}
	}
	if version < 10 {
		if err := s.applyMigrationV10(ctx); err != nil {
			return err
		}
		if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 10;"); err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) applyMigrationV1(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
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
			finished_at DATETIME
		);
	`)
	return err
}

func (s *Store) applyMigrationV2(ctx context.Context) error {
	// Alter applications
	for _, statement := range []string{
		`ALTER TABLE applications ADD COLUMN runtime TEXT NOT NULL DEFAULT 'auto'`,
		`ALTER TABLE applications ADD COLUMN deploy_on_push BOOLEAN NOT NULL DEFAULT 1`,
		`ALTER TABLE applications ADD COLUMN require_confirmation BOOLEAN NOT NULL DEFAULT 1`,
		`ALTER TABLE applications ADD COLUMN auto_rollback BOOLEAN NOT NULL DEFAULT 0`,
		`ALTER TABLE applications ADD COLUMN github_installation_id INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE applications ADD COLUMN health_endpoint TEXT NOT NULL DEFAULT ''`,
	} {
		_, err := s.db.ExecContext(ctx, statement)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}

	// Alter secret_audit
	_, err := s.db.ExecContext(ctx, `ALTER TABLE secret_audit ADD COLUMN target TEXT NOT NULL DEFAULT 'runtime'`)
	if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
		return err
	}

	// Alter deployments
	for _, statement := range []string{
		`ALTER TABLE deployments ADD COLUMN action TEXT NOT NULL DEFAULT 'deploy'`,
		`ALTER TABLE deployments ADD COLUMN requested_ref TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN source_revision TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN source_fingerprint TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN artifact_digest TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN runtime TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN internal_port INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deployments ADD COLUMN health_endpoint TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN manifest TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN configuration_digest TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN failure_stage TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN failure_code TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN failure_detail TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE deployments ADD COLUMN attempt INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deployments ADD COLUMN heartbeat_at DATETIME`,
		`ALTER TABLE deployments ADD COLUMN lease_expires_at DATETIME`,
		`ALTER TABLE deployments ADD COLUMN cancellation_requested_at DATETIME`,
		`ALTER TABLE deployments ADD COLUMN queue_duration_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deployments ADD COLUMN source_duration_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deployments ADD COLUMN build_duration_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deployments ADD COLUMN start_duration_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deployments ADD COLUMN health_duration_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deployments ADD COLUMN route_duration_ms INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE deployments ADD COLUMN current_stage TEXT NOT NULL DEFAULT ''`,
	} {
		_, err = s.db.ExecContext(ctx, statement)
		if err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigrationV3(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS deployment_events (
			id INTEGER PRIMARY KEY,
			deployment_id INTEGER NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
			stage TEXT NOT NULL,
			event_type TEXT NOT NULL,
			message TEXT NOT NULL,
			created_at DATETIME NOT NULL
		);
		CREATE TABLE IF NOT EXISTS alerts (
			id INTEGER PRIMARY KEY,
			severity TEXT NOT NULL,
			message TEXT NOT NULL,
			application_id INTEGER REFERENCES applications(id) ON DELETE CASCADE,
			deployment_id INTEGER REFERENCES deployments(id) ON DELETE SET NULL,
			resolved BOOLEAN NOT NULL DEFAULT 0,
			created_at DATETIME NOT NULL,
			resolved_at DATETIME
		);
		CREATE INDEX IF NOT EXISTS deployment_events_deployment_id ON deployment_events(deployment_id);
		CREATE INDEX IF NOT EXISTS alerts_application_id ON alerts(application_id);
	`)
	if err != nil {
		return err
	}

	// Data fix: A webhook cannot provide an interactive production confirmation. Older
	// records may contain both flags, so migrate them to the safe manual mode.
	if _, err = s.db.ExecContext(ctx, `UPDATE applications SET deploy_on_push = FALSE WHERE deploy_on_push = TRUE AND require_confirmation = TRUE`); err != nil {
		return err
	}

	// Create partial index
	_, err = s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS one_active_deployment_per_application ON deployments(application_id) WHERE status IN ('queued', 'running')`)
	return err
}

func (s *Store) applyMigrationV4(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS application_health (
			application_id INTEGER PRIMARY KEY REFERENCES applications(id) ON DELETE CASCADE,
			internal_status TEXT NOT NULL DEFAULT '', internal_checked_at DATETIME,
			route_status TEXT NOT NULL DEFAULT '', route_checked_at DATETIME,
			external_status TEXT NOT NULL DEFAULT '', external_checked_at DATETIME,
			external_observer TEXT NOT NULL DEFAULT '', external_http_status INTEGER NOT NULL DEFAULT 0,
			external_detail TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS route_probes (
			id INTEGER PRIMARY KEY, application_id INTEGER NOT NULL REFERENCES applications(id) ON DELETE CASCADE,
			success BOOLEAN NOT NULL, checked_at DATETIME NOT NULL, observer TEXT NOT NULL,
			http_status INTEGER NOT NULL DEFAULT 0, detail TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS route_probes_checked_at ON route_probes(checked_at);
	`)
	return err
}

// Webhook delivery IDs are provider-issued, opaque replay guards. Store only
// their SHA-256 fingerprint so the database does not become an event archive.
func (s *Store) applyMigrationV5(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS webhook_deliveries (
			provider TEXT NOT NULL, delivery_hash TEXT NOT NULL,
			received_at DATETIME NOT NULL,
			PRIMARY KEY(provider, delivery_hash)
		);
	`)
	return err
}

// applyMigrationV6 introduces provider-neutral OIDC identities and RBAC. OIDC
// client secrets are intentionally not stored here; deployments receive them
// from the configured secret source at startup.
func (s *Store) applyMigrationV6(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS identity_providers (
			id TEXT PRIMARY KEY,
			issuer TEXT NOT NULL UNIQUE,
			client_id TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL
		);
		CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY,
			email TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			disabled_at DATETIME
		);
		CREATE TABLE IF NOT EXISTS federated_identities (
			provider_id TEXT NOT NULL REFERENCES identity_providers(id),
			subject TEXT NOT NULL,
			user_id INTEGER NOT NULL REFERENCES users(id),
			created_at DATETIME NOT NULL,
			PRIMARY KEY(provider_id, subject)
		);
		CREATE TABLE IF NOT EXISTS role_bindings (
			user_id INTEGER NOT NULL REFERENCES users(id),
			role TEXT NOT NULL CHECK(role IN ('viewer','operator','deployer','admin','security_admin')),
			created_at DATETIME NOT NULL,
			PRIMARY KEY(user_id, role)
		);
		CREATE TABLE IF NOT EXISTS identity_audit (
			id INTEGER PRIMARY KEY,
			user_id INTEGER REFERENCES users(id),
			action TEXT NOT NULL,
			detail TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL
		);
	`)
	return err
}

// applyMigrationV7 adds observed remote agents. There is deliberately no
// desired-release table yet: a node registration must not grant deployment
// authority by itself.
func (s *Store) applyMigrationV7(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS nodes (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			version TEXT NOT NULL,
			capabilities TEXT NOT NULL DEFAULT '[]',
			certificate_fingerprint TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			last_seen_at DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS nodes_last_seen_at ON nodes(last_seen_at);
	`)
	return err
}

// applyMigrationV8 persists narrow webhook burst limits. The scope deliberately
// identifies an application, not an untrusted client address or forwarded IP.
func (s *Store) applyMigrationV8(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS webhook_rate_limits (
			scope TEXT PRIMARY KEY,
			window_started_at DATETIME NOT NULL,
			requests INTEGER NOT NULL CHECK(requests >= 0)
		);
	`)
	return err
}

// applyMigrationV9 makes password lockouts survive a process restart. Scope
// receives a one-way origin fingerprint from the HTTP layer, never a forwarded
// header or a raw address persisted in the database.
func (s *Store) applyMigrationV9(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS unlock_attempts (
			scope TEXT PRIMARY KEY,
			failed_attempts INTEGER NOT NULL DEFAULT 0 CHECK(failed_attempts >= 0),
			lockout_until DATETIME
		);
	`)
	return err
}

func (s *Store) applyMigrationV10(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS cloudflare_config (
			id INTEGER PRIMARY KEY CHECK(id = 1),
			mode TEXT NOT NULL DEFAULT 'disabled',
			account_id TEXT NOT NULL DEFAULT '',
			tunnel_id TEXT NOT NULL DEFAULT '',
			tunnel_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'disabled',
			last_health_check DATETIME,
			error_message TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NOT NULL
		);
		INSERT OR IGNORE INTO cloudflare_config (id, mode, account_id, tunnel_id, tunnel_name, status, updated_at)
		VALUES (1, 'disabled', '', '', '', 'disabled', CURRENT_TIMESTAMP);
	`)
	return err
}

func (s *Store) CloudflareConfig(ctx context.Context) (CloudflareConfig, error) {
	var cfg CloudflareConfig
	var lastHealth sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT mode, account_id, tunnel_id, tunnel_name, status, last_health_check, error_message, updated_at
		FROM cloudflare_config WHERE id = 1
	`).Scan(&cfg.Mode, &cfg.AccountID, &cfg.TunnelID, &cfg.TunnelName, &cfg.Status, &lastHealth, &cfg.ErrorMessage, &cfg.UpdatedAt)
	if err == sql.ErrNoRows {
		return CloudflareConfig{Mode: "disabled", Status: "disabled", UpdatedAt: time.Now().UTC()}, nil
	}
	if err != nil {
		return cfg, err
	}
	if lastHealth.Valid {
		cfg.LastHealthCheck = lastHealth.Time
	}
	return cfg, nil
}

func (s *Store) SaveCloudflareConfig(ctx context.Context, cfg CloudflareConfig) error {
	now := time.Now().UTC()
	var lastHealth interface{} = nil
	if !cfg.LastHealthCheck.IsZero() {
		lastHealth = cfg.LastHealthCheck
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO cloudflare_config (id, mode, account_id, tunnel_id, tunnel_name, status, last_health_check, error_message, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			mode = excluded.mode,
			account_id = excluded.account_id,
			tunnel_id = excluded.tunnel_id,
			tunnel_name = excluded.tunnel_name,
			status = excluded.status,
			last_health_check = excluded.last_health_check,
			error_message = excluded.error_message,
			updated_at = excluded.updated_at
	`, cfg.Mode, cfg.AccountID, cfg.TunnelID, cfg.TunnelName, cfg.Status, lastHealth, cfg.ErrorMessage, now)
	return err
}

func (s *Store) UpdateCloudflareStatus(ctx context.Context, status, errorMessage string) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE cloudflare_config
		SET status = ?, error_message = ?, last_health_check = ?, updated_at = ?
		WHERE id = 1
	`, status, errorMessage, now, now)
	return err
}

// UpsertNode records the heartbeat only if the already enrolled node presents
// the same mTLS certificate fingerprint. Re-enrollment is an explicit future
// administrative workflow; a changed certificate cannot silently take over an
// existing node ID.
func (s *Store) UpsertNode(ctx context.Context, node Node) error {
	if node.ID == "" || node.Name == "" || node.Version == "" || node.CertificateFingerprint == "" {
		return errors.New("node identity, name, version and certificate fingerprint are required")
	}
	capabilities, err := json.Marshal(node.Capabilities)
	if err != nil {
		return fmt.Errorf("encode node capabilities: %w", err)
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `
		UPDATE nodes SET name = ?, version = ?, capabilities = ?, last_seen_at = ?
		WHERE id = ? AND certificate_fingerprint = ?`,
		node.Name, node.Version, string(capabilities), now, node.ID, node.CertificateFingerprint)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		return nil
	}
	var fingerprint string
	err = s.db.QueryRowContext(ctx, `SELECT certificate_fingerprint FROM nodes WHERE id = ?`, node.ID).Scan(&fingerprint)
	if err == nil {
		return errors.New("node certificate fingerprint does not match enrolled identity")
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO nodes(id, name, version, capabilities, certificate_fingerprint, created_at, last_seen_at) VALUES(?, ?, ?, ?, ?, ?, ?)`,
		node.ID, node.Name, node.Version, string(capabilities), node.CertificateFingerprint, now, now)
	return err
}

func (s *Store) Nodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, version, capabilities, certificate_fingerprint, created_at, last_seen_at FROM nodes ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []Node
	for rows.Next() {
		var node Node
		var capabilities string
		if err := rows.Scan(&node.ID, &node.Name, &node.Version, &capabilities, &node.CertificateFingerprint, &node.CreatedAt, &node.LastSeenAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(capabilities), &node.Capabilities); err != nil {
			return nil, fmt.Errorf("decode node capabilities: %w", err)
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// UpsertFederatedIdentity attaches an immutable (provider, subject) identity
// to a local user. E-mail is profile data only and is never used as identity.
func (s *Store) UpsertFederatedIdentity(ctx context.Context, identity FederatedIdentity) (int64, error) {
	if identity.ProviderID == "" || identity.Subject == "" {
		return 0, errors.New("provider and subject are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var userID int64
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM federated_identities WHERE provider_id = ? AND subject = ?`, identity.ProviderID, identity.Subject).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO users(email, display_name, created_at) VALUES(?, ?, ?)`, identity.Email, identity.DisplayName, time.Now().UTC())
		if insertErr != nil {
			return 0, insertErr
		}
		userID, err = result.LastInsertId()
		if err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO federated_identities(provider_id, subject, user_id, created_at) VALUES(?, ?, ?, ?)`, identity.ProviderID, identity.Subject, userID, time.Now().UTC()); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	} else if _, err = tx.ExecContext(ctx, `UPDATE users SET email = ?, display_name = ? WHERE id = ?`, identity.Email, identity.DisplayName, userID); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO identity_audit(user_id, action, created_at) VALUES(?, 'federated_login', ?)`, userID, time.Now().UTC()); err != nil {
		return 0, err
	}
	return userID, tx.Commit()
}

func (s *Store) SetRole(ctx context.Context, userID int64, role string) error {
	if role != RoleViewer && role != RoleOperator && role != RoleDeployer && role != RoleAdmin && role != RoleSecurityAdmin {
		return errors.New("invalid role")
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO role_bindings(user_id, role, created_at) VALUES(?, ?, ?)`, userID, role, time.Now().UTC())
	return err
}

// ClaimWebhookDelivery returns false when a valid provider delivery was
// already accepted. It is durable across process restarts and does not expose
// the original delivery identifier in SQLite or logs.
func (s *Store) ClaimWebhookDelivery(ctx context.Context, provider, deliveryHash string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `INSERT INTO webhook_deliveries(provider, delivery_hash, received_at) VALUES(?, ?, ?) ON CONFLICT(provider, delivery_hash) DO NOTHING`, provider, deliveryHash, time.Now().UTC())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// AllowWebhookRequest consumes one persisted request from scope's fixed time
// window. It is called only after signature verification and replay handling;
// unauthenticated traffic must not turn the database into a rate-limit store.
func (s *Store) AllowWebhookRequest(ctx context.Context, scope string, limit int, window time.Duration) (bool, error) {
	if scope == "" || limit < 1 || window <= 0 {
		return false, errors.New("invalid webhook rate limit")
	}
	now := time.Now().UTC()
	cutoff := now.Add(-window)
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_rate_limits(scope, window_started_at, requests) VALUES(?, ?, 1)
		ON CONFLICT(scope) DO UPDATE SET
			window_started_at = CASE WHEN webhook_rate_limits.window_started_at <= ? THEN excluded.window_started_at ELSE webhook_rate_limits.window_started_at END,
			requests = CASE WHEN webhook_rate_limits.window_started_at <= ? THEN 1 ELSE webhook_rate_limits.requests + 1 END`,
		scope, now, cutoff, cutoff)
	if err != nil {
		return false, err
	}
	var requests int
	if err := s.db.QueryRowContext(ctx, `SELECT requests FROM webhook_rate_limits WHERE scope = ?`, scope).Scan(&requests); err != nil {
		return false, err
	}
	return requests <= limit, nil
}

// UnlockLockoutUntil returns the active lockout for one opaque origin scope.
func (s *Store) UnlockLockoutUntil(ctx context.Context, scope string, now time.Time) (time.Time, error) {
	if scope == "" {
		return time.Time{}, errors.New("unlock scope is required")
	}
	var until sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT lockout_until FROM unlock_attempts WHERE scope = ?`, scope).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	if until.Valid && until.Time.After(now) {
		return until.Time, nil
	}
	return time.Time{}, nil
}

// RegisterFailedUnlock starts a five-minute lockout after five failures for
// the same scope. It is deliberately persisted before the handler replies.
func (s *Store) RegisterFailedUnlock(ctx context.Context, scope string, now time.Time) (time.Time, error) {
	if scope == "" {
		return time.Time{}, errors.New("unlock scope is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback()
	var failures int
	var until sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT failed_attempts, lockout_until FROM unlock_attempts WHERE scope = ?`, scope).Scan(&failures, &until)
	if errors.Is(err, sql.ErrNoRows) {
		failures, until.Valid = 0, false
	} else if err != nil {
		return time.Time{}, err
	}
	if until.Valid && until.Time.After(now) {
		return until.Time, tx.Commit()
	}
	failures++
	lockedUntil := time.Time{}
	if failures >= 5 {
		lockedUntil = now.Add(5 * time.Minute)
		failures = 0
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO unlock_attempts(scope, failed_attempts, lockout_until) VALUES(?, ?, ?)
		ON CONFLICT(scope) DO UPDATE SET failed_attempts = excluded.failed_attempts, lockout_until = excluded.lockout_until`, scope, failures, nullTime(lockedUntil))
	if err != nil {
		return time.Time{}, err
	}
	return lockedUntil, tx.Commit()
}

// ClearUnlockFailures removes expired or successful-attempt state.
func (s *Store) ClearUnlockFailures(ctx context.Context, scope string) error {
	if scope == "" {
		return errors.New("unlock scope is required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM unlock_attempts WHERE scope = ?`, scope)
	return err
}

func (s *Store) UpsertHealthEvidence(ctx context.Context, evidence HealthEvidence) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO application_health(application_id, internal_status, internal_checked_at, route_status, route_checked_at, external_status, external_checked_at, external_observer, external_http_status, external_detail)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(application_id) DO UPDATE SET internal_status=excluded.internal_status, internal_checked_at=excluded.internal_checked_at, route_status=excluded.route_status, route_checked_at=excluded.route_checked_at, external_status=excluded.external_status, external_checked_at=excluded.external_checked_at, external_observer=excluded.external_observer, external_http_status=excluded.external_http_status, external_detail=excluded.external_detail`,
		evidence.ApplicationID, evidence.InternalStatus, nullTime(evidence.InternalCheckedAt), evidence.RouteStatus, nullTime(evidence.RouteCheckedAt), evidence.ExternalStatus, nullTime(evidence.ExternalCheckedAt), evidence.ExternalObserver, evidence.ExternalHTTPStatus, evidence.ExternalDetail)
	return err
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (s *Store) HealthEvidence(ctx context.Context, applicationID int64) (HealthEvidence, error) {
	var item HealthEvidence
	err := s.db.QueryRowContext(ctx, `SELECT application_id, internal_status, internal_checked_at, route_status, route_checked_at, external_status, external_checked_at, external_observer, external_http_status, external_detail FROM application_health WHERE application_id = ?`, applicationID).Scan(&item.ApplicationID, &item.InternalStatus, &item.InternalCheckedAt, &item.RouteStatus, &item.RouteCheckedAt, &item.ExternalStatus, &item.ExternalCheckedAt, &item.ExternalObserver, &item.ExternalHTTPStatus, &item.ExternalDetail)
	if errors.Is(err, sql.ErrNoRows) {
		return HealthEvidence{ApplicationID: applicationID}, nil
	}
	return item, err
}

func (s *Store) RecordRouteProbe(ctx context.Context, applicationID int64, success bool, checkedAt time.Time, observer string, status int, detail string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO route_probes(application_id, success, checked_at, observer, http_status, detail) VALUES(?, ?, ?, ?, ?, ?)`, applicationID, success, checkedAt, observer, status, detail)
	return err
}

func (s *Store) RoutingSLO(ctx context.Context, since time.Time) (float64, error) {
	var total, successful int64
	if err := s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(CASE WHEN success THEN 1 ELSE 0 END), 0) FROM route_probes WHERE checked_at >= ?`, since).Scan(&total, &successful); err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, nil
	}
	return float64(successful) * 100 / float64(total), nil
}

func (s *Store) SetDeploymentStage(ctx context.Context, id int64, stage string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET current_stage = ? WHERE id = ?`, stage, id)
	return err
}

func (s *Store) SetDeploymentStageDurations(ctx context.Context, id int64, queue, source, build, start, health, route int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE deployments SET queue_duration_ms = ?, source_duration_ms = ?, build_duration_ms = ?, start_duration_ms = ?, health_duration_ms = ?, route_duration_ms = ? WHERE id = ?`, queue, source, build, start, health, route, id)
	return err
}

func (s *Store) AddDeploymentEvent(ctx context.Context, deploymentID int64, stage, eventType, message string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO deployment_events(deployment_id, stage, event_type, message, created_at) VALUES(?, ?, ?, ?, ?)`,
		deploymentID, stage, eventType, message, time.Now().UTC())
	return err
}

func (s *Store) DeploymentEvents(ctx context.Context, deploymentID int64) ([]DeploymentEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, deployment_id, stage, event_type, message, created_at FROM deployment_events WHERE deployment_id = ? ORDER BY id ASC`, deploymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DeploymentEvent{}
	for rows.Next() {
		var e DeploymentEvent
		if err := rows.Scan(&e.ID, &e.DeploymentID, &e.Stage, &e.EventType, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, e)
	}
	return result, rows.Err()
}

func (s *Store) LatestFailedDeployment(ctx context.Context, applicationID int64) (Deployment, error) {
	var d Deployment
	var finished, heartbeat, lease, cancellation sql.NullTime
	err := s.db.QueryRowContext(ctx, `SELECT id, application_id, status, image, requested_ref, source_revision, source_fingerprint, artifact_digest, runtime, internal_port, health_endpoint, manifest, configuration_digest, failure_stage, failure_code, failure_detail, log_path, started_at, finished_at, action, attempt, heartbeat_at, lease_expires_at, cancellation_requested_at, queue_duration_ms, source_duration_ms, build_duration_ms, start_duration_ms, health_duration_ms, route_duration_ms, current_stage FROM deployments WHERE application_id = ? AND status = 'failed' ORDER BY id DESC LIMIT 1`, applicationID).Scan(
		&d.ID, &d.ApplicationID, &d.Status, &d.Image, &d.RequestedRef, &d.SourceRevision, &d.SourceFingerprint, &d.ArtifactDigest, &d.Runtime, &d.InternalPort, &d.HealthEndpoint, &d.Manifest, &d.ConfigurationDigest, &d.FailureStage, &d.FailureCode, &d.FailureDetail, &d.LogPath, &d.StartedAt, &finished, &d.Action, &d.Attempt, &heartbeat, &lease, &cancellation, &d.QueueDurationMs, &d.SourceDurationMs, &d.BuildDurationMs, &d.StartDurationMs, &d.HealthDurationMs, &d.RouteDurationMs, &d.CurrentStage)
	if errors.Is(err, sql.ErrNoRows) {
		return Deployment{}, ErrNotFound
	}
	if finished.Valid {
		d.FinishedAt = finished.Time
	}
	if heartbeat.Valid {
		d.HeartbeatAt = heartbeat.Time
	}
	if lease.Valid {
		d.LeaseExpiresAt = lease.Time
	}
	if cancellation.Valid {
		d.CancellationRequestedAt = cancellation.Time
	}
	return d, nil
}

func (s *Store) GetActiveAlerts(ctx context.Context) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, severity, message, application_id, deployment_id, resolved, created_at, resolved_at FROM alerts WHERE resolved = 0 ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Alert{}
	for rows.Next() {
		var a Alert
		var resolvedAt sql.NullTime
		if err := rows.Scan(&a.ID, &a.Severity, &a.Message, &a.ApplicationID, &a.DeploymentID, &a.Resolved, &a.CreatedAt, &resolvedAt); err != nil {
			return nil, err
		}
		if resolvedAt.Valid {
			a.ResolvedAt = resolvedAt.Time
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) GetResolvedAlerts(ctx context.Context) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, severity, message, application_id, deployment_id, resolved, created_at, resolved_at FROM alerts WHERE resolved = 1 ORDER BY resolved_at DESC LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Alert{}
	for rows.Next() {
		var a Alert
		var resolvedAt sql.NullTime
		if err := rows.Scan(&a.ID, &a.Severity, &a.Message, &a.ApplicationID, &a.DeploymentID, &a.Resolved, &a.CreatedAt, &resolvedAt); err != nil {
			return nil, err
		}
		if resolvedAt.Valid {
			a.ResolvedAt = resolvedAt.Time
		}
		result = append(result, a)
	}
	return result, rows.Err()
}

func (s *Store) UpsertAlert(ctx context.Context, severity, message string, appID *int64, depID *int64) error {
	// Look for existing unresolved alert
	var exists bool
	var err error
	if appID != nil {
		err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM alerts WHERE resolved = 0 AND message = ? AND application_id = ?)`, message, *appID).Scan(&exists)
	} else {
		err = s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM alerts WHERE resolved = 0 AND message = ? AND application_id IS NULL)`, message).Scan(&exists)
	}
	if err != nil {
		return err
	}
	if exists {
		return nil // Deduplicated!
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO alerts(severity, message, application_id, deployment_id, resolved, created_at) VALUES(?, ?, ?, ?, 0, ?)`,
		severity, message, appID, depID, time.Now().UTC())
	return err
}

func (s *Store) ResolveAlert(ctx context.Context, appID *int64, messagePrefix string) error {
	now := time.Now().UTC()
	if appID != nil {
		_, err := s.db.ExecContext(ctx, `UPDATE alerts SET resolved = 1, resolved_at = ? WHERE resolved = 0 AND application_id = ? AND message LIKE ?`, now, *appID, messagePrefix+"%")
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET resolved = 1, resolved_at = ? WHERE resolved = 0 AND application_id IS NULL AND message LIKE ?`, now, messagePrefix+"%")
	return err
}

func (s *Store) ResolveAllAlertsExcept(ctx context.Context, activeAlertMessages []string) error {
	now := time.Now().UTC()
	if len(activeAlertMessages) == 0 {
		_, err := s.db.ExecContext(ctx, `UPDATE alerts SET resolved = 1, resolved_at = ? WHERE resolved = 0`, now)
		return err
	}
	placeholders := strings.TrimRight(strings.Repeat("?,", len(activeAlertMessages)), ",")
	args := make([]any, 0, len(activeAlertMessages)+1)
	args = append(args, now)
	for _, m := range activeAlertMessages {
		args = append(args, m)
	}
	_, err := s.db.ExecContext(ctx, "UPDATE alerts SET resolved = 1, resolved_at = ? WHERE resolved = 0 AND message NOT IN ("+placeholders+")", args...)
	return err
}

func (s *Store) Backup(ctx context.Context, destPath string) error {
	escapedPath := strings.ReplaceAll(destPath, "'", "''")
	_, err := s.db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s'", escapedPath))
	return err
}

// Snapshot returns a consistent SQLite serialization without materializing a
// temporary database file. The modernc driver exposes SQLite's serialize API
// through database/sql Raw; callers must zero the returned bytes when done.
func (s *Store) Snapshot(ctx context.Context) ([]byte, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var snapshot []byte
	err = conn.Raw(func(driverConn any) error {
		serializer, ok := driverConn.(interface{ Serialize() ([]byte, error) })
		if !ok {
			return errors.New("SQLite driver does not support serialization")
		}
		var serializeErr error
		snapshot, serializeErr = serializer.Serialize()
		return serializeErr
	})
	if err != nil {
		return nil, fmt.Errorf("serialize SQLite database: %w", err)
	}
	if len(snapshot) == 0 {
		return nil, errors.New("empty SQLite snapshot")
	}
	return snapshot, nil
}

// IntegrityCheck verifies the SQLite file before it is accepted as a restore.
func (s *Store) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := s.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("sqlite integrity check: %s", result)
	}
	return nil
}

func (s *Store) BuildDurationSLO(ctx context.Context, thresholdMs int64) (float64, error) {
	var total, compliant int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments WHERE action = 'deploy' AND status = 'successful' AND build_duration_ms > 0`).Scan(&total)
	if err != nil {
		return 0, err
	}
	if total == 0 {
		return 100.0, nil
	}
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments WHERE action = 'deploy' AND status = 'successful' AND build_duration_ms <= ? AND build_duration_ms > 0`, thresholdMs).Scan(&compliant)
	if err != nil {
		return 0, err
	}
	return float64(compliant) * 100.0 / float64(total), nil
}
