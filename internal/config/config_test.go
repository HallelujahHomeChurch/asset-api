package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadEnablesDevelopmentCallerHeaderExplicitly(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AllowDevCallerHeader {
		t.Fatal("development caller header was not enabled")
	}
	if !cfg.AllowedCallers["account-api"] {
		t.Fatal("account-api is missing from the default caller allowlist")
	}
}

func TestLoadUsesDatabasePoolDefaults(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "true")
	t.Setenv("DB_MAX_OPEN_CONNS", "")
	t.Setenv("DB_MAX_IDLE_CONNS", "")
	t.Setenv("DB_CONN_MAX_LIFETIME", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBMaxOpenConns != 10 {
		t.Fatalf("DBMaxOpenConns = %d, want 10", cfg.DBMaxOpenConns)
	}
	if cfg.DBMaxIdleConns != 5 {
		t.Fatalf("DBMaxIdleConns = %d, want 5", cfg.DBMaxIdleConns)
	}
	if cfg.DBConnMaxLifetime != 30*time.Minute {
		t.Fatalf("DBConnMaxLifetime = %s, want 30m", cfg.DBConnMaxLifetime)
	}
}

func TestLoadReadsOptionalScanQueueURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "true")
	t.Setenv("ASSET_SCAN_QUEUE_URL", "https://storage.queue.core.windows.net/asset-scan")
	t.Setenv("ASSET_SCAN_DISPATCH_ENABLED", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ScanQueueURL != "https://storage.queue.core.windows.net/asset-scan" || !cfg.ScanDispatchEnabled {
		t.Fatalf("ScanQueueURL=%q", cfg.ScanQueueURL)
	}
}

func TestLoadRequiresQueueURLWhenScanDispatchIsEnabled(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "true")
	t.Setenv("ASSET_SCAN_DISPATCH_ENABLED", "true")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "ASSET_SCAN_QUEUE_URL") {
		t.Fatalf("Load() error=%v", err)
	}
}

func TestLoadCanDisableEmbeddedScannerForQueueCutover(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "true")
	t.Setenv("ASSET_EMBEDDED_SCAN_ENABLED", "false")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmbeddedScanEnabled {
		t.Fatal("embedded scanner remained enabled")
	}
}

func TestLoadReadsDatabasePoolSettings(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "true")
	t.Setenv("DB_MAX_OPEN_CONNS", "12")
	t.Setenv("DB_MAX_IDLE_CONNS", "6")
	t.Setenv("DB_CONN_MAX_LIFETIME", "45m")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBMaxOpenConns != 12 || cfg.DBMaxIdleConns != 6 || cfg.DBConnMaxLifetime != 45*time.Minute {
		t.Fatalf("unexpected database pool config: %+v", cfg)
	}
}

func TestLoadRejectsInvalidDatabasePoolSettings(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{name: "nonpositive open", key: "DB_MAX_OPEN_CONNS", value: "0", want: "DB_MAX_OPEN_CONNS"},
		{name: "nonpositive idle", key: "DB_MAX_IDLE_CONNS", value: "-1", want: "DB_MAX_IDLE_CONNS"},
		{name: "nonpositive lifetime", key: "DB_CONN_MAX_LIFETIME", value: "0s", want: "DB_CONN_MAX_LIFETIME"},
		{name: "idle exceeds open", key: "DB_MAX_IDLE_CONNS", value: "11", want: "DB_MAX_IDLE_CONNS cannot exceed DB_MAX_OPEN_CONNS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("DATABASE_URL", "postgres://test")
			t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "true")
			t.Setenv(tt.key, tt.value)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load() error = %v, want error containing %q", err, tt.want)
			}
		})
	}
}

func TestLoadRequiresAppAPITokenOutsideDevelopment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://test")
	t.Setenv("ASSET_ALLOW_DEV_CALLER_HEADER", "false")
	t.Setenv("APP_API_TOKEN", "")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "APP_API_TOKEN") {
		t.Fatalf("Load() error = %v, want APP_API_TOKEN error", err)
	}

	t.Setenv("APP_API_TOKEN", "secret")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}
