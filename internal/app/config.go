package app

import (
	"os"
	"strconv"
)

type Config struct {
	Address                 string
	DatabasePath            string
	Environment             string
	WorkspacePath           string
	LogPath                 string
	DockerNetwork           string
	Runtime                 string
	WebhookSecret           string
	NotificationWebhook     string
	DiskAlertPercent        int
	CertificateAlertDays    int
	RetentionDays           int
	RetainedImages          int
	LocalRepositoriesPath   string
	GitHubAppID             string
	GitHubAppPrivateKeyPath string
	GitHubAPIURL            string
}

func LoadConfig() Config {
	return Config{
		Address:                 environment("MINIDOCK_ADDRESS", "127.0.0.1:8080"),
		DatabasePath:            environment("MINIDOCK_DATABASE_PATH", "./data/minidock.db"),
		Environment:             environment("MINIDOCK_ENVIRONMENT", "development"),
		WorkspacePath:           environment("MINIDOCK_WORKSPACE_PATH", "./data/apps"),
		LogPath:                 environment("MINIDOCK_LOG_PATH", "./data/logs"),
		DockerNetwork:           environment("MINIDOCK_DOCKER_NETWORK", "minidock"),
		Runtime:                 environment("MINIDOCK_RUNTIME", "auto"),
		WebhookSecret:           environment("MINIDOCK_GITHUB_WEBHOOK_SECRET", ""),
		NotificationWebhook:     environment("MINIDOCK_NOTIFICATION_WEBHOOK", ""),
		DiskAlertPercent:        environmentInt("MINIDOCK_DISK_ALERT_PERCENT", 85),
		CertificateAlertDays:    environmentInt("MINIDOCK_CERTIFICATE_ALERT_DAYS", 21),
		RetentionDays:           environmentInt("MINIDOCK_RETENTION_DAYS", 30),
		RetainedImages:          environmentInt("MINIDOCK_RETAINED_IMAGES", 3),
		LocalRepositoriesPath:   environment("MINIDOCK_LOCAL_REPOSITORIES_PATH", userRepositoriesPath()),
		GitHubAppID:             environment("MINIDOCK_GITHUB_APP_ID", ""),
		GitHubAppPrivateKeyPath: environment("MINIDOCK_GITHUB_APP_PRIVATE_KEY_PATH", ""),
		GitHubAPIURL:            environment("MINIDOCK_GITHUB_API_URL", "https://api.github.com"),
	}
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
