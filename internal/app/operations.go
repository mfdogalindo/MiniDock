package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/julieta/minidock/internal/app/views"
	"github.com/julieta/minidock/internal/backup"
	"github.com/julieta/minidock/internal/deploy"
	"github.com/julieta/minidock/internal/security"
	"github.com/julieta/minidock/internal/store"
)

type operationalAlert struct{ Severity, Message string }

type operationApplication struct {
	Application   store.Application
	Deployment    store.Deployment
	HasDeployment bool
	Container     deploy.ContainerStatus
	Unavailable   string
	Health        store.HealthEvidence
}

type operationsData struct {
	Applications                  []operationApplication
	Alerts                        []store.Alert
	ResolvedAlerts                []store.Alert
	DiskUsed                      string
	DiskSLOCompliant              bool
	DiskBudgetPercent             int
	BuildSLOPercent               float64
	BuildSLOLimitMinutes          int
	BuildSLOCompliant             bool
	RoutingSLOPercent             float64
	RoutingSLOCompliant           bool
	RetentionDays, RetainedImages int
	CleanupSchedule               string
	CSRFToken                     string
	LastBackupSuccess             string
	LastBackupError               string
	BackupAge                     string
	LastRestoreTest               string
}

func (a *App) operations(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	applications, err := a.store.Applications(r.Context())
	if err != nil {
		http.Error(w, "applications unavailable", http.StatusServiceUnavailable)
		return
	}
	settings, err := a.store.AutomationSettings(r.Context())
	if err != nil {
		http.Error(w, "automation settings unavailable", http.StatusServiceUnavailable)
		return
	}

	a.cacheMu.RLock()
	diskUsed := a.diskUsedCache
	alertsCache := a.alertsCache
	statusCache := a.statusCache
	statusCacheErr := a.statusCacheErr
	a.cacheMu.RUnlock()

	activeAlerts, err := a.store.GetActiveAlerts(r.Context())
	if err != nil {
		activeAlerts = []store.Alert{}
	}
	for _, alert := range alertsCache {
		exists := false
		for _, dbAlert := range activeAlerts {
			if dbAlert.Message == alert.Message {
				exists = true
				break
			}
		}
		if !exists {
			activeAlerts = append(activeAlerts, store.Alert{
				Severity:  alert.Severity,
				Message:   alert.Message,
				CreatedAt: time.Now().UTC(),
			})
		}
	}
	resolvedAlerts, err := a.store.GetResolvedAlerts(r.Context())
	if err != nil {
		resolvedAlerts = []store.Alert{}
	}

	// Calculate Disk SLO
	diskUsedFloat := 0.0
	if used, err := diskUsage(a.config.DatabasePath); err == nil {
		diskUsedFloat = used
	}
	diskBudgetPercent := a.config.DiskAlertPercent
	diskSLOCompliant := diskUsedFloat <= float64(diskBudgetPercent)

	// Calculate Build Duration SLO
	buildSLOLimit := a.config.SLOBuildDurationMinutes
	buildSLOPercent, err := a.store.BuildDurationSLO(r.Context(), int64(buildSLOLimit)*60*1000)
	if err != nil {
		buildSLOPercent = 100.0
	}
	buildSLOCompliant := buildSLOPercent >= 95.0

	// Routing availability is calculated from persisted external HTTP probes,
	// never from the Docker container snapshot.
	routingSLOPercent, err := a.store.RoutingSLO(r.Context(), time.Now().UTC().Add(-a.config.SLOProbeWindow))
	if err != nil {
		routingSLOPercent = 0
	}
	routingSLOCompliant := routingSLOPercent >= 99.0

	data := operationsData{
		RetentionDays:        retentionDays(a.config),
		RetainedImages:       retainedImages(a.config),
		CleanupSchedule:      settings.CleanupSchedule,
		DiskUsed:             diskUsed,
		Alerts:               activeAlerts,
		ResolvedAlerts:       resolvedAlerts,
		DiskSLOCompliant:     diskSLOCompliant,
		DiskBudgetPercent:    diskBudgetPercent,
		BuildSLOPercent:      buildSLOPercent,
		BuildSLOLimitMinutes: buildSLOLimit,
		BuildSLOCompliant:    buildSLOCompliant,
		RoutingSLOPercent:    routingSLOPercent,
		RoutingSLOCompliant:  routingSLOCompliant,
		CSRFToken:            a.csrfToken(r),
	}

	lastSuccess, _ := a.store.SettingString(r.Context(), "backup_last_success")
	lastError, _ := a.store.SettingString(r.Context(), "backup_last_error")
	lastRestore, _ := a.store.SettingString(r.Context(), "backup_last_restore_test")

	backupAge := "Ningún backup realizado"
	if lastSuccess != "" {
		if t, err := time.Parse(time.RFC3339, lastSuccess); err == nil {
			backupAge = time.Since(t).Truncate(time.Second).String() + " atrás"
		} else {
			backupAge = lastSuccess
		}
	}
	if lastRestore == "" {
		lastRestore = "No ensayada aún"
	}
	data.LastBackupSuccess = lastSuccess
	data.LastBackupError = lastError
	data.BackupAge = backupAge
	data.LastRestoreTest = lastRestore

	for _, application := range applications {
		item := operationApplication{Application: application}
		deployments, err := a.store.Deployments(r.Context(), application.ID)
		if err == nil && len(deployments) > 0 {
			item.Deployment, item.HasDeployment = deployments[0], true
		}

		if errMsg, exists := statusCacheErr[application.ID]; exists {
			item.Unavailable = errMsg
		} else if status, exists := statusCache[application.ID]; exists {
			item.Container = status
		}
		item.Health, _ = a.store.HealthEvidence(r.Context(), application.ID)

		data.Applications = append(data.Applications, item)
	}
	var vApps []views.OperationApplication
	for _, appItem := range data.Applications {
		vApps = append(vApps, views.OperationApplication{
			Application:   appItem.Application,
			Deployment:    appItem.Deployment,
			HasDeployment: appItem.HasDeployment,
			Container:     appItem.Container,
			Unavailable:   appItem.Unavailable,
			Health:        appItem.Health,
		})
	}
	a.renderComponent(w, r, views.Operations(views.OperationsData{
		Applications:         vApps,
		Alerts:               data.Alerts,
		ResolvedAlerts:       data.ResolvedAlerts,
		DiskUsed:             data.DiskUsed,
		DiskSLOCompliant:     data.DiskSLOCompliant,
		DiskBudgetPercent:    data.DiskBudgetPercent,
		BuildSLOPercent:      data.BuildSLOPercent,
		BuildSLOLimitMinutes: data.BuildSLOLimitMinutes,
		BuildSLOCompliant:    data.BuildSLOCompliant,
		RoutingSLOPercent:    data.RoutingSLOPercent,
		RoutingSLOCompliant:  data.RoutingSLOCompliant,
		RetentionDays:        data.RetentionDays,
		RetainedImages:       data.RetainedImages,
		CleanupSchedule:      data.CleanupSchedule,
		CSRFToken:            data.CSRFToken,
		LastBackupSuccess:    data.LastBackupSuccess,
		LastBackupError:      data.LastBackupError,
		BackupAge:            data.BackupAge,
		LastRestoreTest:      data.LastRestoreTest,
	}))
}

func (a *App) StartCollector(ctx context.Context) {
	a.runCollection(ctx)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.runCollection(ctx)
			}
		}
	}()
}

func (a *App) runCollection(ctx context.Context) {
	applications, err := a.store.Applications(ctx)
	if err != nil {
		return
	}

	newStatusCache := make(map[int64]deploy.ContainerStatus)
	newStatusCacheErr := make(map[int64]string)
	newCertAlertsCache := make(map[int64]string)
	var activeAlertMessages []string

	// Check disk usage
	var diskUsed string
	if used, err := diskUsage(a.config.DatabasePath); err != nil {
		msg := "No se pudo consultar el espacio de disco: " + err.Error()
		activeAlertMessages = append(activeAlertMessages, msg)
		_ = a.store.UpsertAlert(ctx, "warning", msg, nil, nil)
	} else {
		diskUsed = fmt.Sprintf("%.1f%%", used)
		if used >= float64(a.config.DiskAlertPercent) {
			msg := fmt.Sprintf("Espacio de disco al %.1f%% (umbral: %d%%).", used, a.config.DiskAlertPercent)
			activeAlertMessages = append(activeAlertMessages, msg)
			_ = a.store.UpsertAlert(ctx, "critical", msg, nil, nil)
		}
	}

	for _, application := range applications {
		// Get deployment status from db
		deployments, err := a.store.Deployments(ctx, application.ID)
		var depID *int64
		if err == nil && len(deployments) > 0 {
			depID = &deployments[0].ID
			if deployments[0].Status == "failed" {
				msg := "El último despliegue de " + application.Name + " falló."
				activeAlertMessages = append(activeAlertMessages, msg)
				_ = a.store.UpsertAlert(ctx, "critical", msg, &application.ID, depID)
			}
		}

		// Query container status
		statusCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		status, err := a.executor.Status(statusCtx, application)
		cancel()
		if err != nil {
			newStatusCacheErr[application.ID] = err.Error()
			if !strings.Contains(err.Error(), "Apple Container") {
				msg := "No se pudo obtener el estado de " + application.Name + "."
				activeAlertMessages = append(activeAlertMessages, msg)
				_ = a.store.UpsertAlert(ctx, "warning", msg, &application.ID, nil)
			}
		} else {
			newStatusCache[application.ID] = status
			if status.State != "running" || status.Health == "unhealthy" {
				msg := "El servicio " + application.Name + " no está saludable."
				activeAlertMessages = append(activeAlertMessages, msg)
				_ = a.store.UpsertAlert(ctx, "critical", msg, &application.ID, nil)
			}
		}

		internalStatus := "unavailable"
		if err == nil {
			internalStatus = "failed"
			if status.State == "running" && status.Health != "unhealthy" {
				internalStatus = "healthy"
			}
		}
		routeStatus := "not_applied"
		if depID != nil && deployments[0].Status == "successful" {
			routeStatus = "applied"
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
		probe := a.executor.ProbeRoute(probeCtx, application)
		probeCancel()
		externalStatus := "failed"
		if probe.Success {
			externalStatus = "successful"
		}
		now := time.Now().UTC()
		_ = a.store.UpsertHealthEvidence(ctx, store.HealthEvidence{ApplicationID: application.ID, InternalStatus: internalStatus, InternalCheckedAt: now, RouteStatus: routeStatus, RouteCheckedAt: now, ExternalStatus: externalStatus, ExternalCheckedAt: probe.CheckedAt, ExternalObserver: probe.Observer, ExternalHTTPStatus: probe.StatusCode, ExternalDetail: probe.Detail})
		_ = a.store.RecordRouteProbe(ctx, application.ID, probe.Success, probe.CheckedAt, probe.Observer, probe.StatusCode, probe.Detail)
		if !probe.Success {
			msg := "La sonda externa de " + application.Name + " falló."
			activeAlertMessages = append(activeAlertMessages, msg)
			_ = a.store.UpsertAlert(ctx, "critical", msg, &application.ID, nil)
		}

		// Check certificate
		if alert := certificateAlert(application.Domain, a.config.CertificateAlertDays); alert != "" {
			newCertAlertsCache[application.ID] = alert
			activeAlertMessages = append(activeAlertMessages, alert)
			_ = a.store.UpsertAlert(ctx, "warning", alert, &application.ID, nil)
		}
	}

	// Resolve any alerts that are no longer active
	_ = a.store.ResolveAllAlertsExcept(ctx, activeAlertMessages)

	// Fetch active alerts from DB to synchronize memory alertsCache
	dbAlerts, _ := a.store.GetActiveAlerts(ctx)
	var newAlerts []operationalAlert
	for _, dbAlert := range dbAlerts {
		newAlerts = append(newAlerts, operationalAlert{dbAlert.Severity, dbAlert.Message})
	}

	// Update cache
	a.cacheMu.Lock()
	a.statusCache = newStatusCache
	a.statusCacheErr = newStatusCacheErr
	a.certAlertsCache = newCertAlertsCache
	a.diskUsedCache = diskUsed
	a.alertsCache = newAlerts
	a.cacheMu.Unlock()
}

func diskUsage(path string) (float64, error) {
	if path == "" {
		path = "."
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(path), &stat); err != nil {
		return 0, err
	}
	if stat.Blocks == 0 {
		return 0, fmt.Errorf("filesystem has no blocks")
	}
	return float64(stat.Blocks-stat.Bavail) * 100 / float64(stat.Blocks), nil
}

func certificateAlert(domain string, threshold int) string {
	host := strings.TrimSpace(strings.Split(domain, ":")[0])
	if host == "" || host == "localhost" || net.ParseIP(host) != nil {
		return ""
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	connection, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, "443"), &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	if err != nil {
		return "El certificado de " + host + " no pudo verificarse."
	}
	defer connection.Close()
	certificates := connection.ConnectionState().PeerCertificates
	if len(certificates) == 0 {
		return "El certificado de " + host + " no pudo leerse."
	}
	remaining := time.Until(certificates[0].NotAfter)
	if remaining <= 0 {
		return "El certificado de " + host + " venció."
	}
	if remaining <= time.Duration(threshold)*24*time.Hour {
		return fmt.Sprintf("El certificado de %s vence en %.0f días.", host, remaining.Hours()/24)
	}
	return ""
}

// retention is manual and authenticated: cleanup is irreversible, while the
// policy is visible on the operations page before the button is used.
func (a *App) retention(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	if err := a.runRetention(r.Context()); err != nil {
		http.Error(w, "retention unavailable", http.StatusServiceUnavailable)
		return
	}
	http.Redirect(w, r, "/operations", http.StatusSeeOther)
}

type cleanupCandidateInfo struct {
	ApplicationName string
	DeploymentID    int64
	Image           string
	LogPath         string
	LogSize         string
	AgeDays         int
}

func (a *App) retentionPreview(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	applications, err := a.store.Applications(r.Context())
	if err != nil {
		http.Error(w, "applications unavailable", http.StatusServiceUnavailable)
		return
	}
	before := time.Now().UTC().AddDate(0, 0, -retentionDays(a.config))
	var candidates []cleanupCandidateInfo
	totalLogSizeBytes := int64(0)
	imagesCount := 0

	for _, application := range applications {
		cDep, err := a.store.RetentionCandidates(r.Context(), application.ID, before, retainedImages(a.config))
		if err != nil {
			continue
		}
		for _, c := range cDep {
			info := cleanupCandidateInfo{
				ApplicationName: application.Name,
				DeploymentID:    c.ID,
				Image:           c.Image,
				LogPath:         c.LogPath,
			}
			if c.Image != "" && c.Status == "successful" {
				imagesCount++
			}
			path, errPath := a.deploymentLogPath(c.LogPath)
			if errPath == nil {
				if stat, errStat := os.Stat(path); errStat == nil {
					totalLogSizeBytes += stat.Size()
					info.LogSize = fmt.Sprintf("%.2f KB", float64(stat.Size())/1024)
				}
			}
			info.AgeDays = int(time.Since(c.StartedAt).Hours() / 24)
			candidates = append(candidates, info)
		}
	}

	var vCandidates []views.CleanupCandidateInfo
	for _, c := range candidates {
		vCandidates = append(vCandidates, views.CleanupCandidateInfo{
			ApplicationName: c.ApplicationName,
			DeploymentID:    c.DeploymentID,
			Image:           c.Image,
			LogPath:         c.LogPath,
			LogSize:         c.LogSize,
			AgeDays:         c.AgeDays,
		})
	}
	a.renderComponent(w, r, views.OperationsCleanupPreview(views.OperationsCleanupPreviewData{
		Candidates:     vCandidates,
		TotalLogsSize:  fmt.Sprintf("%.2f MB", float64(totalLogSizeBytes)/(1024*1024)),
		TotalImages:    imagesCount,
		RetentionDays:  retentionDays(a.config),
		RetainedImages: retainedImages(a.config),
		CSRFToken:      a.csrfToken(r),
	}))
}

func (a *App) runRetention(ctx context.Context) error {
	applications, err := a.store.Applications(ctx)
	if err != nil {
		return err
	}
	before := time.Now().UTC().AddDate(0, 0, -retentionDays(a.config))
	for _, application := range applications {
		candidates, err := a.store.RetentionCandidates(ctx, application.ID, before, retainedImages(a.config))
		if err != nil {
			return err
		}
		ids := make([]int64, 0, len(candidates))
		for _, candidate := range candidates {
			if candidate.Status == "successful" && candidate.Image != "" {
				removeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err = a.executor.RemoveImage(removeCtx, application, candidate.Image)
				cancel()
				if err != nil {
					continue
				}
			}
			path, pathErr := a.deploymentLogPath(candidate.LogPath)
			if pathErr != nil {
				continue
			}
			if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
				continue
			}
			ids = append(ids, candidate.ID)
		}
		if err = a.store.DeleteDeployments(ctx, application.ID, ids); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) updateCleanupSchedule(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	if err := a.store.UpdateCleanupSchedule(r.Context(), r.FormValue("cleanup_schedule")); err != nil {
		http.Error(w, "invalid cleanup schedule", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/operations", http.StatusSeeOther)
}

func retentionDays(config Config) int {
	if config.RetentionDays > 0 {
		return config.RetentionDays
	}
	return 30
}

func retainedImages(config Config) int {
	if config.RetainedImages > 0 {
		return config.RetainedImages
	}
	return 3
}

func (a *App) operationLogs(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	appID, err := strconv.ParseInt(r.URL.Query().Get("application"), 10, 64)
	if err != nil || appID < 1 {
		http.Error(w, "select an application", http.StatusBadRequest)
		return
	}
	deploymentID, err := strconv.ParseInt(r.URL.Query().Get("deployment"), 10, 64)
	if err != nil || deploymentID < 1 {
		http.Error(w, "select a deployment", http.StatusBadRequest)
		return
	}
	if _, err = a.store.Deployment(r.Context(), appID, deploymentID); err != nil {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/applications/"+strconv.FormatInt(appID, 10)+"/deployments/"+strconv.FormatInt(deploymentID, 10)+"/logs", http.StatusSeeOther)
}

func (a *App) manualBackup(w http.ResponseWriter, r *http.Request) {
	if !a.requireAuthorization(w, r) {
		return
	}
	if err := a.runBackup(r.Context()); err != nil {
		log.Printf("[AUDIT] Manual database backup failed: %v", err)
		http.Error(w, "backup unavailable; unlock MiniDock and retry", http.StatusServiceUnavailable)
		return
	}
	log.Printf("[AUDIT] Manual database backup triggered from %s", r.RemoteAddr)
	http.Redirect(w, r, "/operations", http.StatusSeeOther)
}

func (a *App) backupLoop(ctx context.Context) {
	interval := a.config.BackupInterval
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	// Wait a brief moment on startup before initial backup
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}

	if err := a.runBackup(ctx); err != nil {
		log.Printf("[AUDIT] Initial database backup failed: %v", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.runBackup(ctx); err != nil {
				log.Printf("[AUDIT] Scheduled database backup failed: %v", err)
			}
		}
	}
}

func (a *App) runBackup(ctx context.Context) error {
	if initialized, err := a.store.IsInitialized(ctx); err != nil || !initialized {
		return nil
	}
	key := a.keyCopy()
	if len(key) == 0 {
		_ = a.store.UpsertAlert(ctx, "warning", "MD-P0-02: MiniDock está bloqueado; los backups automáticos cifrados están pausados hasta el desbloqueo desde la UI.", nil, nil)
		_ = a.store.SetSettingString(ctx, "backup_last_error", "MiniDock está bloqueado; el respaldo cifrado requiere ingresar la contraseña en la UI")
		return fmt.Errorf("backup key unavailable: unlock MiniDock in the UI")
	}
	defer security.Zero(key)

	data, err := a.store.Snapshot(ctx)
	if err != nil {
		_ = a.store.SetSettingString(ctx, "backup_last_error", err.Error())
		return fmt.Errorf("snapshot SQLite backup: %w", err)
	}
	defer security.Zero(data)

	timestamp := time.Now().UTC().Format("20060102-150405")
	encoded, err := backup.Seal(key, data)
	if err != nil {
		_ = a.store.SetSettingString(ctx, "backup_last_error", err.Error())
		return fmt.Errorf("encrypt backup: %w", err)
	}
	name := fmt.Sprintf("backup-%s.mdbk", timestamp)
	if a.config.BackupProvider == "s3" || a.config.BackupProvider == "minio" {
		s3Config := backup.S3Config{Endpoint: a.config.BackupS3Endpoint, Region: a.config.BackupS3Region, Bucket: a.config.BackupS3Bucket, Prefix: a.config.BackupS3Prefix, AccessKey: a.config.BackupS3AccessKey, SecretKey: a.config.BackupS3SecretKey}
		if err := s3Config.Validate(); err != nil {
			_ = a.store.SetSettingString(ctx, "backup_last_error", err.Error())
			return err
		}
		reader, writer := io.Pipe()
		upload := make(chan error, 1)
		go func() {
			upload <- backup.UploadS3(ctx, s3Config, name, reader)
		}()
		_, writeErr := writer.Write(encoded)
		closeErr := writer.CloseWithError(writeErr)
		if writeErr != nil {
			_ = a.store.SetSettingString(ctx, "backup_last_error", writeErr.Error())
			return fmt.Errorf("stream encrypted backup: %w", writeErr)
		}
		if closeErr != nil {
			_ = a.store.SetSettingString(ctx, "backup_last_error", closeErr.Error())
			return fmt.Errorf("close backup stream: %w", closeErr)
		}
		if err := <-upload; err != nil {
			_ = a.store.SetSettingString(ctx, "backup_last_error", err.Error())
			return err
		}
		log.Printf("[AUDIT] Encrypted database backup uploaded: %s", name)
		_ = a.store.SetSettingString(ctx, "backup_last_success", time.Now().UTC().Format(time.RFC3339))
		_ = a.store.SetSettingString(ctx, "backup_last_error", "")
		return nil
	}
	if a.config.BackupProvider != "" && a.config.BackupProvider != "local" {
		err := fmt.Errorf("unsupported backup provider %q", a.config.BackupProvider)
		_ = a.store.SetSettingString(ctx, "backup_last_error", err.Error())
		return err
	}
	backupDir := a.config.BackupPath
	if err := os.MkdirAll(backupDir, 0700); err != nil {
		_ = a.store.SetSettingString(ctx, "backup_last_error", err.Error())
		return fmt.Errorf("create backup directory: %w", err)
	}
	encPath := filepath.Join(backupDir, name)
	if err := os.WriteFile(encPath, encoded, 0600); err != nil {
		_ = a.store.SetSettingString(ctx, "backup_last_error", err.Error())
		return fmt.Errorf("write encrypted backup: %w", err)
	}
	log.Printf("[AUDIT] Encrypted database backup created: %s", filepath.Base(encPath))
	_ = a.store.SetSettingString(ctx, "backup_last_success", time.Now().UTC().Format(time.RFC3339))
	_ = a.store.SetSettingString(ctx, "backup_last_error", "")

	// Clean up old backups (retention)
	files, err := os.ReadDir(backupDir)
	if err != nil {
		return nil
	}
	var backups []string
	for _, f := range files {
		fname := f.Name()
		if strings.HasPrefix(fname, "backup-") && strings.HasSuffix(fname, ".mdbk") {
			backups = append(backups, filepath.Join(backupDir, fname))
		}
	}
	sort.Strings(backups)
	retention := a.config.BackupRetention
	if retention < 1 {
		retention = 7
	}
	if len(backups) > retention {
		for _, b := range backups[:len(backups)-retention] {
			_ = os.Remove(b)
			log.Printf("[AUDIT] Removed old database backup: %s", filepath.Base(b))
		}
	}
	return nil
}

type PreflightItem struct {
	Name    string
	OK      bool
	Details string
	Impact  string
	Action  string
}

type HostPreflightResult struct {
	AllOK     bool
	Docker    PreflightItem
	Caddy     PreflightItem
	Network   PreflightItem
	DiskSpace PreflightItem
}

type PreflightCheckResult struct {
	AllOK        bool
	Docker       PreflightItem
	Caddy        PreflightItem
	Network      PreflightItem
	DiskSpace    PreflightItem
	RepoAccess   PreflightItem
	DomainHealth PreflightItem
}

func (a *App) RunHostPreflightCheck(ctx context.Context) HostPreflightResult {
	res := HostPreflightResult{
		AllOK:     true,
		Docker:    PreflightItem{Name: "Estado de Docker"},
		Caddy:     PreflightItem{Name: "Estado de Caddy"},
		Network:   PreflightItem{Name: "Red de Workloads"},
		DiskSpace: PreflightItem{Name: "Espacio Libre en Disco"},
	}

	// 1. Docker check
	if err := a.runtimeReady(ctx); err != nil {
		res.Docker.OK = false
		res.Docker.Details = "Daemon o socket Docker no responde: " + err.Error()
		res.Docker.Impact = "MiniDock puede guardar proyectos, pero no construir ni iniciar releases."
		res.Docker.Action = "Inicia Docker Desktop u OrbStack y comprueba de nuevo esta pantalla."
		res.AllOK = false
	} else {
		res.Docker.OK = true
		res.Docker.Details = "Socket y runtime Docker operativos"
	}

	// 2. Caddy check
	if err := a.proxyReady(ctx); err != nil {
		if a.config.Environment == "development" {
			res.Caddy.OK = true
			res.Caddy.Details = "Proxy Caddy no requerido en desarrollo local"
		} else {
			res.Caddy.OK = false
			res.Caddy.Details = "Proxy Caddy inaccesible: " + err.Error()
			res.Caddy.Impact = "Un release nuevo no podría recibir tráfico ni completar la conmutación segura."
			res.Caddy.Action = "Inicia el servicio Caddy y verifica MINIDOCK_PROXY_URL y la red de control."
			res.AllOK = false
		}
	} else {
		res.Caddy.OK = true
		res.Caddy.Details = "API de control y proxy Caddy operativo"
	}

	// 3. Network check
	if strings.TrimSpace(a.config.DockerNetwork) == "" {
		res.Network.OK = false
		res.Network.Details = "Red Docker no configurada"
		res.Network.Impact = "Los contenedores quedarían sin la red interna aislada que exige MiniDock."
		res.Network.Action = "Define MINIDOCK_DOCKER_NETWORK y ejecuta scripts/prepare-runtime-network.sh."
		res.AllOK = false
	} else {
		res.Network.OK = true
		res.Network.Details = fmt.Sprintf("Red %s (%s) lista", a.config.DockerNetwork, a.config.DockerNetworkSubnet)
	}

	// 4. Disk space check
	usage, err := diskUsage(a.config.DatabasePath)
	if err != nil {
		res.DiskSpace.OK = false
		res.DiskSpace.Details = "Error leyendo espacio en disco: " + err.Error()
		res.DiskSpace.Impact = "MiniDock no puede garantizar espacio para el workspace, la imagen y el rollback."
		res.DiskSpace.Action = "Comprueba que MINIDOCK_DATABASE_PATH esté en un volumen accesible."
		res.AllOK = false
	} else if usage > 90.0 {
		res.DiskSpace.OK = false
		res.DiskSpace.Details = fmt.Sprintf("Alerta: Disco al %.1f%% de capacidad", usage)
		res.DiskSpace.Impact = "El build puede agotar el disco y dejar artefactos incompletos."
		res.DiskSpace.Action = "Libera espacio o usa la limpieza previsualizable de Operación + logs."
		res.AllOK = false
	} else {
		res.DiskSpace.OK = true
		res.DiskSpace.Details = fmt.Sprintf("Espacio suficiente (uso actual: %.1f%%)", usage)
	}

	return res
}

func (a *App) RunPreflightCheck(ctx context.Context, app store.Application) PreflightCheckResult {
	host := a.RunHostPreflightCheck(ctx)
	res := PreflightCheckResult{
		AllOK:        host.AllOK,
		Docker:       host.Docker,
		Caddy:        host.Caddy,
		Network:      host.Network,
		DiskSpace:    host.DiskSpace,
		RepoAccess:   PreflightItem{Name: "Acceso al Repositorio"},
		DomainHealth: PreflightItem{Name: "Dominio y Health Endpoint"},
	}

	// 5. Repository access check
	if strings.HasPrefix(app.Repository, "file://") {
		repoPath := strings.TrimPrefix(app.Repository, "file://")
		if _, err := os.Stat(repoPath); err != nil {
			res.RepoAccess.OK = false
			res.RepoAccess.Details = fmt.Sprintf("Ruta local inaccesible: %v", err)
			res.RepoAccess.Impact = "El build no podría leer el código seleccionado."
			res.RepoAccess.Action = "Vuelve a seleccionar una carpeta dentro del directorio de repositorios permitido."
			res.AllOK = false
		} else {
			res.RepoAccess.OK = true
			res.RepoAccess.Details = fmt.Sprintf("Directorio local %s accesible", repoPath)
		}
	} else if strings.TrimSpace(app.Repository) != "" {
		res.RepoAccess.OK = true
		res.RepoAccess.Details = fmt.Sprintf("Origen registrado: %s", app.Repository)
	} else {
		res.RepoAccess.OK = false
		res.RepoAccess.Details = "Sin repositorio asignado"
		res.RepoAccess.Impact = "No existe un origen del que construir el release."
		res.RepoAccess.Action = "Edita la aplicación y selecciona una carpeta o repositorio Git."
		res.AllOK = false
	}

	// 6. Domain & health endpoint check
	domain := strings.TrimSpace(app.Domain)
	health := strings.TrimSpace(app.HealthEndpoint)
	if health == "" {
		health = "/"
	}
	if domain == "" {
		res.DomainHealth.OK = false
		res.DomainHealth.Details = "Dominio no configurado"
		res.DomainHealth.Impact = "Caddy no puede crear una ruta estable para el release."
		res.DomainHealth.Action = "Asigna un dominio local nombre.localhost o un subdominio público propio."
		res.AllOK = false
	} else if !strings.HasPrefix(health, "/") {
		res.DomainHealth.OK = false
		res.DomainHealth.Details = "Health endpoint debe iniciar con '/'"
		res.DomainHealth.Impact = "MiniDock no puede verificar el candidato antes de dirigirle tráfico."
		res.DomainHealth.Action = "Corrige el health check para que sea una ruta, por ejemplo /healthz o /."
		res.AllOK = false
	} else {
		res.DomainHealth.OK = true
		res.DomainHealth.Details = fmt.Sprintf("Dominio %s con probe en %s", domain, health)
	}

	return res
}
