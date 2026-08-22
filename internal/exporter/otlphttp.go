package exporter

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	collectorlogsv1 "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonv1 "go.opentelemetry.io/proto/otlp/common/v1"
	logsv1 "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcev1 "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"

	"log-collector/internal/config"
	"log-collector/internal/logging"
	"log-collector/internal/model"
)

// Exporter 将内部 Record 批量编码成 OTLP Logs protobuf，并通过 HTTP 上报。
type Exporter struct {
	cfg    config.ExportConfig
	client *http.Client
	log    *logging.Logger
	input  chan model.Record
}

// New 校验 endpoint；未指定路径时自动补充标准 OTLP/HTTP 路径 /v1/logs。
func New(cfg config.ExportConfig, logger *logging.Logger) (*Exporter, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid OTLP endpoint %q", cfg.Endpoint)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/logs"
		cfg.Endpoint = u.String()
	}
	return &Exporter{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout.Duration}, log: logger, input: make(chan model.Record, cfg.QueueSize)}, nil
}

// Enqueue 将日志写入有界内存队列；队列满时施加反压，而不是静默丢弃。
func (e *Exporter) Enqueue(ctx context.Context, record model.Record) error {
	select {
	case e.input <- record:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run 在达到 batch_size 或 flush_interval 时发送批次。
// 收到退出信号后会先排空当前内存队列，再执行最后一次发送。
func (e *Exporter) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.cfg.FlushInterval.Duration)
	defer ticker.Stop()
	batch := make([]model.Record, 0, e.cfg.BatchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := e.sendWithRetry(ctx, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for {
		select {
		case record := <-e.input:
			batch = append(batch, record)
			if len(batch) >= e.cfg.BatchSize {
				if err := flush(); err != nil {
					return err
				}
			}
		case <-ticker.C:
			if err := flush(); err != nil {
				return err
			}
		case <-ctx.Done():
			draining := true
			for draining {
				select {
				case record := <-e.input:
					batch = append(batch, record)
				default:
					draining = false
				}
			}
			shutdownCtx, cancel := context.WithTimeout(context.Background(), e.cfg.Timeout.Duration)
			defer cancel()
			if len(batch) > 0 {
				return e.sendWithRetry(shutdownCtx, batch)
			}
			return nil
		}
	}
}

func (e *Exporter) sendWithRetry(ctx context.Context, records []model.Record) error {
	// 一个批次在成功前不会从调用栈释放；失败时按 1、2、4... 秒指数退避。
	start := time.Now()
	delay := e.cfg.Retry.Initial.Duration
	if delay <= 0 {
		delay = time.Second
	}
	for attempt := 0; ; attempt++ {
		err := e.send(ctx, records)
		if err == nil {
			return nil
		}
		if !e.cfg.Retry.Enabled || (e.cfg.Retry.MaxElapsed.Duration > 0 && time.Since(start)+delay > e.cfg.Retry.MaxElapsed.Duration) {
			return err
		}
		e.log.Warn("OTLP export failed; retrying", "attempt", attempt+1, "delay", delay, "error", err)
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
		max := e.cfg.Retry.MaxInterval.Duration
		if max <= 0 {
			max = 30 * time.Second
		}
		delay = time.Duration(math.Min(float64(max), float64(delay*2)))
	}
}

func (e *Exporter) send(ctx context.Context, records []model.Record) error {
	reqData := &collectorlogsv1.ExportLogsServiceRequest{}
	type group struct {
		attrs   map[string]string
		records []*logsv1.LogRecord
	}
	groups := make(map[string]*group)
	for _, r := range records {
		lr := &logsv1.LogRecord{TimeUnixNano: uint64(r.Timestamp.UnixNano()), ObservedTimeUnixNano: uint64(r.ObservedTimestamp.UnixNano()), Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: r.Body}}, Attributes: keyValues(r.Attributes)}
		key := attributeKey(r.ResourceAttributes)
		g := groups[key]
		if g == nil {
			g = &group{attrs: r.ResourceAttributes}
			groups[key] = g
		}
		g.records = append(g.records, lr)
	}
	for _, g := range groups {
		reqData.ResourceLogs = append(reqData.ResourceLogs, &logsv1.ResourceLogs{Resource: &resourcev1.Resource{Attributes: keyValues(g.attrs)}, ScopeLogs: []*logsv1.ScopeLogs{{Scope: &commonv1.InstrumentationScope{Name: "log-collector"}, LogRecords: g.records}}})
	}
	b, err := proto.Marshal(reqData)
	if err != nil {
		return err
	}
	var body io.Reader = bytes.NewReader(b)
	compressed := false
	if strings.EqualFold(e.cfg.Compression, "gzip") {
		// OTLP/HTTP 使用标准 gzip Content-Encoding，不改变 protobuf 数据模型。
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(b); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		body = &buf
		compressed = true
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.Endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-protobuf")
	if compressed {
		req.Header.Set("Content-Encoding", "gzip")
	}
	for k, v := range e.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// 限制错误响应读取长度，避免异常网关返回大响应导致额外内存压力。
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("OTLP HTTP status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	// 读取到 EOF 后再关闭，确保 Go HTTP Transport 可以复用连接。
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func keyValues(values map[string]string) []*commonv1.KeyValue {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]*commonv1.KeyValue, 0, len(values))
	for _, k := range keys {
		v := values[k]
		out = append(out, &commonv1.KeyValue{Key: k, Value: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: v}}})
	}
	return out
}

func attributeKey(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(values[k])
		b.WriteByte(0)
	}
	return b.String()
}
