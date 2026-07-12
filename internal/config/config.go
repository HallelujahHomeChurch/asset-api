package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port                string
	DatabaseURL         string
	PublicBaseURL       string
	StorageBackend      string
	LocalDirectory      string
	LocalUploadBaseURL  string
	LocalSigningKey     string
	AzureAccountURL     string
	AzureContainer      string
	ServiceBusNamespace string
	ServiceBusQueue     string
	AllowedCallers      map[string]bool
	ShutdownTimeout     time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Port:                value("PORT", "8080"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		PublicBaseURL:       value("ASSET_PUBLIC_BASE_URL", "http://localhost:8080/api/assets"),
		StorageBackend:      value("ASSET_STORAGE_BACKEND", "local"),
		LocalDirectory:      value("ASSET_LOCAL_DIR", ".data/assets"),
		LocalUploadBaseURL:  value("ASSET_LOCAL_UPLOAD_BASE_URL", "http://localhost:8080/dev/uploads"),
		LocalSigningKey:     value("ASSET_LOCAL_SIGNING_KEY", "local-development-only-change-me"),
		AzureAccountURL:     os.Getenv("ASSET_AZURE_ACCOUNT_URL"),
		AzureContainer:      value("ASSET_AZURE_CONTAINER", "assets"),
		ServiceBusNamespace: os.Getenv("ASSET_SCAN_SERVICE_BUS_NAMESPACE"),
		ServiceBusQueue:     os.Getenv("ASSET_SCAN_SERVICE_BUS_QUEUE"),
		AllowedCallers:      splitSet(value("ASSET_ALLOWED_CALLERS", "hhc-web-api,hhc-line-function-bot")),
		ShutdownTimeout:     10 * time.Second,
	}
	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}
	if cfg.StorageBackend != "local" && cfg.StorageBackend != "azure" {
		return Config{}, fmt.Errorf("unsupported ASSET_STORAGE_BACKEND %q", cfg.StorageBackend)
	}
	if cfg.StorageBackend == "azure" && cfg.AzureAccountURL == "" {
		return Config{}, fmt.Errorf("ASSET_AZURE_ACCOUNT_URL is required for azure storage")
	}
	if cfg.StorageBackend == "azure" && (cfg.ServiceBusNamespace == "" || cfg.ServiceBusQueue == "") {
		return Config{}, fmt.Errorf("Defender scan Service Bus namespace and queue are required for azure storage")
	}
	if value := os.Getenv("SHUTDOWN_TIMEOUT_SECONDS"); value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds <= 0 {
			return Config{}, fmt.Errorf("invalid SHUTDOWN_TIMEOUT_SECONDS")
		}
		cfg.ShutdownTimeout = time.Duration(seconds) * time.Second
	}
	return cfg, nil
}

func value(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
func splitSet(value string) map[string]bool {
	result := map[string]bool{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = true
		}
	}
	return result
}
