package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("export:\n  endpoint: http://localhost:4318\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Export.BatchSize != 500 {
		t.Fatalf("batch size = %d", cfg.Export.BatchSize)
	}
	if cfg.Performance.ReadInterval.Duration.String() != "1s" {
		t.Fatalf("read interval = %s", cfg.Performance.ReadInterval.Duration)
	}
}

func TestRejectsBothMultilineModes(t *testing.T) {
	cfg := defaults()
	cfg.Export.Endpoint = "http://localhost:4318"
	cfg.Sources.Files = []FileRule{{Name: "x", Include: []string{"*.log"}, Multiline: MultilineConfig{StartPattern: "x", ContinuationPattern: "y"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}
