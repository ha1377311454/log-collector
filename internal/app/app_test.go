package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"log-collector/internal/config"
	"log-collector/internal/logging"
	"log-collector/internal/model"
)

func TestSubmitRecordIgnoresWebhookFailureWithoutExporter(t *testing.T) {
	webhookErr := errors.New("WeChat webhook rejected message: errcode=45009")
	sendWebhook := func(context.Context, model.Record) error { return webhookErr }

	if err := submitRecord(context.Background(), model.Record{}, logging.Nop(), sendWebhook, nil); err != nil {
		t.Fatalf("submitRecord() error = %v, want nil", err)
	}
}

func TestSubmitRecordContinuesToExporterAfterWebhookFailure(t *testing.T) {
	webhookErr := errors.New("WeChat webhook rejected message: errcode=45009")
	sendWebhook := func(context.Context, model.Record) error { return webhookErr }
	exported := false
	sendExport := func(context.Context, model.Record) error {
		exported = true
		return nil
	}

	if err := submitRecord(context.Background(), model.Record{}, logging.Nop(), sendWebhook, sendExport); err != nil {
		t.Fatalf("submitRecord() error = %v, want nil", err)
	}
	if !exported {
		t.Fatal("exporter was not called after webhook failure")
	}
}

func TestSubmitRecordIgnoresExporterFailure(t *testing.T) {
	exportErr := errors.New("export queue stopped")
	sendExport := func(context.Context, model.Record) error { return exportErr }

	if err := submitRecord(context.Background(), model.Record{}, logging.Nop(), nil, sendExport); err != nil {
		t.Fatalf("submitRecord() error = %v, want nil", err)
	}
}

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
