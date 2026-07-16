package deploy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/julieta/minidock/internal/runtime"
	"github.com/julieta/minidock/internal/store"
)

func TestDeployRejectsUnsafeApplicationName(t *testing.T) {
	_, err := (Executor{}).Deploy(context.Background(), store.Application{Name: "../escape"}, DeploymentConfiguration{}, io.Discard)
	if err == nil {
		t.Fatal("expected invalid application name to be rejected")
	}
}

func TestSelectRuntimePrefersDockerForAutomaticDeployments(t *testing.T) {
	available := map[string]bool{"docker": true, "apple": true}
	got, err := selectRuntime("auto", available)
	if err != nil || got != "docker" {
		t.Fatalf("selectRuntime(auto) = %q, %v; want docker", got, err)
	}
	got, err = selectRuntime("apple", available)
	if err == nil || got != "" || !strings.Contains(err.Error(), "experimental") {
		t.Fatalf("selectRuntime(apple) = %q, %v; want explicit unsupported-runtime error", got, err)
	}
}

func TestExecutorAcceptsRegisteredRuntimeAdapter(t *testing.T) {
	executor := Executor{Adapters: map[string]RuntimeAdapter{"future": DockerAdapter{}}}
	if !executor.SupportsRuntime("future") {
		t.Fatal("registered runtime adapter was not exposed")
	}
	name, err := executor.runtime(store.Application{Runtime: "future"})
	if err != nil || name != "future" {
		t.Fatalf("registered runtime = %q, %v", name, err)
	}
}

func TestFailureDetailsUsesStableStageAndCode(t *testing.T) {
	err := failure("build", "build_failed", errors.New("exit status 1"))
	stage, code, detail := FailureDetails(err)
	if stage != "build" || code != "build_failed" || detail != "exit status 1" {
		t.Fatalf("failure details = %q, %q, %q", stage, code, detail)
	}
}

func TestEnsureDockerNetworkRequiresAName(t *testing.T) {
	err := (Executor{}).ensureDockerNetwork(context.Background(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("ensureDockerNetwork() = %v, want missing-network error", err)
	}
}

func TestValidateDockerNetworkRequiresInternalBridgeAndSubnet(t *testing.T) {
	valid := `[{"Name":"minidock","Driver":"bridge","Internal":true,"IPAM":{"Config":[{"Subnet":"172.31.251.0/24"}]}}]`
	if err := validateDockerNetwork([]byte(valid), "minidock", "172.31.251.0/24"); err != nil {
		t.Fatalf("valid internal bridge rejected: %v", err)
	}
	for _, invalid := range []string{
		`[{"Name":"minidock","Driver":"bridge","Internal":false,"IPAM":{"Config":[{"Subnet":"172.31.251.0/24"}]}}]`,
		`[{"Name":"minidock","Driver":"overlay","Internal":true,"IPAM":{"Config":[{"Subnet":"172.31.251.0/24"}]}}]`,
		`[{"Name":"minidock","Driver":"bridge","Internal":true,"IPAM":{"Config":[{"Subnet":"172.31.252.0/24"}]}}]`,
	} {
		if err := validateDockerNetwork([]byte(invalid), "minidock", "172.31.251.0/24"); err == nil {
			t.Fatalf("unsafe network accepted: %s", invalid)
		}
	}
}

func TestDockerProxyConfiguredRejectsUnixEndpoint(t *testing.T) {
	// dev.sh sets this globally so local development can use the host daemon.
	// This test exercises the production topology, where a raw socket is unsafe.
	t.Setenv("MINIDOCK_ENVIRONMENT", "production")
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	if err := DockerProxyConfigured(); err == nil || !strings.Contains(err.Error(), "tcp://") {
		t.Fatalf("raw Docker endpoint accepted: %v", err)
	}
	// The test intentionally does not assert success: a raw socket can exist in
	// the test host and must make the production topology fail closed.
}

func TestValidLocalRepositoryStaysWithinConfiguredRoot(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "service")
	if err := os.Mkdir(repository, 0700); err != nil {
		t.Fatal(err)
	}
	if !ValidLocalRepository(root, "file://"+repository) {
		t.Fatal("local repository below root was rejected")
	}
	if ValidLocalRepository(root, t.TempDir()) {
		t.Fatal("repository outside root was accepted")
	}
}

func TestLocalRepositoryGitDetection(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	git := filepath.Join(root, "git")
	if err := os.MkdirAll(plain, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(git, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if LocalRepositoryIsGit(root, "file://"+plain) {
		t.Fatal("plain source was identified as Git")
	}
	if !LocalRepositoryIsGit(root, "file://"+git) {
		t.Fatal("Git source was not identified")
	}
}

func TestDirectoryFingerprintIsStableAndExcludesGitMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: main"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := directoryFingerprint(root)
	if err != nil || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("first fingerprint = %q, %v", first, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: other"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := directoryFingerprint(root)
	if err != nil || second != first {
		t.Fatalf("git metadata changed fingerprint: %q, %v", second, err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.txt"), []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	third, err := directoryFingerprint(root)
	if err != nil || third == first {
		t.Fatalf("source change did not alter fingerprint: %q, %v", third, err)
	}
}

func TestPublicConfigurationDigestIsDeterministic(t *testing.T) {
	first := PublicConfigurationDigest(map[string]string{"PORT": "8080", "PUBLIC_URL": "https://example.test"}, map[string]string{"MODE": "production"})
	second := PublicConfigurationDigest(map[string]string{"PUBLIC_URL": "https://example.test", "PORT": "8080"}, map[string]string{"MODE": "production"})
	if !strings.HasPrefix(first, "sha256:") || first != second {
		t.Fatalf("configuration digests = %q, %q", first, second)
	}
}

func TestNonSecretRuntimeConfigurationIsJSONAndDoesNotMergeSecrets(t *testing.T) {
	configuration := NonSecretRuntimeConfiguration(map[string]string{"MODE": "production"})
	if configuration != `{"MODE":"production"}` {
		t.Fatalf("runtime configuration = %q", configuration)
	}
}

func TestGeneratedDockerfileUsesTemplateWithoutChangingRepository(t *testing.T) {
	work := t.TempDir()
	path, cleanup, err := generatedDockerfile(store.Application{Name: "site", Type: "static", InternalPort: 8080}, work)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	contents, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(contents), "FROM caddy") {
		t.Fatalf("unexpected template: %v %s", err, contents)
	}
	if _, err := os.Stat(filepath.Join(work, "Dockerfile")); !os.IsNotExist(err) {
		t.Fatal("template wrote into repository")
	}
}

func TestGeneratedDockerfileUpgradesLegacyViteSSRDetection(t *testing.T) {
	work := t.TempDir()
	if err := os.WriteFile(filepath.Join(work, "package.json"), []byte(`{"devDependencies":{"vite":"latest"},"scripts":{"build":"vite build --ssr"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "server.js"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	path, cleanup, err := generatedDockerfile(store.Application{Name: "site", Type: "node_ssr", InternalPort: 8080}, work)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	contents, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(contents), `CMD ["node", "server.js"]`) {
		t.Fatalf("legacy Vite SSR did not receive its template: %v %s", err, contents)
	}
}

func TestCustomApplicationRequiresDockerfile(t *testing.T) {
	_, _, err := generatedDockerfile(store.Application{Type: "custom"}, t.TempDir())
	if err == nil {
		t.Fatal("custom application without Dockerfile was accepted")
	}
}

func TestViteSSRTemplateUsesExplicitHealthEndpoint(t *testing.T) {
	dockerfile, ok := runtime.Dockerfile("vite_ssr", 8080, "/healthz")
	if !ok || !strings.Contains(dockerfile, "127.0.0.1:'+process.env.PORT+'/healthz") {
		t.Fatalf("Vite SSR health check is not explicit: %s", dockerfile)
	}
}

func TestParseContainerStatus(t *testing.T) {
	status, err := parseContainerInspect("running\thealthy\tminidock/api:12\t2026-07-14T12:00:00Z\n")
	if err != nil || status.State != "running" || status.Health != "healthy" || status.Image != "minidock/api:12" {
		t.Fatalf("unexpected inspect status: %#v, %v", status, err)
	}
	cpu, memory, err := parseContainerStats("0.42%\t20.1MiB / 256MiB\n")
	if err != nil || cpu != "0.42%" || memory != "20.1MiB / 256MiB" {
		t.Fatalf("unexpected stats: %q %q %v", cpu, memory, err)
	}
	if _, err := parseContainerInspect("not valid"); err == nil {
		t.Fatal("invalid inspect output was accepted")
	}
}

func TestContainerAppRootAndKernelCheck(t *testing.T) {
	root := t.TempDir()
	if got := containerAppRoot("FIELD VALUE\nappRoot  " + root + "\n"); got != root {
		t.Fatalf("containerAppRoot() = %q, want %q", got, root)
	}
	if hasContainerKernel(root) {
		t.Fatal("empty kernel directory was considered configured")
	}
	if err := os.Mkdir(filepath.Join(root, "kernels"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "kernels", "default"), []byte("kernel"), 0600); err != nil {
		t.Fatal(err)
	}
	if !hasContainerKernel(root) {
		t.Fatal("configured kernel was not detected")
	}
}

func TestCaddyProxyAdapterVerify(t *testing.T) {
	calledCount := 0
	server := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledCount++
		if r.Host != "my-app.local" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if calledCount == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	executor := Executor{
		ProxyURL:      server.URL,
		HealthTimeout: 1000 * time.Millisecond,
	}

	adapter := CaddyProxyAdapter{}
	app := store.Application{
		Domain: "my-app.local",
	}

	ctx := context.Background()
	err := adapter.Verify(ctx, executor, app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calledCount != 2 {
		t.Fatalf("expected 2 calls, got %d", calledCount)
	}

	for _, status := range []int{http.StatusFound, http.StatusInternalServerError} {
		server := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				t.Fatalf("probe path = %q, want /healthz", r.URL.Path)
			}
			w.WriteHeader(status)
		}))
		err := adapter.Verify(ctx, Executor{ProxyURL: server.URL, HealthTimeout: 50 * time.Millisecond}, store.Application{Domain: "my-app.local", HealthEndpoint: "/healthz"})
		server.Close()
		if err == nil {
			t.Fatalf("expected status %d to fail route verification", status)
		}
	}

	redirectServer := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://my-app.local"+r.URL.Path)
		w.WriteHeader(http.StatusPermanentRedirect)
	}))
	if probe := adapter.Probe(ctx, Executor{ProxyURL: redirectServer.URL, HealthTimeout: time.Second}, store.Application{Domain: "my-app.local", HealthEndpoint: "/healthz"}); !probe.Success || probe.StatusCode != http.StatusPermanentRedirect {
		t.Fatalf("same-host Caddy HTTPS redirect = %#v, want success", probe)
	}
	redirectServer.Close()

	externalRedirect := &http.Response{
		StatusCode: http.StatusPermanentRedirect,
		Header:     http.Header{"Location": {"https://attacker.example/healthz"}},
		Request:    httptest.NewRequest(http.MethodGet, "http://caddy/healthz", nil),
	}
	if validCaddyHTTPSRedirect(externalRedirect, store.Application{Domain: "my-app.local"}, "/healthz") {
		t.Fatal("cross-host redirect was accepted as Caddy route evidence")
	}

	contractServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" {
			t.Fatalf("probe path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("ready=minidock"))
	}))
	defer contractServer.Close()
	contract := Executor{ProxyURL: contractServer.URL, HealthTimeout: time.Second, ProxyExpectedStatus: http.StatusCreated, ProxyExpectedContent: "ready=minidock"}
	if probe := adapter.Probe(ctx, contract, store.Application{Domain: "my-app.local", HealthEndpoint: "/ready"}); !probe.Success || probe.StatusCode != http.StatusCreated || probe.Observer == "" {
		t.Fatalf("configured contract probe = %#v", probe)
	}
	contract.ProxyExpectedContent = "absent"
	if probe := adapter.Probe(ctx, contract, store.Application{Domain: "my-app.local", HealthEndpoint: "/ready"}); probe.Success {
		t.Fatalf("content mismatch was accepted: %#v", probe)
	}

	calledCount = 0
	failingServer := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledCount++
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer failingServer.Close()

	executorFailing := Executor{
		ProxyURL:      failingServer.URL,
		HealthTimeout: 50 * time.Millisecond,
	}

	err = adapter.Verify(ctx, executorFailing, app)
	if err == nil {
		t.Fatal("expected verify to fail on gateway errors")
	}
}

func TestCaddyProxyAdapterApplyCreatesThenReplacesStableRoute(t *testing.T) {
	requests := 0
	server := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/id/minidock-app-7" && r.URL.Path != "/config/apps/http/servers/srv0/routes" {
			t.Fatalf("unexpected Caddy API path %s", r.URL.Path)
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"172.18.0.4:8080"}]}]}`)
			return
		}
		if !strings.Contains(string(mustRead(t, r.Body)), `"dial":"172.18.0.4:8080"`) {
			t.Fatal("route did not contain requested candidate upstream")
		}
		if requests == 1 {
			if r.Method != http.MethodPatch {
				t.Fatalf("existing route method = %s, want PATCH", r.Method)
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	err := (CaddyProxyAdapter{}).Apply(context.Background(), Executor{CaddyAdminURL: server.URL}, store.Application{ID: 7, Domain: "api.example.test"}, "172.18.0.4:8080")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("Caddy calls = %d, want create fallback and verification", requests)
	}
}

func TestCaddyProxyAdapterRejectsStaleAppliedUpstream(t *testing.T) {
	server := newIPv4Server(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"handle":[{"handler":"reverse_proxy","upstreams":[{"dial":"172.18.0.3:8080"}]}]}`)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	err := (CaddyProxyAdapter{}).Apply(context.Background(), Executor{CaddyAdminURL: server.URL}, store.Application{ID: 7, Domain: "api.example.test"}, "172.18.0.4:8080")
	if err == nil || !strings.Contains(err.Error(), "did not retain candidate upstream") {
		t.Fatalf("stale Caddy route was accepted: %v", err)
	}
}

func TestRuntimeEnvFileProtectsSecretAndRejectsAmbiguousValues(t *testing.T) {
	path, err := runtimeEnvFile(t.TempDir(), map[string]string{"PUBLIC": "visible", "RUNTIME_SECRET": "canary-secret"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("runtime env permissions = %o, want 600", info.Mode().Perm())
	}
	contents, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(contents), "RUNTIME_SECRET=canary-secret") {
		t.Fatalf("runtime env content = %q, %v", contents, err)
	}
	removeRuntimeEnvFile(path)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime env file still exists: %v", err)
	}
	if _, err := runtimeEnvFile(t.TempDir(), map[string]string{"SECRET": "line1\nline2"}); err == nil {
		t.Fatal("multiline runtime secret was accepted")
	}
}

func mustRead(t *testing.T, body io.ReadCloser) []byte {
	t.Helper()
	value, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// newIPv4Server keeps proxy tests runnable in sandboxes where IPv6 loopback is
// disabled, while preserving the real HTTP client/server interaction.
func newIPv4Server(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) {
			t.Skip("the current sandbox forbids loopback listeners")
		}
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func TestVerifyArtifactMissingDigestOrImage(t *testing.T) {
	executor := Executor{}
	app := store.Application{Name: "test-app", Runtime: "docker"}

	err := executor.VerifyArtifact(context.Background(), app, "", "sha256:123")
	stage, code, _ := FailureDetails(err)
	if stage != "rollback" || code != "artifact_evidence_missing" {
		t.Fatalf("expected artifact_evidence_missing, got stage=%q, code=%q", stage, code)
	}

	err = executor.VerifyArtifact(context.Background(), app, "image:v1", "")
	stage, code, _ = FailureDetails(err)
	if stage != "rollback" || code != "artifact_evidence_missing" {
		t.Fatalf("expected artifact_evidence_missing, got stage=%q, code=%q", stage, code)
	}
}

func TestVerifyArtifactMissingImageInRuntime(t *testing.T) {
	executor := Executor{}
	app := store.Application{Name: "test-app", Runtime: "docker"}

	err := executor.VerifyArtifact(context.Background(), app, "nonexistent-image:latest", "sha256:abc")
	if err == nil {
		t.Fatal("expected error for nonexistent image in runtime")
	}
	stage, code, _ := FailureDetails(err)
	if stage != "rollback" {
		t.Fatalf("expected stage rollback, got %q", stage)
	}
	if code != "artifact_missing" && code != "runtime_unavailable" {
		t.Fatalf("expected artifact_missing or runtime_unavailable, got code %q", code)
	}
}

func TestValidateDockerContainerSecurityArgs(t *testing.T) {
	safeArgs := []string{"run", "--detach", "--name", "minidock-app", "--network", "minidock", "nginx:alpine"}
	if err := ValidateDockerContainerSecurityArgs(safeArgs); err != nil {
		t.Fatalf("safe args rejected: %v", err)
	}

	unsafeCases := [][]string{
		{"run", "--privileged", "nginx:alpine"},
		{"run", "-v", "/var/run/docker.sock:/var/run/docker.sock", "nginx:alpine"},
		{"run", "--volume=/etc:/etc", "nginx:alpine"},
		{"run", "--network", "host", "nginx:alpine"},
		{"run", "--device", "/dev/snd", "nginx:alpine"},
		{"run", "--cap-add", "SYS_ADMIN", "nginx:alpine"},
	}

	for _, unsafe := range unsafeCases {
		if err := ValidateDockerContainerSecurityArgs(unsafe); err == nil {
			t.Fatalf("unsafe args accepted: %v", unsafe)
		}
	}
}
