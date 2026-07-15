package deploy

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/julieta/minidock/internal/runtime"
	"github.com/julieta/minidock/internal/store"
)

var safeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Failure is the machine-readable cause of a release failure. Its stage is a
// stable contract for the store and UI; Err retains the actionable provider
// detail for the log.
type Failure struct {
	Stage string
	Code  string
	Err   error
}

func (f *Failure) Error() string { return f.Err.Error() }
func (f *Failure) Unwrap() error { return f.Err }

func failure(stage, code string, err error) error {
	if err == nil {
		return nil
	}
	return &Failure{Stage: stage, Code: code, Err: err}
}

// FailureDetails returns the normalized cause, with a safe fallback for
// adapter errors that have not yet been classified.
func FailureDetails(err error) (stage, code, detail string) {
	var classified *Failure
	if errors.As(err, &classified) {
		return classified.Stage, classified.Code, classified.Err.Error()
	}
	if err == nil {
		return "", "", ""
	}
	return "start", "runtime_error", err.Error()
}

type Executor struct {
	WorkspacePath, LogPath, Network, Runtime                                  string
	LocalRepositoriesPath, GitHubAppID, GitHubAppPrivateKeyPath, GitHubAPIURL string
	// Adapters lets a host add a runtime without making releases or callers
	// depend on a Docker-specific command. Empty uses the built-in adapters.
	Adapters map[string]RuntimeAdapter
}

// RuntimeAdapter materializes the provider-neutral release contract for one
// runtime. Provider-specific networking, labels and command-line details stay
// behind this boundary.
type RuntimeAdapter interface {
	Name() string
	BuildCommand() string
	SupportsBuildSecrets() bool
	Start(context.Context, Executor, io.Writer, store.Application, string, map[string]string) (string, error)
	Control(context.Context, Executor, io.Writer, store.Application, string) error
	Logs(context.Context, Executor, io.Writer, store.Application) error
	Status(context.Context, Executor, store.Application) (ContainerStatus, error)
	RemoveImage(context.Context, Executor, store.Application, string) error
}

func (e Executor) adapters() map[string]RuntimeAdapter {
	if len(e.Adapters) != 0 {
		return e.Adapters
	}
	return map[string]RuntimeAdapter{
		"docker": DockerAdapter{},
		"apple":  AppleContainerAdapter{},
	}
}

// SupportsRuntime lets callers validate an explicit runtime choice without
// hard-coding the built-in provider names.
func (e Executor) SupportsRuntime(name string) bool {
	_, ok := e.adapters()[name]
	return ok
}

func (e Executor) adapter(application store.Application) (RuntimeAdapter, error) {
	name, err := e.runtime(application)
	if err != nil {
		return nil, err
	}
	adapter, ok := e.adapters()[name]
	if !ok {
		return nil, fmt.Errorf("runtime adapter %q is not configured", name)
	}
	return adapter, nil
}

// RuntimeDiagnostic describes whether a locally installed container runtime is
// ready for MiniDock. Detail is intentionally operational guidance, not a
// command error that users must decipher during a deployment.
type RuntimeDiagnostic struct {
	Name, Detail, SetupCommand string
	Installed, Ready           bool
}

// ValidLocalRepository allows a local checkout only beneath the host directory
// explicitly mounted into MiniDock. It prevents the panel from turning Git
// clone into arbitrary filesystem read access.
func ValidLocalRepository(root, repository string) bool {
	_, err := LocalRepositoryPath(root, repository)
	return err == nil
}

// LocalRepositoryPath resolves a file:// source beneath the configured root.
// The returned path is safe to use as a build context.
func LocalRepositoryPath(root, repository string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("local repository root is not configured")
	}
	if strings.HasPrefix(repository, "file://") {
		repository = strings.TrimPrefix(repository, "file://")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(repository)
	if err != nil {
		return "", err
	}
	if path != root && !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("local repository is outside the configured root")
	}
	return path, nil
}

func LocalRepositoryIsGit(root, repository string) bool {
	path, err := LocalRepositoryPath(root, repository)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

type DeploymentConfiguration struct {
	Runtime      map[string]string
	BuildArgs    map[string]string
	BuildSecrets map[string]string
}

// ContainerStatus is a point-in-time operational view. Values are display
// strings because the container CLIs already provide human-readable CPU and
// memory units and MiniDock does not aggregate them yet.
type ContainerStatus struct {
	State, Health, Image, StartedAt string
	CPU, Memory                     string
}

func Available() map[string]bool {
	available := map[string]bool{"docker": false, "apple": false}
	for _, diagnostic := range RuntimeDiagnostics() {
		available[diagnostic.Name] = diagnostic.Ready
	}
	return available
}

// RuntimeDiagnostics checks the command-line clients without modifying the
// host. In particular, Apple Container requires its API service and a default
// kernel before it can build or run a workload.
func RuntimeDiagnostics() []RuntimeDiagnostic {
	return []RuntimeDiagnostic{
		appleRuntimeDiagnostic(),
		runtimeDiagnostic("docker", "docker", []string{"info"}, "", "Docker"),
	}
}

func appleRuntimeDiagnostic() RuntimeDiagnostic {
	path, err := exec.LookPath("container")
	if err != nil {
		return RuntimeDiagnostic{Name: "apple", Detail: "Apple Container no está instalado."}
	}
	output, err := exec.Command(path, "system", "status").CombinedOutput()
	if err != nil {
		return RuntimeDiagnostic{Name: "apple", Installed: true, Detail: "Apple Container está instalado, pero aún no está preparado. Inicia su servicio; si solicita un kernel predeterminado, completa esa instalación.", SetupCommand: "container system start"}
	}
	root := containerAppRoot(string(output))
	if root == "" || !hasContainerKernel(root) {
		return RuntimeDiagnostic{Name: "apple", Installed: true, Detail: "Apple Container está activo, pero no tiene un kernel predeterminado. Configúralo antes de desplegar.", SetupCommand: "container system kernel set --recommended"}
	}
	return RuntimeDiagnostic{Name: "apple", Installed: true, Ready: true, Detail: "Apple Container está listo."}
}

func containerAppRoot(status string) string {
	for _, line := range strings.Split(status, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "appRoot") {
			return strings.TrimSpace(strings.TrimPrefix(line, "appRoot"))
		}
	}
	return ""
}

func hasContainerKernel(root string) bool {
	entries, err := os.ReadDir(filepath.Join(root, "kernels"))
	return err == nil && len(entries) > 0
}

func runtimeDiagnostic(name, command string, arguments []string, setup, label string) RuntimeDiagnostic {
	path, err := exec.LookPath(command)
	if err != nil {
		return RuntimeDiagnostic{Name: name, Detail: label + " no está instalado."}
	}
	output, err := exec.Command(path, arguments...).CombinedOutput()
	if err == nil {
		return RuntimeDiagnostic{Name: name, Installed: true, Ready: true, Detail: label + " está listo."}
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		detail = label + " no responde."
	}
	return RuntimeDiagnostic{Name: name, Installed: true, Detail: detail, SetupCommand: setup}
}

func (e Executor) runtime(application store.Application) (string, error) {
	wanted := application.Runtime
	if wanted == "" || wanted == "auto" {
		wanted = e.Runtime
	}
	if wanted == "" {
		wanted = "auto"
	}
	available := Available()
	for name := range e.adapters() {
		available[name] = true
	}
	return selectRuntime(wanted, available)
}

// selectRuntime keeps the automatic choice aligned with MiniDock's default
// exposure path. Caddy Docker Proxy reads Docker labels, while Apple Container
// requires a separately configured reverse proxy and cannot share that dynamic
// routing. Apple Container remains selectable explicitly for such hosts.
func selectRuntime(wanted string, available map[string]bool) (string, error) {
	if wanted == "auto" {
		if available["docker"] {
			return "docker", nil
		}
		if available["apple"] {
			return "apple", nil
		}
		return "", fmt.Errorf("no supported container runtime is available")
	}
	if !available[wanted] {
		return "", fmt.Errorf("requested runtime %q is not available", wanted)
	}
	return wanted, nil
}

func (e Executor) Deploy(ctx context.Context, application store.Application, configuration DeploymentConfiguration, output io.Writer) (string, error) {
	_, _ = fmt.Fprintln(output, "[preflight] verificando el runtime de contenedores")
	adapter, err := e.adapter(application)
	if err != nil {
		return "", failure("start", "runtime_unavailable", fmt.Errorf("runtime preflight: %w", err))
	}
	_, _ = fmt.Fprintf(output, "[preflight] runtime seleccionado: %s\n", adapter.Name())
	if !safeName.MatchString(application.Name) {
		return "", fmt.Errorf("invalid application name")
	}
	_, _ = fmt.Fprintln(output, "[source] preparando el código fuente")
	if err := os.MkdirAll(e.WorkspacePath, 0700); err != nil {
		return "", failure("source", "workspace_unavailable", err)
	}
	repo := filepath.Join(e.WorkspacePath, application.Name)
	cloneEnvironment, err := e.cloneEnvironment(ctx, application)
	if err != nil {
		return "", failure("source", "authentication_failed", err)
	}
	localSource := filepath.IsAbs(application.Repository) || strings.HasPrefix(application.Repository, "file://")
	if localSource && !LocalRepositoryIsGit(e.LocalRepositoriesPath, application.Repository) {
		repo, err = LocalRepositoryPath(e.LocalRepositoriesPath, application.Repository)
		if err != nil {
			return "", failure("source", "local_source_invalid", err)
		}
	} else {
		if _, err := os.Stat(filepath.Join(repo, ".git")); os.IsNotExist(err) {
			if err = e.runWithEnv(ctx, output, cloneEnvironment, "git", "clone", "--branch", application.Branch, "--single-branch", application.Repository, repo); err != nil {
				return "", failure("source", "clone_failed", err)
			}
		}
		if err = e.runWithEnv(ctx, output, cloneEnvironment, "git", "-C", repo, "fetch", "origin", application.Branch); err != nil {
			return "", failure("source", "fetch_failed", err)
		}
		// FETCH_HEAD works for branches, tags, and fully-qualified refs without
		// treating a tag as a remote branch.
		if err = e.run(ctx, output, "git", "-C", repo, "checkout", "--force", "FETCH_HEAD"); err != nil {
			return "", failure("source", "checkout_failed", err)
		}
	}
	work := filepath.Clean(filepath.Join(repo, application.WorkDir))
	if work != repo && !strings.HasPrefix(work, repo+string(os.PathSeparator)) {
		return "", failure("source", "work_directory_invalid", fmt.Errorf("work directory escapes repository"))
	}
	dockerfile, cleanup, err := generatedDockerfile(application, work)
	if err != nil {
		return "", failure("build", "contract_invalid", err)
	}
	defer cleanup()
	_, _ = fmt.Fprintln(output, "[build] construyendo la imagen; aquí aparecerán las dependencias y el build del proyecto")
	image := fmt.Sprintf("minidock/%s:%d", application.Name, time.Now().UTC().Unix())
	command := adapter.BuildCommand()
	buildArgs := []string{"build", "--tag", image}
	if dockerfile != "" {
		buildArgs = append(buildArgs, "--file", dockerfile)
	}
	if !adapter.SupportsBuildSecrets() && len(configuration.BuildSecrets) > 0 {
		return image, failure("build", "build_secrets_unsupported", fmt.Errorf("runtime %s does not support build secrets", adapter.Name()))
	}
	for _, name := range sortedNames(configuration.BuildArgs) {
		buildArgs = append(buildArgs, "--build-arg", name+"="+configuration.BuildArgs[name])
	}
	for _, name := range sortedNames(configuration.BuildSecrets) {
		buildArgs = append(buildArgs, "--secret", "id="+name+",env="+name)
	}
	buildArgs = append(buildArgs, work)
	if err := e.runWithEnv(ctx, output, configuration.BuildSecrets, command, buildArgs...); err != nil {
		return image, failure("build", "build_failed", err)
	}
	return adapter.Start(ctx, e, output, application, image, configuration.Runtime)
}

func (e Executor) cloneEnvironment(ctx context.Context, application store.Application) (map[string]string, error) {
	if filepath.IsAbs(application.Repository) || strings.HasPrefix(application.Repository, "file://") {
		if !ValidLocalRepository(e.LocalRepositoriesPath, application.Repository) {
			return nil, fmt.Errorf("local repository is outside MINIDOCK_LOCAL_REPOSITORIES_PATH")
		}
		return nil, nil
	}
	if application.GitHubInstallationID == 0 {
		return nil, nil
	}
	token, err := e.githubInstallationToken(ctx, application.GitHubInstallationID)
	if err != nil {
		return nil, err
	}
	repository, err := url.Parse(application.Repository)
	if err != nil || repository.Scheme != "https" || repository.Host == "" {
		return nil, fmt.Errorf("GitHub App requires an HTTPS repository URL")
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	return map[string]string{"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "http.https://" + repository.Host + "/.extraheader", "GIT_CONFIG_VALUE_0": "AUTHORIZATION: basic " + encoded}, nil
}

func (e Executor) githubInstallationToken(ctx context.Context, installationID int64) (string, error) {
	if e.GitHubAppID == "" || e.GitHubAppPrivateKeyPath == "" {
		return "", fmt.Errorf("GitHub App is not configured")
	}
	keyBytes, err := os.ReadFile(e.GitHubAppPrivateKeyPath)
	if err != nil {
		return "", fmt.Errorf("read GitHub App key: %w", err)
	}
	key, err := parseRSAPrivateKey(keyBytes)
	if err != nil {
		return "", err
	}
	jwt, err := githubJWT(e.GitHubAppID, key)
	if err != nil {
		return "", err
	}
	base := strings.TrimRight(e.GitHubAPIURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", base, installationID), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("request GitHub installation token: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("GitHub installation token request failed: %s", response.Status)
	}
	var payload struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil || payload.Token == "" {
		return "", fmt.Errorf("read GitHub installation token")
	}
	return payload.Token, nil
}

func parseRSAPrivateKey(value []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(value)
	if block == nil {
		return nil, fmt.Errorf("invalid GitHub App private key")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	rsaKey, ok := key.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("GitHub App private key is not RSA")
	}
	return rsaKey, nil
}

func githubJWT(appID string, key *rsa.PrivateKey) (string, error) {
	now := time.Now().UTC()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": appID})
	claims := base64.RawURLEncoding.EncodeToString(payload)
	unsigned := header + "." + claims
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// generatedDockerfile leaves repository Dockerfiles untouched. Built-in types
// use a temporary Dockerfile so the repository remains free of deployment
// infrastructure; custom types retain the explicit Dockerfile contract.
func generatedDockerfile(application store.Application, work string) (string, func(), error) {
	if _, err := os.Stat(filepath.Join(work, "Dockerfile")); err == nil {
		return "", func() {}, nil
	} else if !os.IsNotExist(err) {
		return "", nil, err
	}
	kind := application.Type
	// Applications created before Vite SSR had its own template may still be
	// recorded as generic node_ssr. Re-inspect that ambiguous type at build time
	// so existing projects become compatible without a database migration.
	if kind == "node_ssr" && runtime.Detect(work).Type == "vite_ssr" {
		kind = "vite_ssr"
	}
	contents, ok := runtime.Dockerfile(kind, application.InternalPort)
	if !ok {
		return "", nil, fmt.Errorf("Dockerfile required for custom application")
	}
	file, err := os.CreateTemp("", "minidock-"+application.Name+"-Dockerfile-*")
	if err != nil {
		return "", nil, fmt.Errorf("create runtime template: %w", err)
	}
	name := file.Name()
	if _, err := file.WriteString(contents); err != nil {
		file.Close()
		os.Remove(name)
		return "", nil, fmt.Errorf("write runtime template: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(name)
		return "", nil, err
	}
	return name, func() { _ = os.Remove(name) }, nil
}

// Rollback starts a previously successful image with the current runtime
// configuration. Images are immutable tags created by Deploy.
func (e Executor) Rollback(ctx context.Context, application store.Application, image string, configuration DeploymentConfiguration, output io.Writer) error {
	adapter, err := e.adapter(application)
	if err != nil {
		return err
	}
	_, err = adapter.Start(ctx, e, output, application, image, configuration.Runtime)
	return err
}

// DockerAdapter keeps Caddy labels and Docker network handling private to the
// Docker implementation. They are not part of a release's public contract.
type DockerAdapter struct{}

func (DockerAdapter) Name() string               { return "docker" }
func (DockerAdapter) BuildCommand() string       { return "docker" }
func (DockerAdapter) SupportsBuildSecrets() bool { return true }

func (DockerAdapter) Start(ctx context.Context, e Executor, output io.Writer, application store.Application, image string, environment map[string]string) (string, error) {
	container := "minidock-" + application.Name
	if err := e.ensureDockerNetwork(ctx, output); err != nil {
		return image, failure("start", "network_unavailable", err)
	}
	_ = e.run(ctx, output, "docker", "rm", "--force", container)
	args := []string{"run", "--detach", "--name", container, "--restart", "unless-stopped", "--network", e.Network, "--label", "minidock.application=" + fmt.Sprint(application.ID), "--label", "caddy=" + application.Domain, "--label", "caddy.reverse_proxy={{upstreams " + fmt.Sprint(application.InternalPort) + "}}"}
	if template, ok := runtime.For(application.Type); ok {
		args = append(args, "--cpus", template.CPUs, "--memory", template.Memory)
	}
	for _, name := range sortedNames(environment) {
		args = append(args, "--env", name+"="+environment[name])
	}
	args = append(args, image)
	if err := e.run(ctx, output, "docker", args...); err != nil {
		return image, failure("start", "container_start_failed", err)
	}
	if err := e.waitHealthy(ctx, container, output); err != nil {
		// Do not leave a known-unhealthy replacement serving traffic. The prior
		// image remains available in the deployment history for rollback.
		_ = e.run(ctx, output, "docker", "rm", "--force", container)
		return image, failure("health", "health_check_failed", err)
	}
	return image, nil
}

// AppleContainerAdapter uses loopback publication until its proxy adapter is
// configured. It therefore does not inherit Docker labels or Docker networks.
type AppleContainerAdapter struct{}

func (AppleContainerAdapter) Name() string               { return "apple" }
func (AppleContainerAdapter) BuildCommand() string       { return "container" }
func (AppleContainerAdapter) SupportsBuildSecrets() bool { return false }

func (AppleContainerAdapter) Start(ctx context.Context, e Executor, output io.Writer, application store.Application, image string, environment map[string]string) (string, error) {
	container := "minidock-" + application.Name
	_ = e.run(ctx, output, "container", "delete", "--force", container)
	result, err := e.runApple(ctx, output, application, image, container, environment)
	return result, failure("start", "container_start_failed", err)
}

// ensureDockerNetwork makes direct local execution behave like Compose, which
// normally creates the named application network before MiniDock starts.
// The inspect/create sequence is safe when the network already exists.
func (e Executor) ensureDockerNetwork(ctx context.Context, output io.Writer) error {
	if strings.TrimSpace(e.Network) == "" {
		return fmt.Errorf("Docker network is not configured")
	}
	check := exec.CommandContext(ctx, "docker", "network", "inspect", e.Network)
	if err := check.Run(); err == nil {
		return nil
	}
	_, _ = fmt.Fprintf(output, "[runtime] creando red Docker %s\n", e.Network)
	if err := e.run(ctx, output, "docker", "network", "create", e.Network); err != nil {
		// Another worker or Compose may have created it between inspect and
		// create. Rechecking turns that benign race into a successful deploy.
		if err := exec.CommandContext(ctx, "docker", "network", "inspect", e.Network).Run(); err != nil {
			return fmt.Errorf("create Docker network %q: %w", e.Network, err)
		}
	}
	return nil
}

func (e Executor) waitHealthy(ctx context.Context, container string, output io.Writer) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		command := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Running}}{{end}}", container)
		status, err := command.Output()
		if err != nil {
			return err
		}
		value := strings.TrimSpace(string(status))
		fmt.Fprintln(output, "health:", value)
		if value == "healthy" || value == "true" {
			return nil
		}
		if value == "unhealthy" || time.Now().After(deadline) {
			return fmt.Errorf("health check failed: %s", value)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (e Executor) Control(ctx context.Context, application store.Application, action string, output io.Writer) error {
	if action != "start" && action != "stop" && action != "restart" {
		return fmt.Errorf("invalid action")
	}
	adapter, err := e.adapter(application)
	if err != nil {
		return err
	}
	return adapter.Control(ctx, e, output, application, action)
}
func (e Executor) Logs(ctx context.Context, application store.Application, output io.Writer) error {
	adapter, err := e.adapter(application)
	if err != nil {
		return err
	}
	return adapter.Logs(ctx, e, output, application)
}

// Status reads a single runtime snapshot. Docker stats is deliberately
// non-streaming so rendering the administration page never leaves a process
// running. Apple Container support remains explicit until it offers an
// equivalent stable stats interface.
func (e Executor) Status(ctx context.Context, application store.Application) (ContainerStatus, error) {
	adapter, err := e.adapter(application)
	if err != nil {
		return ContainerStatus{}, err
	}
	return adapter.Status(ctx, e, application)
}

func (DockerAdapter) Control(ctx context.Context, e Executor, output io.Writer, application store.Application, action string) error {
	return e.run(ctx, output, "docker", action, "minidock-"+application.Name)
}

func (DockerAdapter) Logs(ctx context.Context, e Executor, output io.Writer, application store.Application) error {
	return e.run(ctx, output, "docker", "logs", "--tail", "200", "minidock-"+application.Name)
}

func (DockerAdapter) Status(ctx context.Context, e Executor, application store.Application) (ContainerStatus, error) {
	container := "minidock-" + application.Name
	inspect := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.State.Status}}\t{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}\t{{.Config.Image}}\t{{.State.StartedAt}}", container)
	output, err := inspect.Output()
	if err != nil {
		return ContainerStatus{}, err
	}
	status, err := parseContainerInspect(string(output))
	if err != nil {
		return ContainerStatus{}, err
	}
	stats := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{.CPUPerc}}\t{{.MemUsage}}", container)
	output, err = stats.Output()
	if err != nil {
		return ContainerStatus{}, err
	}
	cpu, memory, err := parseContainerStats(string(output))
	if err != nil {
		return ContainerStatus{}, err
	}
	status.CPU, status.Memory = cpu, memory
	return status, nil
}

// RemoveImage only removes an unreferenced Docker tag chosen by the retention
// policy. It never forces deletion, so Docker protects a running container.
func (e Executor) RemoveImage(ctx context.Context, application store.Application, image string) error {
	adapter, err := e.adapter(application)
	if err != nil {
		return err
	}
	return adapter.RemoveImage(ctx, e, application, image)
}

func (DockerAdapter) RemoveImage(ctx context.Context, e Executor, application store.Application, image string) error {
	return e.run(ctx, io.Discard, "docker", "image", "rm", image)
}

func (AppleContainerAdapter) Control(ctx context.Context, e Executor, output io.Writer, application store.Application, action string) error {
	if action == "restart" {
		if err := e.run(ctx, output, "container", "stop", "minidock-"+application.Name); err != nil {
			return err
		}
		action = "start"
	}
	return e.run(ctx, output, "container", action, "minidock-"+application.Name)
}

func (AppleContainerAdapter) Logs(ctx context.Context, e Executor, output io.Writer, application store.Application) error {
	return e.run(ctx, output, "container", "logs", "-n", "200", "minidock-"+application.Name)
}

func (AppleContainerAdapter) Status(context.Context, Executor, store.Application) (ContainerStatus, error) {
	return ContainerStatus{}, fmt.Errorf("resource metrics are not available for Apple Container yet")
}

func (AppleContainerAdapter) RemoveImage(context.Context, Executor, store.Application, string) error {
	return fmt.Errorf("image retention is not available for Apple Container yet")
}

func parseContainerInspect(output string) (ContainerStatus, error) {
	parts := strings.Split(strings.TrimSpace(output), "\t")
	if len(parts) != 4 || parts[0] == "" || parts[2] == "" {
		return ContainerStatus{}, fmt.Errorf("unexpected docker inspect output")
	}
	return ContainerStatus{State: parts[0], Health: parts[1], Image: parts[2], StartedAt: parts[3]}, nil
}

func parseContainerStats(output string) (cpu, memory string, err error) {
	parts := strings.Split(strings.TrimSpace(output), "\t")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("unexpected docker stats output")
	}
	return parts[0], parts[1], nil
}

func (e Executor) runApple(ctx context.Context, output io.Writer, application store.Application, image, name string, environment map[string]string) (string, error) {
	args := []string{"run", "--detach", "--name", name, "--publish", "127.0.0.1:" + fmt.Sprint(application.InternalPort) + ":" + fmt.Sprint(application.InternalPort)}
	for key, value := range environment {
		args = append(args, "--env", key+"="+value)
	}
	args = append(args, image)
	if err := e.run(ctx, output, "container", args...); err != nil {
		return image, err
	}
	fmt.Fprintln(output, "Apple Container started; reverse proxy must target 127.0.0.1:"+fmt.Sprint(application.InternalPort))
	return image, nil
}
func (e Executor) run(ctx context.Context, output io.Writer, name string, args ...string) error {
	return e.runWithEnv(ctx, output, nil, name, args...)
}

func (e Executor) runWithEnv(ctx context.Context, output io.Writer, environment map[string]string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(environment) > 0 {
		cmd.Env = os.Environ()
		for _, key := range sortedNames(environment) {
			cmd.Env = append(cmd.Env, key+"="+environment[key])
		}
	}
	cmd.Stdout = output
	cmd.Stderr = output
	return cmd.Run()
}

func sortedNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
