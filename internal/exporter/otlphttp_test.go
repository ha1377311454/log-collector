package exporter

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	collectorlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
	"log-collector/internal/config"
	"log-collector/internal/logging"
	"log-collector/internal/model"
)

func TestSendOTLPProtobuf(t *testing.T) {
	var got collectorlogsv1.ExportLogsServiceRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/logs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if err := proto.Unmarshal(b, &got); err != nil {
			t.Errorf("unmarshal: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg := config.ExportConfig{Endpoint: server.URL, Timeout: config.Duration{Duration: time.Second}, BatchSize: 1, QueueSize: 1}
	e, err := New(cfg, logging.Nop())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	resource := map[string]string{"service.name": "demo"}
	if err := e.send(context.Background(), []model.Record{{Body: "hello", Timestamp: now, ObservedTimestamp: now, SeverityText: "ERROR", SeverityNumber: 17, ResourceAttributes: resource}, {Body: "world", Timestamp: now, ObservedTimestamp: now, ResourceAttributes: resource}}); err != nil {
		t.Fatal(err)
	}
	if len(got.ResourceLogs) != 1 || len(got.ResourceLogs[0].ScopeLogs[0].LogRecords) != 2 || got.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Body.GetStringValue() != "hello" {
		t.Fatalf("unexpected request: %v", &got)
	}
	first := got.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if first.SeverityText != "ERROR" || first.SeverityNumber != 17 {
		t.Fatalf("unexpected severity: text=%q number=%d", first.SeverityText, first.SeverityNumber)
	}
}

func TestQueueByteLimitHonorsContextCancellation(t *testing.T) {
	cfg := config.ExportConfig{Endpoint: "http://127.0.0.1:4318", Timeout: config.Duration{Duration: time.Second}, BatchSize: 1, MaxBatchBytes: 1024, QueueSize: 10, MaxQueueBytes: 256}
	e, err := New(cfg, logging.Nop())
	if err != nil {
		t.Fatal(err)
	}
	record := model.Record{Body: "first"}
	if err := e.Enqueue(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := e.Enqueue(ctx, record); err == nil {
		t.Fatal("second record must wait for queue bytes and honor context cancellation")
	}
}

func TestPermanentHTTPErrorIsNotRetried(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, "bad token", http.StatusUnauthorized)
	}))
	defer server.Close()
	cfg := config.ExportConfig{Endpoint: server.URL, Timeout: config.Duration{Duration: time.Second}, Retry: config.RetryConfig{Enabled: true, Initial: config.Duration{Duration: time.Millisecond}, MaxElapsed: config.Duration{Duration: time.Second}}}
	e, err := New(cfg, logging.Nop())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	err = e.sendWithRetry(context.Background(), []model.Record{{Body: "hello", Timestamp: now, ObservedTimestamp: now}})
	if err == nil || requests != 1 {
		t.Fatalf("err=%v requests=%d", err, requests)
	}
}
