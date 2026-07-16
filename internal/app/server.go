package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"
	"github.com/julieta/minidock/internal/app/views"
	"github.com/julieta/minidock/internal/deploy"
	"github.com/julieta/minidock/internal/runtime"
	"github.com/julieta/minidock/internal/security"
	"github.com/julieta/minidock/internal/store"
)

//go:embed templates/*.html static/*.js
var templateFiles embed.FS

var (
	appNameRegex             = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	domainRegex              = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*(?::\d+)?$`)
	cloudflareAccountIDRegex = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

type App struct {
	config          Config
	store           *store.Store
	executor        deploy.Executor
	mu              sync.RWMutex
	key             []byte
	sessions        map[string]time.Time
	statusCache     map[int64]deploy.ContainerStatus
	statusCacheErr  map[int64]string
	certAlertsCache map[int64]string
	diskUsedCache   string
	alertsCache     []operationalAlert
	cacheMu         sync.RWMutex
	runtimeReady    func(context.Context) error
	proxyReady      func(context.Context) error
	cloudflared     CloudflaredConnector
	cloudflareAPI   func() CloudflareAPI
}

func New(config Config, database *store.Store) (*App, error) {
	// Tests and embedders often construct Config directly instead of using
	// LoadConfig. Preserve the secure production defaults in that path too.
	if config.WebhookRateLimit == 0 {
		config.WebhookRateLimit = 30
	}
	if config.WebhookRateWindow == 0 {
		config.WebhookRateWindow = time.Minute
	}
	if config.LocalRepositoriesPath != "" {
		if err := os.MkdirAll(config.LocalRepositoriesPath, 0700); err != nil {
			return nil, fmt.Errorf("create local repositories directory: %w", err)
		}
	}
	app := &App{
		config: config,
		store:  database,
		executor: deploy.Executor{
			WorkspacePath:           config.WorkspacePath,
			LogPath:                 config.LogPath,
			Network:                 config.DockerNetwork,
			NetworkSubnet:           config.DockerNetworkSubnet,
			Runtime:                 config.Runtime,
			LocalRepositoriesPath:   config.LocalRepositoriesPath,
			GitHubAppID:             config.GitHubAppID,
			GitHubAppPrivateKeyPath: config.GitHubAppPrivateKeyPath,
			GitHubAPIURL:            config.GitHubAPIURL,
			SourceTimeout:           config.SourceTimeout,
			BuildTimeout:            config.BuildTimeout,
			StartTimeout:            config.StartTimeout,
			HealthTimeout:           config.HealthTimeout,
			WorkspaceMaxBytes:       config.WorkspaceMaxBytes,
			ProxyURL:                config.ProxyURL,
			CaddyAdminURL:           config.CaddyAdminURL,
			ProxyExpectedStatus:     config.ProxyExpectedStatus,
			ProxyExpectedContent:    config.ProxyExpectedContent,
		},
		sessions:        make(map[string]time.Time),
		statusCache:     make(map[int64]deploy.ContainerStatus),
		statusCacheErr:  make(map[int64]string),
		certAlertsCache: make(map[int64]string),
		runtimeReady:    deploy.DockerReady,
		cloudflared:     NewDockerCloudflaredConnector(config.CloudflaredImage, config.CloudflaredNetwork),
		cloudflareAPI:   func() CloudflareAPI { return NewCloudflareClient() },
	}
	app.proxyReady = app.checkProxyReady
	return app, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /readyz", a.ready)
	mux.HandleFunc("GET /runtimez", a.runtime)
	mux.HandleFunc("GET /api/nodes", a.nodesAPI)
	mux.HandleFunc("GET /", a.dashboard)
	mux.HandleFunc("GET /operations", a.operations)
	mux.HandleFunc("GET /operations/logs", a.operationLogs)
	mux.HandleFunc("POST /operations/retention", a.retention)
	mux.HandleFunc("POST /operations/cleanup-schedule", a.updateCleanupSchedule)
	mux.HandleFunc("GET /applications/new", a.applicationForm)
	mux.HandleFunc("GET /repositories/browse", a.browseLocalRepositories)
	mux.HandleFunc("GET /repositories/refs", a.localRepositoryReferences)
	mux.HandleFunc("GET /repositories/detect", a.detectLocalRuntime)
	mux.HandleFunc("GET /repositories/validate", a.validateLocalRuntime)
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
	mux.HandleFunc("POST /applications/{id}/retry", a.retryApplication)
	mux.HandleFunc("GET /applications/{id}/deployments/{deploymentID}/diff", a.deploymentDiff)
	mux.HandleFunc("POST /applications/{id}/deployments/{deploymentID}/cancel", a.cancelDeployment)
	mux.HandleFunc("POST /applications/{id}/automation", a.updateApplicationAutomation)
	mux.HandleFunc("POST /applications/{id}/{action}", a.controlApplication)
	mux.HandleFunc("GET /applications/{id}/logs", a.applicationLogs)
	mux.HandleFunc("GET /applications/{id}/deployments/{deploymentID}/release-report", a.releaseReport)
	mux.HandleFunc("GET /applications/{id}/deployments/{deploymentID}/logs", a.deploymentLogs)
	mux.HandleFunc("GET /applications/{id}/deployments/{deploymentID}/log-stream", a.deploymentLogStream)
	mux.HandleFunc("GET /operations/retention/preview", a.retentionPreview)
	mux.HandleFunc("POST /operations/backup", a.manualBackup)
	mux.HandleFunc("GET /setup", a.setupForm)
	mux.HandleFunc("POST /setup", a.setup)
	mux.HandleFunc("GET /unlock", a.unlockForm)
	mux.HandleFunc("POST /unlock", a.unlock)
	mux.HandleFunc("POST /lock", a.lock)
	mux.HandleFunc("GET /cloudflare", a.renderCloudflareDashboard)
	mux.HandleFunc("POST /cloudflare/save", a.saveCloudflareConfig)
	mux.HandleFunc("POST /cloudflare/reconnect", a.reconnectCloudflared)
	mux.HandleFunc("POST /applications/{id}/cloudflare/sync", a.syncAppCloudflareDNS)
	mux.HandleFunc("POST /applications/{id}/domain", a.updateApplicationDomain)
	mux.HandleFunc("POST /webhooks/github/{id}", a.githubWebhook)
	return a.securityHeaders(a.csrfCheck(mux))
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
	w.Write([]byte(fmt.Sprintf(`{"status":"ok","version":%q}`, Version)))
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

// runtime distinguishes an alive panel from an instance that can actually
// deploy and reach its configured reverse proxy. It never returns raw command
// output because Docker diagnostics can contain host-specific information.
func (a *App) runtime(w http.ResponseWriter, r *http.Request) {
	if a.config.Runtime == "apple" {
		http.Error(w, "Apple Container readiness is experimental and unsupported", http.StatusServiceUnavailable)
		return
	}
	if err := a.runtimeReady(r.Context()); err != nil {
		log.Printf("runtime readiness failed: %v", err)
		http.Error(w, "Docker socket or daemon is not usable; check the socket mount and its group permissions", http.StatusServiceUnavailable)
		return
	}
	if err := a.proxyReady(r.Context()); err != nil {
		log.Printf("proxy readiness failed: %v", err)
		http.Error(w, "Caddy proxy is not reachable; check the caddy service and MINIDOCK_PROXY_URL", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(fmt.Sprintf(`{"status":"ready","version":%q}`, Version)))
}

func (a *App) checkProxyReady(ctx context.Context) error {
	proxyURL := a.config.ProxyURL
	if proxyURL == "" {
		proxyURL = "http://localhost"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxyURL+"/healthz", nil)
	if err != nil {
		return err
	}
	req.Host = a.config.AdminDomain
	client := &http.Client{
		Timeout: 2 * time.Second,
		// Caddy redirects HTTP to HTTPS using the original Host. Following that
		// redirect from inside the MiniDock container would resolve the public
		// host to the container itself, not the proxy. A redirect is sufficient
		// evidence that the configured proxy accepted the administrative route,
		// and avoids weakening TLS verification for an internal alias.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	response, err := client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if !proxyResponseReady(response.StatusCode) {
		return fmt.Errorf("proxy returned HTTP %d", response.StatusCode)
	}
	return nil
}

func proxyResponseReady(status int) bool {
	return status >= http.StatusOK && status < http.StatusBadRequest
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
	nodes, err := a.store.Nodes(r.Context())
	if err != nil {
		http.Error(w, "nodes unavailable", http.StatusServiceUnavailable)
		return
	}
	p := currentPhase()
	preflightContext, cancelPreflight := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancelPreflight()
	hostReadiness := a.RunHostPreflightCheck(preflightContext)
	a.renderComponent(w, r, views.Dashboard(views.DashboardData{
		Environment:   a.config.Environment,
		Applications:  applications,
		Nodes:         nodes,
		Phase:         views.PhaseStatus{Number: p.Number, Name: p.Name, State: p.State, Next: p.Next},
		CSRFToken:     a.csrfToken(r),
		HostReadiness: hostReadinessView(hostReadiness),
	}))
}

func (a *App) nodesAPI(w http.ResponseWriter, r *http.Request) {
	if !a.authorized(r) {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	nodes, err := a.store.Nodes(r.Context())
	if err != nil {
		http.Error(w, "nodes unavailable", http.StatusServiceUnavailable)
		return
	}
	// Deliberately omit the certificate binding: it is useful for audit-only
	// workflows but does not belong in the routine browser inventory API.
	view := make([]struct {
		ID           string    `json:"id"`
		Name         string    `json:"name"`
		Version      string    `json:"version"`
		Capabilities []string  `json:"capabilities"`
		LastSeenAt   time.Time `json:"last_seen_at"`
	}, 0, len(nodes))
	for _, node := range nodes {
		view = append(view, struct {
			ID           string    `json:"id"`
			Name         string    `json:"name"`
			Version      string    `json:"version"`
			Capabilities []string  `json:"capabilities"`
			LastSeenAt   time.Time `json:"last_seen_at"`
		}{node.ID, node.Name, node.Version, node.Capabilities, node.LastSeenAt})
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(view)
}

type dashboardData struct {
	Environment  string
	Applications []store.Application
	Nodes        []store.Node
	Phase        phaseStatus
	CSRFToken    string
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
	CSRFToken   string
}

func (a *App) applicationForm(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	a.renderApplicationForm(w, r, store.Application{
		Branch: "main", WorkDir: ".", Type: "static", Runtime: "auto", InternalPort: 8080,
	}, "", http.StatusOK)
}

func (a *App) renderApplicationForm(w http.ResponseWriter, r *http.Request, application store.Application, message string, status int) {
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	readiness := a.RunHostPreflightCheck(ctx)
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	a.renderComponent(w, r, views.ApplicationForm(views.ApplicationFormData{
		Application:   application,
		CSRFToken:     a.csrfToken(r),
		Error:         message,
		HostReadiness: hostReadinessView(readiness),
	}))
}

func (a *App) createApplication(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	port := 0
	var err error
	if value := strings.TrimSpace(r.Form.Get("internal_port")); value != "" {
		port, err = strconv.Atoi(value)
	}
	healthEndpoint := strings.TrimSpace(r.Form.Get("health_endpoint"))
	if healthEndpoint != "" && !strings.HasPrefix(healthEndpoint, "/") {
		healthEndpoint = "/" + healthEndpoint
	}
	application := store.Application{
		Name: strings.TrimSpace(r.Form.Get("name")), Repository: strings.TrimSpace(r.Form.Get("repository")),
		Branch: strings.TrimSpace(r.Form.Get("branch")), WorkDir: strings.TrimSpace(r.Form.Get("work_dir")),
		Type: strings.TrimSpace(r.Form.Get("type")), BuildCommand: strings.TrimSpace(r.Form.Get("build_command")),
		RunCommand: strings.TrimSpace(r.Form.Get("run_command")), InternalPort: port, Domain: strings.TrimSpace(r.Form.Get("domain")), Runtime: runtimeChoice(r.Form.Get("runtime")),
		RequireConfirmation:  true,
		GitHubInstallationID: parsePositiveInt(r.Form.Get("github_installation_id")),
		HealthEndpoint:       healthEndpoint,
	}
	applyApplicationDefaults(&application)
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
	} else if application.Type == "custom" {
		// A custom runtime is deliberately Dockerfile-only. MiniDock must not
		// execute arbitrary form input on the host; the commands remain stored
		// only for compatibility with existing databases.
		application.BuildCommand = "Dockerfile del repositorio"
		application.RunCommand = "Dockerfile del repositorio"
	}
	if err != nil || !appNameRegex.MatchString(application.Name) || application.Repository == "" || (a.requiresGitReference(application) && application.Branch == "") || application.WorkDir == "" || !runtime.IsSupported(application.Type) || !domainRegex.MatchString(application.Domain) || (application.Runtime != "auto" && !a.executor.SupportsRuntime(application.Runtime)) || application.InternalPort < 1 || application.InternalPort > 65535 || !a.validRepositorySource(application) {
		a.renderApplicationForm(w, r, application, "Completa todos los campos correctamente. El nombre de aplicación debe contener solo letras minúsculas, números y guiones, y el dominio debe ser válido (por ejemplo, app.local o localhost:8080).", http.StatusBadRequest)
		return
	}
	if application.Domain == a.config.AdminDomain {
		a.renderApplicationForm(w, r, application, "El dominio de la aplicación no puede ser igual al dominio administrativo ("+a.config.AdminDomain+").", http.StatusBadRequest)
		return
	}
	registered, err := a.store.CreateApplication(r.Context(), application)
	if err != nil {
		a.renderApplicationForm(w, r, application, "No se pudo registrar la aplicación. El nombre y el dominio deben ser únicos.", http.StatusBadRequest)
		return
	}
	detailURL := "/applications/" + strconv.FormatInt(registered.ID, 10)
	if r.Form.Get("intent") != "deploy" {
		http.Redirect(w, r, detailURL+"?notice="+url.QueryEscape("Aplicación creada. Puedes revisar la configuración antes de desplegar."), http.StatusSeeOther)
		return
	}
	preflightContext, cancelPreflight := context.WithTimeout(r.Context(), 4*time.Second)
	preflight := a.RunPreflightCheck(preflightContext, registered)
	cancelPreflight()
	if !preflight.AllOK {
		http.Redirect(w, r, detailURL+"?error="+url.QueryEscape("La aplicación se guardó, pero el host aún no está listo para desplegar. "+preflightFailureSummary(preflight)), http.StatusSeeOther)
		return
	}
	if err := a.queueDeployment(r.Context(), registered.ID, "deploy"); err != nil {
		http.Redirect(w, r, detailURL+"?error="+url.QueryEscape("La aplicación se creó, pero el primer despliegue no pudo ponerse en cola: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, detailURL+"?notice="+url.QueryEscape("Aplicación creada. El primer despliegue ya está en cola."), http.StatusSeeOther)
}

func hostReadinessView(readiness HostPreflightResult) views.HostReadinessData {
	return views.HostReadinessData{
		AllOK:     readiness.AllOK,
		Docker:    views.HostReadinessItem(readiness.Docker),
		Caddy:     views.HostReadinessItem(readiness.Caddy),
		Network:   views.HostReadinessItem(readiness.Network),
		DiskSpace: views.HostReadinessItem(readiness.DiskSpace),
	}
}

func preflightFailureSummary(preflight PreflightCheckResult) string {
	items := []PreflightItem{preflight.Docker, preflight.Caddy, preflight.Network, preflight.DiskSpace, preflight.RepoAccess, preflight.DomainHealth}
	failures := make([]string, 0, len(items))
	for _, item := range items {
		if !item.OK {
			failures = append(failures, item.Name+": "+item.Action)
		}
	}
	return strings.Join(failures, " ")
}

// applyApplicationDefaults keeps the easy path server-side as well as in the
// browser. A crafted or JavaScript-free request therefore receives the same
// conservative defaults without weakening validation of repository sources.
func applyApplicationDefaults(application *store.Application) {
	if application.Name == "" {
		application.Name = applicationNameFromRepository(application.Repository)
	}
	if application.WorkDir == "" {
		application.WorkDir = "."
	}
	if application.Type == "" {
		application.Type = "static"
	}
	if application.Runtime == "" {
		application.Runtime = "auto"
	}
	if application.Branch == "" && !filepath.IsAbs(application.Repository) && !strings.HasPrefix(application.Repository, "file://") {
		application.Branch = "main"
	}
	if application.Domain == "" && application.Name != "" {
		application.Domain = application.Name + ".localhost"
	}
	if template, ok := runtime.For(application.Type); ok {
		if application.InternalPort == 0 {
			application.InternalPort = template.Port
		}
		if application.HealthEndpoint == "" {
			application.HealthEndpoint = template.HealthEndpoint
		}
	}
}

func applicationNameFromRepository(repository string) string {
	repository = strings.TrimSuffix(strings.TrimSpace(repository), "/")
	if parsed, err := url.Parse(repository); err == nil && parsed.Path != "" {
		repository = parsed.Path
	}
	name := strings.TrimSuffix(filepath.Base(repository), ".git")
	name = strings.ToLower(name)
	var result strings.Builder
	previousDash := false
	for _, character := range name {
		valid := character >= 'a' && character <= 'z' || character >= '0' && character <= '9'
		if valid {
			result.WriteRune(character)
			previousDash = false
		} else if result.Len() > 0 && !previousDash {
			result.WriteByte('-')
			previousDash = true
		}
		if result.Len() >= 63 {
			break
		}
	}
	return strings.Trim(result.String(), "-")
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
	CSRFToken       string
	HasSecrets      bool
	HealthEvidence  store.HealthEvidence
	Preflight       PreflightCheckResult
}

// pipelineStatus is a presentation model for the most recent deployment. The
// executor persists a single deployment result, so the stage display is
// intentionally conservative: a failure is attributed to the deployment
// stage and links to the captured log instead of claiming a more precise cause.
type pipelineStatus struct {
	Deployment      store.Deployment
	HasDeployment   bool
	PreflightFailed bool
	Step1State      string
	Step1Msg        string
	Step2State      string
	Step2Msg        string
	Step3State      string
	Step3Msg        string
	Step4State      string
	Step4Msg        string
	Step5State      string
	Step5Msg        string
	Step6State      string
	Step6Msg        string
}

func formatMs(ms int64) string {
	if ms <= 0 {
		return ""
	}
	d := time.Duration(ms) * time.Millisecond
	if d < time.Second {
		return fmt.Sprintf("%.2fs", float64(ms)/1000)
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	min := int(d.Minutes())
	sec := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm %ds", min, sec)
}

func computePipelineStatus(d store.Deployment, preflightFailed bool) pipelineStatus {
	p := pipelineStatus{
		Deployment:      d,
		HasDeployment:   true,
		PreflightFailed: preflightFailed,
	}

	if preflightFailed {
		p.Step1State = "failed"
		p.Step1Msg = "Requiere configuración"
		p.Step2State = "pending"
		p.Step2Msg = "No iniciado"
		p.Step3State = "pending"
		p.Step3Msg = "No iniciado"
		p.Step4State = "pending"
		p.Step4Msg = "No iniciado"
		p.Step5State = "pending"
		p.Step5Msg = "No iniciado"
		p.Step6State = "pending"
		p.Step6Msg = "No iniciado"
		return p
	}

	p.Step1State = "done"
	p.Step1Msg = "Runtime de contenedores"
	if d.QueueDurationMs > 0 {
		p.Step1Msg += " (Cola: " + formatMs(d.QueueDurationMs) + ")"
	}

	switch d.Status {
	case "queued":
		p.Step2State = "active"
		p.Step2Msg = "En cola"
		p.Step3State = "pending"
		p.Step3Msg = "En cola"
		p.Step4State = "pending"
		p.Step4Msg = "Pendiente"
		p.Step5State = "pending"
		p.Step5Msg = "Pendiente"
		p.Step6State = "pending"
		p.Step6Msg = "Pendiente"
	case "running":
		p.Step2State = "done"
		p.Step2Msg = "Completado"
		p.Step3State = "active"
		p.Step3Msg = "En curso"
		p.Step4State = "active"
		p.Step4Msg = "En curso"
		p.Step5State = "pending"
		p.Step5Msg = "Pendiente"
		p.Step6State = "pending"
		p.Step6Msg = "Pendiente"
	case "successful":
		p.Step2State = "done"
		p.Step2Msg = "Completado"
		if d.SourceDurationMs > 0 {
			p.Step2Msg += " (" + formatMs(d.SourceDurationMs) + ")"
		}
		p.Step3State = "done"
		p.Step3Msg = "Procesado"
		if d.BuildDurationMs > 0 {
			p.Step3Msg += " (" + formatMs(d.BuildDurationMs) + ")"
		}
		p.Step4State = "done"
		p.Step4Msg = "Completado"
		if d.StartDurationMs > 0 {
			p.Step4Msg += " (" + formatMs(d.StartDurationMs) + ")"
		}
		p.Step5State = "done"
		p.Step5Msg = "Confirmado"
		if d.HealthDurationMs > 0 {
			p.Step5Msg += " (" + formatMs(d.HealthDurationMs) + ")"
		}
		p.Step6State = "done"
		p.Step6Msg = "Enrutado"
		if d.RouteDurationMs > 0 {
			p.Step6Msg += " (" + formatMs(d.RouteDurationMs) + ")"
		}
	case "failed":
		p.Step2State = "done"
		p.Step2Msg = "Completado"
		if d.SourceDurationMs > 0 {
			p.Step2Msg += " (" + formatMs(d.SourceDurationMs) + ")"
		}
		p.Step3State = "done"
		p.Step3Msg = "Procesado"
		if d.BuildDurationMs > 0 {
			p.Step3Msg += " (" + formatMs(d.BuildDurationMs) + ")"
		}
		p.Step4State = "done"
		p.Step4Msg = "Completado"
		if d.StartDurationMs > 0 {
			p.Step4Msg += " (" + formatMs(d.StartDurationMs) + ")"
		}
		p.Step5State = "done"
		p.Step5Msg = "Confirmado"
		if d.HealthDurationMs > 0 {
			p.Step5Msg += " (" + formatMs(d.HealthDurationMs) + ")"
		}
		p.Step6State = "done"
		p.Step6Msg = "Enrutado"
		if d.RouteDurationMs > 0 {
			p.Step6Msg += " (" + formatMs(d.RouteDurationMs) + ")"
		}

		switch d.FailureStage {
		case "start":
			if d.FailureCode == "runtime_unavailable" {
				p.Step1State = "failed"
				p.Step1Msg = "Runtime no disponible"
				p.Step2State = "pending"
				p.Step2Msg = "No iniciado"
				p.Step3State = "pending"
				p.Step3Msg = "No iniciado"
				p.Step4State = "pending"
				p.Step4Msg = "No iniciado"
				p.Step5State = "pending"
				p.Step5Msg = "No iniciado"
				p.Step6State = "pending"
				p.Step6Msg = "No iniciado"
			} else {
				p.Step4State = "failed"
				p.Step4Msg = "Switch/Arranque fallido"
				p.Step5State = "pending"
				p.Step5Msg = "Pendiente"
				p.Step6State = "pending"
				p.Step6Msg = "Pendiente"
			}
		case "source":
			p.Step2State = "failed"
			p.Step2Msg = "Clon/Fetch fallido"
			p.Step3State = "pending"
			p.Step3Msg = "No iniciado"
			p.Step4State = "pending"
			p.Step4Msg = "No iniciado"
			p.Step5State = "pending"
			p.Step5Msg = "No iniciado"
			p.Step6State = "pending"
			p.Step6Msg = "No iniciado"
		case "build":
			p.Step3State = "failed"
			p.Step3Msg = "Compilación fallida"
			p.Step4State = "pending"
			p.Step4Msg = "No iniciado"
			p.Step5State = "pending"
			p.Step5Msg = "No iniciado"
			p.Step6State = "pending"
			p.Step6Msg = "No iniciado"
		case "health":
			p.Step5State = "failed"
			p.Step5Msg = "Health check fallido"
			p.Step6State = "pending"
			p.Step6Msg = "No iniciado"
		case "route":
			p.Step6State = "failed"
			p.Step6Msg = "Ruta proxy fallida"
		default:
			p.Step4State = "failed"
			p.Step4Msg = "Fallo de ejecución"
			p.Step5State = "pending"
			p.Step5Msg = "Pendiente"
			p.Step6State = "pending"
			p.Step6Msg = "Pendiente"
		}
	default:
		p.Step2State = "pending"
		p.Step2Msg = "Cancelado"
		p.Step3State = "pending"
		p.Step3Msg = "Cancelado"
		p.Step4State = "pending"
		p.Step4Msg = "Cancelado"
		p.Step5State = "pending"
		p.Step5Msg = "Cancelado"
		p.Step6State = "pending"
		p.Step6Msg = "Cancelado"
	}
	return p
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
	hasSecrets, _ := a.store.HasSecrets(r.Context(), application.ID)
	data := applicationDetailData{
		Application:   application,
		Deployments:   deployments,
		Phase:         currentPhase(),
		RuntimeNotice: runtimeNotice(deploy.RuntimeDiagnostics()),
		CSRFToken:     a.csrfToken(r),
		HasSecrets:    hasSecrets,
	}
	if len(deployments) > 0 {
		data.LogPreview, data.LogPreviewError = a.deploymentLogPreview(deployments[0].LogPath)
		data.Pipeline = computePipelineStatus(deployments[0], runtimePreflightFailure(data.LogPreview))
	}
	data.HealthEvidence, _ = a.store.HealthEvidence(r.Context(), application.ID)
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
	data.Preflight = a.RunPreflightCheck(ctx, application)
	vData := views.ApplicationDetailData{
		Application:     data.Application,
		Deployments:     data.Deployments,
		DeployedVersion: data.DeployedVersion,
		Container:       data.Container,
		ContainerError:  data.ContainerError,
		LogPreview:      data.LogPreview,
		LogPreviewError: data.LogPreviewError,
		RuntimeNotice:   data.RuntimeNotice,
		Phase:           views.PhaseStatus{Number: data.Phase.Number, Name: data.Phase.Name, State: data.Phase.State, Next: data.Phase.Next},
		Pipeline: views.PipelineStatus{
			Deployment:      data.Pipeline.Deployment,
			HasDeployment:   data.Pipeline.HasDeployment,
			PreflightFailed: data.Pipeline.PreflightFailed,
			Step1State:      data.Pipeline.Step1State,
			Step1Msg:        data.Pipeline.Step1Msg,
			Step2State:      data.Pipeline.Step2State,
			Step2Msg:        data.Pipeline.Step2Msg,
			Step3State:      data.Pipeline.Step3State,
			Step3Msg:        data.Pipeline.Step3Msg,
			Step4State:      data.Pipeline.Step4State,
			Step4Msg:        data.Pipeline.Step4Msg,
			Step5State:      data.Pipeline.Step5State,
			Step5Msg:        data.Pipeline.Step5Msg,
			Step6State:      data.Pipeline.Step6State,
			Step6Msg:        data.Pipeline.Step6Msg,
		},
		CSRFToken:      data.CSRFToken,
		HasSecrets:     data.HasSecrets,
		HealthEvidence: data.HealthEvidence,
		Notice:         r.URL.Query().Get("notice"),
		Error:          r.URL.Query().Get("error"),
		Preflight: views.PreflightCheckResult{
			AllOK:        data.Preflight.AllOK,
			Docker:       views.PreflightItem(data.Preflight.Docker),
			Caddy:        views.PreflightItem(data.Preflight.Caddy),
			Network:      views.PreflightItem(data.Preflight.Network),
			DiskSpace:    views.PreflightItem(data.Preflight.DiskSpace),
			RepoAccess:   views.PreflightItem(data.Preflight.RepoAccess),
			DomainHealth: views.PreflightItem(data.Preflight.DomainHealth),
		},
	}
	a.renderComponent(w, r, views.ApplicationDetail(vData))
}

// releaseReport exposes the persisted, provider-neutral evidence for a
// release. It deliberately excludes log paths, secret configuration and any
// runtime-specific credentials so it can be retained with an incident or
// acceptance record without needing access to SQLite or Docker.
func (a *App) releaseReport(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	applicationID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	deploymentID, err := strconv.ParseInt(r.PathValue("deploymentID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	application, err := a.store.Application(r.Context(), applicationID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	deployment, err := a.store.Deployment(r.Context(), applicationID, deploymentID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	report := struct {
		Version     int `json:"version"`
		Application struct {
			ID           int64  `json:"id"`
			Name         string `json:"name"`
			Domain       string `json:"domain"`
			InternalPort int    `json:"internal_port"`
		} `json:"application"`
		Release struct {
			ID                  int64     `json:"id"`
			ApplicationID       int64     `json:"application_id"`
			Action              string    `json:"action"`
			Status              string    `json:"status"`
			Image               string    `json:"image"`
			RequestedRef        string    `json:"requested_ref"`
			SourceRevision      string    `json:"source_revision"`
			SourceFingerprint   string    `json:"source_fingerprint"`
			ArtifactDigest      string    `json:"artifact_digest"`
			Runtime             string    `json:"runtime"`
			InternalPort        int       `json:"internal_port"`
			HealthEndpoint      string    `json:"health_endpoint"`
			Manifest            string    `json:"manifest"`
			ConfigurationDigest string    `json:"configuration_digest"`
			FailureStage        string    `json:"failure_stage"`
			FailureCode         string    `json:"failure_code"`
			FailureDetail       string    `json:"failure_detail"`
			CurrentStage        string    `json:"current_stage"`
			QueueDurationMs     int64     `json:"queue_duration_ms"`
			SourceDurationMs    int64     `json:"source_duration_ms"`
			BuildDurationMs     int64     `json:"build_duration_ms"`
			StartDurationMs     int64     `json:"start_duration_ms"`
			HealthDurationMs    int64     `json:"health_duration_ms"`
			RouteDurationMs     int64     `json:"route_duration_ms"`
			StartedAt           time.Time `json:"started_at"`
			FinishedAt          time.Time `json:"finished_at"`
		} `json:"release"`
	}{Version: 1}
	report.Application.ID = application.ID
	report.Application.Name = application.Name
	report.Application.Domain = application.Domain
	report.Application.InternalPort = application.InternalPort
	report.Release.ID = deployment.ID
	report.Release.ApplicationID = deployment.ApplicationID
	report.Release.Action = deployment.Action
	report.Release.Status = deployment.Status
	report.Release.Image = deployment.Image
	report.Release.RequestedRef = deployment.RequestedRef
	report.Release.SourceRevision = deployment.SourceRevision
	report.Release.SourceFingerprint = deployment.SourceFingerprint
	report.Release.ArtifactDigest = deployment.ArtifactDigest
	report.Release.Runtime = deployment.Runtime
	report.Release.InternalPort = deployment.InternalPort
	report.Release.HealthEndpoint = deployment.HealthEndpoint
	report.Release.Manifest = deployment.Manifest
	report.Release.ConfigurationDigest = deployment.ConfigurationDigest
	report.Release.FailureStage = deployment.FailureStage
	report.Release.FailureCode = deployment.FailureCode
	report.Release.FailureDetail = deployment.FailureDetail
	report.Release.CurrentStage = deployment.CurrentStage
	report.Release.QueueDurationMs = deployment.QueueDurationMs
	report.Release.SourceDurationMs = deployment.SourceDurationMs
	report.Release.BuildDurationMs = deployment.BuildDurationMs
	report.Release.StartDurationMs = deployment.StartDurationMs
	report.Release.HealthDurationMs = deployment.HealthDurationMs
	report.Release.RouteDurationMs = deployment.RouteDurationMs
	report.Release.StartedAt = deployment.StartedAt
	report.Release.FinishedAt = deployment.FinishedAt
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=release-"+strconv.FormatInt(deployment.ID, 10)+".json")
	if err := json.NewEncoder(w).Encode(report); err != nil {
		http.Error(w, "could not encode release report", http.StatusInternalServerError)
	}
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
	CSRFToken   string
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
	a.renderComponent(w, r, views.Secrets(views.SecretsData{
		Application: application,
		Environment: environment,
		Target:      target,
		Secrets:     secrets,
		Config:      configuration,
		Audit:       audit,
		Error:       message,
		CSRFToken:   a.csrfToken(r),
	}))
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
	preflightContext, cancelPreflight := context.WithTimeout(r.Context(), 4*time.Second)
	preflight := a.RunPreflightCheck(preflightContext, application)
	cancelPreflight()
	if !preflight.AllOK {
		http.Redirect(w, r, "/applications/"+r.PathValue("id")+"?error="+url.QueryEscape("Despliegue pausado: "+preflightFailureSummary(preflight)), http.StatusSeeOther)
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

func (a *App) retryApplication(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err = a.queueDeployment(r.Context(), application.ID, "retry"); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			http.Error(w, "a deployment is already queued or running", http.StatusConflict)
		} else {
			http.Error(w, "could not queue retry", 500)
		}
		return
	}
	http.Redirect(w, r, "/applications/"+r.PathValue("id"), http.StatusSeeOther)
}

func (a *App) deploymentDiff(w http.ResponseWriter, r *http.Request) {
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

	deployments, err := a.store.Deployments(r.Context(), application.ID)
	var prev store.Deployment
	hasPrev := false
	if err == nil {
		for i, d := range deployments {
			if d.ID == deployment.ID && i+1 < len(deployments) {
				prev = deployments[i+1]
				hasPrev = true
				break
			}
		}
	}

	type diffField struct {
		Name     string
		Current  string
		Previous string
		Changed  bool
	}

	fields := []diffField{
		{"Acción", deployment.Action, prev.Action, deployment.Action != prev.Action && hasPrev},
		{"Estado", deployment.Status, prev.Status, deployment.Status != prev.Status && hasPrev},
		{"Revisión de Origen (Git)", deployment.SourceRevision, prev.SourceRevision, deployment.SourceRevision != prev.SourceRevision && hasPrev},
		{"Huella de Origen", deployment.SourceFingerprint, prev.SourceFingerprint, deployment.SourceFingerprint != prev.SourceFingerprint && hasPrev},
		{"Imagen", deployment.Image, prev.Image, deployment.Image != prev.Image && hasPrev},
		{"Digest del Artefacto", deployment.ArtifactDigest, prev.ArtifactDigest, deployment.ArtifactDigest != prev.ArtifactDigest && hasPrev},
		{"Runtime", deployment.Runtime, prev.Runtime, deployment.Runtime != prev.Runtime && hasPrev},
		{"Puerto Interno", fmt.Sprint(deployment.InternalPort), fmt.Sprint(prev.InternalPort), deployment.InternalPort != prev.InternalPort && hasPrev},
		{"Ruta de Health Check", deployment.HealthEndpoint, prev.HealthEndpoint, deployment.HealthEndpoint != prev.HealthEndpoint && hasPrev},
		{"Digest de Configuración", deployment.ConfigurationDigest, prev.ConfigurationDigest, deployment.ConfigurationDigest != prev.ConfigurationDigest && hasPrev},
	}

	var vFields []views.DiffField
	for _, f := range fields {
		vFields = append(vFields, views.DiffField{
			Name:     f.Name,
			Current:  f.Current,
			Previous: f.Previous,
			Changed:  f.Changed,
		})
	}
	a.renderComponent(w, r, views.DeploymentDiff(views.DeploymentDiffData{
		Application: application,
		Deployment:  deployment,
		Previous:    prev,
		HasPrevious: hasPrev,
		Fields:      vFields,
		CSRFToken:   a.csrfToken(r),
	}))
}

func (a *App) cancelDeployment(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	application, _, err := a.applicationData(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	deploymentID, err := strconv.ParseInt(r.PathValue("deploymentID"), 10, 64)
	if err != nil || deploymentID < 1 {
		http.NotFound(w, r)
		return
	}
	if err := a.store.CancelDeployment(r.Context(), application.ID, deploymentID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.Error(w, "deployment is no longer active", http.StatusConflict)
			return
		}
		http.Error(w, "could not cancel deployment", http.StatusServiceUnavailable)
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
	configurationDigest := deploy.PublicConfigurationDigest(runtime, buildArgs)
	effectiveRuntimeConfiguration := deploy.NonSecretRuntimeConfiguration(runtime)
	publicRuntime := make(map[string]string, len(runtime))
	for name, value := range runtime {
		publicRuntime[name] = value
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
	return deploy.DeploymentConfiguration{Runtime: runtime, PublicRuntime: publicRuntime, BuildArgs: buildArgs, BuildSecrets: buildSecrets, ConfigurationDigest: configurationDigest, EffectiveRuntimeConfiguration: effectiveRuntimeConfiguration}, nil
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
	// Deployments without secrets must remain possible while MiniDock is locked:
	// there is nothing to decrypt, and public configuration is safe to read.
	if len(metadata) == 0 {
		return map[string]string{}, nil
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
	if r.URL.Query().Get("download") == "true" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"container-%s.log\"", application.Name))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	q := strings.ToLower(r.URL.Query().Get("q"))
	if q != "" {
		var buf bytes.Buffer
		if err = a.executor.Logs(r.Context(), application, &buf); err != nil {
			http.Error(w, "container logs unavailable", http.StatusBadGateway)
			return
		}
		lines := strings.Split(buf.String(), "\n")
		for _, line := range lines {
			if strings.Contains(strings.ToLower(line), q) {
				_, _ = fmt.Fprintln(w, line)
			}
		}
		return
	}
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
	if r.URL.Query().Get("download") == "true" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"deploy-%s-%d.log\"", application.Name, deployment.ID))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	q := strings.ToLower(r.URL.Query().Get("q"))
	if q != "" {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, io.LimitReader(file, 2<<20))
		lines := strings.Split(buf.String(), "\n")
		for _, line := range lines {
			if strings.Contains(strings.ToLower(line), q) {
				_, _ = fmt.Fprintln(w, line)
			}
		}
		return
	}
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
	a.renderComponent(w, r, views.Setup(""))
}

func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	initialized, err := a.store.IsInitialized(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if initialized {
		http.Redirect(w, r, "/unlock", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	password := r.Form.Get("password")
	if len(password) < 12 || password != r.Form.Get("password_confirmation") {
		w.WriteHeader(http.StatusBadRequest)
		a.renderComponent(w, r, views.Setup("Usa una contraseña de al menos 12 caracteres y confirma el mismo valor."))
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
		w.WriteHeader(http.StatusBadRequest)
		a.renderComponent(w, r, views.Setup("La configuración ya existe o no pudo guardarse."))
		return
	}
	a.setKey(key)
	a.newSession(w, r)
	log.Printf("[AUDIT] MiniDock initialized successfully")
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
	a.renderComponent(w, r, views.Unlock(""))
}

func (a *App) unlock(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC()
	scope := unlockScope(r)
	lockoutUntil, err := a.store.UnlockLockoutUntil(r.Context(), scope, now)
	if err != nil {
		http.Error(w, "unlock rate limit unavailable", http.StatusServiceUnavailable)
		return
	}
	if !lockoutUntil.IsZero() {
		w.WriteHeader(http.StatusBadRequest)
		a.renderComponent(w, r, views.Unlock(fmt.Sprintf("Demasiados intentos. Inténtalo de nuevo a las %s.", lockoutUntil.Local().Format("15:04:05"))))
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
		lockoutUntil, recordErr := a.store.RegisterFailedUnlock(r.Context(), scope, now)
		if recordErr != nil {
			http.Error(w, "unlock rate limit unavailable", http.StatusServiceUnavailable)
			return
		}
		if !lockoutUntil.IsZero() {
			log.Printf("[AUDIT] Temporary lockout triggered due to 5 failed unlock attempts from %s", r.RemoteAddr)
		}
		w.WriteHeader(http.StatusBadRequest)
		a.renderComponent(w, r, views.Unlock("Contraseña incorrecta."))
		return
	}
	if err := a.store.ClearUnlockFailures(r.Context(), scope); err != nil {
		security.Zero(key)
		http.Error(w, "unlock rate limit unavailable", http.StatusServiceUnavailable)
		return
	}
	a.setKey(key)
	security.Zero(key)
	a.newSession(w, r)
	log.Printf("[AUDIT] MiniDock unlocked successfully from %s", r.RemoteAddr)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func unlockScope(r *http.Request) string {
	origin := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil && host != "" {
		origin = host
	}
	digest := sha256.Sum256([]byte(origin))
	return fmt.Sprintf("unlock:%x", digest[:])
}

func (a *App) lock(w http.ResponseWriter, r *http.Request) {
	a.Lock()
	cookie, err := r.Cookie("minidock_session")
	if err == nil {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{Name: "minidock_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure})
	log.Printf("[AUDIT] MiniDock locked manually from %s", r.RemoteAddr)
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

func (a *App) newSession(w http.ResponseWriter, r *http.Request) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		http.Error(w, "could not create session", http.StatusInternalServerError)
		return
	}
	session := base64.RawURLEncoding.EncodeToString(value)
	expires := time.Now().Add(12 * time.Hour)
	a.mu.Lock()
	for s, exp := range a.sessions {
		if time.Now().After(exp) {
			delete(a.sessions, s)
		}
	}
	a.sessions[session] = expires
	a.mu.Unlock()
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{Name: "minidock_session", Value: session, Path: "/", Expires: expires, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure})
}

func (a *App) renderComponent(w http.ResponseWriter, r *http.Request, component templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := component.Render(r.Context(), w); err != nil {
		http.Error(w, "render component: "+err.Error(), http.StatusInternalServerError)
	}
}

func (a *App) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (a *App) csrfToken(r *http.Request) string {
	cookie, err := r.Cookie("minidock_session")
	if err != nil {
		return ""
	}
	return a.csrfTokenFor(cookie.Value)
}

func (a *App) csrfTokenFor(sessionID string) string {
	a.mu.RLock()
	key := append([]byte(nil), a.key...)
	a.mu.RUnlock()
	if len(key) == 0 {
		return ""
	}
	defer security.Zero(key)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *App) csrfCheck(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !strings.HasPrefix(r.URL.Path, "/webhooks/") && r.URL.Path != "/setup" && r.URL.Path != "/unlock" {
			if a.config.Environment == "test" && r.Header.Get("X-Test-Validate-CSRF") == "" {
				next.ServeHTTP(w, r)
				return
			}
			if !a.authorized(r) {
				http.Redirect(w, r, "/unlock", http.StatusSeeOther)
				return
			}
			cookie, err := r.Cookie("minidock_session")
			if err != nil {
				http.Redirect(w, r, "/unlock", http.StatusSeeOther)
				return
			}
			expected := a.csrfTokenFor(cookie.Value)
			actual := r.FormValue("csrf_token")
			if actual == "" {
				actual = r.Header.Get("X-CSRF-Token")
			}
			if expected == "" || !hmac.Equal([]byte(actual), []byte(expected)) {
				log.Printf("[AUDIT] CSRF validation failed for request on %s from %s", r.URL.Path, r.RemoteAddr)
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) renderCloudflareDashboard(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	cfg, err := a.store.CloudflareConfig(r.Context())
	if err != nil {
		http.Error(w, "error al cargar configuracion Cloudflare", http.StatusInternalServerError)
		return
	}
	apps, err := a.store.Applications(r.Context())
	if err != nil {
		http.Error(w, "error al cargar aplicaciones", http.StatusInternalServerError)
		return
	}
	connectorStatus := "Pendiente de iniciar"
	status, statusErr := a.cloudflared.Status(r.Context())
	if statusErr != nil {
		connectorStatus = statusErr.Error()
	} else {
		connectorStatus = status.Display()
		if cfg.Mode != "disabled" {
			if status.Running() {
				if cfg.Status != "error" {
					cfg.Status = "connected"
				}
			} else if cfg.Status == "connected" {
				cfg.Status = "configured"
			}
		}
	}
	if statusErr == nil && status.Running() && cfg.Mode == "api_token" && cfg.AccountID != "" && cfg.TunnelID != "" {
		if apiToken, err := a.getCloudflareAPIToken(r.Context()); err == nil {
			remoteContext, cancelRemote := context.WithTimeout(r.Context(), 3*time.Second)
			remoteStatus, remoteErr := a.cloudflareAPI().TunnelStatus(remoteContext, apiToken, cfg.AccountID, cfg.TunnelID)
			cancelRemote()
			if remoteErr == nil {
				switch remoteStatus {
				case "healthy":
					connectorStatus = "En ejecución · Cloudflare saludable"
					cfg.Status = "connected"
				case "degraded":
					connectorStatus = "En ejecución · conexión degradada"
					cfg.Status = "error"
					cfg.ErrorMessage = "Cloudflare reporta una conexión degradada"
				case "inactive", "down":
					connectorStatus = "En ejecución · esperando conexión con Cloudflare"
					cfg.Status = "configuring"
				}
			}
		}
	}

	expectedCNAME := "configurar-tunel.cfargotunnel.com"
	if cfg.TunnelID != "" {
		expectedCNAME = fmt.Sprintf("%s.cfargotunnel.com", cfg.TunnelID)
	}

	// DNS is an external dependency. Bound all lookups for this page so an
	// unavailable local resolver cannot keep the Cloudflare dashboard loading.
	dnsContext, cancelDNSLookup := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancelDNSLookup()

	var appInfos []views.ApplicationCloudflareInfo
	for _, app := range apps {
		if !isPublicCloudflareDomain(app.Domain) {
			appInfos = append(appInfos, views.ApplicationCloudflareInfo{
				App:           app,
				ExpectedCNAME: expectedCNAME,
				StatusBadge:   "⚪ Requiere dominio público",
			})
			continue
		}
		isPropagated, currentCNAME, _ := VerifyDNSPropagation(dnsContext, app.Domain, expectedCNAME)
		badge := "🟡 Pendiente"
		if isPropagated {
			badge = "🟢 Propagado"
		}
		appInfos = append(appInfos, views.ApplicationCloudflareInfo{
			App:           app,
			ExpectedCNAME: expectedCNAME,
			ResolvedCNAME: currentCNAME,
			IsPropagated:  isPropagated,
			StatusBadge:   badge,
		})
	}

	noticeMsg := r.URL.Query().Get("notice")
	errorMsg := r.URL.Query().Get("error")

	a.renderComponent(w, r, views.CloudflareDashboard(views.CloudflareData{
		Config:          cfg,
		Apps:            appInfos,
		ConnectorStatus: connectorStatus,
		CSRFToken:       a.csrfToken(r),
		NoticeMessage:   noticeMsg,
		ErrorMessage:    errorMsg,
	}))
}

func (a *App) saveCloudflareConfig(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	mode := strings.TrimSpace(r.Form.Get("mode"))
	tunnelToken := strings.TrimSpace(r.Form.Get("tunnel_token"))
	apiToken := strings.TrimSpace(r.Form.Get("api_token"))
	accountID := strings.TrimSpace(r.Form.Get("account_id"))

	cfg, _ := a.store.CloudflareConfig(r.Context())
	cfg.Mode = mode
	if accountID != "" {
		cfg.AccountID = accountID
	}

	key := a.keyCopy()
	if len(key) > 0 {
		defer security.Zero(key)
	}

	if len(key) == 0 {
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Desbloquea MiniDock para administrar credenciales"), http.StatusSeeOther)
		return
	}

	if mode == "disabled" {
		if err := a.cloudflared.Remove(r.Context()); err != nil {
			a.saveCloudflareFailure(r.Context(), cfg, "No se pudo detener cloudflared: "+err.Error())
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudo detener el conector: "+err.Error()), http.StatusSeeOther)
			return
		}
		cfg.Status = "disabled"
		cfg.ErrorMessage = ""
		if err := a.store.SaveCloudflareConfig(r.Context(), cfg); err != nil {
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Error al guardar en base de datos"), http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, "/cloudflare?notice="+url.QueryEscape("Cloudflare Tunnel desactivado; las credenciales permanecen cifradas para poder reactivarlo"), http.StatusSeeOther)
		return
	}

	if mode == "tunnel_token" {
		if tunnelToken == "" {
			var err error
			tunnelToken, err = a.getCloudflareSecret(r.Context(), "tunnel_token")
			if err != nil {
				http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Pega un Tunnel Token para conectar"), http.StatusSeeOther)
				return
			}
		}
		cfg.TunnelID = tunnelIDFromToken(tunnelToken)
		cfg.TunnelName = "Túnel existente"
		if err := a.putCloudflareSecret(r.Context(), key, "tunnel_token", tunnelToken); err != nil {
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudo cifrar el Tunnel Token"), http.StatusSeeOther)
			return
		}
	} else if mode == "api_token" {
		if apiToken == "" {
			var err error
			apiToken, err = a.getCloudflareSecret(r.Context(), "api_token")
			if err != nil {
				http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Pega un API Token para realizar la configuración automática"), http.StatusSeeOther)
				return
			}
		}
		if !cloudflareAccountIDRegex.MatchString(cfg.AccountID) {
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("El Account ID debe contener exactamente 32 caracteres hexadecimales"), http.StatusSeeOther)
			return
		}
		client := a.cloudflareAPI()
		if err := client.VerifyToken(r.Context(), apiToken); err != nil {
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Token API invalido: "+err.Error()), http.StatusSeeOther)
			return
		}
		tunnelName, err := a.cloudflareTunnelName(r.Context(), cfg)
		if err != nil {
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudo generar la identidad segura del túnel"), http.StatusSeeOther)
			return
		}
		tunnelID, tToken, err := client.CreateOrGetTunnel(r.Context(), apiToken, cfg.AccountID, tunnelName)
		if err != nil {
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Error al crear tunel: "+err.Error()), http.StatusSeeOther)
			return
		}
		cfg.TunnelID = tunnelID
		cfg.TunnelName = tunnelName
		tunnelToken = tToken
		if err := a.putCloudflareSecret(r.Context(), key, "api_token", apiToken); err != nil {
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudo cifrar el API Token"), http.StatusSeeOther)
			return
		}
		if err := a.putCloudflareSecret(r.Context(), key, "tunnel_token", tunnelToken); err != nil {
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudo cifrar el Tunnel Token"), http.StatusSeeOther)
			return
		}
		apps, appsErr := a.store.Applications(r.Context())
		if appsErr != nil {
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudieron cargar las aplicaciones para configurar el túnel"), http.StatusSeeOther)
			return
		}
		if err := client.ConfigureTunnelIngress(r.Context(), apiToken, cfg.AccountID, tunnelID, publicApplicationDomains(apps)); err != nil {
			a.saveCloudflareFailure(r.Context(), cfg, err.Error())
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("El túnel fue creado, pero no se pudo configurar su enrutamiento: "+err.Error()), http.StatusSeeOther)
			return
		}
		if err := syncCloudflareDNSRecords(r.Context(), client, apiToken, tunnelID, publicApplicationDomains(apps)); err != nil {
			a.saveCloudflareFailure(r.Context(), cfg, err.Error())
			http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("El túnel fue creado, pero no se pudo configurar todo el DNS: "+err.Error()), http.StatusSeeOther)
			return
		}
	} else {
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Modo de Cloudflare no válido"), http.StatusSeeOther)
		return
	}

	cfg.Status = "configuring"
	cfg.ErrorMessage = ""
	if err := a.store.SaveCloudflareConfig(r.Context(), cfg); err != nil {
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Error al guardar en base de datos"), http.StatusSeeOther)
		return
	}
	if err := a.cloudflared.Reconcile(r.Context(), tunnelToken); err != nil {
		a.saveCloudflareFailure(r.Context(), cfg, err.Error())
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Cloudflare quedó configurado, pero el conector no pudo iniciarse: "+err.Error()), http.StatusSeeOther)
		return
	}
	cfg.Status = "connected"
	cfg.ErrorMessage = ""

	if err := a.store.SaveCloudflareConfig(r.Context(), cfg); err != nil {
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Error al guardar en base de datos"), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/cloudflare?notice="+url.QueryEscape("Cloudflare Tunnel configurado y cloudflared iniciado automáticamente"), http.StatusSeeOther)
}

func (a *App) getCloudflareAPIToken(ctx context.Context) (string, error) {
	return a.getCloudflareSecret(ctx, "api_token")
}

func (a *App) getCloudflareSecret(ctx context.Context, name string) (string, error) {
	key := a.keyCopy()
	if len(key) == 0 {
		return "", fmt.Errorf("sesión bloqueada")
	}
	defer security.Zero(key)
	nonce, ciphertext, err := a.store.Secret(ctx, "system:cloudflare", name)
	if err != nil {
		return "", err
	}
	decrypted, err := security.Decrypt(key, nonce, ciphertext)
	if err != nil {
		return "", err
	}
	defer security.Zero(decrypted)
	return string(decrypted), nil
}

func (a *App) putCloudflareSecret(ctx context.Context, key []byte, name, value string) error {
	plaintext := []byte(value)
	defer security.Zero(plaintext)
	nonce, ciphertext, err := security.Encrypt(key, plaintext)
	if err != nil {
		return err
	}
	return a.store.PutSecret(ctx, "system:cloudflare", name, nonce, ciphertext)
}

func (a *App) saveCloudflareFailure(ctx context.Context, cfg store.CloudflareConfig, message string) {
	cfg.Status = "error"
	cfg.ErrorMessage = message
	_ = a.store.SaveCloudflareConfig(ctx, cfg)
}

func (a *App) cloudflareTunnelName(ctx context.Context, cfg store.CloudflareConfig) (string, error) {
	if cfg.TunnelID != "" && cfg.TunnelName != "" && cfg.TunnelName != "Túnel existente" {
		return cfg.TunnelName, nil
	}
	instanceID, err := a.store.SettingString(ctx, "cloudflare_instance_id")
	if err != nil {
		return "", err
	}
	validInstanceID, _ := regexp.MatchString(`^[0-9a-f]{12}$`, instanceID)
	if !validInstanceID {
		randomID := make([]byte, 6)
		if _, err := rand.Read(randomID); err != nil {
			return "", err
		}
		instanceID = hex.EncodeToString(randomID)
		if err := a.store.SetSettingString(ctx, "cloudflare_instance_id", instanceID); err != nil {
			return "", err
		}
	}
	return "minidock-" + instanceID, nil
}

func (a *App) reconnectCloudflared(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	cfg, err := a.store.CloudflareConfig(r.Context())
	if err != nil || cfg.Mode == "disabled" {
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Configura Cloudflare antes de iniciar el conector"), http.StatusSeeOther)
		return
	}
	token, err := a.getCloudflareSecret(r.Context(), "tunnel_token")
	if err != nil {
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se encontró un Tunnel Token cifrado"), http.StatusSeeOther)
		return
	}
	if err := a.cloudflared.Reconcile(r.Context(), token); err != nil {
		a.saveCloudflareFailure(r.Context(), cfg, err.Error())
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudo iniciar el conector: "+err.Error()), http.StatusSeeOther)
		return
	}
	cfg.Status = "connected"
	cfg.ErrorMessage = ""
	_ = a.store.SaveCloudflareConfig(r.Context(), cfg)
	http.Redirect(w, r, "/cloudflare?notice="+url.QueryEscape("Conector cloudflared iniciado"), http.StatusSeeOther)
}

func publicApplicationDomains(apps []store.Application) []string {
	domains := make([]string, 0, len(apps))
	for _, application := range apps {
		if hostname := cloudflareHostname(application.Domain); isPublicCloudflareDomain(hostname) {
			domains = append(domains, hostname)
		}
	}
	return domains
}

func cloudflareHostname(domain string) string {
	domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
	if host, _, err := net.SplitHostPort(domain); err == nil {
		return strings.TrimSuffix(host, ".")
	}
	return domain
}

func cloudflareZoneForDomain(zones []CloudflareZone, domain string) (CloudflareZone, bool) {
	domain = cloudflareHostname(domain)
	var selected CloudflareZone
	for _, zone := range zones {
		name := cloudflareHostname(zone.Name)
		if domain != name && !strings.HasSuffix(domain, "."+name) {
			continue
		}
		if len(name) > len(selected.Name) {
			selected = zone
		}
	}
	return selected, selected.ID != ""
}

func syncCloudflareDNSRecords(ctx context.Context, client CloudflareAPI, apiToken, tunnelID string, domains []string) error {
	if len(domains) == 0 {
		return nil
	}
	zones, err := client.ListZones(ctx, apiToken)
	if err != nil {
		return err
	}
	target := tunnelID + ".cfargotunnel.com"
	for _, domain := range domains {
		zone, ok := cloudflareZoneForDomain(zones, domain)
		if !ok {
			return fmt.Errorf("el token no tiene acceso a la zona DNS de %s", domain)
		}
		if err := client.UpsertCNAMERecord(ctx, apiToken, zone.ID, cloudflareHostname(domain), target); err != nil {
			return err
		}
	}
	return nil
}

func tunnelIDFromToken(token string) string {
	token = strings.TrimSpace(token)
	if dot := strings.IndexByte(token, '.'); dot >= 0 {
		token = token[:dot]
	}
	payload, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return ""
	}
	var claims struct {
		TunnelID string `json:"t"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	if matched, _ := regexp.MatchString(`^[0-9a-fA-F-]{36}$`, claims.TunnelID); !matched {
		return ""
	}
	return strings.ToLower(claims.TunnelID)
}

func (a *App) syncAppCloudflareDNS(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	appID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid application id", http.StatusBadRequest)
		return
	}
	app, err := a.store.Application(r.Context(), appID)
	if err != nil {
		http.Error(w, "aplicacion no encontrada", http.StatusNotFound)
		return
	}

	cfg, err := a.store.CloudflareConfig(r.Context())
	if err != nil {
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Configuracion no encontrada"), http.StatusSeeOther)
		return
	}
	if cfg.Mode == "disabled" || cfg.TunnelID == "" {
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("Configura y conecta un túnel de Cloudflare antes de verificar el DNS"), http.StatusSeeOther)
		return
	}
	if !isPublicCloudflareDomain(app.Domain) {
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("El dominio "+app.Domain+" no es público. Para Cloudflare usa un subdominio completo, por ejemplo demo.tudominio.com"), http.StatusSeeOther)
		return
	}

	expectedCNAME := fmt.Sprintf("%s.cfargotunnel.com", cfg.TunnelID)

	if cfg.Mode == "api_token" {
		apiToken, err := a.getCloudflareAPIToken(r.Context())
		if err == nil && apiToken != "" {
			client := a.cloudflareAPI()
			apps, appsErr := a.store.Applications(r.Context())
			if appsErr != nil {
				http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudieron cargar las rutas del túnel"), http.StatusSeeOther)
				return
			}
			if err := client.ConfigureTunnelIngress(r.Context(), apiToken, cfg.AccountID, cfg.TunnelID, publicApplicationDomains(apps)); err != nil {
				http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudo publicar la ruta en el túnel: "+err.Error()), http.StatusSeeOther)
				return
			}
			if err := syncCloudflareDNSRecords(r.Context(), client, apiToken, cfg.TunnelID, []string{app.Domain}); err != nil {
				http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudo configurar el DNS: "+err.Error()), http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/cloudflare?notice="+url.QueryEscape("Ruta y DNS configurados automáticamente para "+app.Domain), http.StatusSeeOther)
			return
		}
	}

	dnsContext, cancelDNSLookup := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancelDNSLookup()
	isPropagated, resolvedCNAME, err := VerifyDNSPropagation(dnsContext, app.Domain, expectedCNAME)
	if err != nil {
		http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("No se pudo consultar el DNS de "+app.Domain+": "+err.Error()), http.StatusSeeOther)
		return
	}

	if isPropagated {
		http.Redirect(w, r, "/cloudflare?notice="+url.QueryEscape("DNS verificado con exito para "+app.Domain), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/cloudflare?error="+url.QueryEscape("El DNS de "+app.Domain+" aún no apunta al túnel. CNAME actual: "+resolvedCNAME), http.StatusSeeOther)
}

func isPublicCloudflareDomain(domain string) bool {
	host := cloudflareHostname(domain)
	return host != "" && host != "localhost" && !strings.HasSuffix(host, ".local") && strings.Contains(host, ".") && net.ParseIP(host) == nil
}

func (a *App) updateApplicationDomain(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	appID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid application id", http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	domain := strings.TrimSpace(r.Form.Get("domain"))
	if !domainRegex.MatchString(domain) || domain == a.config.AdminDomain {
		http.Redirect(w, r, "/applications/"+strconv.FormatInt(appID, 10)+"?error="+url.QueryEscape("Dominio invalido o coincide con el dominio administrativo"), http.StatusSeeOther)
		return
	}
	if err := a.store.UpdateApplicationDomain(r.Context(), appID, domain); err != nil {
		http.Redirect(w, r, "/applications/"+strconv.FormatInt(appID, 10)+"?error="+url.QueryEscape("Error al actualizar dominio"), http.StatusSeeOther)
		return
	}
	if isPublicCloudflareDomain(domain) {
		cfg, cfgErr := a.store.CloudflareConfig(r.Context())
		if cfgErr == nil && cfg.Mode == "api_token" && cfg.TunnelID != "" {
			apiToken, tokenErr := a.getCloudflareAPIToken(r.Context())
			apps, appsErr := a.store.Applications(r.Context())
			if tokenErr == nil && appsErr == nil {
				client := a.cloudflareAPI()
				if routeErr := client.ConfigureTunnelIngress(r.Context(), apiToken, cfg.AccountID, cfg.TunnelID, publicApplicationDomains(apps)); routeErr != nil {
					http.Redirect(w, r, "/applications/"+strconv.FormatInt(appID, 10)+"?error="+url.QueryEscape("Dominio guardado, pero Cloudflare no pudo publicar la ruta: "+routeErr.Error()), http.StatusSeeOther)
					return
				}
				if dnsErr := syncCloudflareDNSRecords(r.Context(), client, apiToken, cfg.TunnelID, []string{domain}); dnsErr != nil {
					http.Redirect(w, r, "/applications/"+strconv.FormatInt(appID, 10)+"?error="+url.QueryEscape("Dominio guardado, pero Cloudflare no pudo configurar el DNS: "+dnsErr.Error()), http.StatusSeeOther)
					return
				}
				http.Redirect(w, r, "/applications/"+strconv.FormatInt(appID, 10)+"?notice="+url.QueryEscape("Dominio actualizado y publicado automáticamente en Cloudflare"), http.StatusSeeOther)
				return
			}
		}
	}
	http.Redirect(w, r, "/applications/"+strconv.FormatInt(appID, 10)+"?notice="+url.QueryEscape("Dominio actualizado exitosamente a "+domain), http.StatusSeeOther)
}
