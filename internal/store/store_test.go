package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestSecurityConfigurationAndSecrets(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	initialized, err := database.IsInitialized(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initialized {
		t.Fatal("database should not be initialized")
	}

	config := SecurityConfig{Salt: []byte("salt-0123456789"), VerifierNonce: []byte("nonce"), VerifierCipher: []byte("ciphertext")}
	if err := database.InitializeSecurity(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	stored, err := database.SecurityConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(stored.Salt) != string(config.Salt) {
		t.Fatal("security salt was not preserved")
	}

	if err := database.PutSecret(context.Background(), "app:example", "token", []byte("nonce"), []byte("ciphertext")); err != nil {
		t.Fatal(err)
	}
	nonce, ciphertext, err := database.Secret(context.Background(), "app:example", "token")
	if err != nil {
		t.Fatal(err)
	}
	if string(nonce) != "nonce" || string(ciphertext) != "ciphertext" {
		t.Fatal("secret payload was not preserved")
	}
}

func TestCreateAndListApplications(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	created, err := database.CreateApplication(context.Background(), Application{
		Name: "web", Repository: "https://github.com/example/web", Branch: "main", WorkDir: ".",
		Type: "static", BuildCommand: "npm run build", RunCommand: "caddy run", InternalPort: 8080, Domain: "web.example.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID == 0 {
		t.Fatal("application ID was not assigned")
	}

	applications, err := database.Applications(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(applications) != 1 || applications[0].Name != "web" || applications[0].InternalPort != 8080 {
		t.Fatalf("unexpected applications: %#v", applications)
	}
}

func TestHealthEvidenceAndRoutingSLO(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	app, err := database.CreateApplication(context.Background(), Application{Name: "web", Repository: "file:///web", Branch: "main", WorkDir: ".", Type: "static", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "web.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	evidence := HealthEvidence{ApplicationID: app.ID, InternalStatus: "healthy", InternalCheckedAt: now, RouteStatus: "applied", RouteCheckedAt: now, ExternalStatus: "successful", ExternalCheckedAt: now, ExternalObserver: "caddy via https://proxy", ExternalHTTPStatus: 200}
	if err := database.UpsertHealthEvidence(context.Background(), evidence); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordRouteProbe(context.Background(), app.ID, true, now, evidence.ExternalObserver, 200, ""); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordRouteProbe(context.Background(), app.ID, false, now, evidence.ExternalObserver, 502, "bad gateway"); err != nil {
		t.Fatal(err)
	}
	got, err := database.HealthEvidence(context.Background(), app.ID)
	if err != nil || got.InternalStatus != "healthy" || got.RouteStatus != "applied" || got.ExternalHTTPStatus != 200 {
		t.Fatalf("health evidence = %#v, %v", got, err)
	}
	slo, err := database.RoutingSLO(context.Background(), now.Add(-time.Minute))
	if err != nil || slo != 50 {
		t.Fatalf("routing SLO = %v, %v; want 50", slo, err)
	}
}

func TestAutomationRejectsAnUnattendedDeploymentThatRequiresConfirmation(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	_, err = database.CreateApplication(t.Context(), Application{Name: "web", Repository: "https://github.com/example/web", Branch: "main", WorkDir: ".", Type: "static", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "web.example.test", DeployOnPush: true, RequireConfirmation: true})
	if err == nil {
		t.Fatal("incompatible automation settings were accepted")
	}
}

func TestDeploymentHistory(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := database.CreateApplication(context.Background(), Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "go build", RunCommand: "./api", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := database.StartDeployment(context.Background(), application.ID, "", "deploy.log")
	if err != nil {
		t.Fatal(err)
	}
	if err = database.SetDeploymentImage(context.Background(), deployment.ID, "minidock/api:1"); err != nil {
		t.Fatal(err)
	}
	if err = database.FinishDeployment(context.Background(), deployment.ID, "successful"); err != nil {
		t.Fatal(err)
	}
	history, err := database.Deployments(context.Background(), application.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Status != "successful" || history[0].Image != "minidock/api:1" || history[0].FinishedAt.IsZero() {
		t.Fatalf("unexpected deployment history: %#v", history)
	}
}

func TestDeploymentPersistsProviderNeutralReleaseMetadataAndFailure(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := database.CreateApplication(t.Context(), Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "go build", RunCommand: "./api", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	release, err := database.QueueDeployment(t.Context(), application.ID, "deploy", "deploy.log")
	if err != nil {
		t.Fatal(err)
	}
	metadata := ReleaseMetadata{RequestedRef: "main", SourceRevision: "a1b2c3", ArtifactDigest: "sha256:abc", Runtime: "docker", InternalPort: 8080, HealthEndpoint: "/healthz", Manifest: `{"version":1}`, ConfigurationDigest: "sha256:def"}
	if err = database.SetDeploymentReleaseMetadata(t.Context(), release.ID, metadata); err != nil {
		t.Fatal(err)
	}
	if err = database.SetDeploymentFailure(t.Context(), release.ID, "build", "build_failed", "exit status 1"); err != nil {
		t.Fatal(err)
	}
	stored, err := database.Deployment(t.Context(), application.ID, release.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RequestedRef != metadata.RequestedRef || stored.SourceRevision != metadata.SourceRevision || stored.ArtifactDigest != metadata.ArtifactDigest || stored.Runtime != metadata.Runtime || stored.HealthEndpoint != metadata.HealthEndpoint || stored.FailureStage != "build" || stored.FailureCode != "build_failed" {
		t.Fatalf("unexpected release record: %#v", stored)
	}
}

func TestDeploymentQueueAllowsOnlyOneActiveJobPerApplication(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := database.CreateApplication(context.Background(), Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := database.QueueDeployment(context.Background(), application.ID, "deploy", "queued.log")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.QueueDeployment(context.Background(), application.ID, "deploy", "duplicate.log"); err == nil {
		t.Fatal("second active deployment was accepted")
	}
	running, err := database.NextQueuedDeployment(context.Background())
	if err != nil || running.ID != queued.ID || running.Status != "running" {
		t.Fatalf("unexpected queued job: %#v, %v", running, err)
	}
	if err = database.FinishDeployment(context.Background(), running.ID, "successful"); err != nil {
		t.Fatal(err)
	}
	if _, err = database.QueueDeployment(context.Background(), application.ID, "rollback", "rollback.log"); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentLeaseIsClaimedRenewedAndReleased(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := database.CreateApplication(t.Context(), Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := database.QueueDeployment(t.Context(), application.ID, "deploy", "queued.log")
	if err != nil {
		t.Fatal(err)
	}
	running, err := database.NextQueuedDeployment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if running.ID != queued.ID || running.Attempt != 1 || running.HeartbeatAt.IsZero() || running.LeaseExpiresAt.Before(running.HeartbeatAt) {
		t.Fatalf("unexpected claimed lease: %#v", running)
	}
	if err := database.HeartbeatDeployment(t.Context(), running.ID); err != nil {
		t.Fatal(err)
	}
	renewed, err := database.Deployment(t.Context(), application.ID, running.ID)
	if err != nil || renewed.LeaseExpiresAt.Before(renewed.HeartbeatAt) || renewed.HeartbeatAt.Before(running.HeartbeatAt) {
		t.Fatalf("unexpected renewed lease: %#v, %v", renewed, err)
	}
	if err := database.FinishDeployment(t.Context(), running.ID, "successful"); err != nil {
		t.Fatal(err)
	}
	finished, err := database.Deployment(t.Context(), application.ID, running.ID)
	if err != nil || !finished.LeaseExpiresAt.IsZero() {
		t.Fatalf("finished deployment retained a lease: %#v, %v", finished, err)
	}
	if err := database.HeartbeatDeployment(t.Context(), running.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("heartbeat accepted a finished deployment: %v", err)
	}
}

func TestCancelDeploymentStopsQueuedWorkAndSignalsRunningWork(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := database.CreateApplication(t.Context(), Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := database.QueueDeployment(t.Context(), application.ID, "deploy", "queued.log")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CancelDeployment(t.Context(), application.ID, queued.ID); err != nil {
		t.Fatal(err)
	}
	cancelled, err := database.Deployment(t.Context(), application.ID, queued.ID)
	if err != nil || cancelled.Status != "cancelled" || cancelled.FinishedAt.IsZero() {
		t.Fatalf("queued cancellation = %#v, %v", cancelled, err)
	}

	queued, err = database.QueueDeployment(t.Context(), application.ID, "deploy", "running.log")
	if err != nil {
		t.Fatal(err)
	}
	running, err := database.NextQueuedDeployment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := database.CancelDeployment(t.Context(), application.ID, running.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.HeartbeatDeployment(t.Context(), running.ID); !errors.Is(err, ErrDeploymentCancelled) {
		t.Fatalf("running cancellation was not signalled to worker: %v", err)
	}
	if err := database.FinishDeployment(t.Context(), running.ID, "successful"); err != nil {
		t.Fatal(err)
	}
	cancelled, err = database.Deployment(t.Context(), application.ID, running.ID)
	if err != nil || cancelled.Status != "cancelled" || cancelled.CancellationRequestedAt.IsZero() {
		t.Fatalf("running cancellation final state = %#v, %v", cancelled, err)
	}
	if queued.ID != running.ID {
		t.Fatal("unexpected running deployment")
	}
}

func TestRecoverInterruptedDeploymentsFailsOnlyRunningJobs(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := database.CreateApplication(t.Context(), Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	running, err := database.StartDeployment(t.Context(), application.ID, "", "running.log")
	if err != nil {
		t.Fatal(err)
	}
	if err = database.FinishDeployment(t.Context(), running.ID, "successful"); err != nil {
		t.Fatal(err)
	}
	queued, err := database.QueueDeployment(t.Context(), application.ID, "deploy", "queued.log")
	if err != nil {
		t.Fatal(err)
	}
	running, err = database.NextQueuedDeployment(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if count, err := database.RecoverInterruptedDeployments(t.Context()); err != nil || count != 1 {
		t.Fatalf("recovered = %d, %v", count, err)
	}
	recovered, err := database.Deployment(t.Context(), application.ID, running.ID)
	if err != nil || recovered.Status != "failed" || recovered.FailureStage != "start" || recovered.FailureCode != "worker_restarted" || recovered.FinishedAt.IsZero() {
		t.Fatalf("recovered deployment = %#v, %v", recovered, err)
	}
	if queued.ID != running.ID {
		t.Fatal("queued deployment was not the recovered running deployment")
	}
	if _, err = database.QueueDeployment(t.Context(), application.ID, "deploy", "next.log"); err != nil {
		t.Fatalf("application stayed blocked after recovery: %v", err)
	}
}

func TestPreviousSuccessfulDeploymentSkipsCurrentImage(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := database.CreateApplication(context.Background(), Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, image := range []string{"minidock/api:one", "minidock/api:two"} {
		deployment, err := database.StartDeployment(context.Background(), application.ID, image, "deploy.log")
		if err != nil {
			t.Fatal(err)
		}
		if err = database.FinishDeployment(context.Background(), deployment.ID, "successful"); err != nil {
			t.Fatal(err)
		}
	}
	previous, err := database.PreviousSuccessfulDeployment(context.Background(), application.ID)
	if err != nil || previous.Image != "minidock/api:one" {
		t.Fatalf("rollback target = %#v, %v", previous, err)
	}
}

func TestApplicationConfigurationAndBuildSecret(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := database.CreateApplication(context.Background(), Application{Name: "web", Repository: "https://example.test/web", Branch: "main", WorkDir: ".", Type: "static", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "web.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.PutApplicationConfiguration(context.Background(), application.ID, "production", "build", "PUBLIC_URL", "https://example.test"); err != nil {
		t.Fatal(err)
	}
	values, err := database.ApplicationConfiguration(context.Background(), application.ID, "production", "build")
	if err != nil || len(values) != 1 || values[0].Name != "PUBLIC_URL" {
		t.Fatalf("unexpected configuration: %#v, %v", values, err)
	}
	if err := database.PutApplicationSecret(context.Background(), application.ID, "production", "build", "NPM_TOKEN", []byte("nonce"), []byte("ciphertext")); err != nil {
		t.Fatal(err)
	}
	secrets, err := database.ApplicationSecrets(context.Background(), application.ID, "production", "build")
	if err != nil || len(secrets) != 1 || secrets[0].Target != "build" {
		t.Fatalf("unexpected build secrets: %#v, %v", secrets, err)
	}
	if err := database.RecordSecretUse(context.Background(), application.ID, "production", "build", []string{"NPM_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	audit, err := database.SecretAudit(context.Background(), application.ID)
	if err != nil || len(audit) != 2 || audit[0].Action != "used" || audit[0].Target != "build" {
		t.Fatalf("unexpected secret audit: %#v, %v", audit, err)
	}
}

func TestRetentionCandidatesKeepRecentSuccessfulDeployments(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := database.CreateApplication(t.Context(), Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"successful", "successful", "failed"} {
		deployment, err := database.QueueDeployment(t.Context(), application.ID, "deploy", filepath.Join(t.TempDir(), status+".log"))
		if err != nil {
			t.Fatal(err)
		}
		if status == "successful" {
			if err := database.SetDeploymentImage(t.Context(), deployment.ID, "minidock/api:"+strconv.FormatInt(deployment.ID, 10)); err != nil {
				t.Fatal(err)
			}
		}
		if err := database.FinishDeployment(t.Context(), deployment.ID, status); err != nil {
			t.Fatal(err)
		}
	}
	candidates, err := database.RetentionCandidates(t.Context(), application.ID, time.Now().Add(time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 || candidates[0].Status != "failed" || candidates[1].Status != "successful" {
		t.Fatalf("unexpected retention candidates: %#v", candidates)
	}
}

func TestAlertsEventsAndSLO(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	ctx := context.Background()

	app, err := database.CreateApplication(ctx, Application{
		Name: "test-app", Repository: "https://example.test/app", Branch: "main", WorkDir: ".",
		Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "test.local",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = database.UpsertAlert(ctx, "critical", "Low disk space", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = database.UpsertAlert(ctx, "critical", "Low disk space", nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	active, err := database.GetActiveAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].Message != "Low disk space" {
		t.Fatalf("expected 1 active alert, got: %#v", active)
	}

	err = database.UpsertAlert(ctx, "warning", "High latency", &app.ID, nil)
	if err != nil {
		t.Fatal(err)
	}

	active, _ = database.GetActiveAlerts(ctx)
	if len(active) != 2 {
		t.Fatalf("expected 2 active alerts, got: %#v", active)
	}

	err = database.ResolveAlert(ctx, &app.ID, "High latency")
	if err != nil {
		t.Fatal(err)
	}

	active, _ = database.GetActiveAlerts(ctx)
	if len(active) != 1 || active[0].Message != "Low disk space" {
		t.Fatalf("expected 1 active alert remaining, got: %#v", active)
	}

	resolved, err := database.GetResolvedAlerts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 1 || resolved[0].Message != "High latency" {
		t.Fatalf("expected 1 resolved alert, got: %#v", resolved)
	}

	err = database.ResolveAllAlertsExcept(ctx, []string{})
	if err != nil {
		t.Fatal(err)
	}
	active, _ = database.GetActiveAlerts(ctx)
	if len(active) != 0 {
		t.Fatalf("expected 0 active alerts, got: %#v", active)
	}

	deployment, err := database.QueueDeployment(ctx, app.ID, "deploy", "deploy.log")
	if err != nil {
		t.Fatal(err)
	}

	err = database.AddDeploymentEvent(ctx, deployment.ID, "build", "info", "Compiling Go binary")
	if err != nil {
		t.Fatal(err)
	}

	events, err := database.DeploymentEvents(ctx, deployment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Stage != "build" || events[0].EventType != "info" || events[0].Message != "Compiling Go binary" {
		t.Fatalf("unexpected events: %#v", events)
	}

	err = database.SetDeploymentStageDurations(ctx, deployment.ID, 100, 200, 3000, 400, 500, 600)
	if err != nil {
		t.Fatal(err)
	}
	err = database.FinishDeployment(ctx, deployment.ID, "successful")
	if err != nil {
		t.Fatal(err)
	}

	slo, err := database.BuildDurationSLO(ctx, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if slo != 100.0 {
		t.Fatalf("expected 100%% SLO, got %f", slo)
	}

	slo, err = database.BuildDurationSLO(ctx, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if slo != 0.0 {
		t.Fatalf("expected 0%% SLO, got %f", slo)
	}
}

func TestDatabaseMigrations(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "minidock_migration.db")

	// 1. Test starting a database from scratch
	database, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	var version int
	err = database.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	if err != nil {
		t.Fatal(err)
	}
	if version != 10 {
		t.Fatalf("expected database user_version to be 10, got %d", version)
	}

	// 2. Test compatibility mode for an existing database with version 0 but settings table exists
	dbPathCompat := filepath.Join(t.TempDir(), "minidock_compat.db")
	dbCompat, err := sql.Open("sqlite", dbPathCompat)
	if err != nil {
		t.Fatal(err)
	}
	defer dbCompat.Close()

	// Manually create just the settings table to simulate version 0 with settings table
	_, err = dbCompat.ExecContext(ctx, `
		CREATE TABLE settings (
			key TEXT PRIMARY KEY,
			value BLOB NOT NULL
		);
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Now open it using store.Open
	storeCompat, err := Open(dbPathCompat)
	if err != nil {
		t.Fatal(err)
	}
	defer storeCompat.Close()

	err = storeCompat.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version)
	if err != nil {
		t.Fatal(err)
	}
	if version != 10 {
		t.Fatalf("expected compat database user_version to be 10, got %d", version)
	}
}

func TestClaimWebhookDeliveryIsDurableAndIdempotent(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "webhooks.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	claimed, err := database.ClaimWebhookDelivery(t.Context(), "github", "delivery-hash")
	if err != nil || !claimed {
		t.Fatalf("first delivery claim = %v, %v", claimed, err)
	}
	claimed, err = database.ClaimWebhookDelivery(t.Context(), "github", "delivery-hash")
	if err != nil || claimed {
		t.Fatalf("duplicate delivery claim = %v, %v", claimed, err)
	}
}

func TestWebhookRateLimitPersistsWithinWindow(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "webhook-rate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for attempt := 1; attempt <= 3; attempt++ {
		allowed, err := database.AllowWebhookRequest(t.Context(), "github:42", 2, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if allowed != (attempt <= 2) {
			t.Fatalf("attempt %d allowed = %v", attempt, allowed)
		}
	}
	if _, err := database.AllowWebhookRequest(t.Context(), "", 1, time.Minute); err == nil {
		t.Fatal("empty rate-limit scope was accepted")
	}
}

func TestUnlockFailuresAreScopedAndPersistent(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "unlock-rate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	now := time.Now().UTC()
	for attempt := 1; attempt <= 5; attempt++ {
		until, err := database.RegisterFailedUnlock(t.Context(), "unlock:origin-a", now)
		if err != nil {
			t.Fatal(err)
		}
		if until.IsZero() != (attempt < 5) {
			t.Fatalf("attempt %d lockout = %s", attempt, until)
		}
	}
	if until, err := database.UnlockLockoutUntil(t.Context(), "unlock:origin-a", now); err != nil || until.IsZero() {
		t.Fatalf("active lockout = %s, %v", until, err)
	}
	if until, err := database.UnlockLockoutUntil(t.Context(), "unlock:origin-b", now); err != nil || !until.IsZero() {
		t.Fatalf("other origin lockout = %s, %v", until, err)
	}
	if err := database.ClearUnlockFailures(t.Context(), "unlock:origin-a"); err != nil {
		t.Fatal(err)
	}
	if until, err := database.UnlockLockoutUntil(t.Context(), "unlock:origin-a", now); err != nil || !until.IsZero() {
		t.Fatalf("cleared lockout = %s, %v", until, err)
	}
}

func TestDatabaseMigratesVersion2DataToCurrentSchema(t *testing.T) {

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "minidock_v2.db")
	legacy, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// This is a real v2-shaped database, not an empty settings-table shortcut.
	// It contains user data whose automation flags must be corrected by v3.
	statements := []string{
		`CREATE TABLE settings (key TEXT PRIMARY KEY, value BLOB NOT NULL)`,
		`CREATE TABLE applications (id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, repository TEXT NOT NULL, branch TEXT NOT NULL, work_dir TEXT NOT NULL, type TEXT NOT NULL, build_command TEXT NOT NULL, run_command TEXT NOT NULL, internal_port INTEGER NOT NULL, domain TEXT NOT NULL UNIQUE, created_at DATETIME NOT NULL, runtime TEXT NOT NULL DEFAULT 'auto', deploy_on_push BOOLEAN NOT NULL DEFAULT 1, require_confirmation BOOLEAN NOT NULL DEFAULT 1, auto_rollback BOOLEAN NOT NULL DEFAULT 0, github_installation_id INTEGER NOT NULL DEFAULT 0, health_endpoint TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, application_id INTEGER NOT NULL, status TEXT NOT NULL, image TEXT NOT NULL, log_path TEXT NOT NULL, started_at DATETIME NOT NULL, finished_at DATETIME, action TEXT NOT NULL DEFAULT 'deploy', requested_ref TEXT NOT NULL DEFAULT '', source_revision TEXT NOT NULL DEFAULT '', source_fingerprint TEXT NOT NULL DEFAULT '', artifact_digest TEXT NOT NULL DEFAULT '', runtime TEXT NOT NULL DEFAULT '', internal_port INTEGER NOT NULL DEFAULT 0, health_endpoint TEXT NOT NULL DEFAULT '', manifest TEXT NOT NULL DEFAULT '', configuration_digest TEXT NOT NULL DEFAULT '', failure_stage TEXT NOT NULL DEFAULT '', failure_code TEXT NOT NULL DEFAULT '', failure_detail TEXT NOT NULL DEFAULT '', attempt INTEGER NOT NULL DEFAULT 0, heartbeat_at DATETIME, lease_expires_at DATETIME, cancellation_requested_at DATETIME, queue_duration_ms INTEGER NOT NULL DEFAULT 0, source_duration_ms INTEGER NOT NULL DEFAULT 0, build_duration_ms INTEGER NOT NULL DEFAULT 0, start_duration_ms INTEGER NOT NULL DEFAULT 0, health_duration_ms INTEGER NOT NULL DEFAULT 0, route_duration_ms INTEGER NOT NULL DEFAULT 0, current_stage TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE secrets (scope TEXT NOT NULL, name TEXT NOT NULL, nonce BLOB NOT NULL, ciphertext BLOB NOT NULL, updated_at DATETIME NOT NULL, PRIMARY KEY (scope, name))`,
		`CREATE TABLE secret_audit (id INTEGER PRIMARY KEY, application_id INTEGER NOT NULL, environment TEXT NOT NULL, name TEXT NOT NULL, action TEXT NOT NULL, created_at DATETIME NOT NULL, target TEXT NOT NULL DEFAULT 'runtime')`,
		`CREATE TABLE application_configuration (application_id INTEGER NOT NULL, environment TEXT NOT NULL, target TEXT NOT NULL, name TEXT NOT NULL, value TEXT NOT NULL, updated_at DATETIME NOT NULL, PRIMARY KEY (application_id, environment, target, name))`,
		`CREATE TABLE automation_settings (id INTEGER PRIMARY KEY CHECK(id = 1), cleanup_schedule TEXT NOT NULL DEFAULT 'manual')`,
		`INSERT INTO applications (id, name, repository, branch, work_dir, type, build_command, run_command, internal_port, domain, created_at, deploy_on_push, require_confirmation) VALUES (7, 'legacy', 'file:///legacy', 'main', '.', 'custom', '', '', 8080, 'legacy.example.test', CURRENT_TIMESTAMP, 1, 1)`,
		`PRAGMA user_version = 2`,
	}
	for _, statement := range statements {
		if _, err := legacy.ExecContext(ctx, statement); err != nil {
			legacy.Close()
			t.Fatalf("create v2 fixture: %v", err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := database.Application(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if application.Name != "legacy" || application.DeployOnPush {
		t.Fatalf("v2 application was not preserved and made safe: %#v", application)
	}
	var version int
	if err := database.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 10 {
		t.Fatalf("migrated version = %d, want 10", version)
	}
	if _, err := database.db.ExecContext(ctx, `INSERT INTO deployment_events (deployment_id, stage, event_type, message, created_at) VALUES (1, 'upgrade', 'verified', 'fixture', CURRENT_TIMESTAMP)`); err == nil {
		t.Fatal("v3 foreign key was unexpectedly satisfiable without a deployment")
	}
}

func TestFederatedIdentityUsesProviderAndSubject(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if _, err := database.db.ExecContext(ctx, `INSERT INTO identity_providers(id, issuer, client_id, created_at) VALUES('oidc-a', 'https://issuer.example', 'minidock', CURRENT_TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	userID, err := database.UpsertFederatedIdentity(ctx, FederatedIdentity{ProviderID: "oidc-a", Subject: "stable-subject", Email: "first@example.test", DisplayName: "First"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRole(ctx, userID, RoleDeployer); err != nil {
		t.Fatal(err)
	}
	if _, err := database.UpsertFederatedIdentity(ctx, FederatedIdentity{ProviderID: "oidc-a", Subject: "stable-subject", Email: "renamed@example.test", DisplayName: "Renamed"}); err != nil {
		t.Fatal(err)
	}
	var users, roles int
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM role_bindings WHERE user_id = ? AND role = ?`, userID, RoleDeployer).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if users != 1 || roles != 1 {
		t.Fatalf("users=%d roles=%d, want one stable identity and role", users, roles)
	}
	if err := database.SetRole(ctx, userID, "root"); err == nil {
		t.Fatal("invalid role accepted")
	}
}

func TestNodeEnrollmentBindsCertificateAndUpdatesHeartbeat(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	first := Node{ID: "edge-01", Name: "Edge one", Version: "v0.1.0", Capabilities: []string{"runtime-observation"}, CertificateFingerprint: "cert-a"}
	if err := database.UpsertNode(ctx, first); err != nil {
		t.Fatal(err)
	}
	updated := first
	updated.Version = "v0.1.1"
	updated.Capabilities = []string{"runtime-observation", "health"}
	if err := database.UpsertNode(ctx, updated); err != nil {
		t.Fatal(err)
	}
	nodes, err := database.Nodes(ctx)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %#v, %v", nodes, err)
	}
	if nodes[0].Version != "v0.1.1" || len(nodes[0].Capabilities) != 2 || nodes[0].CertificateFingerprint != "cert-a" {
		t.Fatalf("unexpected persisted node: %#v", nodes[0])
	}
	changedCertificate := updated
	changedCertificate.CertificateFingerprint = "cert-b"
	if err := database.UpsertNode(ctx, changedCertificate); err == nil {
		t.Fatal("node certificate rotation unexpectedly took over existing node")
	}
}
