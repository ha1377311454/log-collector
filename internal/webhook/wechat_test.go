package webhook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"log-collector/internal/config"
	"log-collector/internal/model"
)

func TestSendErrorLogAsWeChatMarkdownMessage(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request: method=%s content-type=%s", r.Method, r.Header.Get("Content-Type"))
		}
		var message markdownMessage
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Errorf("decode request: %v", err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if message.MsgType != "markdown" || message.Markdown.Content == "" {
			t.Fatalf("unexpected message: %+v", message)
		}
		for _, expected := range []string{"### 🚨 API 日志告警", "**主机：** app-host", "**日志级别：**", "**日志来源：** application", "**日志文件：** /var/log/application.log", "**请求 ID：**", "request-a", "**日志内容**", "query failed", "\\`metric.name\\`", "\n> <font color=\"warning\">\tat com.example.QueryService.execute"} {
			if !strings.Contains(message.Markdown.Content, expected) {
				t.Errorf("message content does not contain %q: %s", expected, message.Markdown.Content)
			}
		}
		if strings.Contains(message.Markdown.Content, "<br>") || strings.Contains(message.Markdown.Content, "&#39;") {
			t.Errorf("message contains unwanted HTML formatting: %s", message.Markdown.Content)
		}
		t.Logf("WeChat markdown alert:\n%s", message.Markdown.Content)
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	client, err := New(config.WebhookConfig{URL: server.URL, Title: "API 日志告警", Timeout: config.Duration{Duration: time.Second}, MaxContentLength: 4000})
	if err != nil {
		t.Fatal(err)
	}
	record := model.Record{
		Body:               "ERROR - request-a - query failed: SELECT `metric.name` WHERE value < 1\n\tat com.example.QueryService.execute(QueryService.java:72)",
		SeverityText:       "ERROR",
		Attributes:         map[string]string{"request.id": "request-a"},
		ResourceAttributes: map[string]string{"host.name": "app-host", "log.source.rule": "application", "log.file.path": "/var/log/application.log"},
	}
	if err := client.Send(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestSendIgnoresNonErrorLog(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	client, err := New(config.WebhookConfig{URL: server.URL, Timeout: config.Duration{Duration: time.Second}, MaxContentLength: 4000})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Send(context.Background(), model.Record{SeverityText: "INFO", Body: "completed"}); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want 0", requests)
	}
}
