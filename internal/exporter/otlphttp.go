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
	"sync"
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

	// channel 限制记录条数，这组字段额外限制排队记录的总字节数。
	queueMu     sync.Mutex
	queuedBytes int64
	queueNotify chan struct{}
}

type httpStatusError struct {
	status     int
	retryAfter time.Duration
	message    string
}

func (e *httpStatusError) Error() string { return e.message }

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
	return &Exporter{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout.Duration}, log: logger, input: make(chan model.Record, cfg.QueueSize), queueNotify: make(chan struct{})}, nil
}

// Enqueue 将日志写入有界内存队列；队列满时施加反压，而不是静默丢弃。
func (e *Exporter) Enqueue(ctx context.Context, record model.Record) error {
	size := recordSize(record)
	if size > e.cfg.MaxQueueBytes {
		return fmt.Errorf("log record estimated size %d exceeds export.max_queue_bytes %d", size, e.cfg.MaxQueueBytes)
	}
	if err := e.reserveQueueBytes(ctx, size); err != nil {
		return err
	}
	select {
	case e.input <- record:
		return nil
	case <-ctx.Done():
		e.releaseQueueBytes(size)
		return ctx.Err()
	}
}

// Run 在达到 batch_size 或 flush_interval 时发送批次。
// 收到退出信号后会先排空当前内存队列，再执行最后一次发送。
func (e *Exporter) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.cfg.FlushInterval.Duration)
	defer ticker.Stop()
	batch := make([]model.Record, 0, e.cfg.BatchSize)
	var batchBytes int64
	flush := func(sendCtx context.Context) error {
		if len(batch) == 0 {
			return nil
		}
		if err := e.sendWithRetry(sendCtx, batch); err != nil {
			return err
		}
		batch = batch[:0]
		batchBytes = 0
		return nil
	}
	appendRecord := func(sendCtx context.Context, record model.Record) error {
		size := recordSize(record)
		if len(batch) > 0 && (len(batch) >= e.cfg.BatchSize || batchBytes+size > e.cfg.MaxBatchBytes) {
			if err := flush(sendCtx); err != nil {
				return err
			}
		}
		batch = append(batch, record)
		batchBytes += size
		return nil
	}
	for {
		select {
		case record := <-e.input:
			e.releaseQueueBytes(recordSize(record))
			if err := appendRecord(ctx, record); err != nil {
				return err
			}
			if len(batch) >= e.cfg.BatchSize || batchBytes >= e.cfg.MaxBatchBytes {
				if err := flush(ctx); err != nil {
					return err
				}
			}
		case <-ticker.C:
			if err := flush(ctx); err != nil {
				return err
			}
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), e.cfg.Timeout.Duration)
			defer cancel()
			draining := true
			for draining {
				select {
				case record := <-e.input:
					e.releaseQueueBytes(recordSize(record))
					if err := appendRecord(shutdownCtx, record); err != nil {
						return err
					}
				default:
					draining = false
				}
			}
			return flush(shutdownCtx)
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
		if statusErr, ok := err.(*httpStatusError); ok {
			if statusErr.status != http.StatusRequestTimeout && statusErr.status != http.StatusTooManyRequests && statusErr.status < 500 {
				return err
			}
			if statusErr.retryAfter > delay {
				delay = statusErr.retryAfter
			}
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
	reqData := BuildRequest(records)
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
		_, _ = io.Copy(io.Discard, resp.Body)
		return &httpStatusError{status: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")), message: fmt.Sprintf("OTLP HTTP status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))}
	}
	// 读取到 EOF 后再关闭，确保 Go HTTP Transport 可以复用连接。
	responseData, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return err
	}
	if len(responseData) > 0 {
		var response collectorlogsv1.ExportLogsServiceResponse
		if err := proto.Unmarshal(responseData, &response); err != nil {
			return fmt.Errorf("decode OTLP response: %w", err)
		}
		if partial := response.PartialSuccess; partial != nil && partial.RejectedLogRecords > 0 {
			return fmt.Errorf("OTLP partial success rejected %d records: %s", partial.RejectedLogRecords, partial.ErrorMessage)
		}
	}
	return nil
}

// BuildRequest 将内部日志记录转换成 Exporter 实际发送的 OTLP Logs 请求。
func BuildRequest(records []model.Record) *collectorlogsv1.ExportLogsServiceRequest {
	reqData := &collectorlogsv1.ExportLogsServiceRequest{}
	type group struct {
		attrs   map[string]string
		records []*logsv1.LogRecord
	}
	groups := make(map[string]*group)
	for _, r := range records {
		lr := &logsv1.LogRecord{TimeUnixNano: uint64(r.Timestamp.UnixNano()), ObservedTimeUnixNano: uint64(r.ObservedTimestamp.UnixNano()), SeverityText: r.SeverityText, SeverityNumber: logsv1.SeverityNumber(r.SeverityNumber), Body: &commonv1.AnyValue{Value: &commonv1.AnyValue_StringValue{StringValue: r.Body}}, Attributes: keyValues(r.Attributes), TraceId: r.TraceID}
		key := r.ResourceKey
		if key == "" {
			key = attributeKey(r.ResourceAttributes)
		}
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
	return reqData
}

func (e *Exporter) reserveQueueBytes(ctx context.Context, size int64) error {
	for {
		e.queueMu.Lock()
		if e.queuedBytes+size <= e.cfg.MaxQueueBytes {
			e.queuedBytes += size
			e.queueMu.Unlock()
			return nil
		}
		notify := e.queueNotify
		e.queueMu.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (e *Exporter) releaseQueueBytes(size int64) {
	e.queueMu.Lock()
	e.queuedBytes -= size
	close(e.queueNotify)
	e.queueNotify = make(chan struct{})
	e.queueMu.Unlock()
}

// recordSize 是内存保护用的保守估算，包含正文、属性和值以及固定结构开销。
func recordSize(record model.Record) int64 {
	size := int64(len(record.Body) + len(record.TraceID) + 128)
	for k, v := range record.Attributes {
		size += int64(len(k) + len(v) + 32)
	}
	for k, v := range record.ResourceAttributes {
		size += int64(len(k) + len(v) + 32)
	}
	return size
}

func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil {
		return seconds
	}
	if when, err := http.ParseTime(value); err == nil {
		if delay := time.Until(when); delay > 0 {
			return delay
		}
	}
	return 0
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
