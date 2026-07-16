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

	"github.com/julieta/minidock/internal/deploy"
	"github.com/julieta/minidock/internal/runtime"
	"github.com/julieta/minidock/internal/store"
)

type githubPush struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
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
	// GitHub delivery IDs make retries safe. Fingerprint the opaque identifier
	// rather than storing it verbatim in the operational database.
	if delivery := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery")); delivery != "" {
		digest := sha256.Sum256([]byte(delivery))
		claimed, err := a.store.ClaimWebhookDelivery(r.Context(), "github", hex.EncodeToString(digest[:]))
		if err != nil {
			http.Error(w, "could not record webhook", http.StatusInternalServerError)
			return
		}
		if !claimed {
			w.WriteHeader(http.StatusNoContent)
			return
		}
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
	allowed, err := a.store.AllowWebhookRequest(r.Context(), fmt.Sprintf("github:%d", application.ID), a.config.WebhookRateLimit, a.config.WebhookRateWindow)
	if err != nil {
		http.Error(w, "could not apply webhook rate limit", http.StatusInternalServerError)
		return
	}
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(a.config.WebhookRateWindow.Seconds())))
		http.Error(w, "webhook rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	a.mu.RLock()
	isLocked := len(a.key) == 0
	a.mu.RUnlock()
	if isLocked {
		hasSecrets, err := a.store.HasSecrets(r.Context(), application.ID)
		if err != nil {
			http.Error(w, "database error", http.StatusInternalServerError)
			return
		}
		if hasSecrets {
			http.Error(w, "MiniDock is locked and application contains secrets", http.StatusServiceUnavailable)
			return
		}
	}
	// A webhook cannot complete the interactive confirmation required for a
	// production deployment. Older database rows may contain both flags, so
	// enforce the invariant here as well as in the settings form.
	if application.RequireConfirmation {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var push githubPush
	if json.Unmarshal(body, &push) != nil || !matchesGitReference(application.Branch, push.Ref) || (push.After != "" && !validGitCommit(push.After)) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if push.Repository.FullName != "" && !matchesGitHubRepository(application.Repository, push.Repository.FullName) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := a.queueDeploymentRef(r.Context(), application.ID, "deploy", push.After); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		http.Error(w, "could not queue deployment", 500)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func validGitCommit(value string) bool {
	if len(value) != 40 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value != strings.Repeat("0", 40)
}

func matchesGitHubRepository(repository, fullName string) bool {
	repository = strings.TrimSuffix(strings.TrimSpace(strings.ToLower(repository)), ".git")
	fullName = strings.Trim(strings.ToLower(fullName), "/")
	if repository == "" || fullName == "" {
		return false
	}
	if strings.HasPrefix(repository, "git@github.com:") {
		return strings.TrimPrefix(repository, "git@github.com:") == fullName
	}
	if strings.HasPrefix(repository, "https://github.com/") || strings.HasPrefix(repository, "http://github.com/") {
		parts := strings.SplitN(repository, "github.com/", 2)
		return len(parts) == 2 && strings.Trim(parts[1], "/") == fullName
	}
	return false
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
	return a.queueDeploymentRef(ctx, applicationID, action, "")
}

func (a *App) queueDeploymentRef(ctx context.Context, applicationID int64, action, requestedRef string) error {
	if err := os.MkdirAll(a.config.LogPath, 0700); err != nil {
		return err
	}
	logPath := filepath.Join(a.config.LogPath, fmt.Sprintf("deploy-%d-%d.log", applicationID, time.Now().UTC().UnixNano()))
	deployment, err := a.store.QueueDeployment(ctx, applicationID, action, logPath)
	if err != nil {
		return err
	}
	application, err := a.store.Application(ctx, applicationID)
	if err != nil {
		return err
	}
	healthEndpoint := application.HealthEndpoint
	if healthEndpoint == "" {
		if t, ok := runtime.For(application.Type); ok {
			healthEndpoint = t.HealthEndpoint
		} else {
			healthEndpoint = "/healthz"
		}
	}
	manifest, err := json.Marshal(struct {
		Version          int    `json:"version"`
		ApplicationType  string `json:"application_type"`
		RequestedRuntime string `json:"requested_runtime"`
		InternalPort     int    `json:"internal_port"`
		HealthEndpoint   string `json:"health_endpoint"`
	}{Version: 1, ApplicationType: application.Type, RequestedRuntime: application.Runtime, InternalPort: application.InternalPort, HealthEndpoint: healthEndpoint})
	if err != nil {
		return err
	}
	if requestedRef == "" {
		requestedRef = application.Branch
	}
	return a.store.SetDeploymentReleaseMetadata(ctx, deployment.ID, store.ReleaseMetadata{
		RequestedRef: requestedRef,
		InternalPort: application.InternalPort,
		Manifest:     string(manifest),
	})
}

func (a *App) StartQueue(ctx context.Context) error {
	applications, err := a.store.Applications(ctx)
	if err != nil {
		return fmt.Errorf("list applications for reconciliation: %w", err)
	}
	// Candidate containers are not releases and must never survive a process
	// crash. A runtime unavailable at startup is non-fatal: individual jobs
	// will record their own normalized preflight failure when they run.
	reconciliation, reconciliationErr := a.executor.Reconcile(ctx, applications, io.Discard)
	if reconciliationErr != nil {
		_ = a.store.UpsertAlert(ctx, "critical", "MD-P0-03: reconciliation could not restore a Docker switch", nil, nil)
	}
	for _, applicationID := range reconciliation.RestoredApplicationIDs {
		id := applicationID
		_ = a.store.UpsertAlert(ctx, "warning", "MD-P0-03: previous release restored after interrupted switch", &id, nil)
	}
	if _, err := a.store.RecoverInterruptedDeployments(ctx); err != nil {
		return fmt.Errorf("recover interrupted deployments: %w", err)
	}
	workers := a.config.MaxConcurrentDeployments
	if workers < 1 {
		workers = 1
	}
	for range workers {
		go a.queueLoop(ctx)
	}
	go a.cleanupLoop(ctx)
	go a.backupLoop(ctx)
	a.StartCollector(ctx)
	return nil
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
	output := &limitedLogWriter{Writer: logFile, Limit: a.config.LogMaxBytes}
	jobCtx, stopJob := context.WithCancel(ctx)
	defer stopJob()
	heartbeatErr := a.heartbeatDeployment(jobCtx, d.ID, stopJob)
	configuration, err := a.deploymentConfiguration(ctx, application.ID, "production")
	if err == nil && d.Action == "retry" {
		failedDep, errFailed := a.store.LatestFailedDeployment(ctx, application.ID)
		if errFailed == nil && failedDep.Image != "" && (failedDep.FailureStage == "start" || failedDep.FailureStage == "health" || failedDep.FailureStage == "route") {
			configuration.ReuseImage = failedDep.Image
			configuration.ReuseSourceRevision = failedDep.SourceRevision
			configuration.ReuseSourceFingerprint = failedDep.SourceFingerprint
		}
		d.Action = "deploy"
	}

	tracker := &deploy.StageTracker{
		QueueDuration: time.Duration(d.QueueDurationMs) * time.Millisecond,
		OnStageChange: func(stage string) {
			_ = a.store.SetDeploymentStage(ctx, d.ID, stage)
		},
		OnEvent: func(stage, eventType, message string) {
			_ = a.store.AddDeploymentEvent(ctx, d.ID, stage, eventType, message)
		},
	}
	jobCtx = deploy.WithTracker(jobCtx, tracker)

	if err == nil && d.Action == "deploy" {
		// A webhook records an immutable commit SHA in requested_ref. Build that
		// exact revision rather than resolving the mutable application branch when
		// a queued job eventually reaches a worker.
		if validGitCommit(d.RequestedRef) {
			application.Branch = d.RequestedRef
		}
		var release deploy.ReleaseResult
		release, err = a.executor.Deploy(jobCtx, application, configuration, output)
		if release.Image != "" {
			_ = a.store.SetDeploymentImage(ctx, d.ID, release.Image)
		}
		healthEndpoint := application.HealthEndpoint
		if healthEndpoint == "" {
			if t, ok := runtime.For(application.Type); ok {
				healthEndpoint = t.HealthEndpoint
			} else {
				healthEndpoint = "/healthz"
			}
		}
		_ = a.store.SetDeploymentReleaseMetadata(ctx, d.ID, store.ReleaseMetadata{
			RequestedRef:        d.RequestedRef,
			SourceRevision:      release.SourceRevision,
			SourceFingerprint:   release.SourceFingerprint,
			ArtifactDigest:      release.ArtifactDigest,
			Runtime:             release.Runtime,
			InternalPort:        application.InternalPort,
			HealthEndpoint:      healthEndpoint,
			Manifest:            releaseManifest(d.Manifest, configuration.EffectiveRuntimeConfiguration, release.ObservedContainerID),
			ConfigurationDigest: configuration.ConfigurationDigest,
		})
	}
	if err == nil && d.Action == "rollback" {
		var previous store.Deployment
		previous, err = a.store.PreviousSuccessfulDeployment(ctx, application.ID)
		if err == nil {
			configuration, err = rollbackConfiguration(configuration, previous.Manifest)
		}
		if err == nil {
			err = a.executor.VerifyArtifact(jobCtx, application, previous.Image, previous.ArtifactDigest)
			if err == nil {
				_ = a.store.SetDeploymentImage(ctx, d.ID, previous.Image)
				metadata := rollbackReleaseMetadata(previous, configuration.ConfigurationDigest)
				metadata.Manifest = releaseManifest(previous.Manifest, configuration.EffectiveRuntimeConfiguration, "")
				_ = a.store.SetDeploymentReleaseMetadata(ctx, d.ID, metadata)
				var release deploy.ReleaseResult
				release, err = a.executor.Rollback(jobCtx, application, previous.Image, configuration, output)
				metadata.Manifest = releaseManifest(previous.Manifest, configuration.EffectiveRuntimeConfiguration, release.ObservedContainerID)
				_ = a.store.SetDeploymentReleaseMetadata(ctx, d.ID, metadata)
			}
		}
	}
	if err == nil && d.Action == "auto_rollback" {
		var previous store.Deployment
		previous, err = a.store.LatestSuccessfulDeployment(ctx, application.ID)
		if err == nil {
			configuration, err = rollbackConfiguration(configuration, previous.Manifest)
		}
		if err == nil {
			err = a.executor.VerifyArtifact(jobCtx, application, previous.Image, previous.ArtifactDigest)
			if err == nil {
				_ = a.store.SetDeploymentImage(ctx, d.ID, previous.Image)
				metadata := rollbackReleaseMetadata(previous, configuration.ConfigurationDigest)
				metadata.Manifest = releaseManifest(previous.Manifest, configuration.EffectiveRuntimeConfiguration, "")
				_ = a.store.SetDeploymentReleaseMetadata(ctx, d.ID, metadata)
				var release deploy.ReleaseResult
				release, err = a.executor.Rollback(jobCtx, application, previous.Image, configuration, output)
				metadata.Manifest = releaseManifest(previous.Manifest, configuration.EffectiveRuntimeConfiguration, release.ObservedContainerID)
				_ = a.store.SetDeploymentReleaseMetadata(ctx, d.ID, metadata)
			}
		}
	}
	stopJob()
	if heartbeatFailure := <-heartbeatErr; err == nil && heartbeatFailure != nil {
		err = fmt.Errorf("deployment lease lost: %w", heartbeatFailure)
	}
	cancelled, cancellationErr := a.store.DeploymentCancellationRequested(ctx, d.ID)
	if cancellationErr != nil && cancellationErr != store.ErrNotFound {
		_, _ = fmt.Fprintln(output, "could not read deployment cancellation state:", cancellationErr)
	}
	status := "successful"
	if cancelled {
		status = "cancelled"
		_, _ = fmt.Fprintln(output, "deployment cancelled by operator")
	} else if err != nil {
		status = "failed"
		_, _ = fmt.Fprintln(output, "deployment error:", err)
		stage, code, detail := deploy.FailureDetails(err)
		if d.Action == "rollback" || d.Action == "auto_rollback" {
			stage = "rollback"
			if code == "" || code == "runtime_error" {
				code = "rollback_failed"
			}
		}
		_ = a.store.SetDeploymentFailure(ctx, d.ID, stage, code, detail)
	}

	_ = a.store.SetDeploymentStageDurations(ctx, d.ID,
		tracker.QueueDuration.Milliseconds(),
		tracker.SourceDuration.Milliseconds(),
		tracker.BuildDuration.Milliseconds(),
		tracker.StartDuration.Milliseconds(),
		tracker.HealthDuration.Milliseconds(),
		tracker.RouteDuration.Milliseconds(),
	)

	_ = a.store.FinishDeployment(ctx, d.ID, status)
	a.notifyDeployment(ctx, application, d, status, err)
	if status == "failed" && d.Action == "deploy" && application.AutoRollback && shouldTriggerAutoRollback(err) {
		if queueErr := a.queueDeployment(ctx, application.ID, "auto_rollback"); queueErr != nil {
			_, _ = fmt.Fprintln(output, "automatic rollback could not be queued:", queueErr)
		} else {
			_, _ = fmt.Fprintln(output, "automatic rollback queued after failed runtime health/routing")
		}
	}
	return err
}

// releaseManifest augments the queued, non-secret application contract with
// the exact public runtime configuration requested and the container identity
// observed after health and routing completed. Keeping this evidence in the
// existing versioned manifest makes it available to recovery without relying
// on mutable Docker names or deployment logs.
func releaseManifest(manifest, effectiveRuntimeConfiguration, observedContainerID string) string {
	var value map[string]json.RawMessage
	if json.Unmarshal([]byte(manifest), &value) != nil || value == nil {
		value = map[string]json.RawMessage{"version": json.RawMessage("1")}
	}
	if effectiveRuntimeConfiguration != "" {
		value["effective_runtime_configuration"] = json.RawMessage(effectiveRuntimeConfiguration)
	}
	if observedContainerID != "" {
		encoded, _ := json.Marshal(observedContainerID)
		value["observed_container_id"] = encoded
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return manifest
	}
	return string(encoded)
}

// rollbackConfiguration restores the recorded public runtime configuration of
// the target release while retaining currently unlocked secret values. Secrets
// are intentionally not persisted in release evidence; a locked MiniDock will
// therefore fail before it can silently recreate a different release.
func rollbackConfiguration(configuration deploy.DeploymentConfiguration, manifest string) (deploy.DeploymentConfiguration, error) {
	var value struct {
		EffectiveRuntimeConfiguration map[string]string `json:"effective_runtime_configuration"`
	}
	if err := json.Unmarshal([]byte(manifest), &value); err != nil {
		return configuration, fmt.Errorf("read rollback release manifest: %w", err)
	}
	if value.EffectiveRuntimeConfiguration == nil {
		return configuration, fmt.Errorf("rollback release has no effective runtime configuration")
	}
	for name := range configuration.PublicRuntime {
		delete(configuration.Runtime, name)
	}
	configuration.PublicRuntime = make(map[string]string, len(value.EffectiveRuntimeConfiguration))
	for name, value := range value.EffectiveRuntimeConfiguration {
		configuration.PublicRuntime[name] = value
		configuration.Runtime[name] = value
	}
	configuration.EffectiveRuntimeConfiguration = deploy.NonSecretRuntimeConfiguration(configuration.PublicRuntime)
	configuration.ConfigurationDigest = deploy.PublicConfigurationDigest(configuration.PublicRuntime, configuration.BuildArgs)
	return configuration, nil
}

// limitedLogWriter retains the beginning of a deployment log up to the host
// policy limit, then discards further output without making the runtime CLI
// fail because of a short write.
type limitedLogWriter struct {
	Writer  io.Writer
	Limit   int64
	written int64
}

func (w *limitedLogWriter) Write(p []byte) (int, error) {
	if w.Limit <= 0 || w.written >= w.Limit {
		return len(p), nil
	}
	available := w.Limit - w.written
	toWrite := p
	if int64(len(toWrite)) > available {
		toWrite = toWrite[:available]
	}
	n, err := w.Writer.Write(toWrite)
	w.written += int64(n)
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// heartbeatDeployment keeps the durable job lease alive while a runtime
// command is executing. If the store can no longer confirm ownership, the
// job context is cancelled so a worker cannot keep mutating a release after
// its state became unknown.
func (a *App) heartbeatDeployment(ctx context.Context, deploymentID int64, cancel context.CancelFunc) <-chan error {
	result := make(chan error, 1)
	go func() {
		defer close(result)
		ticker := time.NewTicker(store.DeploymentLeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.store.HeartbeatDeployment(ctx, deploymentID); err != nil {
					result <- err
					cancel()
					return
				}
			}
		}
	}()
	return result
}

func shouldTriggerAutoRollback(err error) bool {
	if err == nil {
		return false
	}
	stage, _, _ := deploy.FailureDetails(err)
	return stage == "start" || stage == "health" || stage == "route"
}

// rollbackReleaseMetadata carries forward the immutable identity of the image
// being restored. The configuration digest is recomputed for this operation:
// a rollback starts an old artifact with the configuration active now, rather
// than silently claiming it used the configuration from its original release.
func rollbackReleaseMetadata(source store.Deployment, configurationDigest string) store.ReleaseMetadata {
	return store.ReleaseMetadata{
		RequestedRef:        source.RequestedRef,
		SourceRevision:      source.SourceRevision,
		SourceFingerprint:   source.SourceFingerprint,
		ArtifactDigest:      source.ArtifactDigest,
		Runtime:             source.Runtime,
		InternalPort:        source.InternalPort,
		HealthEndpoint:      source.HealthEndpoint,
		Manifest:            source.Manifest,
		ConfigurationDigest: configurationDigest,
	}
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
