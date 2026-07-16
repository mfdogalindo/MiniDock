package deploy

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/julieta/minidock/internal/runtime"
	"github.com/julieta/minidock/internal/store"
)

var (
	safeName            = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	safeEnvironmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

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
	WorkspacePath, LogPath, Network, NetworkSubnet, Runtime                   string
	LocalRepositoriesPath, GitHubAppID, GitHubAppPrivateKeyPath, GitHubAPIURL string
	// Adapters lets a host add a runtime without making releases or callers
	// depend on a Docker-specific command. Empty uses the built-in adapters.
	Adapters map[string]RuntimeAdapter
	Proxy    ProxyAdapter
	ProxyURL string
	// CaddyAdminURL is deliberately separate from ProxyURL: the former is a
	// private control-plane endpoint while the latter verifies public traffic.
	CaddyAdminURL                                            string
	ProxyExpectedStatus                                      int
	ProxyExpectedContent                                     string
	SourceTimeout, BuildTimeout, StartTimeout, HealthTimeout time.Duration
	WorkspaceMaxBytes                                        int64
}

func (e Executor) sourceTimeout() time.Duration {
	if e.SourceTimeout > 0 {
		return e.SourceTimeout
	}
	return 5 * time.Minute
}
func (e Executor) buildTimeout() time.Duration {
	if e.BuildTimeout > 0 {
		return e.BuildTimeout
	}
	return 20 * time.Minute
}
func (e Executor) startTimeout() time.Duration {
	if e.StartTimeout > 0 {
		return e.StartTimeout
	}
	return 2 * time.Minute
}
func (e Executor) healthTimeout() time.Duration {
	if e.HealthTimeout > 0 {
		return e.HealthTimeout
	}
	return 30 * time.Second
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

// ProxyAdapter defines the contract for exposing applications via a reverse proxy.
// Independent of the runtime, it can apply routes and verify routing health.
type ProxyAdapter interface {
	Name() string
	Apply(ctx context.Context, e Executor, application store.Application, target string) error
	Verify(ctx context.Context, e Executor, application store.Application) error
}

// RouteProbe is evidence from an HTTP request made through the configured
// reverse proxy. It deliberately records the observer and response contract,
// rather than inferring routing from a container state.
type RouteProbe struct {
	Success          bool
	StatusCode       int
	CheckedAt        time.Time
	Observer, Detail string
}

func (e Executor) proxy() ProxyAdapter {
	if e.Proxy != nil {
		return e.Proxy
	}
	return CaddyProxyAdapter{}
}

// ProbeRoute verifies the public route and returns the evidence used by the
// operations collector. Built-in Caddy exposes its HTTP response; custom
// adapters remain compatible while they implement the richer contract.
func (e Executor) ProbeRoute(ctx context.Context, application store.Application) RouteProbe {
	if probe, ok := e.proxy().(interface {
		Probe(context.Context, Executor, store.Application) RouteProbe
	}); ok {
		return probe.Probe(ctx, e, application)
	}
	err := e.proxy().Verify(ctx, e, application)
	return RouteProbe{Success: err == nil, CheckedAt: time.Now().UTC(), Observer: e.proxy().Name(), Detail: errorDetail(err)}
}

func errorDetail(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
	Runtime, BuildArgs, BuildSecrets map[string]string
	// PublicRuntime is the unmerged runtime configuration. Runtime may also
	// contain decrypted secrets and therefore must never be persisted or used
	// as rollback evidence.
	PublicRuntime map[string]string
	// EffectiveRuntimeConfiguration is the canonical JSON snapshot of the
	// non-secret runtime configuration requested for this release. It is kept
	// separately from Runtime because Runtime also contains decrypted secrets.
	// The snapshot is release evidence and is safe to persist.
	EffectiveRuntimeConfiguration string
	// ConfigurationDigest identifies the public build/runtime configuration
	// consumed by a release. Secret values are deliberately excluded.
	ConfigurationDigest    string
	ReuseImage             string
	ReuseSourceRevision    string
	ReuseSourceFingerprint string
}

type StageTracker struct {
	QueueDuration  time.Duration
	SourceDuration time.Duration
	BuildDuration  time.Duration
	StartDuration  time.Duration
	HealthDuration time.Duration
	RouteDuration  time.Duration

	OnStageChange func(stage string)
	OnEvent       func(stage, eventType, message string)
}

type trackerKey struct{}

func WithTracker(ctx context.Context, tracker *StageTracker) context.Context {
	return context.WithValue(ctx, trackerKey{}, tracker)
}

func TrackerFromContext(ctx context.Context) *StageTracker {
	if tracker, ok := ctx.Value(trackerKey{}).(*StageTracker); ok {
		return tracker
	}
	return nil
}

// PublicConfigurationDigest returns a deterministic digest of non-secret
// configuration. It is suitable for persisted release evidence, unlike a
// digest of the complete deployment environment which could leak information
// about whether a secret changed.
func PublicConfigurationDigest(runtime, buildArgs map[string]string) string {
	encoded, err := json.Marshal(struct {
		BuildArgs map[string]string `json:"build_args"`
		Runtime   map[string]string `json:"runtime"`
	}{BuildArgs: buildArgs, Runtime: runtime})
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest)
}

// ReleaseResult is the evidence produced by a runtime adapter while
// materializing a provider-neutral release. It deliberately contains no
// secrets or provider configuration such as Docker labels.
type ReleaseResult struct {
	Image, SourceRevision, SourceFingerprint, ArtifactDigest, Runtime string
	// ObservedContainerID is the runtime identity observed after the release
	// became healthy. It is intentionally not inferred from a container name:
	// a name can be reused after a crash or a manual intervention.
	ObservedContainerID string
}

// ReconciliationResult records durable switch recovery completed at startup.
// Callers persist this result because the interrupted worker may no longer be
// available to append an event to its own deployment log.
type ReconciliationResult struct {
	RestoredApplicationIDs []int64
}

// NonSecretRuntimeConfiguration serializes configuration whose values are
// explicitly stored as public application configuration. Callers must never
// pass the merged runtime environment here because it can include secrets.
func NonSecretRuntimeConfiguration(values map[string]string) string {
	encoded, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	return string(encoded)
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

// DockerReady checks the same Docker API access that a deployment needs. It
// intentionally uses the CLI because MiniDock uses it for every Docker action;
// this catches a mounted-but-unreadable socket before a release is queued.
func DockerReady(ctx context.Context) error {
	if err := DockerProxyConfigured(); err != nil {
		return err
	}
	command := exec.CommandContext(ctx, "docker", "info")
	if output, err := command.CombinedOutput(); err != nil {
		detail := strings.TrimSpace(string(output))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("Docker socket or daemon is not usable: %s", detail)
	}
	return nil
}

// DockerProxyConfigured makes a raw Unix socket an invalid runtime topology.
// Docker's API grants daemon-equivalent privileges, so MiniDock may only reach
// it through the private TCP socket proxy declared by Compose.
func DockerProxyConfigured() error {
	if os.Getenv("MINIDOCK_ENVIRONMENT") == "development" {
		return nil
	}
	host := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if !strings.HasPrefix(host, "tcp://") {
		return fmt.Errorf("DOCKER_HOST must point to the private Docker socket proxy using tcp://")
	}
	if info, err := os.Stat("/var/run/docker.sock"); err == nil && info.Mode()&os.ModeSocket != 0 {
		return fmt.Errorf("raw Docker socket is present; remove the mount and use the socket proxy")
	}
	return nil
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

// selectRuntime keeps the automatic choice aligned with MiniDock's supported
// exposure path. Apple Container is deliberately not selectable yet: it lacks
// the equivalent route, probe, digest, metrics and retention contracts, so a
// successful release could otherwise make an unsupported claim.
func selectRuntime(wanted string, available map[string]bool) (string, error) {
	if wanted == "auto" {
		if available["docker"] {
			return "docker", nil
		}
		return "", fmt.Errorf("Docker is the only supported runtime; Apple Container is experimental until it implements equivalent route and health verification")
	}
	if wanted == "apple" {
		return "", fmt.Errorf("Apple Container is experimental and cannot create a supported release: route, health, digest, metrics and retention verification are unavailable")
	}
	if !available[wanted] {
		return "", fmt.Errorf("requested runtime %q is not available", wanted)
	}
	return wanted, nil
}

func (e Executor) Deploy(ctx context.Context, application store.Application, configuration DeploymentConfiguration, output io.Writer) (ReleaseResult, error) {
	result := ReleaseResult{}
	tracker := TrackerFromContext(ctx)

	_, _ = fmt.Fprintln(output, "[preflight] verificando el runtime de contenedores")
	adapter, err := e.adapter(application)
	if err != nil {
		return result, failure("start", "runtime_unavailable", fmt.Errorf("runtime preflight: %w", err))
	}
	result.Runtime = adapter.Name()
	_, _ = fmt.Fprintf(output, "[preflight] runtime seleccionado: %s\n", adapter.Name())
	if !safeName.MatchString(application.Name) {
		return result, fmt.Errorf("invalid application name")
	}

	var image string
	if configuration.ReuseImage != "" {
		if tracker != nil {
			tracker.OnStageChange("start")
			tracker.OnEvent("start", "info", fmt.Sprintf("Reusando imagen construida previamente: %s", configuration.ReuseImage))
		}
		image = configuration.ReuseImage
		result.Image = image
		result.SourceRevision = configuration.ReuseSourceRevision
		result.SourceFingerprint = configuration.ReuseSourceFingerprint
	} else {
		if tracker != nil {
			tracker.OnStageChange("source")
			tracker.OnEvent("source", "info", "Iniciando descarga y preparación del código fuente")
		}
		sourceStart := time.Now()

		_, _ = fmt.Fprintln(output, "[source] preparando el código fuente")
		sourceCtx, cancelSource := context.WithTimeout(ctx, e.sourceTimeout())
		defer cancelSource()
		if err := os.MkdirAll(e.WorkspacePath, 0700); err != nil {
			return result, failure("source", "workspace_unavailable", err)
		}
		repo := filepath.Join(e.WorkspacePath, application.Name)
		cloneEnvironment, err := e.cloneEnvironment(sourceCtx, application)
		if err != nil {
			return result, failure("source", "authentication_failed", err)
		}
		localSource := filepath.IsAbs(application.Repository) || strings.HasPrefix(application.Repository, "file://")
		if localSource && !LocalRepositoryIsGit(e.LocalRepositoriesPath, application.Repository) {
			repo, err = LocalRepositoryPath(e.LocalRepositoriesPath, application.Repository)
			if err != nil {
				return result, failure("source", "local_source_invalid", err)
			}
		} else {
			if _, err := os.Stat(filepath.Join(repo, ".git")); os.IsNotExist(err) {
				cloneArgs := []string{"clone", "--branch", application.Branch, "--single-branch", application.Repository, repo}
				// A webhook may pin a commit SHA. It is not a remote branch and Git
				// rejects it as --branch on a fresh clone, so fetch it afterwards.
				if isCommitSHA(application.Branch) {
					cloneArgs = []string{"clone", application.Repository, repo}
				}
				if err = e.runWithEnv(sourceCtx, output, cloneEnvironment, "git", cloneArgs...); err != nil {
					return result, failure("source", "clone_failed", err)
				}
			}
			if err = e.runWithEnv(sourceCtx, output, cloneEnvironment, "git", "-C", repo, "fetch", "origin", application.Branch); err != nil {
				return result, failure("source", "fetch_failed", err)
			}
			// FETCH_HEAD works for branches, tags, and fully-qualified refs without
			// treating a tag as a remote branch.
			if err = e.run(sourceCtx, output, "git", "-C", repo, "checkout", "--force", "FETCH_HEAD"); err != nil {
				return result, failure("source", "checkout_failed", err)
			}
		}
		work := filepath.Clean(filepath.Join(repo, application.WorkDir))
		if work != repo && !strings.HasPrefix(work, repo+string(os.PathSeparator)) {
			return result, failure("source", "work_directory_invalid", fmt.Errorf("work directory escapes repository"))
		}
		result.SourceRevision, result.SourceFingerprint, err = sourceEvidence(sourceCtx, repo, work)
		if err != nil {
			return result, failure("source", "source_fingerprint_failed", err)
		}
		cancelSource()

		if result.SourceRevision != "" {
			_, _ = fmt.Fprintf(output, "[source] revisión Git detectada: %s\n", result.SourceRevision)
			if tracker != nil {
				tracker.OnEvent("source", "info", fmt.Sprintf("Revisión Git: %s", result.SourceRevision))
			}
		} else {
			_, _ = fmt.Fprintln(output, "[source] no hay reproducibilidad Git para esta fuente")
		}
		if result.SourceFingerprint != "" {
			_, _ = fmt.Fprintf(output, "[source] huella del árbol de fuentes: %s\n", result.SourceFingerprint)
		}

		_, _ = fmt.Fprintln(output, "[source] analizando el contexto de construcción y espacio en disco")
		dockerIgnorePath := filepath.Join(work, ".dockerignore")
		hasCustomDockerIgnore := false
		if _, err := os.Stat(dockerIgnorePath); err == nil {
			hasCustomDockerIgnore = true
		}

		size, sizeErr := directorySize(work)
		if sizeErr == nil {
			sizeMB := float64(size) / (1024 * 1024)
			_, _ = fmt.Fprintf(output, "[source] tamaño del contexto de build: %.2f MB\n", sizeMB)
			if e.WorkspaceMaxBytes > 0 && size > e.WorkspaceMaxBytes {
				return result, failure("source", "workspace_quota_exceeded", fmt.Errorf("workspace is %d bytes; limit is %d bytes", size, e.WorkspaceMaxBytes))
			}
			if sizeMB > 50.0 && !hasCustomDockerIgnore {
				_, _ = fmt.Fprintln(output, "[source] ADVERTENCIA: el tamaño del contexto de construcción es alto (>50MB) y no existe un .dockerignore personalizado.")
				if tracker != nil {
					tracker.OnEvent("source", "warning", fmt.Sprintf("Contexto de construcción es alto (%.2f MB) sin .dockerignore", sizeMB))
				}
			}
		} else {
			return result, failure("source", "workspace_measure_failed", sizeErr)
		}

		if diskInfo, diskErr := diskSpaceInfo(work); diskErr == nil {
			_, _ = fmt.Fprintf(output, "[source] espacio en disco disponible en host: %.2f GB (%.1f%% usado)\n", diskInfo.AvailableGB, diskInfo.UsedPercent)
			if diskInfo.AvailableGB < 2.0 {
				_, _ = fmt.Fprintln(output, "[source] ADVERTENCIA: el espacio en disco disponible en el host es extremadamente bajo (<2GB)")
				if tracker != nil {
					tracker.OnEvent("source", "warning", fmt.Sprintf("Espacio en disco bajo: %.2f GB", diskInfo.AvailableGB))
				}
			}
		}

		var ignoreCleanup func() = func() {}
		if !hasCustomDockerIgnore {
			_, _ = fmt.Fprintln(output, "[source] aplicando .dockerignore gestionado para omitir directorios innecesarios")
			ignoreContents := ".git\nnode_modules\n.next\n.nuxt\ndist\nbuild\ntmp\n.gemini\n"
			if err := os.WriteFile(dockerIgnorePath, []byte(ignoreContents), 0600); err == nil {
				ignoreCleanup = func() {
					_ = os.Remove(dockerIgnorePath)
				}
			} else {
				_, _ = fmt.Fprintln(output, "[source] advertencia: no se pudo crear el archivo .dockerignore gestionado:", err)
			}
		} else {
			_, _ = fmt.Fprintln(output, "[source] usando archivo .dockerignore existente en el repositorio")
		}
		defer ignoreCleanup()
		dockerfile, cleanup, err := generatedDockerfile(application, work)
		if err != nil {
			return result, failure("build", "contract_invalid", err)
		}
		defer cleanup()

		if tracker != nil {
			tracker.SourceDuration = time.Since(sourceStart)
			tracker.OnEvent("source", "info", fmt.Sprintf("Código fuente preparado en %s", tracker.SourceDuration.Round(time.Millisecond)))
			tracker.OnStageChange("build")
			tracker.OnEvent("build", "info", "Iniciando compilación y empaquetado de la imagen Docker")
		}

		buildStart := time.Now()
		_, _ = fmt.Fprintln(output, "[build] construyendo la imagen; aquí aparecerán las dependencias y el build del proyecto")
		image = fmt.Sprintf("minidock/%s:%d", application.Name, time.Now().UTC().Unix())
		result.Image = image
		command := adapter.BuildCommand()
		buildArgs := []string{"build", "--tag", image}
		if dockerfile != "" {
			buildArgs = append(buildArgs, "--file", dockerfile)
		}
		if !adapter.SupportsBuildSecrets() && len(configuration.BuildSecrets) > 0 {
			return result, failure("build", "build_secrets_unsupported", fmt.Errorf("runtime %s does not support build secrets", adapter.Name()))
		}
		for _, name := range sortedNames(configuration.BuildArgs) {
			buildArgs = append(buildArgs, "--build-arg", name+"="+configuration.BuildArgs[name])
		}
		for _, name := range sortedNames(configuration.BuildSecrets) {
			buildArgs = append(buildArgs, "--secret", "id="+name+",env="+name)
		}
		buildArgs = append(buildArgs, work)
		buildCtx, cancelBuild := context.WithTimeout(ctx, e.buildTimeout())
		err = e.runWithEnv(buildCtx, output, configuration.BuildSecrets, command, buildArgs...)
		cancelBuild()
		if err != nil {
			return result, failure("build", "build_failed", err)
		}

		if tracker != nil {
			tracker.BuildDuration = time.Since(buildStart)
			tracker.OnEvent("build", "info", fmt.Sprintf("Imagen construida con éxito en %s", tracker.BuildDuration.Round(time.Millisecond)))
		}
	}

	startCtx, cancelStart := context.WithTimeout(ctx, e.startTimeout())
	result.Image, err = adapter.Start(startCtx, e, output, application, image, configuration.Runtime)
	cancelStart()
	if err != nil {
		return result, err
	}
	result.ArtifactDigest = e.artifactDigest(ctx, adapter.Name(), image)
	if adapter.Name() == "docker" {
		result.ObservedContainerID = e.dockerContainerID(ctx, "minidock-"+application.Name)
	}
	return result, nil
}

func isCommitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func directorySize(root string) (int64, error) {
	var size int64
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func (e Executor) artifactDigest(ctx context.Context, runtimeName, image string) string {
	if runtimeName != "docker" {
		return ""
	}
	// Local images do not normally have RepoDigests. The image ID is still a
	// content-addressed sha256 identifier and is the stable evidence available
	// before the image is pushed to a registry.
	output, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", image).Output()
	if err != nil {
		return ""
	}
	digest := strings.TrimSpace(string(output))
	if strings.HasPrefix(digest, "sha256:") {
		return digest
	}
	return ""
}

func sourceEvidence(ctx context.Context, repository, work string) (revision, fingerprint string, err error) {
	if output, gitErr := exec.CommandContext(ctx, "git", "-C", repository, "rev-parse", "HEAD").Output(); gitErr == nil {
		revision = strings.TrimSpace(string(output))
	}
	fingerprint, err = directoryFingerprint(work)
	return revision, fingerprint, err
}

func directoryFingerprint(root string) (string, error) {
	hash := sha256.New()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && entry.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, path := range files {
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		if _, err = io.WriteString(hash, filepath.ToSlash(relative)+"\x00"); err != nil {
			return "", err
		}
		file, err := os.Open(path)
		if err != nil {
			return "", err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
	}
	return "sha256:" + fmt.Sprintf("%x", hash.Sum(nil)), nil
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
	contents, ok := runtime.Dockerfile(kind, application.InternalPort, application.HealthEndpoint)
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

// Rollback starts a previously successful image with the target release's
// recorded public runtime configuration plus currently unlocked secrets.
// Images are immutable tags created by Deploy.
func (e Executor) Rollback(ctx context.Context, application store.Application, image string, configuration DeploymentConfiguration, output io.Writer) (ReleaseResult, error) {
	result := ReleaseResult{Image: image}
	adapter, err := e.adapter(application)
	if err != nil {
		return result, err
	}
	result.Runtime = adapter.Name()
	result.Image, err = adapter.Start(ctx, e, output, application, image, configuration.Runtime)
	if err != nil {
		return result, err
	}
	result.ArtifactDigest = e.artifactDigest(ctx, adapter.Name(), image)
	if adapter.Name() == "docker" {
		result.ObservedContainerID = e.dockerContainerID(ctx, "minidock-"+application.Name)
	}
	return result, nil
}

// VerifyArtifact proves that a rollback target still exists and is the exact
// content-addressed artifact recorded by its successful release. A tag alone
// is mutable and is therefore never sufficient evidence for a rollback.
func (e Executor) VerifyArtifact(ctx context.Context, application store.Application, image, digest string) error {
	if image == "" || digest == "" {
		return failure("rollback", "artifact_evidence_missing", fmt.Errorf("rollback release has no immutable artifact digest"))
	}
	adapter, err := e.adapter(application)
	if err != nil {
		return failure("rollback", "runtime_unavailable", err)
	}
	actual := e.artifactDigest(ctx, adapter.Name(), image)
	if actual == "" {
		return failure("rollback", "artifact_missing", fmt.Errorf("image %q is not available in runtime %s", image, adapter.Name()))
	}
	if actual != digest {
		return failure("rollback", "artifact_digest_mismatch", fmt.Errorf("image %q digest %s does not match recorded digest %s", image, actual, digest))
	}
	return nil
}

// Reconcile converges the durable Docker switch state after a process crash.
// A candidate never receives proxy labels. A preserved previous container is
// either restored when no primary exists or removed once the new primary is
// present, so the same domain never retains two active routes.
func (e Executor) Reconcile(ctx context.Context, applications []store.Application, output io.Writer) (ReconciliationResult, error) {
	result := ReconciliationResult{}
	for _, application := range applications {
		adapter, err := e.adapter(application)
		if err != nil || adapter.Name() != "docker" {
			continue
		}
		container := "minidock-" + application.Name
		candidate := container + "-candidate"
		next := container + "-next"
		previous := container + "-previous"
		// Caddy resumes the bootstrap Caddyfile after a restart. Re-apply the
		// durable active colour before cleaning anything: a candidate is the live
		// colour after the first successful API switch, not disposable scratch.
		if e.dockerContainerExists(ctx, candidate) {
			target, targetErr := e.dockerContainerTarget(ctx, candidate, application.InternalPort)
			if targetErr != nil {
				return result, targetErr
			}
			if err := e.proxy().Apply(ctx, e, application, target); err != nil {
				return result, fmt.Errorf("restore Caddy route for %s: %w", application.Name, err)
			}
			// A -next container is an interrupted, unconfirmed deployment. Keep the
			// already serving candidate and discard only the staging colour.
			if err := e.removeDockerContainerIfPresent(ctx, output, next); err != nil {
				return result, err
			}
			continue
		}
		primaryExists := e.dockerContainerExists(ctx, container)
		previousExists := e.dockerContainerExists(ctx, previous)
		if primaryExists {
			target, targetErr := e.dockerContainerTarget(ctx, container, application.InternalPort)
			if targetErr != nil {
				return result, targetErr
			}
			if err := e.proxy().Apply(ctx, e, application, target); err != nil {
				return result, fmt.Errorf("restore Caddy route for %s: %w", application.Name, err)
			}
			if err := e.removeDockerContainerIfPresent(ctx, output, next); err != nil {
				return result, err
			}
		}
		// A stopped previous container is the durable rollback point during a
		// switch. If MiniDock died after stopping it but before starting the
		// replacement, restore that exact container (including its runtime
		// environment) rather than recreating it from mutable configuration.
		if !primaryExists && previousExists {
			_, _ = fmt.Fprintf(output, "[reconcile] restoring interrupted switch for %s\n", application.Name)
			if err := e.run(ctx, output, "docker", "start", previous); err != nil {
				return result, fmt.Errorf("restart previous %s: %w", previous, err)
			}
			if err := e.waitHealthy(ctx, previous, output); err != nil {
				return result, failure("rollback", "reconcile_restore_unhealthy", fmt.Errorf("previous %s did not become healthy: %w", previous, err))
			}
			if err := e.run(ctx, output, "docker", "rename", previous, container); err != nil {
				return result, fmt.Errorf("rename restored %s: %w", previous, err)
			}
			target, targetErr := e.dockerContainerTarget(ctx, container, application.InternalPort)
			if targetErr != nil {
				return result, targetErr
			}
			if err := e.proxy().Apply(ctx, e, application, target); err != nil {
				return result, fmt.Errorf("restore Caddy route for %s: %w", application.Name, err)
			}
			result.RestoredApplicationIDs = append(result.RestoredApplicationIDs, application.ID)
			continue
		}
		// If the new primary exists, any previous container is stale. Removing it
		// guarantees that Caddy cannot retain two active routes for one domain.
		if primaryExists && previousExists {
			if err := e.removeDockerContainerIfPresent(ctx, output, previous); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

func (e Executor) dockerContainerExists(ctx context.Context, container string) bool {
	return exec.CommandContext(ctx, "docker", "container", "inspect", container).Run() == nil
}

func (e Executor) dockerContainerTarget(ctx context.Context, container string, port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid container port %d", port)
	}
	// Every managed release is attached to the dedicated MiniDock network. The
	// IP is deliberately captured before the route switch; renaming a Docker
	// container afterwards does not alter it, so Caddy never observes a DNS gap.
	format := "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}"
	output, err := exec.CommandContext(ctx, "docker", "inspect", "--format", format, container).Output()
	if err != nil {
		return "", fmt.Errorf("inspect candidate address: %w", err)
	}
	ip := strings.TrimSpace(string(output))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("container %s has no usable network address", container)
	}
	return net.JoinHostPort(ip, fmt.Sprint(port)), nil
}

func (e Executor) removeDockerContainerIfPresent(ctx context.Context, output io.Writer, container string) error {
	if !e.dockerContainerExists(ctx, container) {
		return nil
	}
	_, _ = fmt.Fprintf(output, "[reconcile] removing abandoned container %s\n", container)
	if err := e.run(ctx, output, "docker", "rm", "--force", container); err != nil {
		return fmt.Errorf("remove Docker container %s: %w", container, err)
	}
	return nil
}

// DockerAdapter keeps Caddy labels and Docker network handling private to the
// Docker implementation. They are not part of a release's public contract.
type DockerAdapter struct{}

func (DockerAdapter) Name() string               { return "docker" }
func (DockerAdapter) BuildCommand() string       { return "docker" }
func (DockerAdapter) SupportsBuildSecrets() bool { return true }

func (DockerAdapter) Start(ctx context.Context, e Executor, output io.Writer, application store.Application, image string, environment map[string]string) (string, error) {
	tracker := TrackerFromContext(ctx)
	container := "minidock-" + application.Name
	if err := e.ensureDockerNetwork(ctx, output); err != nil {
		return image, failure("start", "network_unavailable", err)
	}
	candidate := container + "-candidate"
	// After the first successful API switch, -candidate is the live (blue or
	// green) container. Stage the next release under -next so it can be proven
	// healthy without touching the serving process.
	active := container
	staging := candidate
	if e.dockerContainerExists(ctx, candidate) {
		active, staging = candidate, container+"-next"
	}
	_ = e.run(ctx, output, "docker", "rm", "--force", staging)

	if tracker != nil {
		tracker.OnStageChange("start")
		tracker.OnEvent("start", "info", "Creando contenedor candidato para validación")
	}
	startStart := time.Now()
	if err := e.runDockerContainer(ctx, output, application, image, environment, staging, false); err != nil {
		return image, failure("start", "candidate_start_failed", err)
	}
	startDur := time.Since(startStart)
	if tracker != nil {
		tracker.StartDuration += startDur
	}

	if tracker != nil {
		tracker.OnStageChange("health")
		tracker.OnEvent("health", "info", "Ejecutando health check en el contenedor candidato")
	}
	healthStart := time.Now()
	if err := e.waitHealthy(ctx, staging, output); err != nil {
		_ = e.run(ctx, output, "docker", "rm", "--force", staging)
		return image, failure("health", "health_check_failed", err)
	}
	healthDur := time.Since(healthStart)
	if tracker != nil {
		tracker.HealthDuration += healthDur
		tracker.OnEvent("health", "info", fmt.Sprintf("Health check de candidato exitoso en %s", healthDur.Round(time.Millisecond)))
	}

	if tracker != nil {
		tracker.OnStageChange("route")
		tracker.OnEvent("route", "info", "Health check superado; conmutando la ruta Blue-Green en Caddy")
	}
	routeStart := time.Now()
	target, err := e.dockerContainerTarget(ctx, staging, application.InternalPort)
	if err != nil {
		return image, failure("route", "candidate_address_unavailable", err)
	}
	_, _ = fmt.Fprintf(output, "[route] actualizando Caddy hacia candidato saludable\n")
	if err := e.proxy().Apply(ctx, e, application, target); err != nil {
		_ = e.run(ctx, output, "docker", "rm", "--force", staging)
		return image, failure("route", "proxy_switch_failed", err)
	}
	if err := e.proxy().Verify(ctx, e, application); err != nil {
		// Caddy accepted the new route but public validation failed. Restore the
		// known active upstream before tearing down the candidate.
		if e.dockerContainerExists(ctx, active) {
			if oldTarget, oldErr := e.dockerContainerTarget(ctx, active, application.InternalPort); oldErr == nil {
				_ = e.proxy().Apply(ctx, e, application, oldTarget)
			}
		}
		_ = e.run(ctx, output, "docker", "rm", "--force", staging)
		return image, failure("route", "proxy_validation_failed", err)
	}
	routeDur := time.Since(routeStart)
	if tracker != nil {
		tracker.RouteDuration = routeDur
		tracker.OnEvent("route", "info", fmt.Sprintf("Dominio enrutado y disponible en %s", routeDur.Round(time.Millisecond)))
	}
	// Only after Caddy serves the candidate do we retire the old color. Existing
	// requests retain their upstream connection while the route is swapped.
	if e.dockerContainerExists(ctx, active) && active != staging {
		if err := e.removeDockerContainerIfPresent(ctx, output, active); err != nil {
			return image, failure("route", "previous_cleanup_failed", err)
		}
	}
	if staging != candidate {
		if err := e.run(ctx, output, "docker", "rename", staging, candidate); err != nil {
			return image, failure("route", "candidate_promote_failed", err)
		}
	}

	return image, nil
}

func (e Executor) runDockerContainer(ctx context.Context, output io.Writer, application store.Application, image string, environment map[string]string, container string, routed bool) error {
	args := []string{"run", "--detach", "--name", container, "--restart", "unless-stopped", "--network", e.Network, "--label", "minidock.application=" + fmt.Sprint(application.ID)}
	// Routing is controlled through Caddy's private JSON Admin API. Do not add
	// caddy-docker-proxy labels here: a Docker event could otherwise overwrite
	// the atomically switched route.
	if template, ok := runtime.For(application.Type); ok {
		args = append(args, "--cpus", template.CPUs, "--memory", template.Memory)
	}
	envFile, err := runtimeEnvFile(e.LogPath, environment)
	if err != nil {
		return err
	}
	defer removeRuntimeEnvFile(envFile)
	if envFile != "" {
		// Docker CLI reads this local file and sends the environment over its API.
		// The command line and release log therefore never contain secret values.
		args = append(args, "--env-file", envFile)
	}
	args = append(args, image)
	if err := ValidateDockerContainerSecurityArgs(args); err != nil {
		return err
	}
	return e.run(ctx, output, "docker", args...)
}

// ValidateDockerContainerSecurityArgs enforces that MiniDock workload containers
// never receive host mounts, privileged flags, host network or additional capabilities.
func ValidateDockerContainerSecurityArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--privileged" || strings.HasPrefix(arg, "--privileged=") {
			return failure("start", "forbidden_privileged_mode", fmt.Errorf("privileged mode is forbidden"))
		}
		if arg == "--device" || strings.HasPrefix(arg, "--device=") {
			return failure("start", "forbidden_device_mapping", fmt.Errorf("device mapping is forbidden"))
		}
		if arg == "--cap-add" || strings.HasPrefix(arg, "--cap-add=") {
			return failure("start", "forbidden_cap_add", fmt.Errorf("additional capabilities are forbidden"))
		}
		if arg == "--security-opt" || strings.HasPrefix(arg, "--security-opt=") {
			return failure("start", "forbidden_security_opt", fmt.Errorf("custom security options are forbidden"))
		}
		if arg == "-v" || arg == "--volume" || strings.HasPrefix(arg, "--volume=") {
			return failure("start", "forbidden_host_mount", fmt.Errorf("host volume mounts are forbidden"))
		}
		if (arg == "--net" || arg == "--network" || strings.HasPrefix(arg, "--network=")) && (strings.Contains(arg, "host") || (i+1 < len(args) && args[i+1] == "host")) {
			return failure("start", "forbidden_host_network", fmt.Errorf("host network mode is forbidden"))
		}
	}
	return nil
}

// runtimeEnvFile materializes runtime environment values with restrictive
// permissions for the short time Docker CLI needs to read them. Docker is the
// only supported release runtime; Apple Container remains experimental and
// does not provide an equivalent env-file contract.
func runtimeEnvFile(directory string, environment map[string]string) (string, error) {
	if len(environment) == 0 {
		return "", nil
	}
	if directory == "" {
		directory = os.TempDir()
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", fmt.Errorf("create runtime environment directory: %w", err)
	}
	file, err := os.CreateTemp(directory, ".minidock-runtime-*.env")
	if err != nil {
		return "", fmt.Errorf("create runtime environment file: %w", err)
	}
	path := file.Name()
	if err := file.Chmod(0600); err != nil {
		file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("protect runtime environment file: %w", err)
	}
	data := make([]byte, 0)
	for _, name := range sortedNames(environment) {
		if !safeEnvironmentName.MatchString(name) || strings.ContainsAny(environment[name], "\x00\r\n") {
			file.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("invalid runtime environment entry %q", name)
		}
		data = append(data, name+"="+environment[name]+"\n"...)
	}
	_, writeErr := file.Write(data)
	for index := range data {
		data[index] = 0
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		if writeErr != nil {
			return "", fmt.Errorf("write runtime environment file: %w", writeErr)
		}
		return "", fmt.Errorf("close runtime environment file: %w", closeErr)
	}
	return path, nil
}

func removeRuntimeEnvFile(path string) {
	if path == "" {
		return
	}
	// Best-effort overwrite limits accidental recovery on filesystems where it
	// is meaningful; unlinking remains the security boundary for this temp file.
	if info, err := os.Stat(path); err == nil && info.Size() > 0 {
		if file, openErr := os.OpenFile(path, os.O_WRONLY, 0600); openErr == nil {
			_, _ = file.Write(make([]byte, info.Size()))
			_ = file.Close()
		}
	}
	_ = os.Remove(path)
}

func (e Executor) dockerContainerImage(ctx context.Context, container string) string {
	output, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Config.Image}}", container).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (e Executor) dockerContainerID(ctx context.Context, container string) string {
	output, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Id}}", container).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (e Executor) switchFailure(ctx context.Context, output io.Writer, application store.Application, hasPrevious bool, stage, code string, cause error) error {
	if !hasPrevious {
		return failure(stage, code, cause)
	}
	if err := e.restoreDockerRelease(ctx, output, application); err != nil {
		return failure("rollback", "rollback_restore_failed", fmt.Errorf("%s failed: %v; previous release was not restored: %w", code, cause, err))
	}
	return failure(stage, code, cause)
}

func (e Executor) restoreDockerRelease(ctx context.Context, output io.Writer, application store.Application) error {
	_, _ = fmt.Fprintln(output, "[rollback] restaurando el contenedor anterior preservado")
	container := "minidock-" + application.Name
	previous := container + "-previous"
	if !e.dockerContainerExists(ctx, previous) {
		return fmt.Errorf("preserved previous container is missing")
	}
	if err := e.run(ctx, output, "docker", "start", previous); err != nil {
		return err
	}
	if err := e.waitHealthy(ctx, previous, output); err != nil {
		return err
	}
	if err := e.proxy().Verify(ctx, e, application); err != nil {
		return err
	}
	if err := e.run(ctx, output, "docker", "rename", previous, container); err != nil {
		return err
	}
	return nil
}

// AppleContainerAdapter uses loopback publication until its proxy adapter is
// configured. It therefore does not inherit Docker labels or Docker networks.
type AppleContainerAdapter struct{}

func (AppleContainerAdapter) Name() string               { return "apple" }
func (AppleContainerAdapter) BuildCommand() string       { return "container" }
func (AppleContainerAdapter) SupportsBuildSecrets() bool { return false }

func (AppleContainerAdapter) Start(ctx context.Context, e Executor, output io.Writer, application store.Application, image string, environment map[string]string) (string, error) {
	tracker := TrackerFromContext(ctx)
	if tracker != nil {
		tracker.OnStageChange("start")
		tracker.OnEvent("start", "info", "Levantando contenedor Apple")
	}
	startStart := time.Now()
	container := "minidock-" + application.Name
	_ = e.run(ctx, output, "container", "delete", "--force", container)
	result, err := e.runApple(ctx, output, application, image, container, environment)
	if tracker != nil {
		tracker.StartDuration = time.Since(startStart)
		if err == nil {
			tracker.OnEvent("start", "info", "Contenedor Apple iniciado")
		}
	}
	return result, failure("start", "container_start_failed", err)
}

// ensureDockerNetwork makes direct local execution behave like Compose, which
// normally creates the named application network before MiniDock starts.
// The inspect/create sequence is safe when the network already exists.
type dockerNetworkInspection struct {
	Name     string
	Driver   string
	Internal bool
	IPAM     struct {
		Config []struct {
			Subnet string
		}
	}
}

// validateDockerNetwork ensures workloads cannot silently land on an external
// bridge. The Caddy route is the only supported ingress to this network.
func validateDockerNetwork(output []byte, name, subnet string) error {
	var networks []dockerNetworkInspection
	if err := json.Unmarshal(output, &networks); err != nil || len(networks) != 1 {
		return fmt.Errorf("inspect Docker network %q: invalid response", name)
	}
	network := networks[0]
	if network.Name != name || network.Driver != "bridge" || !network.Internal {
		return fmt.Errorf("Docker network %q must be an internal bridge", name)
	}
	if subnet == "" {
		return nil
	}
	for _, config := range network.IPAM.Config {
		if config.Subnet == subnet {
			return nil
		}
	}
	return fmt.Errorf("Docker network %q must use subnet %s", name, subnet)
}

func (e Executor) ensureDockerNetwork(ctx context.Context, output io.Writer) error {
	if strings.TrimSpace(e.Network) == "" {
		return fmt.Errorf("Docker network is not configured")
	}
	inspect := func() error {
		result, err := exec.CommandContext(ctx, "docker", "network", "inspect", e.Network).Output()
		if err != nil {
			return err
		}
		return validateDockerNetwork(result, e.Network, e.NetworkSubnet)
	}
	if err := inspect(); err == nil {
		return nil
	}
	_, _ = fmt.Fprintf(output, "[runtime] creando red Docker %s\n", e.Network)
	args := []string{"network", "create", "--driver", "bridge", "--internal"}
	if strings.TrimSpace(e.NetworkSubnet) != "" {
		args = append(args, "--subnet", e.NetworkSubnet)
	}
	args = append(args, e.Network)
	if err := e.run(ctx, output, "docker", args...); err != nil {
		// Another worker or Compose may have created it between inspect and
		// create. Rechecking turns that benign race into a successful deploy.
		if inspectErr := inspect(); inspectErr != nil {
			return fmt.Errorf("create Docker network %q: %w", e.Network, err)
		}
	}
	return inspect()
}

func (e Executor) waitHealthy(ctx context.Context, container string, output io.Writer) error {
	deadline := time.Now().Add(e.healthTimeout())
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

type CaddyProxyAdapter struct{}

func (CaddyProxyAdapter) Name() string { return "caddy" }

func (CaddyProxyAdapter) Apply(ctx context.Context, e Executor, application store.Application, target string) error {
	if strings.TrimSpace(target) == "" {
		return fmt.Errorf("caddy target is empty")
	}
	return caddyClient{baseURL: e.CaddyAdminURL}.UpsertRoute(ctx, application, target)
}

func (CaddyProxyAdapter) Verify(ctx context.Context, e Executor, application store.Application) error {
	probe := (CaddyProxyAdapter{}).Probe(ctx, e, application)
	if probe.Success {
		return nil
	}
	return fmt.Errorf("caddy proxy route verification failed for domain %q: %s", application.Domain, probe.Detail)
}

func (CaddyProxyAdapter) Probe(ctx context.Context, e Executor, application store.Application) RouteProbe {
	proxyURL := e.ProxyURL
	if proxyURL == "" {
		proxyURL = "http://localhost"
	}

	client := &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	deadline := time.Now().Add(e.healthTimeout())
	result := RouteProbe{CheckedAt: time.Now().UTC(), Observer: "caddy via " + proxyURL}
	var lastErr error
	for time.Now().Before(deadline) {
		endpoint := strings.TrimSpace(application.HealthEndpoint)
		if endpoint == "" {
			endpoint = "/"
		}
		if !strings.HasPrefix(endpoint, "/") {
			result.Detail = fmt.Sprintf("proxy health endpoint must start with /: %q", endpoint)
			return result
		}
		req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(proxyURL, "/")+endpoint, nil)
		if err != nil {
			result.Detail = err.Error()
			return result
		}
		req.Host = application.Domain

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			select {
			case <-ctx.Done():
				result.Detail = ctx.Err().Error()
				return result
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}
		// A route is successful only when the configured health resource returns
		// its expected success status. In particular, redirects and arbitrary 5xx
		// replies are not proof that the application is reachable through Caddy.
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		result.StatusCode = resp.StatusCode
		if readErr != nil {
			lastErr = fmt.Errorf("read proxy health response: %w", readErr)
		} else if validCaddyHTTPSRedirect(resp, application, endpoint) && expectedProxyStatus(e) == http.StatusOK && e.ProxyExpectedContent == "" {
			// Caddy's HTTP listener redirects before dispatching the HTTPS route.
			// Apply already read back the exact managed route from the private
			// Admin API, and the candidate passed its internal health check. The
			// same-host redirect proves the public listener is reachable without
			// disabling TLS verification for Caddy's internal DNS alias.
			result.Success = true
			return result
		} else if resp.StatusCode != expectedProxyStatus(e) {
			lastErr = fmt.Errorf("caddy proxy returned status %d for %s; expected %d", resp.StatusCode, endpoint, expectedProxyStatus(e))
		} else if expected := e.ProxyExpectedContent; expected != "" && !strings.Contains(string(body), expected) {
			lastErr = fmt.Errorf("caddy proxy response for %s does not contain configured expected content", endpoint)
		} else {
			result.Success = true
			return result
		}

		select {
		case <-ctx.Done():
			result.Detail = ctx.Err().Error()
			return result
		case <-time.After(500 * time.Millisecond):
		}
	}

	if lastErr != nil {
		result.Detail = lastErr.Error()
		return result
	}
	result.Detail = fmt.Sprintf("caddy proxy route verification timed out for domain %q", application.Domain)
	return result
}

func validCaddyHTTPSRedirect(response *http.Response, application store.Application, endpoint string) bool {
	if response == nil || response.StatusCode != http.StatusPermanentRedirect {
		return false
	}
	location, err := response.Location()
	if err != nil || location.Scheme != "https" || location.Path != endpoint {
		return false
	}
	return strings.EqualFold(routeServerName(location.Host), routeServerName(application.Domain))
}

func routeServerName(domain string) string {
	domain = strings.TrimSpace(domain)
	if host, _, err := net.SplitHostPort(domain); err == nil {
		return strings.TrimSuffix(host, ".")
	}
	return strings.TrimSuffix(domain, ".")
}

func expectedProxyStatus(e Executor) int {
	if e.ProxyExpectedStatus > 0 {
		return e.ProxyExpectedStatus
	}
	return http.StatusOK
}

// caddyClient manages only routes whose IDs are owned by MiniDock. It uses the
// documented /id endpoint instead of positional route indexes, which are
// unstable when an operator changes unrelated Caddy configuration.
type caddyClient struct {
	baseURL string
	client  *http.Client
}

func caddyRouteID(applicationID int64) string { return fmt.Sprintf("minidock-app-%d", applicationID) }

func (c caddyClient) endpoint(path string) (string, error) {
	base := strings.TrimRight(strings.TrimSpace(c.baseURL), "/")
	if base == "" {
		return "", fmt.Errorf("Caddy Admin API URL is not configured")
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" {
		return "", fmt.Errorf("invalid Caddy Admin API URL")
	}
	return base + path, nil
}

func (c caddyClient) httpClient() *http.Client {
	if c.client != nil {
		return c.client
	}
	return &http.Client{Timeout: 3 * time.Second}
}

func (c caddyClient) UpsertRoute(ctx context.Context, application store.Application, target string) error {
	// The route is a complete Caddy HTTP route. A single replacement is atomic
	// from Caddy's perspective: new requests use one upstream, never a blend of
	// the old and new candidates.
	route := map[string]any{
		"@id":   caddyRouteID(application.ID),
		"match": []any{map[string]any{"host": []string{application.Domain}}},
		"handle": []any{map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": []any{map[string]any{"dial": target}},
		}},
	}
	body, err := json.Marshal(route)
	if err != nil {
		return err
	}
	path := "/id/" + caddyRouteID(application.ID)
	endpoint, err := c.endpoint(path)
	if err != nil {
		return err
	}
	// PATCH is strict replacement for an object addressed by @id. A missing
	// route is appended to srv0, the server emitted by Caddy's Caddyfile adapter.
	request, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.httpClient().Do(request)
	if err == nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		response.Body.Close()
		return c.verifyRouteTarget(ctx, endpoint, target)
	}
	if response != nil {
		response.Body.Close()
	}
	if err != nil {
		return fmt.Errorf("replace Caddy route: %w", err)
	}
	// Caddy returns 404 when no object has that ID. Append a new route only in
	// that case; other failures must not be hidden by a second mutation.
	if response.StatusCode != http.StatusNotFound {
		return fmt.Errorf("replace Caddy route: HTTP %d", response.StatusCode)
	}
	endpoint, err = c.endpoint("/config/apps/http/servers/srv0/routes")
	if err != nil {
		return err
	}
	request, err = http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err = c.httpClient().Do(request)
	if err != nil {
		return fmt.Errorf("create Caddy route: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("create Caddy route: HTTP %d", response.StatusCode)
	}
	return c.verifyRouteTarget(ctx, path, target)
}

func (c caddyClient) verifyRouteTarget(ctx context.Context, endpointOrPath, target string) error {
	endpoint := endpointOrPath
	if strings.HasPrefix(endpointOrPath, "/") {
		var err error
		endpoint, err = c.endpoint(endpointOrPath)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	response, err := c.httpClient().Do(request)
	if err != nil {
		return fmt.Errorf("read applied Caddy route: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("read applied Caddy route: HTTP %d", response.StatusCode)
	}
	var route struct {
		Handle []struct {
			Handler   string `json:"handler"`
			Upstreams []struct {
				Dial string `json:"dial"`
			} `json:"upstreams"`
		} `json:"handle"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&route); err != nil {
		return fmt.Errorf("decode applied Caddy route: %w", err)
	}
	for _, handler := range route.Handle {
		if handler.Handler != "reverse_proxy" {
			continue
		}
		for _, upstream := range handler.Upstreams {
			if upstream.Dial == target {
				return nil
			}
		}
	}
	return fmt.Errorf("Caddy route did not retain candidate upstream %q", target)
}

type DiskSpace struct {
	AvailableGB float64
	UsedPercent float64
}

func diskSpaceInfo(path string) (DiskSpace, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return DiskSpace{}, err
	}
	if stat.Blocks == 0 {
		return DiskSpace{}, fmt.Errorf("filesystem has no blocks")
	}
	free := float64(stat.Bavail) * float64(stat.Bsize)
	usedPercent := float64(stat.Blocks-stat.Bavail) * 100 / float64(stat.Blocks)
	return DiskSpace{
		AvailableGB: free / (1024 * 1024 * 1024),
		UsedPercent: usedPercent,
	}, nil
}
