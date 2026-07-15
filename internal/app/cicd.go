package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/julieta/minidock/internal/store"
)

type githubPush struct {
	Ref string `json:"ref"`
}

func (a *App) githubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-GitHub-Event") != "push" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if a.config.WebhookSecret == "" {
		http.Error(w, "webhook is not configured", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid payload", 400)
		return
	}
	if !validGitHubSignature(a.config.WebhookSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	application, err := a.store.Application(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if !application.DeployOnPush {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var push githubPush
	if json.Unmarshal(body, &push) != nil || !matchesGitReference(application.Branch, push.Ref) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := a.queueDeployment(r.Context(), application.ID, "deploy"); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.Error(w, "could not queue deployment", 500)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func matchesGitReference(reference, pushed string) bool {
	if strings.HasPrefix(reference, "refs/") {
		return pushed == reference
	}
	return pushed == "refs/heads/"+reference || pushed == "refs/tags/"+reference
}

func validGitHubSignature(secret string, body []byte, header string) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

func (a *App) queueDeployment(ctx context.Context, applicationID int64, action string) error {
	if err := os.MkdirAll(a.config.LogPath, 0700); err != nil {
		return err
	}
	logPath := filepath.Join(a.config.LogPath, fmt.Sprintf("deploy-%d-%d.log", applicationID, time.Now().UTC().UnixNano()))
	_, err := a.store.QueueDeployment(ctx, applicationID, action, logPath)
	return err
}

func (a *App) StartQueue(ctx context.Context) {
	go a.queueLoop(ctx)
	go a.cleanupLoop(ctx)
}

// cleanupLoop keeps scheduled cleanup intentionally conservative: it checks
// hourly and never runs more often than the selected interval. Manual cleanup
// remains available regardless of this setting.
func (a *App) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			settings, err := a.store.AutomationSettings(ctx)
			if err != nil || settings.CleanupSchedule == "manual" {
				continue
			}
			interval := 24 * time.Hour
			if settings.CleanupSchedule == "weekly" {
				interval = 7 * 24 * time.Hour
			}
			if !last.IsZero() && now.Sub(last) < interval {
				continue
			}
			if a.runRetention(ctx) == nil {
				last = now
			}
		}
	}
}

func (a *App) queueLoop(ctx context.Context) {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := a.runNextDeployment(ctx); err != nil && err != store.ErrNotFound {
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (a *App) runNextDeployment(ctx context.Context) error {
	d, err := a.store.NextQueuedDeployment(ctx)
	if err != nil {
		return err
	}
	application, err := a.store.Application(ctx, d.ApplicationID)
	if err != nil {
		_ = a.store.FinishDeployment(ctx, d.ID, "failed")
		return err
	}
	logFile, err := os.OpenFile(d.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		_ = a.store.FinishDeployment(ctx, d.ID, "failed")
		return err
	}
	defer logFile.Close()
	configuration, err := a.deploymentConfiguration(ctx, application.ID, "production")
	if err == nil && d.Action == "deploy" {
		var image string
		image, err = a.executor.Deploy(ctx, application, configuration, logFile)
		if image != "" {
			_ = a.store.SetDeploymentImage(ctx, d.ID, image)
		}
	}
	if err == nil && d.Action == "rollback" {
		var previous store.Deployment
		previous, err = a.store.PreviousSuccessfulDeployment(ctx, application.ID)
		if err == nil {
			_ = a.store.SetDeploymentImage(ctx, d.ID, previous.Image)
			err = a.executor.Rollback(ctx, application, previous.Image, configuration, logFile)
		}
	}
	if err == nil && d.Action == "auto_rollback" {
		var previous store.Deployment
		previous, err = a.store.LatestSuccessfulDeployment(ctx, application.ID)
		if err == nil {
			_ = a.store.SetDeploymentImage(ctx, d.ID, previous.Image)
			err = a.executor.Rollback(ctx, application, previous.Image, configuration, logFile)
		}
	}
	status := "successful"
	if err != nil {
		status = "failed"
		_, _ = fmt.Fprintln(logFile, "deployment error:", err)
	}
	_ = a.store.FinishDeployment(ctx, d.ID, status)
	a.notifyDeployment(ctx, application, d, status, err)
	if status == "failed" && d.Action == "deploy" && application.AutoRollback && isHealthCheckFailure(err) {
		if queueErr := a.queueDeployment(ctx, application.ID, "auto_rollback"); queueErr != nil {
			_, _ = fmt.Fprintln(logFile, "automatic rollback could not be queued:", queueErr)
		} else {
			_, _ = fmt.Fprintln(logFile, "automatic rollback queued after failed health check")
		}
	}
	return err
}

func isHealthCheckFailure(err error) bool {
	return err != nil && strings.Contains(err.Error(), "health check failed")
}

func (a *App) notifyDeployment(ctx context.Context, application store.Application, deployment store.Deployment, status string, deploymentErr error) {
	if a.config.NotificationWebhook == "" {
		return
	}
	payload := map[string]string{"application": application.Name, "action": deployment.Action, "status": status, "deployment_id": strconv.FormatInt(deployment.ID, 10)}
	if deploymentErr != nil {
		payload["error"] = deploymentErr.Error()
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.config.NotificationWebhook, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err == nil {
		response.Body.Close()
	}
}
