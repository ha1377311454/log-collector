package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"log-collector/internal/config"
)

func TestLoggerWritesJSONFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.log")
	logger, err := New(config.LogConfig{Level: "info", Format: "json", File: path, MaxSizeMB: 1, MaxBackups: 1, MaxAgeDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	logger.Info("started", "source", "test")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"msg":"started"`) || !strings.Contains(string(b), `"source":"test"`) {
		t.Fatalf("unexpected log: %s", b)
	}
}

func TestLoggerSetLevelTakesEffectWithoutRebuild(t *testing.T) {
	path := filepath.Join(t.TempDir(), "collector.log")
	logger, err := New(config.LogConfig{Level: "info", Format: "json", File: path, MaxSizeMB: 1})
	if err != nil {
		t.Fatal(err)
	}
	logger.Debug("before reload")
	if err := logger.SetLevel("debug"); err != nil {
		t.Fatal(err)
	}
	logger.Debug("after reload")
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "before reload") || !strings.Contains(string(b), "after reload") {
		t.Fatalf("unexpected log after level reload: %s", b)
	}
}
