package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port               string
	DatabaseURL        string
	PublicBaseURL      string
	StorageBackend     string
	LocalDirectory     string
	LocalUploadBaseURL string
	LocalSigningKey    string
	AzureAccountURL    string
	AzureContainer     string
	ClamAVHost         string
	ClamAVPort         int
	ClamAVTimeout      time.Duration
	ClamAVMaxFileSize  int64
	ClamAVMaxRetries   int
	AllowedCallers     map[string]bool
	ShutdownTimeout    time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		Port:               value("PORT", "8080"),
		DatabaseURL:        os.Getenv("DATABASE_URL"),
		PublicBaseURL:      value("ASSET_PUBLIC_BASE_URL", "http://localhost:8080/api/assets"),
		StorageBackend:     value("ASSET_STORAGE_BACKEND", "local"),
		LocalDirectory:     value("ASSET_LOCAL_DIR", ".data/assets"),
		LocalUploadBaseURL: value("ASSET_LOCAL_UPLOAD_BASE_URL", "http://localhost:8080/dev/uploads"),
		LocalSigningKey:    value("ASSET_LOCAL_SIGNING_KEY", "local-development-only-change-me"),
		AzureAccountURL:    os.Getenv("ASSET_AZURE_ACCOUNT_URL"),
		AzureContainer:     value("ASSET_AZURE_CONTAINER", "assets"),
		ClamAVHost:         value("CLAMAV_HOST", "127.0.0.1"),
		ClamAVPort:         3310,
		ClamAVTimeout:      2 * time.Minute,
		ClamAVMaxFileSize:  25 << 20,
		ClamAVMaxRetries:   5,
		AllowedCallers:     splitSet(value("ASSET_ALLOWED_CALLERS", "hhc-web-api,hhc-line-function-bot")),
		ShutdownTimeout:    10 * time.Second,
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
	if err := positiveInt("CLAMAV_PORT", &cfg.ClamAVPort); err != nil {
		return Config{}, err
	}
	if err := positiveInt("CLAMAV_MAX_RETRIES", &cfg.ClamAVMaxRetries); err != nil {
		return Config{}, err
	}
	if err := positiveInt64("CLAMAV_MAX_FILE_SIZE_BYTES", &cfg.ClamAVMaxFileSize); err != nil {
		return Config{}, err
	}
	if value := os.Getenv("CLAMAV_TIMEOUT_SECONDS"); value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds <= 0 {
			return Config{}, fmt.Errorf("invalid CLAMAV_TIMEOUT_SECONDS")
		}
		cfg.ClamAVTimeout = time.Duration(seconds) * time.Second
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

func positiveInt(key string, destination *int) error {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("invalid %s", key)
	}
	*destination = parsed
	return nil
}

func positiveInt64(key string, destination *int64) error {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fmt.Errorf("invalid %s", key)
	}
	*destination = parsed
	return nil
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
