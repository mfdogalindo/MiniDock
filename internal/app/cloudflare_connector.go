package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	cloudflaredContainerName  = "minidock-cloudflared"
	cloudflaredManagedLabel   = "io.minidock.managed"
	cloudflaredComponentLabel = "io.minidock.component"
	cloudflaredTokenHashLabel = "io.minidock.token-sha256"
)

type CloudflaredStatus struct {
	State  string
	Detail string
}

func (s CloudflaredStatus) Running() bool { return s.State == "running" }

func (s CloudflaredStatus) Display() string {
	switch s.State {
	case "running":
		return "En ejecución"
	case "missing":
		return "Pendiente de iniciar"
	case "":
		return "Estado desconocido"
	default:
		return "Detenido (" + s.State + ")"
	}
}

type CloudflaredConnector interface {
	Status(context.Context) (CloudflaredStatus, error)
	Reconcile(context.Context, string) error
	Remove(context.Context) error
}

type dockerCommandRunner interface {
	Run(context.Context, map[string]string, ...string) ([]byte, error)
}

type execDockerCommandRunner struct{}

func (execDockerCommandRunner) Run(ctx context.Context, environment map[string]string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	command.Env = os.Environ()
	for key, value := range environment {
		command.Env = append(command.Env, key+"="+value)
	}
	return command.CombinedOutput()
}

type DockerCloudflaredConnector struct {
	Image   string
	Network string
	runner  dockerCommandRunner
}

func NewDockerCloudflaredConnector(image, network string) *DockerCloudflaredConnector {
	if strings.TrimSpace(image) == "" {
		image = "cloudflare/cloudflared:2026.7.2"
	}
	if strings.TrimSpace(network) == "" {
		network = "minidock-edge"
	}
	return &DockerCloudflaredConnector{Image: image, Network: network, runner: execDockerCommandRunner{}}
}

func (c *DockerCloudflaredConnector) command(ctx context.Context, environment map[string]string, args ...string) ([]byte, error) {
	if c.runner == nil {
		c.runner = execDockerCommandRunner{}
	}
	return c.runner.Run(ctx, environment, args...)
}

func (c *DockerCloudflaredConnector) inspect(ctx context.Context) (exists, managed bool, state, tokenHash, image string, err error) {
	format := `{{.State.Status}}|{{index .Config.Labels "` + cloudflaredManagedLabel + `"}}|{{index .Config.Labels "` + cloudflaredComponentLabel + `"}}|{{index .Config.Labels "` + cloudflaredTokenHashLabel + `"}}|{{.Config.Image}}`
	output, inspectErr := c.command(ctx, nil, "container", "inspect", "--format", format, cloudflaredContainerName)
	if inspectErr != nil {
		detail := strings.ToLower(strings.TrimSpace(string(output)) + " " + inspectErr.Error())
		if strings.Contains(detail, "no such container") || strings.Contains(detail, "not found") {
			return false, false, "", "", "", nil
		}
		return false, false, "", "", "", fmt.Errorf("no se pudo inspeccionar el conector: %s", safeDockerError(output, inspectErr))
	}
	parts := strings.Split(strings.TrimSpace(string(output)), "|")
	if len(parts) != 5 {
		return true, false, "", "", "", fmt.Errorf("respuesta inesperada de Docker al inspeccionar el conector")
	}
	managed = parts[1] == "true" && parts[2] == "cloudflared"
	return true, managed, parts[0], parts[3], parts[4], nil
}

func (c *DockerCloudflaredConnector) Status(parent context.Context) (CloudflaredStatus, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	if _, err := c.command(ctx, nil, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return CloudflaredStatus{}, fmt.Errorf("Docker no está disponible para administrar el conector")
	}
	exists, managed, state, _, _, err := c.inspect(ctx)
	if err != nil {
		return CloudflaredStatus{}, err
	}
	if !exists {
		return CloudflaredStatus{State: "missing"}, nil
	}
	if !managed {
		return CloudflaredStatus{}, fmt.Errorf("el nombre %s está ocupado por un contenedor ajeno a MiniDock", cloudflaredContainerName)
	}
	return CloudflaredStatus{State: state}, nil
}

func cloudflaredTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (c *DockerCloudflaredConnector) Reconcile(parent context.Context, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("Cloudflare no entregó un Tunnel Token")
	}
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()

	if _, err := c.command(ctx, nil, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("Docker no está disponible para iniciar cloudflared")
	}
	if err := c.removeLegacyComposeConnector(ctx); err != nil {
		return err
	}
	exists, managed, state, currentHash, currentImage, err := c.inspect(ctx)
	if err != nil {
		return err
	}
	wantedHash := cloudflaredTokenHash(token)
	if exists && !managed {
		return fmt.Errorf("el nombre %s está ocupado por un contenedor ajeno a MiniDock", cloudflaredContainerName)
	}
	if exists && currentHash == wantedHash && currentImage == c.Image {
		if state == "running" {
			return nil
		}
		if output, startErr := c.command(ctx, nil, "container", "start", cloudflaredContainerName); startErr != nil {
			return fmt.Errorf("no se pudo reiniciar el conector: %s", safeDockerError(output, startErr))
		}
		return nil
	}

	if _, imageErr := c.command(ctx, nil, "image", "inspect", c.Image); imageErr != nil {
		if output, pullErr := c.command(ctx, nil, "pull", c.Image); pullErr != nil {
			return fmt.Errorf("no se pudo descargar la imagen oficial de cloudflared: %s", safeDockerError(output, pullErr))
		}
	}
	if exists {
		if output, removeErr := c.command(ctx, nil, "container", "rm", "--force", cloudflaredContainerName); removeErr != nil {
			return fmt.Errorf("no se pudo actualizar el conector: %s", safeDockerError(output, removeErr))
		}
	}

	args := []string{
		"container", "create",
		"--name", cloudflaredContainerName,
		"--label", cloudflaredManagedLabel + "=true",
		"--label", cloudflaredComponentLabel + "=cloudflared",
		"--label", cloudflaredTokenHashLabel + "=" + wantedHash,
		"--restart", "unless-stopped",
		"--network", c.Network,
		"--memory", "256m",
		"--cpus", "0.50",
		"--pids-limit", "128",
		"--log-opt", "max-size=10m",
		"--log-opt", "max-file=3",
		"--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--env", "TUNNEL_TOKEN",
		c.Image,
		"tunnel", "--no-autoupdate", "run",
	}
	// The token is inherited by the short-lived Docker CLI instead of being put
	// in its argv. Docker still stores it in the connector configuration, which
	// is only accessible through the already privileged, ACL-limited API.
	if output, createErr := c.command(ctx, map[string]string{"TUNNEL_TOKEN": token}, args...); createErr != nil {
		return fmt.Errorf("no se pudo crear el conector: %s", safeDockerError(output, createErr, token))
	}
	if output, startErr := c.command(ctx, nil, "container", "start", cloudflaredContainerName); startErr != nil {
		_, _ = c.command(ctx, nil, "container", "rm", "--force", cloudflaredContainerName)
		return fmt.Errorf("no se pudo iniciar el conector: %s", safeDockerError(output, startErr))
	}
	return nil
}

// removeLegacyComposeConnector migrates only a cloudflared service belonging
// to the same Compose project as MiniDock. It never searches by a broad image
// or service name across unrelated projects.
func (c *DockerCloudflaredConnector) removeLegacyComposeConnector(ctx context.Context) error {
	self, err := os.Hostname()
	if err != nil || strings.TrimSpace(self) == "" {
		return nil
	}
	projectOutput, err := c.command(ctx, nil, "container", "inspect", "--format", `{{index .Config.Labels "com.docker.compose.project"}}`, self)
	if err != nil {
		return nil
	}
	project := strings.TrimSpace(string(projectOutput))
	if project == "" || strings.ContainsAny(project, " \t\r\n") {
		return nil
	}
	output, err := c.command(ctx, nil, "container", "ls", "--all", "--quiet",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service=cloudflared")
	if err != nil {
		return fmt.Errorf("no se pudo comprobar el conector anterior de Compose: %s", safeDockerError(output, err))
	}
	for _, containerID := range strings.Fields(string(output)) {
		if removeOutput, removeErr := c.command(ctx, nil, "container", "rm", "--force", containerID); removeErr != nil {
			return fmt.Errorf("no se pudo migrar el conector anterior: %s", safeDockerError(removeOutput, removeErr))
		}
	}
	return nil
}

func (c *DockerCloudflaredConnector) Remove(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	exists, managed, _, _, _, err := c.inspect(ctx)
	if err != nil || !exists {
		return err
	}
	if !managed {
		return fmt.Errorf("MiniDock no eliminará un contenedor que no administra")
	}
	if output, removeErr := c.command(ctx, nil, "container", "rm", "--force", cloudflaredContainerName); removeErr != nil {
		return fmt.Errorf("no se pudo detener el conector: %s", safeDockerError(output, removeErr))
	}
	return nil
}

func safeDockerError(output []byte, err error, secrets ...string) string {
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return err.Error()
	}
	for _, secret := range secrets {
		if secret != "" {
			detail = strings.ReplaceAll(detail, secret, "[REDACTADO]")
		}
	}
	if len(detail) > 400 {
		detail = detail[:400]
	}
	return detail
}
