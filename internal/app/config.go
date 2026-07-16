package app

import (
	"os"
	"strconv"
	"time"
)

const Version = "1.0.0"

type Config struct {
	Address                  string
	DatabasePath             string
	Environment              string
	WorkspacePath            string
	LogPath                  string
	DockerNetwork            string
	DockerNetworkSubnet      string
	Runtime                  string
	WebhookSecret            string
	WebhookRateLimit         int
	WebhookRateWindow        time.Duration
	NotificationWebhook      string
	DiskAlertPercent         int
	CertificateAlertDays     int
	RetentionDays            int
	RetainedImages           int
	LocalRepositoriesPath    string
	GitHubAppID              string
	GitHubAppPrivateKeyPath  string
	GitHubAPIURL             string
	SourceTimeout            time.Duration
	BuildTimeout             time.Duration
	StartTimeout             time.Duration
	HealthTimeout            time.Duration
	WorkspaceMaxBytes        int64
	LogMaxBytes              int64
	MaxConcurrentDeployments int
	AdminDomain              string
	ProxyURL                 string
	CaddyAdminURL            string
	ProxyExpectedStatus      int
	ProxyExpectedContent     string
	SLOProbeWindow           time.Duration
	BackupPath               string
	BackupInterval           time.Duration
	BackupRetention          int
	BackupProvider           string
	BackupS3Endpoint         string
	BackupS3Region           string
	BackupS3Bucket           string
	BackupS3Prefix           string
	BackupS3AccessKey        string
	BackupS3SecretKey        string
	SLOBuildDurationMinutes  int
	AgentAddress             string
	AgentTLSCertificatePath  string
	AgentTLSPrivateKeyPath   string
	AgentTLSClientCAPath     string
	CloudflaredImage         string
	CloudflaredNetwork       string
}

func LoadConfig() Config {
	return Config{
		Address:                  environment("MINIDOCK_ADDRESS", "127.0.0.1:8080"),
		DatabasePath:             environment("MINIDOCK_DATABASE_PATH", "./data/minidock.db"),
		Environment:              environment("MINIDOCK_ENVIRONMENT", "development"),
		WorkspacePath:            environment("MINIDOCK_WORKSPACE_PATH", "./data/apps"),
		LogPath:                  environment("MINIDOCK_LOG_PATH", "./data/logs"),
		DockerNetwork:            environment("MINIDOCK_DOCKER_NETWORK", "minidock"),
		DockerNetworkSubnet:      environment("MINIDOCK_DOCKER_NETWORK_SUBNET", "172.31.251.0/24"),
		Runtime:                  environment("MINIDOCK_RUNTIME", "auto"),
		WebhookSecret:            environment("MINIDOCK_GITHUB_WEBHOOK_SECRET", ""),
		WebhookRateLimit:         environmentInt("MINIDOCK_GITHUB_WEBHOOK_RATE_LIMIT", 30),
		WebhookRateWindow:        environmentDuration("MINIDOCK_GITHUB_WEBHOOK_RATE_WINDOW", time.Minute),
		NotificationWebhook:      environment("MINIDOCK_NOTIFICATION_WEBHOOK", ""),
		DiskAlertPercent:         environmentInt("MINIDOCK_DISK_ALERT_PERCENT", 85),
		CertificateAlertDays:     environmentInt("MINIDOCK_CERTIFICATE_ALERT_DAYS", 21),
		RetentionDays:            environmentInt("MINIDOCK_RETENTION_DAYS", 30),
		RetainedImages:           environmentInt("MINIDOCK_RETAINED_IMAGES", 3),
		LocalRepositoriesPath:    environment("MINIDOCK_LOCAL_REPOSITORIES_PATH", userRepositoriesPath()),
		GitHubAppID:              environment("MINIDOCK_GITHUB_APP_ID", ""),
		GitHubAppPrivateKeyPath:  environment("MINIDOCK_GITHUB_APP_PRIVATE_KEY_PATH", ""),
		GitHubAPIURL:             environment("MINIDOCK_GITHUB_API_URL", "https://api.github.com"),
		SourceTimeout:            environmentDuration("MINIDOCK_SOURCE_TIMEOUT", 5*time.Minute),
		BuildTimeout:             environmentDuration("MINIDOCK_BUILD_TIMEOUT", 20*time.Minute),
		StartTimeout:             environmentDuration("MINIDOCK_START_TIMEOUT", 2*time.Minute),
		HealthTimeout:            environmentDuration("MINIDOCK_HEALTH_TIMEOUT", 30*time.Second),
		WorkspaceMaxBytes:        environmentInt64("MINIDOCK_WORKSPACE_MAX_BYTES", 2<<30),
		LogMaxBytes:              environmentInt64("MINIDOCK_LOG_MAX_BYTES", 20<<20),
		MaxConcurrentDeployments: environmentInt("MINIDOCK_MAX_CONCURRENT_DEPLOYMENTS", 1),
		AdminDomain:              environment("MINIDOCK_ADMIN_DOMAIN", environment("MINIDOCK_DOMAIN", "localhost")),
		ProxyURL:                 environment("MINIDOCK_PROXY_URL", "http://localhost"),
		CaddyAdminURL:            environment("MINIDOCK_CADDY_ADMIN_URL", "http://127.0.0.1:2019"),
		ProxyExpectedStatus:      environmentInt("MINIDOCK_PROXY_EXPECTED_STATUS", 200),
		ProxyExpectedContent:     environment("MINIDOCK_PROXY_EXPECTED_CONTENT", ""),
		SLOProbeWindow:           environmentDuration("MINIDOCK_SLO_PROBE_WINDOW", 24*time.Hour),
		BackupPath:               environment("MINIDOCK_BACKUP_PATH", "./data/backups"),
		BackupInterval:           environmentDuration("MINIDOCK_BACKUP_INTERVAL", 24*time.Hour),
		BackupRetention:          environmentInt("MINIDOCK_BACKUP_RETENTION", 7),
		BackupProvider:           environment("MINIDOCK_BACKUP_PROVIDER", "local"),
		BackupS3Endpoint:         environment("MINIDOCK_BACKUP_S3_ENDPOINT", ""),
		BackupS3Region:           environment("MINIDOCK_BACKUP_S3_REGION", "us-east-1"),
		BackupS3Bucket:           environment("MINIDOCK_BACKUP_S3_BUCKET", ""),
		BackupS3Prefix:           environment("MINIDOCK_BACKUP_S3_PREFIX", "minidock"),
		BackupS3AccessKey:        environment("MINIDOCK_BACKUP_S3_ACCESS_KEY", ""),
		BackupS3SecretKey:        environment("MINIDOCK_BACKUP_S3_SECRET_KEY", ""),
		SLOBuildDurationMinutes:  environmentInt("MINIDOCK_SLO_BUILD_DURATION_MINUTES", 10),
		AgentAddress:             environment("MINIDOCK_AGENT_ADDRESS", ""),
		AgentTLSCertificatePath:  environment("MINIDOCK_AGENT_TLS_CERTIFICATE_PATH", ""),
		AgentTLSPrivateKeyPath:   environment("MINIDOCK_AGENT_TLS_PRIVATE_KEY_PATH", ""),
		AgentTLSClientCAPath:     environment("MINIDOCK_AGENT_TLS_CLIENT_CA_PATH", ""),
		CloudflaredImage:         environment("MINIDOCK_CLOUDFLARED_IMAGE", "cloudflare/cloudflared:2026.7.2"),
		CloudflaredNetwork:       environment("MINIDOCK_CLOUDFLARED_NETWORK", "minidock-edge"),
	}
}

func environmentDuration(key string, fallback time.Duration) time.Duration {
	value, err := time.ParseDuration(os.Getenv(key))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func environmentInt64(key string, fallback int64) int64 {
	value, err := strconv.ParseInt(os.Getenv(key), 10, 64)
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func userRepositoriesPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "./repositories"
	}
	return home
}

func environmentInt(key string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(key))
	if err != nil || value < 1 {
		return fallback
	}
	return value
}

func environment(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
