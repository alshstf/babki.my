package config_test

import (
	"testing"

	"babki.my/babki/internal/platform/config"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, ":8080")
	}
	if cfg.LogLevel != "info" || cfg.LogFormat != "json" {
		t.Errorf("log defaults = %q/%q, want info/json", cfg.LogLevel, cfg.LogFormat)
	}
	if !cfg.AutoMigrate {
		t.Error("AutoMigrate default = false, want true")
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("BABKI_HTTP_ADDR", ":9090")
	t.Setenv("BABKI_DATABASE_URL", "postgres://u:p@localhost:5432/babki")
	t.Setenv("BABKI_AUTO_MIGRATE", "false")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("HTTPAddr = %q, want :9090", cfg.HTTPAddr)
	}
	if cfg.DatabaseURL != "postgres://u:p@localhost:5432/babki" {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.AutoMigrate {
		t.Error("AutoMigrate = true, want false")
	}
}
