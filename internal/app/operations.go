package app

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/julieta/minidock/internal/deploy"
	"github.com/julieta/minidock/internal/store"
)

type operationalAlert struct{ Severity, Message string }

type operationApplication struct {
	Application   store.Application
	Deployment    store.Deployment
	HasDeployment bool
	Container     deploy.ContainerStatus
	Unavailable   string
}

type operationsData struct {
	Applications                  []operationApplication
	Alerts                        []operationalAlert
	DiskUsed                      string
	RetentionDays, RetainedImages int
	CleanupSchedule               string
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
	data := operationsData{RetentionDays: retentionDays(a.config), RetainedImages: retainedImages(a.config), CleanupSchedule: settings.CleanupSchedule}
	if used, err := diskUsage(a.config.DatabasePath); err != nil {
		data.Alerts = append(data.Alerts, operationalAlert{"warning", "No se pudo consultar el espacio de disco: " + err.Error()})
	} else {
		data.DiskUsed = fmt.Sprintf("%.1f%%", used)
		if used >= float64(a.config.DiskAlertPercent) {
			data.Alerts = append(data.Alerts, operationalAlert{"critical", fmt.Sprintf("Espacio de disco al %.1f%% (umbral: %d%%).", used, a.config.DiskAlertPercent)})
		}
	}
	for _, application := range applications {
		item := operationApplication{Application: application}
		deployments, err := a.store.Deployments(r.Context(), application.ID)
		if err == nil && len(deployments) > 0 {
			item.Deployment, item.HasDeployment = deployments[0], true
			if item.Deployment.Status == "failed" {
				data.Alerts = append(data.Alerts, operationalAlert{"critical", "El último despliegue de " + application.Name + " falló."})
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		item.Container, err = a.executor.Status(ctx, application)
		cancel()
		if err != nil {
			item.Unavailable = err.Error()
			if !strings.Contains(err.Error(), "Apple Container") {
				data.Alerts = append(data.Alerts, operationalAlert{"warning", "No se pudo obtener el estado de " + application.Name + "."})
			}
		} else if item.Container.State != "running" || item.Container.Health == "unhealthy" {
			data.Alerts = append(data.Alerts, operationalAlert{"critical", "El servicio " + application.Name + " no está saludable."})
		}
		if alert := certificateAlert(application.Domain, a.config.CertificateAlertDays); alert != "" {
			data.Alerts = append(data.Alerts, operationalAlert{"warning", alert})
		}
		data.Applications = append(data.Applications, item)
	}
	a.render(w, "operations.html", data)
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
