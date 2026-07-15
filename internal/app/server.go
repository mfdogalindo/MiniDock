package app

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/julieta/minidock/internal/deploy"
	"github.com/julieta/minidock/internal/runtime"
	"github.com/julieta/minidock/internal/security"
	"github.com/julieta/minidock/internal/store"
)

//go:embed templates/*.html static/*.js
var templateFiles embed.FS

type App struct {
	config    Config
	store     *store.Store
	executor  deploy.Executor
	templates *template.Template
	mu        sync.RWMutex
	key       []byte
	sessions  map[string]time.Time
}

func New(config Config, database *store.Store) (*App, error) {
	if config.LocalRepositoriesPath != "" {
		if err := os.MkdirAll(config.LocalRepositoriesPath, 0700); err != nil {
			return nil, fmt.Errorf("create local repositories directory: %w", err)
		}
	}
	templates, err := template.ParseFS(templateFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &App{config: config, store: database, executor: deploy.Executor{WorkspacePath: config.WorkspacePath, LogPath: config.LogPath, Network: config.DockerNetwork, Runtime: config.Runtime, LocalRepositoriesPath: config.LocalRepositoriesPath, GitHubAppID: config.GitHubAppID, GitHubAppPrivateKeyPath: config.GitHubAppPrivateKeyPath, GitHubAPIURL: config.GitHubAPIURL}, templates: templates, sessions: make(map[string]time.Time)}, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /readyz", a.ready)
	mux.HandleFunc("GET /", a.dashboard)
	mux.HandleFunc("GET /operations", a.operations)
	mux.HandleFunc("GET /operations/logs", a.operationLogs)
	mux.HandleFunc("POST /operations/retention", a.retention)
	mux.HandleFunc("POST /operations/cleanup-schedule", a.updateCleanupSchedule)
	mux.HandleFunc("GET /applications/new", a.applicationForm)
	mux.HandleFunc("GET /repositories/browse", a.browseLocalRepositories)
	mux.HandleFunc("GET /repositories/refs", a.localRepositoryReferences)
	mux.HandleFunc("GET /repositories/detect", a.detectLocalRuntime)
	mux.HandleFunc("POST /repositories/folders", a.createLocalRepositoryFolder)
	mux.HandleFunc("GET /static/application-form.js", a.applicationFormScript)
	mux.HandleFunc("GET /static/deployment-log.js", a.deploymentLogScript)
	mux.HandleFunc("POST /applications", a.createApplication)
	mux.HandleFunc("GET /applications/{id}", a.applicationDetail)
	mux.HandleFunc("GET /applications/{id}/secrets", a.secretsPage)
	mux.HandleFunc("POST /applications/{id}/secrets", a.saveSecret)
	mux.HandleFunc("POST /applications/{id}/secrets/{name}/delete", a.deleteSecret)
	mux.HandleFunc("POST /applications/{id}/configuration", a.saveConfiguration)
	mux.HandleFunc("POST /applications/{id}/configuration/{name}/delete", a.deleteConfiguration)
	mux.HandleFunc("POST /applications/{id}/deploy", a.deployApplication)
	mux.HandleFunc("POST /applications/{id}/rollback", a.rollbackApplication)
	mux.HandleFunc("POST /applications/{id}/automation", a.updateApplicationAutomation)
	mux.HandleFunc("POST /applications/{id}/{action}", a.controlApplication)
	mux.HandleFunc("GET /applications/{id}/logs", a.applicationLogs)
	mux.HandleFunc("GET /applications/{id}/deployments/{deploymentID}/logs", a.deploymentLogs)
	mux.HandleFunc("GET /applications/{id}/deployments/{deploymentID}/log-stream", a.deploymentLogStream)
	mux.HandleFunc("GET /setup", a.setupForm)
	mux.HandleFunc("POST /setup", a.setup)
	mux.HandleFunc("GET /unlock", a.unlockForm)
	mux.HandleFunc("POST /unlock", a.unlock)
	mux.HandleFunc("POST /lock", a.lock)
	mux.HandleFunc("POST /webhooks/github/{id}", a.githubWebhook)
	return a.securityHeaders(mux)
}

func (a *App) applicationFormScript(w http.ResponseWriter, r *http.Request) {
	a.staticScript(w, r, "static/application-form.js")
}

func (a *App) deploymentLogScript(w http.ResponseWriter, r *http.Request) {
	a.staticScript(w, r, "static/deployment-log.js")
}

func (a *App) staticScript(w http.ResponseWriter, r *http.Request, name string) {
	contents, err := templateFiles.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(contents)
}

func (a *App) health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func (a *App) ready(w http.ResponseWriter, r *http.Request) {
	initialized, err := a.store.IsInitialized(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if !initialized {
		http.Error(w, "setup required", http.StatusServiceUnavailable)
		return
	}
	a.mu.RLock()
	unlocked := len(a.key) > 0
	a.mu.RUnlock()
	if !unlocked {
		http.Error(w, "unlock required", http.StatusServiceUnavailable)
		return
	}
	a.health(w, r)
}

func (a *App) dashboard(w http.ResponseWriter, r *http.Request) {
	initialized, err := a.store.IsInitialized(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if !initialized {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if !a.authorized(r) {
		http.Redirect(w, r, "/unlock", http.StatusSeeOther)
		return
	}
	applications, err := a.store.Applications(r.Context())
	if err != nil {
		http.Error(w, "applications unavailable", http.StatusServiceUnavailable)
		return
	}
	a.render(w, "dashboard.html", dashboardData{Environment: a.config.Environment, Applications: applications, Phase: currentPhase()})
}

type dashboardData struct {
	Environment  string
	Applications []store.Application
	Phase        phaseStatus
}

type phaseStatus struct {
	Number            int
	Name, State, Next string
}

// The active phase is deliberately explicit in the panel. Detailed acceptance
// criteria remain versioned in docs/PROGRESO.md.
func currentPhase() phaseStatus {
	return phaseStatus{Number: 6, Name: "Flujo guiado y automatización", State: "En progreso", Next: "Validar un despliegue real y las reglas de automatización"}
}

type applicationFormData struct {
	Error       string
	Application store.Application
}

func (a *App) applicationForm(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	a.render(w, "application_form.html", applicationFormData{Application: store.Application{Branch: "main"}})
}

func (a *App) createApplication(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	port, err := strconv.Atoi(r.Form.Get("internal_port"))
	application := store.Application{
		Name: strings.TrimSpace(r.Form.Get("name")), Repository: strings.TrimSpace(r.Form.Get("repository")),
		Branch: strings.TrimSpace(r.Form.Get("branch")), WorkDir: strings.TrimSpace(r.Form.Get("work_dir")),
		Type: strings.TrimSpace(r.Form.Get("type")), BuildCommand: strings.TrimSpace(r.Form.Get("build_command")),
		RunCommand: strings.TrimSpace(r.Form.Get("run_command")), InternalPort: port, Domain: strings.TrimSpace(r.Form.Get("domain")), Runtime: runtimeChoice(r.Form.Get("runtime")),
		RequireConfirmation:  true,
		GitHubInstallationID: parsePositiveInt(r.Form.Get("github_installation_id")),
	}
	if template, ok := runtime.For(application.Type); ok {
		if port == 0 {
			application.InternalPort = template.Port
		}
		// Commands are retained for backwards compatibility, but template builds
		// are intentionally declarative and do not execute form-provided commands.
		if application.BuildCommand == "" {
			application.BuildCommand = "plantilla " + application.Type
		}
		if application.RunCommand == "" {
			application.RunCommand = "plantilla " + application.Type
		}
	}
	if err != nil || application.Name == "" || application.Repository == "" || (a.requiresGitReference(application) && application.Branch == "") || application.WorkDir == "" || !runtime.IsSupported(application.Type) || (application.Type == "custom" && (application.BuildCommand == "" || application.RunCommand == "")) || application.Domain == "" || (application.Runtime != "auto" && application.Runtime != "docker" && application.Runtime != "apple") || application.InternalPort < 1 || application.InternalPort > 65535 || !a.validRepositorySource(application) {
		a.renderError(w, "application_form.html", "Completa todos los campos y usa un puerto entre 1 y 65535.", applicationFormData{Application: application})
		return
	}
	if _, err := a.store.CreateApplication(r.Context(), application); err != nil {
		a.renderError(w, "application_form.html", "No se pudo registrar la aplicación. El nombre y el dominio deben ser únicos.", applicationFormData{Application: application})
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func parsePositiveInt(value string) int64 {
	value = strings.TrimSpace(value)
	id, _ := strconv.ParseInt(value, 10, 64)
	if id < 0 {
		return 0
	}
	return id
}

func (a *App) validRepositorySource(application store.Application) bool {
	if application.GitHubInstallationID > 0 {
		return strings.HasPrefix(application.Repository, "https://github.com/") && a.config.GitHubAppID != "" && a.config.GitHubAppPrivateKeyPath != ""
	}
	if filepath.IsAbs(application.Repository) || strings.HasPrefix(application.Repository, "file://") {
		return deploy.ValidLocalRepository(a.config.LocalRepositoriesPath, application.Repository)
	}
	return true
}

func (a *App) requiresGitReference(application store.Application) bool {
	if filepath.IsAbs(application.Repository) || strings.HasPrefix(application.Repository, "file://") {
		return deploy.LocalRepositoryIsGit(a.config.LocalRepositoriesPath, application.Repository)
	}
	return true
}

func runtimeChoice(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return "auto"
}

func (a *App) requireAuthorization(w http.ResponseWriter, r *http.Request) bool {
	if a.authorized(r) {
		return true
	}
	http.Redirect(w, r, "/unlock", http.StatusSeeOther)
	return false
}

type applicationDetailData struct {
	Application     store.Application
	Deployments     []store.Deployment
	DeployedVersion string
	Container       deploy.ContainerStatus
	ContainerError  string
	LogPreview      string
	LogPreviewError string
	RuntimeNotice   string
	Phase           phaseStatus
	Pipeline        pipelineStatus
}

// pipelineStatus is a presentation model for the most recent deployment. The
// executor persists a single deployment result, so the stage display is
// intentionally conservative: a failure is attributed to the deployment
// stage and links to the captured log instead of claiming a more precise cause.
type pipelineStatus struct {
	Deployment      store.Deployment
	HasDeployment   bool
	PreflightFailed bool
}

func (a *App) applicationDetail(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, deployments, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data := applicationDetailData{Application: application, Deployments: deployments, Phase: currentPhase(), RuntimeNotice: runtimeNotice(deploy.RuntimeDiagnostics())}
	if len(deployments) > 0 {
		data.LogPreview, data.LogPreviewError = a.deploymentLogPreview(deployments[0].LogPath)
		data.Pipeline = pipelineStatus{Deployment: deployments[0], HasDeployment: true, PreflightFailed: runtimePreflightFailure(data.LogPreview)}
	}
	for _, deployment := range deployments {
		if deployment.Status == "successful" && deployment.Image != "" {
			data.DeployedVersion = deployment.Image
			break
		}
	}
	// Runtime inspection is bounded so an unavailable Docker daemon cannot
	// stall the panel request indefinitely. The deployment history remains
	// available even when a live snapshot cannot be obtained.
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	container, err := a.executor.Status(ctx, application)
	if err != nil {
		data.ContainerError = "No disponible: " + err.Error()
	} else {
		data.Container = container
	}
	a.render(w, "application_detail.html", data)
}

func runtimePreflightFailure(log string) bool {
	log = strings.ToLower(log)
	return strings.Contains(log, "runtime preflight:") || strings.Contains(log, "no supported container runtime") || strings.Contains(log, "default kernel not configured")
}

func runtimeNotice(diagnostics []deploy.RuntimeDiagnostic) string {
	var apple, docker deploy.RuntimeDiagnostic
	for _, diagnostic := range diagnostics {
		switch diagnostic.Name {
		case "apple":
			apple = diagnostic
		case "docker":
			docker = diagnostic
		}
	}
	if apple.Installed && !apple.Ready && docker.Ready {
		return "Apple Container está instalado pero no está listo; MiniDock está usando Docker como respaldo. Para preferir Apple Container ejecuta `" + apple.SetupCommand + "` y completa la configuración de kernel si se solicita."
	}
	if apple.Installed && !apple.Ready {
		return "Apple Container está instalado pero no está listo. Ejecuta `" + apple.SetupCommand + "` y completa la configuración de kernel si se solicita antes de desplegar."
	}
	return ""
}

// deploymentLogPreview returns the newest part of a deployment log for the
// application page. It is bounded to keep a large build log from making the
// page slow; the full, immutable log remains available through its own route.
func (a *App) deploymentLogPreview(logPath string) (string, string) {
	path, err := a.deploymentLogPath(logPath)
	if err != nil {
		return "", "El log no está disponible."
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return "", "El worker todavía no ha creado el log."
	}
	if err != nil {
		return "", "No se pudo abrir el log."
	}
	defer file.Close()
	const maxPreview = 16 << 10
	if info, statErr := file.Stat(); statErr == nil && info.Size() > maxPreview {
		if _, err := file.Seek(-maxPreview, io.SeekEnd); err != nil {
			return "", "No se pudo leer el log."
		}
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxPreview))
	if err != nil {
		return "", "No se pudo leer el log."
	}
	return string(contents), ""
}

type secretsData struct {
	Application store.Application
	Environment string
	Target      string
	Secrets     []store.SecretMetadata
	Config      []store.Configuration
	Audit       []store.SecretAudit
	Error       string
}

func secretEnvironment(value string) string {
	if value == "staging" {
		return "staging"
	}
	return "production"
}

func secretTarget(value string) string {
	if value == "build" {
		return "build"
	}
	return "runtime"
}

func validSecretName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, char := range name {
		if !(char == '_' || char >= 'A' && char <= 'Z' || i > 0 && char >= '0' && char <= '9') {
			return false
		}
	}
	return true
}

func (a *App) secretsPage(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.renderSecrets(w, r, application, secretEnvironment(r.URL.Query().Get("environment")), secretTarget(r.URL.Query().Get("target")), "")
}

func (a *App) renderSecrets(w http.ResponseWriter, r *http.Request, application store.Application, environment, target, message string) {
	secrets, err := a.store.ApplicationSecrets(r.Context(), application.ID, environment, target)
	if err != nil {
		http.Error(w, "secrets unavailable", http.StatusServiceUnavailable)
		return
	}
	audit, err := a.store.SecretAudit(r.Context(), application.ID)
	if err != nil {
		http.Error(w, "secret audit unavailable", http.StatusServiceUnavailable)
		return
	}
	configuration, err := a.store.ApplicationConfiguration(r.Context(), application.ID, environment, target)
	if err != nil {
		http.Error(w, "configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	a.render(w, "secrets.html", secretsData{Application: application, Environment: environment, Target: target, Secrets: secrets, Config: configuration, Audit: audit, Error: message})
}

func (a *App) saveSecret(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	environment, target := secretEnvironment(r.Form.Get("environment")), secretTarget(r.Form.Get("target"))
	name, value := strings.TrimSpace(r.Form.Get("name")), r.Form.Get("value")
	if !validSecretName(name) || value == "" {
		w.WriteHeader(http.StatusBadRequest)
		a.renderSecrets(w, r, application, environment, target, "Usa un nombre de variable válido en MAYÚSCULAS y proporciona un valor.")
		return
	}
	key := a.keyCopy()
	if len(key) == 0 {
		http.Redirect(w, r, "/unlock", http.StatusSeeOther)
		return
	}
	nonce, ciphertext, err := security.Encrypt(key, []byte(value))
	security.Zero(key)
	if err == nil {
		err = a.store.PutApplicationSecret(r.Context(), application.ID, environment, target, name, nonce, ciphertext)
	}
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		a.renderSecrets(w, r, application, environment, target, "No se pudo guardar el secreto.")
		return
	}
	http.Redirect(w, r, "/applications/"+r.PathValue("id")+"/secrets?environment="+environment+"&target="+target, http.StatusSeeOther)
}

func (a *App) deleteSecret(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	environment, target, name := secretEnvironment(r.Form.Get("environment")), secretTarget(r.Form.Get("target")), r.PathValue("name")
	if !validSecretName(name) || a.store.DeleteApplicationSecret(r.Context(), application.ID, environment, target, name) != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/applications/"+r.PathValue("id")+"/secrets?environment="+environment+"&target="+target, http.StatusSeeOther)
}

func (a *App) saveConfiguration(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	environment, target := secretEnvironment(r.Form.Get("environment")), secretTarget(r.Form.Get("target"))
	name, value := strings.TrimSpace(r.Form.Get("name")), r.Form.Get("value")
	if !validSecretName(name) {
		w.WriteHeader(http.StatusBadRequest)
		a.renderSecrets(w, r, application, environment, target, "Usa un nombre de variable válido en MAYÚSCULAS.")
		return
	}
	if err := a.store.PutApplicationConfiguration(r.Context(), application.ID, environment, target, name, value); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		a.renderSecrets(w, r, application, environment, target, "No se pudo guardar la configuración.")
		return
	}
	http.Redirect(w, r, "/applications/"+r.PathValue("id")+"/secrets?environment="+environment+"&target="+target, http.StatusSeeOther)
}

func (a *App) deleteConfiguration(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	environment, target, name := secretEnvironment(r.Form.Get("environment")), secretTarget(r.Form.Get("target")), r.PathValue("name")
	if !validSecretName(name) || a.store.DeleteApplicationConfiguration(r.Context(), application.ID, environment, target, name) != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/applications/"+r.PathValue("id")+"/secrets?environment="+environment+"&target="+target, http.StatusSeeOther)
}

func (a *App) deployApplication(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if application.RequireConfirmation && r.FormValue("confirm_production") != application.Name {
		http.Error(w, "confirm production deployment by entering the application name", http.StatusBadRequest)
		return
	}
	if err = a.queueDeployment(r.Context(), application.ID, "deploy"); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			http.Error(w, "a deployment is already queued or running", http.StatusConflict)
		} else {
			http.Error(w, "could not queue deployment", 500)
		}
		return
	}
	http.Redirect(w, r, "/applications/"+r.PathValue("id"), http.StatusSeeOther)
}

func (a *App) updateApplicationAutomation(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	deployOnPush := r.Form.Get("deploy_on_push") == "on"
	// A push cannot complete an interactive production confirmation. Leaving
	// confirmation on therefore makes the deployment deliberately manual.
	requireConfirmation := r.Form.Get("require_confirmation") == "on"
	if deployOnPush && requireConfirmation {
		http.Error(w, "push deployments require production confirmation to be disabled", http.StatusBadRequest)
		return
	}
	if err := a.store.UpdateApplicationAutomation(r.Context(), application.ID, deployOnPush, requireConfirmation, r.Form.Get("auto_rollback") == "on"); err != nil {
		http.Error(w, "could not save automation", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/applications/"+r.PathValue("id"), http.StatusSeeOther)
}

func (a *App) rollbackApplication(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err = a.queueDeployment(r.Context(), application.ID, "rollback"); err != nil {
		http.Error(w, "could not queue rollback", http.StatusConflict)
		return
	}
	http.Redirect(w, r, "/applications/"+r.PathValue("id"), http.StatusSeeOther)
}

func (a *App) deploymentConfiguration(ctx context.Context, applicationID int64, environment string) (deploy.DeploymentConfiguration, error) {
	runtime, err := a.configurationValues(ctx, applicationID, environment, "runtime")
	if err != nil {
		return deploy.DeploymentConfiguration{}, err
	}
	buildArgs, err := a.configurationValues(ctx, applicationID, environment, "build")
	if err != nil {
		return deploy.DeploymentConfiguration{}, err
	}
	buildSecrets, err := a.deploymentSecrets(ctx, applicationID, environment, "build")
	if err != nil {
		return deploy.DeploymentConfiguration{}, err
	}
	runtimeSecrets, err := a.deploymentSecrets(ctx, applicationID, environment, "runtime")
	if err != nil {
		return deploy.DeploymentConfiguration{}, err
	}
	for name, value := range runtimeSecrets {
		runtime[name] = value
	}
	return deploy.DeploymentConfiguration{Runtime: runtime, BuildArgs: buildArgs, BuildSecrets: buildSecrets}, nil
}

func (a *App) configurationValues(ctx context.Context, applicationID int64, environment, target string) (map[string]string, error) {
	values, err := a.store.ApplicationConfiguration(ctx, applicationID, environment, target)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(values))
	for _, value := range values {
		result[value.Name] = value.Value
	}
	return result, nil
}

func (a *App) deploymentSecrets(ctx context.Context, applicationID int64, environment, target string) (map[string]string, error) {
	metadata, err := a.store.ApplicationSecrets(ctx, applicationID, environment, target)
	if err != nil {
		return nil, err
	}
	key := a.keyCopy()
	if len(key) == 0 {
		return nil, fmt.Errorf("master key unavailable")
	}
	defer security.Zero(key)
	values := make(map[string]string, len(metadata))
	for _, secret := range metadata {
		nonce, ciphertext, err := a.store.ApplicationSecret(ctx, applicationID, environment, target, secret.Name)
		if err != nil {
			return nil, err
		}
		plaintext, err := security.Decrypt(key, nonce, ciphertext)
		if err != nil {
			return nil, err
		}
		values[secret.Name] = string(plaintext)
		security.Zero(plaintext)
	}
	if err := a.store.RecordSecretUse(ctx, applicationID, environment, target, sortedMapNames(values)); err != nil {
		return nil, err
	}
	return values, nil
}

func sortedMapNames(values map[string]string) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	return names
}

func (a *App) controlApplication(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err = a.executor.Control(r.Context(), application, r.PathValue("action"), os.Stderr); err != nil {
		http.Error(w, "container action failed", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/applications/"+r.PathValue("id"), http.StatusSeeOther)
}

func (a *App) applicationLogs(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err = a.executor.Logs(r.Context(), application, w); err != nil {
		http.Error(w, "container logs unavailable", http.StatusBadGateway)
	}
}

// deploymentLogs serves the immutable log captured by the queue worker. It is
// deliberately separate from applicationLogs, which asks the runtime for the
// current container output and can therefore change after a rollback.
func (a *App) deploymentLogs(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("deploymentID"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	deployment, err := a.store.Deployment(r.Context(), application.ID, id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path, err := a.deploymentLogPath(deployment.LogPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		http.Error(w, "deployment log is not available yet", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "deployment log unavailable", http.StatusServiceUnavailable)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// A log is useful for diagnosis, but a single HTTP response must not become
	// an unbounded memory or bandwidth sink. The complete file remains on disk.
	_, _ = io.Copy(w, io.LimitReader(file, 2<<20))
}

// deploymentLogStream follows the captured deployment log using Server-Sent
// Events. Logs are written by a local worker, so periodically reading the
// bounded tail is portable across Docker, Apple Container, and local filesystems.
func (a *App) deploymentLogStream(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("deploymentID"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	last := "\x00"
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		deployment, err := a.store.Deployment(r.Context(), application.ID, id)
		if err != nil {
			return
		}
		preview, message := a.deploymentLogPreview(deployment.LogPath)
		value := preview
		if value == "" {
			value = message
		}
		if value != last {
			payload, _ := json.Marshal(value)
			_, _ = fmt.Fprintf(w, "event: log\ndata: %s\n\n", payload)
			flusher.Flush()
			last = value
		}
		if deployment.Status == "successful" || deployment.Status == "failed" {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) deploymentLogPath(path string) (string, error) {
	root, err := filepath.Abs(a.config.LogPath)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("deployment log is outside the configured log directory")
	}
	return path, nil
}

func (a *App) applicationData(r *http.Request) (store.Application, []store.Deployment, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return store.Application{}, nil, err
	}
	application, err := a.store.Application(r.Context(), id)
	if err != nil {
		return store.Application{}, nil, err
	}
	deployments, err := a.store.Deployments(r.Context(), id)
	return application, deployments, err
}

func (a *App) setupForm(w http.ResponseWriter, r *http.Request) {
	initialized, err := a.store.IsInitialized(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if initialized {
		http.Redirect(w, r, "/unlock", http.StatusSeeOther)
		return
	}
	a.render(w, "setup.html", map[string]string{})
}

func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	password := r.Form.Get("password")
	if len(password) < 12 || password != r.Form.Get("password_confirmation") {
		a.renderError(w, "setup.html", "Usa una contraseña de al menos 12 caracteres y confirma el mismo valor.")
		return
	}
	salt, err := security.NewSalt()
	if err != nil {
		http.Error(w, "could not initialize security", http.StatusInternalServerError)
		return
	}
	key, err := security.DeriveKey(password, salt)
	if err != nil {
		http.Error(w, "could not derive key", http.StatusInternalServerError)
		return
	}
	defer security.Zero(key)
	nonce, cipher, err := security.NewVerifier(key)
	if err != nil {
		http.Error(w, "could not initialize verifier", http.StatusInternalServerError)
		return
	}
	if err := a.store.InitializeSecurity(r.Context(), store.SecurityConfig{Salt: salt, VerifierNonce: nonce, VerifierCipher: cipher}); err != nil {
		a.renderError(w, "setup.html", "La configuración ya existe o no pudo guardarse.")
		return
	}
	a.setKey(key)
	a.newSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) unlockForm(w http.ResponseWriter, r *http.Request) {
	initialized, err := a.store.IsInitialized(r.Context())
	if err != nil || !initialized {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if a.authorized(r) {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, "unlock.html", map[string]string{})
}

func (a *App) unlock(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	config, err := a.store.SecurityConfig(r.Context())
	if err != nil {
		http.Error(w, "security configuration unavailable", http.StatusServiceUnavailable)
		return
	}
	key, err := security.DeriveKey(r.Form.Get("password"), config.Salt)
	if err != nil {
		http.Error(w, "could not derive key", http.StatusInternalServerError)
		return
	}
	if err := security.ValidateKey(key, config.VerifierNonce, config.VerifierCipher); err != nil {
		security.Zero(key)
		a.renderError(w, "unlock.html", "Contraseña incorrecta.")
		return
	}
	a.setKey(key)
	security.Zero(key)
	a.newSession(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) lock(w http.ResponseWriter, r *http.Request) {
	a.Lock()
	cookie, err := r.Cookie("minidock_session")
	if err == nil {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "minidock_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/unlock", http.StatusSeeOther)
}

func (a *App) Lock() {
	a.mu.Lock()
	defer a.mu.Unlock()
	security.Zero(a.key)
	a.key = nil
	a.sessions = make(map[string]time.Time)
}

func (a *App) setKey(key []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	security.Zero(a.key)
	a.key = append(a.key[:0], key...)
}

func (a *App) keyCopy() []byte {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]byte(nil), a.key...)
}

func (a *App) authorized(r *http.Request) bool {
	cookie, err := r.Cookie("minidock_session")
	if err != nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.key) == 0 {
		return false
	}
	expires, ok := a.sessions[cookie.Value]
	return ok && time.Now().Before(expires)
}

func (a *App) newSession(w http.ResponseWriter) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	session := base64.RawURLEncoding.EncodeToString(value)
	expires := time.Now().Add(12 * time.Hour)
	a.mu.Lock()
	a.sessions[session] = expires
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "minidock_session", Value: session, Path: "/", Expires: expires, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "render template", http.StatusInternalServerError)
	}
}

func (a *App) renderError(w http.ResponseWriter, name, message string, data ...any) {
	w.WriteHeader(http.StatusBadRequest)
	if len(data) > 0 {
		switch value := data[0].(type) {
		case applicationFormData:
			value.Error = message
			a.render(w, name, value)
			return
		}
	}
	a.render(w, name, map[string]string{"Error": message})
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
