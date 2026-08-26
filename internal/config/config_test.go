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

func TestRejectsInvalidMultilineStartPattern(t *testing.T) {
	cfg := defaults()
	cfg.Export.Endpoint = "http://localhost:4318"
	cfg.Sources.Files = []FileRule{{Name: "x", Include: []string{"*.log"}, Multiline: MultilineConfig{StartPattern: "["}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLoadRejectsUnknownYAMLField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := []byte("export:\n  endpoint: http://127.0.0.1:4318\n  batch_szie: 100\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("unknown YAML field must be rejected")
	}
}

func TestRejectsAttributeExtractorWithoutCaptureGroup(t *testing.T) {
	cfg := defaults()
	cfg.Export.Endpoint = "http://localhost:4318"
	cfg.Sources.Files = []FileRule{{Name: "x", Include: []string{"*.log"}, Extractors: []AttributeExtractorConfig{{Key: "request.id", Pattern: `request_id=\S+`}}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRejectsTraceIDExtractorWithoutCaptureGroup(t *testing.T) {
	cfg := defaults()
	cfg.Export.Endpoint = "http://localhost:4318"
	cfg.Sources.Files = []FileRule{{Name: "x", Include: []string{"*.log"}, TraceID: TraceIDExtractorConfig{Pattern: `trace_id=[0-9a-f]+`}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRejectsUnknownDropLevel(t *testing.T) {
	cfg := defaults()
	cfg.Export.Endpoint = "http://localhost:4318"
	cfg.Sources.Files = []FileRule{{Name: "x", Include: []string{"*.log"}, DropLevels: []string{"verbose"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestAllowsWebhookOnlyOutput(t *testing.T) {
	cfg := defaults()
	cfg.Export.Enabled = false
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "https://example.com/webhook"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsAllOutputsDisabled(t *testing.T) {
	cfg := defaults()
	cfg.Export.Enabled = false
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRejectsEmptyWebhookIgnoreKeyword(t *testing.T) {
	cfg := defaults()
	cfg.Export.Endpoint = "http://localhost:4318"
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "https://example.com/webhook"
	cfg.Webhook.IgnoreKeywords = []string{" "}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestRejectsEmptyWebhookErrorTypeKeyword(t *testing.T) {
	cfg := defaults()
	cfg.Export.Endpoint = "http://localhost:4318"
	cfg.Webhook.Enabled = true
	cfg.Webhook.URL = "https://example.com/webhook"
	cfg.Webhook.ErrorTypeKeywords = []string{""}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}
