package app

import (
	"reflect"
	"testing"

	"log-collector/internal/config"
)

func TestRestartRequiredChangesIgnoresDynamicConfiguration(t *testing.T) {
	active := config.Config{Log: config.LogConfig{Level: "info"}, Webhook: config.WebhookConfig{Title: "old"}}
	next := active
	next.Log.Level = "debug"
	next.Webhook.Title = "new"
	next.Webhook.IgnoreKeywords = []string{"ignored"}
	if changes := restartRequiredChanges(active, next); len(changes) != 0 {
		t.Fatalf("unexpected restart changes: %v", changes)
	}
}

func TestRestartRequiredChangesReportsStaticSections(t *testing.T) {
	active := config.Config{}
	next := active
	next.Export.Enabled = true
	next.Log.Format = "console"
	want := []string{"log except level", "export"}
	if changes := restartRequiredChanges(active, next); !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes = %v, want %v", changes, want)
	}
}
