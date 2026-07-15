package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/julieta/minidock/internal/deploy"
	"github.com/julieta/minidock/internal/store"
)

func TestRuntimeNoticeExplainsAppleContainerFallback(t *testing.T) {
	notice := runtimeNotice([]deploy.RuntimeDiagnostic{
		{Name: "apple", Installed: true, SetupCommand: "container system start"},
		{Name: "docker", Installed: true, Ready: true},
	})
	if !strings.Contains(notice, "Docker como respaldo") || !strings.Contains(notice, "container system start") {
		t.Fatalf("unexpected runtime notice: %q", notice)
	}
}

func TestRuntimePreflightFailure(t *testing.T) {
	if !runtimePreflightFailure("Error: default kernel not configured for architecture arm64") {
		t.Fatal("kernel setup failure was not classified as preflight")
	}
	if runtimePreflightFailure("[build] npm ci failed") {
		t.Fatal("build failure was incorrectly classified as preflight")
	}
}

func TestSetupUnlockAndLockFlow(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	handler := application.Handler()

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/setup" {
		t.Fatalf("unexpected initial response: %d %s", response.Code, response.Header().Get("Location"))
	}

	form := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request = httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("setup status = %d", response.Code)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected session cookie, got %d", len(cookies))
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Estado del servidor") {
		t.Fatalf("dashboard was not available: %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/lock", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("lock status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookies[0])
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/unlock" {
		t.Fatalf("dashboard should be locked: %d %s", response.Code, response.Header().Get("Location"))
	}
}

func TestRegisterApplication(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	handler := application.Handler()

	form := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]

	form = url.Values{
		"name": {"web"}, "repository": {"https://github.com/example/web"}, "branch": {"main"}, "work_dir": {"."},
		"type": {"static"}, "build_command": {"npm run build"}, "run_command": {"caddy run"}, "internal_port": {"8080"}, "domain": {"web.example.test"},
	}
	request = httptest.NewRequest(http.MethodPost, "/applications", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatalf("unexpected create response: %d %s", response.Code, response.Header().Get("Location"))
	}

	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "web.example.test") {
		t.Fatalf("application was not shown: %d %s", response.Code, response.Body.String())
	}
}

func TestApplicationFormUsesCSPCompatibleExternalScript(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]
	request = httptest.NewRequest(http.MethodGet, "/applications/new", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `src="/static/application-form.js"`) || strings.Contains(response.Body.String(), `<script>(()`) {
		t.Fatalf("application form still embeds inline JavaScript: %d", response.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/static/application-form.js", nil)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "repositories/browse") || !strings.Contains(response.Body.String(), "enhanceSelect") || !strings.Contains(response.Body.String(), "vite_ssr") || !strings.Contains(response.Body.String(), "select.style.display = 'none'") {
		t.Fatalf("application form script unavailable: %d", response.Code)
	}
}

func TestApplicationSecretsAreEncryptedAndAudited(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	handler := application.Handler()

	form := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]

	registered, err := database.CreateApplication(request.Context(), store.Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "go build", RunCommand: "./api", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	form = url.Values{"environment": {"staging"}, "target": {"runtime"}, "name": {"API_TOKEN"}, "value": {"do-not-render-this"}}
	request = httptest.NewRequest(http.MethodPost, "/applications/"+strconv.FormatInt(registered.ID, 10)+"/secrets", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("save secret status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodGet, "/applications/"+strconv.FormatInt(registered.ID, 10)+"/secrets?environment=staging&target=runtime", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "API_TOKEN") || strings.Contains(response.Body.String(), "do-not-render-this") {
		t.Fatalf("secret page exposed an unexpected value: %d %s", response.Code, response.Body.String())
	}
	audit, err := database.SecretAudit(request.Context(), registered.ID)
	if err != nil || len(audit) != 1 || audit[0].Action != "created" {
		t.Fatalf("unexpected audit: %#v, %v", audit, err)
	}
}

func TestApplicationPublicConfiguration(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	handler := application.Handler()
	form := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]
	registered, err := database.CreateApplication(request.Context(), store.Application{Name: "web", Repository: "https://example.test/web", Branch: "main", WorkDir: ".", Type: "static", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "web.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	form = url.Values{"environment": {"production"}, "target": {"build"}, "name": {"PUBLIC_API_URL"}, "value": {"https://api.example.test"}}
	request = httptest.NewRequest(http.MethodPost, "/applications/"+strconv.FormatInt(registered.ID, 10)+"/configuration", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("save configuration status = %d", response.Code)
	}
	values, err := database.ApplicationConfiguration(request.Context(), registered.ID, "production", "build")
	if err != nil || len(values) != 1 || values[0].Value != "https://api.example.test" {
		t.Fatalf("unexpected configuration: %#v, %v", values, err)
	}
}

func TestDeploymentWithoutSecretsDoesNotRequireMasterKey(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	values, err := application.deploymentSecrets(t.Context(), 42, "production", "runtime")
	if err != nil || len(values) != 0 {
		t.Fatalf("empty deployment secrets = %#v, %v", values, err)
	}
}

func TestDeploymentLogsAreScopedToApplication(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	logs := t.TempDir()
	application, err := New(Config{Environment: "test", LogPath: logs}, database)
	if err != nil {
		t.Fatal(err)
	}
	handler := application.Handler()
	form := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]

	first, err := database.CreateApplication(t.Context(), store.Application{Name: "one", Repository: "https://example.test/one", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "one.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := database.CreateApplication(t.Context(), store.Application{Name: "two", Repository: "https://example.test/two", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "two.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(logs, "deploy.log")
	if err := os.WriteFile(logPath, []byte("build succeeded\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deployment, err := database.QueueDeployment(t.Context(), first.ID, "deploy", logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.FinishDeployment(t.Context(), deployment.ID, "failed"); err != nil {
		t.Fatal(err)
	}

	request = httptest.NewRequest(http.MethodGet, "/applications/"+strconv.FormatInt(first.ID, 10)+"/deployments/"+strconv.FormatInt(deployment.ID, 10)+"/logs", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "build succeeded\n" {
		t.Fatalf("deployment log response = %d %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/applications/"+strconv.FormatInt(first.ID, 10), nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Log reciente") || !strings.Contains(response.Body.String(), "build succeeded") {
		t.Fatalf("application detail did not include log preview: %d %q", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/applications/"+strconv.FormatInt(first.ID, 10)+"/deployments/"+strconv.FormatInt(deployment.ID, 10)+"/log-stream", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(response.Body.String(), "event: log") || !strings.Contains(response.Body.String(), "build succeeded") {
		t.Fatalf("deployment log stream response = %d %q", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodGet, "/applications/"+strconv.FormatInt(second.ID, 10)+"/deployments/"+strconv.FormatInt(deployment.ID, 10)+"/logs", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("cross-application log response = %d", response.Code)
	}
}

func TestOperationsPageAndManualRetentionRequireAuthorization(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test", DatabasePath: filepath.Join(t.TempDir(), "minidock.db"), RetentionDays: 30, RetainedImages: 3}, database)
	if err != nil {
		t.Fatal(err)
	}
	handler := application.Handler()
	request := httptest.NewRequest(http.MethodGet, "/operations", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/unlock" {
		t.Fatalf("operations without session = %d %s", response.Code, response.Header().Get("Location"))
	}
	form := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request = httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]
	request = httptest.NewRequest(http.MethodGet, "/operations", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Operación y observabilidad") {
		t.Fatalf("operations response = %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/operations/retention", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/operations" {
		t.Fatalf("retention response = %d %s", response.Code, response.Header().Get("Location"))
	}
}

func TestGitHubWebhookValidatesSignatureBranchAndQueuesDeployment(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test", LogPath: t.TempDir(), WebhookSecret: "hook-secret"}, database)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := database.CreateApplication(t.Context(), store.Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test", DeployOnPush: true})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"ref":"refs/heads/main"}`)
	request := httptest.NewRequest(http.MethodPost, "/webhooks/github/"+strconv.FormatInt(registered.ID, 10), strings.NewReader(string(body)))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", githubSignature("hook-secret", body))
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("webhook status = %d: %s", response.Code, response.Body.String())
	}
	deployments, err := database.Deployments(t.Context(), registered.ID)
	if err != nil || len(deployments) != 1 || deployments[0].Status != "queued" {
		t.Fatalf("unexpected queued deployment: %#v, %v", deployments, err)
	}
	request = httptest.NewRequest(http.MethodPost, "/webhooks/github/"+strconv.FormatInt(registered.ID, 10), strings.NewReader(string(body)))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", "sha256=bad")
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("invalid signature status = %d", response.Code)
	}
	otherBranch := []byte(`{"ref":"refs/heads/release"}`)
	request = httptest.NewRequest(http.MethodPost, "/webhooks/github/"+strconv.FormatInt(registered.ID, 10), strings.NewReader(string(otherBranch)))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", githubSignature("hook-secret", otherBranch))
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("other branch status = %d", response.Code)
	}
	deployments, err = database.Deployments(t.Context(), registered.ID)
	if err != nil || len(deployments) != 1 {
		t.Fatalf("other branch created a deployment: %#v, %v", deployments, err)
	}
}

func TestGitHubWebhookAcceptsConfiguredTagReference(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test", LogPath: t.TempDir(), WebhookSecret: "hook-secret"}, database)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := database.CreateApplication(t.Context(), store.Application{Name: "api", Repository: "https://example.test/api", Branch: "refs/tags/v1.2.0", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test", DeployOnPush: true})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"ref":"refs/tags/v1.2.0"}`)
	request := httptest.NewRequest(http.MethodPost, "/webhooks/github/"+strconv.FormatInt(registered.ID, 10), strings.NewReader(string(body)))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", githubSignature("hook-secret", body))
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("tag webhook status = %d", response.Code)
	}
}

func TestAutomationRulesRequireProductionConfirmationAndControlPush(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test", LogPath: t.TempDir(), WebhookSecret: "hook-secret"}, database)
	if err != nil {
		t.Fatal(err)
	}
	handler := application.Handler()
	form := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]

	registered, err := database.CreateApplication(t.Context(), store.Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	form = url.Values{"require_confirmation": {"on"}, "auto_rollback": {"on"}}
	request = httptest.NewRequest(http.MethodPost, "/applications/"+strconv.FormatInt(registered.ID, 10)+"/automation", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("save automation status = %d", response.Code)
	}

	body := []byte(`{"ref":"refs/heads/main"}`)
	request = httptest.NewRequest(http.MethodPost, "/webhooks/github/"+strconv.FormatInt(registered.ID, 10), strings.NewReader(string(body)))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", githubSignature("hook-secret", body))
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("manual-mode webhook status = %d", response.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "/applications/"+strconv.FormatInt(registered.ID, 10)+"/deploy", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("unconfirmed deploy status = %d", response.Code)
	}
	form = url.Values{"confirm_production": {registered.Name}}
	request = httptest.NewRequest(http.MethodPost, "/applications/"+strconv.FormatInt(registered.ID, 10)+"/deploy", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("confirmed deploy status = %d", response.Code)
	}
	deployments, err := database.Deployments(t.Context(), registered.ID)
	if err != nil || len(deployments) != 1 || deployments[0].Status != "queued" {
		t.Fatalf("unexpected deployment after confirmation: %#v, %v", deployments, err)
	}
}

func TestRepositoryBrowserOnlyListsConfiguredLocalRoot(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "service", ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".hidden"), 0700); err != nil {
		t.Fatal(err)
	}
	application, err := New(Config{Environment: "test", LocalRepositoriesPath: root}, database)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]
	request = httptest.NewRequest(http.MethodGet, "/repositories/browse", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"name":"service"`) || !strings.Contains(response.Body.String(), `"repository":true`) || strings.Contains(response.Body.String(), ".hidden") {
		t.Fatalf("unexpected browser response: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodGet, "/repositories/browse?path=../../outside", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "outside") {
		t.Fatalf("browser escaped root: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest(http.MethodPost, "/repositories/folders", strings.NewReader(`{"path":"","name":"new-service"}`))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create repository folder status = %d: %s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "new-service")); err != nil {
		t.Fatalf("created repository folder missing: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "new-service", "package.json"), []byte(`{"devDependencies":{"vite":"latest"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/repositories/detect?path=new-service", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"type":"static"`) {
		t.Fatalf("unexpected runtime detection: %d %s", response.Code, response.Body.String())
	}
	form = url.Values{
		"name": {"local-code"}, "repository": {"file://" + filepath.Join(root, "new-service")}, "branch": {""}, "work_dir": {"."},
		"type": {"custom"}, "internal_port": {"8080"}, "domain": {"local-code.example.test"},
	}
	request = httptest.NewRequest(http.MethodPost, "/applications", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("local non-Git source was rejected: %d %s", response.Code, response.Body.String())
	}
}

func githubSignature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
