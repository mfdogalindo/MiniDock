package app

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/julieta/minidock/internal/backup"
	"github.com/julieta/minidock/internal/deploy"
	"github.com/julieta/minidock/internal/security"
	"github.com/julieta/minidock/internal/store"
)

// recoveryRuntime is intentionally inert: it lets the queue startup test
// exercise recovery without requiring a Docker daemon.
type recoveryRuntime struct{}

type fakeCloudflaredConnector struct {
	token   string
	removed bool
}

func (f *fakeCloudflaredConnector) Status(context.Context) (CloudflaredStatus, error) {
	if f.token != "" {
		return CloudflaredStatus{State: "running"}, nil
	}
	return CloudflaredStatus{State: "missing"}, nil
}
func (f *fakeCloudflaredConnector) Reconcile(_ context.Context, token string) error {
	f.token = token
	return nil
}
func (f *fakeCloudflaredConnector) Remove(context.Context) error {
	f.removed = true
	f.token = ""
	return nil
}

type fakeCloudflareAPI struct {
	ingressDomains []string
	dnsDomains     []string
}

func (f *fakeCloudflareAPI) VerifyToken(_ context.Context, token string) error {
	if token != "api-secret" {
		return fmt.Errorf("unexpected token")
	}
	return nil
}
func (f *fakeCloudflareAPI) ListZones(context.Context, string) ([]CloudflareZone, error) {
	return []CloudflareZone{{ID: "zone-id", Name: "example.com", Status: "active"}}, nil
}
func (f *fakeCloudflareAPI) CreateOrGetTunnel(context.Context, string, string, string) (string, string, error) {
	return "94b12d10-7a7d-4e75-bdec-f42e5f8577f4", "tunnel-secret", nil
}
func (f *fakeCloudflareAPI) ConfigureTunnelIngress(_ context.Context, _, _, _ string, domains []string) error {
	f.ingressDomains = append([]string(nil), domains...)
	return nil
}
func (f *fakeCloudflareAPI) TunnelStatus(context.Context, string, string, string) (string, error) {
	return "healthy", nil
}
func (f *fakeCloudflareAPI) UpsertCNAMERecord(_ context.Context, _, _, hostname, _ string) error {
	f.dnsDomains = append(f.dnsDomains, hostname)
	return nil
}

func (recoveryRuntime) Name() string               { return "recovery" }
func (recoveryRuntime) BuildCommand() string       { return "true" }
func (recoveryRuntime) SupportsBuildSecrets() bool { return false }
func (recoveryRuntime) Start(context.Context, deploy.Executor, io.Writer, store.Application, string, map[string]string) (string, error) {
	return "", nil
}
func (recoveryRuntime) Control(context.Context, deploy.Executor, io.Writer, store.Application, string) error {
	return nil
}
func (recoveryRuntime) Logs(context.Context, deploy.Executor, io.Writer, store.Application) error {
	return nil
}
func (recoveryRuntime) Status(context.Context, deploy.Executor, store.Application) (deploy.ContainerStatus, error) {
	return deploy.ContainerStatus{}, nil
}
func (recoveryRuntime) RemoveImage(context.Context, deploy.Executor, store.Application, string) error {
	return nil
}

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

func TestQueueStartupRecoversInterruptedRunningDeployment(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	registered, err := database.CreateApplication(t.Context(), store.Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test", Runtime: "recovery"})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := database.QueueDeployment(t.Context(), registered.ID, "deploy", filepath.Join(t.TempDir(), "deploy.log"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.NextQueuedDeployment(t.Context()); err != nil {
		t.Fatal(err)
	}
	application, err := New(Config{Environment: "test", MaxConcurrentDeployments: 1}, database)
	if err != nil {
		t.Fatal(err)
	}
	application.executor.Adapters = map[string]deploy.RuntimeAdapter{"recovery": recoveryRuntime{}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := application.StartQueue(ctx); err != nil {
		t.Fatal(err)
	}
	recovered, err := database.Deployment(t.Context(), registered.ID, queued.ID)
	if err != nil || recovered.Status != "failed" || recovered.FailureStage != "start" || recovered.FailureCode != "worker_restarted" {
		t.Fatalf("queue startup did not recover job: %#v, %v", recovered, err)
	}
}

func TestReleaseReportContainsPersistedEvidenceAndExcludesLogPath(t *testing.T) {
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
	setup := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(setup.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]
	registered, err := database.CreateApplication(t.Context(), store.Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test"})
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := database.QueueDeployment(t.Context(), registered.ID, "deploy", filepath.Join(t.TempDir(), "private.log"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetDeploymentReleaseMetadata(t.Context(), deployment.ID, store.ReleaseMetadata{RequestedRef: "main", SourceRevision: "abc123", SourceFingerprint: "sha256:source", ArtifactDigest: "sha256:image", Runtime: "docker", InternalPort: 8080, HealthEndpoint: "/healthz", Manifest: `{"version":1}`, ConfigurationDigest: "sha256:config"}); err != nil {
		t.Fatal(err)
	}
	if err := database.FinishDeployment(t.Context(), deployment.ID, "successful"); err != nil {
		t.Fatal(err)
	}
	if err := database.SetDeploymentStage(t.Context(), deployment.ID, "route"); err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/applications/"+strconv.FormatInt(registered.ID, 10)+"/deployments/"+strconv.FormatInt(deployment.ID, 10)+"/release-report", nil)
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("release report response = %d %q", response.Code, response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	for _, expected := range []string{`"source_revision":"abc123"`, `"artifact_digest":"sha256:image"`, `"configuration_digest":"sha256:config"`, `"current_stage":"route"`, `"route_duration_ms":0`, `"domain":"api.example.test"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("release report missing %s: %s", expected, body)
		}
	}
	if strings.Contains(body, "private.log") || strings.Contains(body, "log_path") {
		t.Fatalf("release report leaked log path: %s", body)
	}
}

func TestRollbackReleaseMetadataPreservesArtifactEvidence(t *testing.T) {
	source := store.Deployment{
		RequestedRef: "v1.2.3", SourceRevision: "a1b2c3", SourceFingerprint: "sha256:source",
		ArtifactDigest: "sha256:image", Runtime: "docker", InternalPort: 8080,
		HealthEndpoint: "/healthz", Manifest: `{"version":1}`,
		ConfigurationDigest: "sha256:original-config",
	}
	got := rollbackReleaseMetadata(source, "sha256:current-config")
	if got.RequestedRef != source.RequestedRef || got.SourceRevision != source.SourceRevision || got.ArtifactDigest != source.ArtifactDigest || got.Runtime != source.Runtime || got.Manifest != source.Manifest {
		t.Fatalf("rollback metadata did not preserve release evidence: %#v", got)
	}
	if got.ConfigurationDigest != "sha256:current-config" {
		t.Fatalf("rollback configuration digest = %q, want current configuration", got.ConfigurationDigest)
	}
}

func TestReleaseManifestPersistsOnlyPublicRuntimeConfiguration(t *testing.T) {
	manifest := releaseManifest(`{"version":1}`, `{"PUBLIC_MODE":"production"}`, "docker-container-id")
	for _, expected := range []string{`"effective_runtime_configuration":{"PUBLIC_MODE":"production"}`, `"observed_container_id":"docker-container-id"`} {
		if !strings.Contains(manifest, expected) {
			t.Fatalf("release manifest missing %s: %s", expected, manifest)
		}
	}
	if strings.Contains(manifest, "RUNTIME_SECRET") {
		t.Fatalf("release manifest leaked a secret: %s", manifest)
	}
}

func TestRollbackConfigurationRestoresPublicValuesWithoutReplacingSecrets(t *testing.T) {
	configuration := deploy.DeploymentConfiguration{
		Runtime:       map[string]string{"PUBLIC_MODE": "new", "RUNTIME_SECRET": "current-secret"},
		PublicRuntime: map[string]string{"PUBLIC_MODE": "new"},
		BuildArgs:     map[string]string{"BUILD_MODE": "current"},
	}
	got, err := rollbackConfiguration(configuration, `{"effective_runtime_configuration":{"PUBLIC_MODE":"old"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime["PUBLIC_MODE"] != "old" || got.Runtime["RUNTIME_SECRET"] != "current-secret" {
		t.Fatalf("rollback runtime = %#v", got.Runtime)
	}
	if got.ConfigurationDigest != deploy.PublicConfigurationDigest(map[string]string{"PUBLIC_MODE": "old"}, configuration.BuildArgs) {
		t.Fatalf("rollback configuration digest = %s", got.ConfigurationDigest)
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

func TestRuntimeReadinessDistinguishesDockerAndProxyFailures(t *testing.T) {
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

	// Liveness must remain available even when Docker is not.
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", response.Code)
	}

	application.runtimeReady = func(context.Context) error { return fmt.Errorf("permission denied") }
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/runtimez", nil))
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "permission denied") {
		t.Fatalf("runtime socket failure response = %d %q", response.Code, response.Body.String())
	}

	application.runtimeReady = func(context.Context) error { return nil }
	application.proxyReady = func(context.Context) error { return fmt.Errorf("connection refused") }
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/runtimez", nil))
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "Caddy proxy") {
		t.Fatalf("runtime proxy failure response = %d %q", response.Code, response.Body.String())
	}

	application.proxyReady = func(context.Context) error { return nil }
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/runtimez", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"status":"ready"`) || !strings.Contains(response.Body.String(), `"version":"1.0.0"`) {
		t.Fatalf("runtime success response = %d %q", response.Code, response.Body.String())
	}
}

func TestProxyReadinessAcceptsCaddyHTTPSRedirect(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusPermanentRedirect} {
		if !proxyResponseReady(status) {
			t.Fatalf("status %d should prove proxy readiness", status)
		}
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusBadGateway, http.StatusServiceUnavailable} {
		if proxyResponseReady(status) {
			t.Fatalf("status %d must fail proxy readiness", status)
		}
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
	if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/applications/1?notice=") {
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

func TestCreateAndDeployAppliesSafeDefaults(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test", LogPath: t.TempDir(), DockerNetwork: "minidock-test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	application.runtimeReady = func(context.Context) error { return nil }
	application.proxyReady = func(context.Context) error { return nil }
	handler := application.Handler()
	setup := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(setup.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]

	form := url.Values{"repository": {"https://github.com/example/My Web.git"}, "type": {"static"}, "intent": {"deploy"}}
	request = httptest.NewRequest(http.MethodPost, "/applications", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || !strings.HasPrefix(response.Header().Get("Location"), "/applications/1?notice=") {
		t.Fatalf("create and deploy response = %d %s", response.Code, response.Header().Get("Location"))
	}
	registered, err := database.Application(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if registered.Name != "my-web" || registered.Branch != "main" || registered.WorkDir != "." || registered.Domain != "my-web.localhost" || registered.Runtime != "auto" || registered.InternalPort != 8080 || registered.HealthEndpoint != "/" || !registered.RequireConfirmation {
		t.Fatalf("unexpected defaults: %#v", registered)
	}
	deployments, err := database.Deployments(t.Context(), registered.ID)
	if err != nil || len(deployments) != 1 || deployments[0].Status != "queued" || deployments[0].Action != "deploy" {
		t.Fatalf("first deployment was not queued: %#v, %v", deployments, err)
	}
}

func TestCreateAndDeployPreservesApplicationWhenHostIsNotReady(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test", LogPath: t.TempDir(), DockerNetwork: "minidock-test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	application.runtimeReady = func(context.Context) error { return fmt.Errorf("daemon stopped") }
	application.proxyReady = func(context.Context) error { return nil }
	handler := application.Handler()
	setup := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(setup.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookie := response.Result().Cookies()[0]

	form := url.Values{"repository": {"https://github.com/example/saved-app.git"}, "type": {"static"}, "intent": {"deploy"}}
	request = httptest.NewRequest(http.MethodPost, "/applications", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "error=") {
		t.Fatalf("expected actionable redirect, got %d %s", response.Code, response.Header().Get("Location"))
	}
	registered, err := database.Application(t.Context(), 1)
	if err != nil || registered.Name != "saved-app" {
		t.Fatalf("application was not preserved: %#v, %v", registered, err)
	}
	deployments, err := database.Deployments(t.Context(), registered.ID)
	if err != nil || len(deployments) != 0 {
		t.Fatalf("deployment should not be queued while host is unavailable: %#v, %v", deployments, err)
	}
}

func TestApplicationNameFromRepository(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/My App.git": "my-app",
		"file:///repos/café_API":             "caf-api",
		"https://example.test/.git":          "",
	}
	for repository, expected := range cases {
		if got := applicationNameFromRepository(repository); got != expected {
			t.Errorf("applicationNameFromRepository(%q) = %q, want %q", repository, got, expected)
		}
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
	application.runtimeReady = func(context.Context) error { return fmt.Errorf("daemon stopped") }
	application.proxyReady = func(context.Context) error { return nil }
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
	if !strings.Contains(response.Body.String(), "Inicia Docker Desktop u OrbStack") || !strings.Contains(response.Body.String(), `value="deploy" disabled`) || !strings.Contains(response.Body.String(), `value="save"`) {
		t.Fatalf("form does not keep save available while blocking deploy: %s", response.Body.String())
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

func TestGitHubWebhookRateLimitIsPersistentPerApplication(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test", LogPath: t.TempDir(), WebhookSecret: "hook-secret", WebhookRateLimit: 1, WebhookRateWindow: time.Minute}, database)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := database.CreateApplication(t.Context(), store.Application{Name: "api", Repository: "https://example.test/api", Branch: "main", WorkDir: ".", Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "api.example.test", DeployOnPush: true})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"ref":"refs/heads/main"}`)
	for attempt := 1; attempt <= 2; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/webhooks/github/"+strconv.FormatInt(registered.ID, 10), strings.NewReader(string(body)))
		request.Header.Set("X-GitHub-Event", "push")
		request.Header.Set("X-Hub-Signature-256", githubSignature("hook-secret", body))
		response := httptest.NewRecorder()
		application.Handler().ServeHTTP(response, request)
		if want := []int{http.StatusAccepted, http.StatusTooManyRequests}[attempt-1]; response.Code != want {
			t.Fatalf("attempt %d status = %d, want %d", attempt, response.Code, want)
		}
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
	application, err := New(Config{Environment: "test", LogPath: t.TempDir(), WebhookSecret: "hook-secret", DockerNetwork: "minidock-test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	application.runtimeReady = func(context.Context) error { return nil }
	application.proxyReady = func(context.Context) error { return nil }
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

func loginApp(t *testing.T, handler http.Handler) *http.Cookie {
	setup := url.Values{"password": {"a sufficiently long password"}, "password_confirmation": {"a sufficiently long password"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(setup.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookies := response.Result().Cookies()
	if len(cookies) > 0 {
		return cookies[0]
	}
	// Try /unlock if already set up
	unlock := url.Values{"password": {"a sufficiently long password"}}
	request = httptest.NewRequest(http.MethodPost, "/unlock", strings.NewReader(unlock.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	cookies = response.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("failed to login")
	}
	return cookies[0]
}

func TestCreateApplicationValidationRules(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	application, err := New(Config{Environment: "test", AdminDomain: "admin.local"}, database)
	if err != nil {
		t.Fatal(err)
	}

	cookie := loginApp(t, application.Handler())

	// Case 1: Invalid domain format
	form := url.Values{
		"name": {"valid-app"}, "repository": {"https://github.com/org/repo"}, "branch": {"main"}, "work_dir": {"."},
		"type": {"go"}, "internal_port": {"8080"}, "domain": {"invalid_domain_with_underscores!"},
	}
	request := httptest.NewRequest(http.MethodPost, "/applications", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Completa todos los campos correctamente") {
		t.Fatalf("expected validation error for invalid domain, got status = %d: %s", response.Code, response.Body.String())
	}

	// Case 2: Domain same as AdminDomain
	form = url.Values{
		"name": {"valid-app"}, "repository": {"https://github.com/org/repo"}, "branch": {"main"}, "work_dir": {"."},
		"type": {"go"}, "internal_port": {"8080"}, "domain": {"admin.local"},
	}
	request = httptest.NewRequest(http.MethodPost, "/applications", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "El dominio de la aplicación no puede ser igual al dominio administrativo") {
		t.Fatalf("expected validation error for admin domain collision, got status = %d: %s", response.Code, response.Body.String())
	}

	// Case 3: Invalid app name format
	form = url.Values{
		"name": {"INVALID_NAME"}, "repository": {"https://github.com/org/repo"}, "branch": {"main"}, "work_dir": {"."},
		"type": {"go"}, "internal_port": {"8080"}, "domain": {"app.local"},
	}
	request = httptest.NewRequest(http.MethodPost, "/applications", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Completa todos los campos correctamente") {
		t.Fatalf("expected validation error for invalid name, got status = %d: %s", response.Code, response.Body.String())
	}
}

func TestOperationsBackgroundCache(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	application, err := New(Config{Environment: "test", DiskAlertPercent: 85}, database)
	if err != nil {
		t.Fatal(err)
	}

	application.cacheMu.Lock()
	application.diskUsedCache = "42.0%"
	application.alertsCache = []operationalAlert{
		{"warning", "Test alert message"},
	}
	application.statusCache[1] = deploy.ContainerStatus{
		State:  "running",
		Health: "healthy",
		CPU:    "1.5%",
		Memory: "50MiB",
		Image:  "minidock/test:latest",
	}
	application.cacheMu.Unlock()

	cookie := loginApp(t, application.Handler())

	_, err = database.CreateApplication(t.Context(), store.Application{
		ID: 1, Name: "cached-app", Repository: "https://example.test/repo", Branch: "main", WorkDir: ".",
		Type: "go", BuildCommand: "build", RunCommand: "run", InternalPort: 8080, Domain: "cached.local",
	})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/operations", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("operations page failed: %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "42.0%") {
		t.Fatalf("expected cached disk usage in body, got: %s", body)
	}
	if !strings.Contains(body, "Test alert message") {
		t.Fatalf("expected cached alert message in body, got: %s", body)
	}
	if !strings.Contains(body, "cached-app") {
		t.Fatalf("expected application name in body, got: %s", body)
	}
	if !strings.Contains(body, "running") || !strings.Contains(body, "healthy") {
		t.Fatalf("expected container status in body, got: %s", body)
	}
}

func TestCSRFProtection(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}

	cookie := loginApp(t, application.Handler())

	// POST request without token should be rejected if X-Test-Validate-CSRF is set
	form := url.Values{"name": {"new-app"}}
	request := httptest.NewRequest(http.MethodPost, "/applications", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Test-Validate-CSRF", "true")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("expected forbidden (403), got %d", response.Code)
	}

	// POST request with token should succeed (or proceed past CSRF check to form validation, returning bad request 400 instead of forbidden 403)
	request = httptest.NewRequest(http.MethodPost, "/applications", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("X-Test-Validate-CSRF", "true")
	request.Header.Set("X-CSRF-Token", application.csrfTokenFor(cookie.Value))
	request.AddCookie(cookie)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code == http.StatusForbidden {
		t.Fatalf("expected past CSRF check, got forbidden (403)")
	}
}

func TestUnlockLockout(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}

	// Initialize security first via setup
	setup := url.Values{"password": {"masterpassword12"}, "password_confirmation": {"masterpassword12"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(setup.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)

	// Lock the app to clear the key
	application.Lock()

	// Try 5 failed unlock attempts
	for i := 0; i < 5; i++ {
		unlock := url.Values{"password": {"wrongpassword"}}
		request = httptest.NewRequest(http.MethodPost, "/unlock", strings.NewReader(unlock.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response = httptest.NewRecorder()
		application.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("expected bad request (400) on failed unlock, got %d", response.Code)
		}
	}

	// The 6th attempt should be blocked by lockout immediately
	unlock := url.Values{"password": {"masterpassword12"}}
	request = httptest.NewRequest(http.MethodPost, "/unlock", strings.NewReader(unlock.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Demasiados intentos") {
		t.Fatalf("expected lockout block, got status %d body: %s", response.Code, response.Body.String())
	}

	// The lockout lives in SQLite, not in the App process. A replacement process
	// sharing the database must reject the same client origin immediately.
	restarted, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	restarted.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Demasiados intentos") {
		t.Fatalf("expected persisted lockout block, got status %d body: %s", response.Code, response.Body.String())
	}
}

func TestSQLiteBackupAndEncryption(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	backupDir := t.TempDir()
	application, err := New(Config{Environment: "test", BackupPath: backupDir, BackupRetention: 2}, database)
	if err != nil {
		t.Fatal(err)
	}

	// Initialize security via setup
	setup := url.Values{"password": {"masterpassword12"}, "password_confirmation": {"masterpassword12"}}
	request := httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(setup.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)

	// Run backup (encrypted)
	err = application.runBackup(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	// Check that an encrypted file was created
	files, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatal(err)
	}
	var encFile string
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".mdbk") {
			encFile = filepath.Join(backupDir, f.Name())
			break
		}
	}
	if encFile == "" {
		t.Fatal("expected encrypted backup file to be created")
	}

	// Verify decryption
	encData, err := os.ReadFile(encFile)
	if err != nil {
		t.Fatal(err)
	}
	key := application.keyCopy()
	defer security.Zero(key)
	decrypted, err := backup.Open(key, encData)
	if err != nil {
		t.Fatalf("failed to decrypt backup: %v", err)
	}
	if len(decrypted) == 0 {
		t.Fatal("decrypted data is empty")
	}

	// A locked KMS must reject the backup and must not create a plaintext fallback.
	application.Lock()
	err = application.runBackup(t.Context())
	if err == nil {
		t.Fatal("expected locked backup to be rejected")
	}
	files2, _ := os.ReadDir(backupDir)
	for _, f := range files2 {
		if strings.HasSuffix(f.Name(), ".db") && !strings.Contains(f.Name(), "temp") {
			t.Fatalf("unexpected plaintext backup %s", f.Name())
		}
	}
}

func TestWebhookRejectionOnLockedWithSecrets(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	application, err := New(Config{Environment: "test", LogPath: t.TempDir(), WebhookSecret: "github-secret"}, database)
	if err != nil {
		t.Fatal(err)
	}

	// Setup application in DB
	appData, err := database.CreateApplication(t.Context(), store.Application{
		Name: "test-app", Repository: "https://github.com/org/repo", Branch: "main", WorkDir: ".",
		Type: "go", InternalPort: 8080, Domain: "test.local", DeployOnPush: true, RequireConfirmation: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Make sure MiniDock is locked
	application.Lock()

	// Webhook request when locked and application has NO secrets -> should succeed (StatusAccepted 202)
	body := []byte(`{"ref":"refs/heads/main"}`)
	sig := githubSignature("github-secret", body)
	request := httptest.NewRequest(http.MethodPost, "/webhooks/github/"+strconv.FormatInt(appData.ID, 10), strings.NewReader(string(body)))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", sig)
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusAccepted {
		t.Fatalf("expected accepted (202), got %d", response.Code)
	}

	// Add a secret to the application
	// Note: We need a master key to encrypt a secret. Let's unlock first.
	unlockSetup := url.Values{"password": {"masterpassword12"}, "password_confirmation": {"masterpassword12"}}
	request = httptest.NewRequest(http.MethodPost, "/setup", strings.NewReader(unlockSetup.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)

	// Add a secret via db helper or PutSecret directly
	scope := fmt.Sprintf("application:%d:production:runtime", appData.ID)
	err = database.PutSecret(t.Context(), scope, "API_KEY", []byte("nonce"), []byte("ciphertext"))
	if err != nil {
		t.Fatal(err)
	}

	// Lock MiniDock again
	application.Lock()

	// Webhook request when locked and application HAS secrets -> should be rejected (StatusServiceUnavailable 503)
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/webhooks/github/"+strconv.FormatInt(appData.ID, 10), strings.NewReader(string(body)))
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-Hub-Signature-256", sig)
	application.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected service unavailable (503) because app has secrets and minidock is locked, got %d", response.Code)
	}
}

func TestRunPreflightCheck(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	tempDir := t.TempDir()
	cfg := Config{
		Environment:         "test",
		DatabasePath:        filepath.Join(tempDir, "minidock.db"),
		DockerNetwork:       "minidock-test",
		DockerNetworkSubnet: "172.31.251.0/24",
	}
	application, err := New(cfg, database)
	if err != nil {
		t.Fatal(err)
	}

	appData := store.Application{
		Name:           "Preflight App",
		Repository:     "file://" + tempDir,
		Domain:         "preflight.local",
		HealthEndpoint: "/healthz",
	}

	res := application.RunPreflightCheck(t.Context(), appData)
	if !res.RepoAccess.OK {
		t.Fatalf("expected RepoAccess OK, got %#v", res.RepoAccess)
	}
	if !res.DomainHealth.OK {
		t.Fatalf("expected DomainHealth OK, got %#v", res.DomainHealth)
	}
	if !res.Network.OK {
		t.Fatalf("expected Network OK, got %#v", res.Network)
	}
}

func TestCanarySecretNoLeak(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}

	canarySecret := "CANARY_SECRET_12345_DO_NOT_LEAK"
	appData, err := database.CreateApplication(t.Context(), store.Application{
		Name:           "Canary App",
		Repository:     "file://" + t.TempDir(),
		Branch:         "main",
		WorkDir:        ".",
		Type:           "custom",
		InternalPort:   8080,
		Domain:         "canary.local",
		Runtime:        "docker",
		HealthEndpoint: "/healthz",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Encrypt & store secret
	key := []byte("01234567890123456789012345678901")
	nonce, cipherText, err := security.Encrypt(key, []byte(canarySecret))
	if err != nil {
		t.Fatal(err)
	}
	err = database.PutApplicationSecret(t.Context(), appData.ID, "production", "runtime", "API_KEY", nonce, cipherText)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Verify secrets list metadata HTML page does NOT leak raw secret value
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/applications/%d/secrets", appData.ID), nil)
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if strings.Contains(response.Body.String(), canarySecret) {
		t.Fatal("secrets metadata view leaked canary secret value")
	}

	// 2. Verify application detail page does NOT leak raw secret value
	request = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/applications/%d", appData.ID), nil)
	response = httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)
	if strings.Contains(response.Body.String(), canarySecret) {
		t.Fatal("application detail view leaked canary secret value")
	}
}

func TestBackupAlertWhenLocked(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	password := "testpassword123"
	salt, _ := security.NewSalt()
	key, _ := security.DeriveKey(password, salt)
	nonce, cipher, _ := security.NewVerifier(key)
	err = database.InitializeSecurity(t.Context(), store.SecurityConfig{Salt: salt, VerifierNonce: nonce, VerifierCipher: cipher})
	if err != nil {
		t.Fatal(err)
	}

	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}

	// Ensure application is locked (no key in memory)
	application.Lock()

	err = application.runBackup(t.Context())
	if err == nil {
		t.Fatal("expected runBackup to fail when application is locked")
	}

	alerts, err := database.GetActiveAlerts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, alert := range alerts {
		if strings.Contains(alert.Message, "MD-P0-02:") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected alert MD-P0-02 when backup fails due to lock, got %#v", alerts)
	}
}

func TestHealthEndpointIncludesVersion(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	application.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"version":"1.0.0"`) {
		t.Fatalf("expected version 1.0.0 in response, got %s", response.Body.String())
	}
}

func TestIsPublicCloudflareDomain(t *testing.T) {
	for _, domain := range []string{"demo.example.com", "demo.example.com:443", "DEMO.EXAMPLE.COM."} {
		if !isPublicCloudflareDomain(domain) {
			t.Errorf("isPublicCloudflareDomain(%q) = false, want true", domain)
		}
	}
	for _, domain := range []string{"demo", "demo.local", "localhost", "localhost:8080", "192.0.2.1"} {
		if isPublicCloudflareDomain(domain) {
			t.Errorf("isPublicCloudflareDomain(%q) = true, want false", domain)
		}
	}
}

func TestCloudflaredConnectorStatus(t *testing.T) {
	if got := (CloudflaredStatus{State: "running"}).Display(); got != "En ejecución" {
		t.Fatalf("running connector display = %q", got)
	}
	if got := (CloudflaredStatus{State: "exited"}).Display(); got != "Detenido (exited)" {
		t.Fatalf("exited connector display = %q", got)
	}
	if got := (CloudflaredStatus{State: "missing"}).Display(); got != "Pendiente de iniciar" {
		t.Fatalf("missing connector display = %q", got)
	}
}

func TestTunnelIDFromToken(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"a":"account","t":"94b12d10-7a7d-4e75-bdec-f42e5f8577f4","s":"secret"}`))
	if got := tunnelIDFromToken(payload); got != "94b12d10-7a7d-4e75-bdec-f42e5f8577f4" {
		t.Fatalf("tunnelIDFromToken() = %q", got)
	}
	if got := tunnelIDFromToken("not-a-token"); got != "" {
		t.Fatalf("invalid token returned tunnel ID %q", got)
	}
}

func TestCloudflareZoneForDomainRequiresLabelBoundary(t *testing.T) {
	zones := []CloudflareZone{{ID: "parent", Name: "example.com"}, {ID: "child", Name: "dev.example.com"}}
	zone, ok := cloudflareZoneForDomain(zones, "api.dev.example.com")
	if !ok || zone.ID != "child" {
		t.Fatalf("selected zone = %#v, %v", zone, ok)
	}
	if zone, ok := cloudflareZoneForDomain(zones, "attacker-example.com"); ok {
		t.Fatalf("suffix without DNS label boundary matched %#v", zone)
	}
}

func TestCloudflareTunnelNameIsStableAndRepairsInvalidStoredIdentity(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetSettingString(t.Context(), "cloudflare_instance_id", "../../unsafe"); err != nil {
		t.Fatal(err)
	}
	first, err := application.cloudflareTunnelName(t.Context(), store.CloudflareConfig{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := application.cloudflareTunnelName(t.Context(), store.CloudflareConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !regexp.MustCompile(`^minidock-[0-9a-f]{12}$`).MatchString(first) {
		t.Fatalf("stable tunnel names = %q and %q", first, second)
	}
}

func TestAutomaticCloudflareSetupConfiguresDNSAndStartsManagedConnector(t *testing.T) {
	database, err := store.Open(filepath.Join(t.TempDir(), "minidock.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if _, err := database.CreateApplication(t.Context(), store.Application{
		Name: "demo", Repository: "https://example.test/demo", Branch: "main", WorkDir: ".",
		Type: "go", BuildCommand: "go build", RunCommand: "./demo", InternalPort: 8080, Domain: "demo.example.com",
	}); err != nil {
		t.Fatal(err)
	}
	application, err := New(Config{Environment: "test"}, database)
	if err != nil {
		t.Fatal(err)
	}
	connector := &fakeCloudflaredConnector{}
	cloudflare := &fakeCloudflareAPI{}
	application.cloudflared = connector
	application.cloudflareAPI = func() CloudflareAPI { return cloudflare }
	handler := application.Handler()
	cookie := loginApp(t, handler)

	form := url.Values{
		"mode":       {"api_token"},
		"api_token":  {"api-secret"},
		"account_id": {"0123456789abcdef0123456789abcdef"},
	}
	request := httptest.NewRequest(http.MethodPost, "/cloudflare/save", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || !strings.Contains(response.Header().Get("Location"), "configurado") {
		t.Fatalf("automatic setup response = %d, %s", response.Code, response.Header().Get("Location"))
	}
	if connector.token != "tunnel-secret" {
		t.Fatalf("connector token = %q", connector.token)
	}
	if fmt.Sprint(cloudflare.ingressDomains) != "[demo.example.com]" || fmt.Sprint(cloudflare.dnsDomains) != "[demo.example.com]" {
		t.Fatalf("ingress = %v, DNS = %v", cloudflare.ingressDomains, cloudflare.dnsDomains)
	}
	cfg, err := database.CloudflareConfig(t.Context())
	if err != nil || cfg.Status != "connected" || cfg.TunnelID == "" {
		t.Fatalf("saved Cloudflare config = %#v, %v", cfg, err)
	}
	_, ciphertext, err := database.Secret(t.Context(), "system:cloudflare", "api_token")
	if err != nil || strings.Contains(string(ciphertext), "api-secret") {
		t.Fatalf("API token was not stored as ciphertext: %v", err)
	}
}
