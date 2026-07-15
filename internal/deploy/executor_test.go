package deploy

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/julieta/minidock/internal/store"
)

func TestDeployRejectsUnsafeApplicationName(t *testing.T) {
	_, err := (Executor{}).Deploy(context.Background(), store.Application{Name: "../escape"}, DeploymentConfiguration{}, io.Discard)
	if err == nil {
		t.Fatal("expected invalid application name to be rejected")
	}
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
