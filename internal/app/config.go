package app

import "os"

type Config struct {
	Address      string
	DatabasePath string
	Environment  string
}

func LoadConfig() Config {
	return Config{
		Address:      environment("MINIDOCK_ADDRESS", "127.0.0.1:8080"),
		DatabasePath: environment("MINIDOCK_DATABASE_PATH", "./data/minidock.db"),
		Environment:  environment("MINIDOCK_ENVIRONMENT", "development"),
	}
}

func environment(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
