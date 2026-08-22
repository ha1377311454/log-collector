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
	if err := e.send(context.Background(), []model.Record{{Body: "hello", Timestamp: now, ObservedTimestamp: now, ResourceAttributes: resource}, {Body: "world", Timestamp: now, ObservedTimestamp: now, ResourceAttributes: resource}}); err != nil {
		t.Fatal(err)
	}
	if len(got.ResourceLogs) != 1 || len(got.ResourceLogs[0].ScopeLogs[0].LogRecords) != 2 || got.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Body.GetStringValue() != "hello" {
		t.Fatalf("unexpected request: %v", &got)
	}
}
