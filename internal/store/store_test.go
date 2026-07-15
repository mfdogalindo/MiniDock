package store

import (
	"context"
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
