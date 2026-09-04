package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 是日志采集器的顶层配置，包含自身日志、位点、数据源和 OTLP 输出配置。
type Config struct {
	Log         LogConfig         `yaml:"log"`
	State       StateConfig       `yaml:"state"`
	Sources     SourcesConfig     `yaml:"sources"`
	Export      ExportConfig      `yaml:"export"`
	Webhook     WebhookConfig     `yaml:"wechat_webhook"`
	Performance PerformanceConfig `yaml:"performance"`
	FlowControl FlowControlConfig `yaml:"flow_control"`
}

// FlowControlConfig 控制进入 Exporter 队列前的全局日志速率。
type FlowControlConfig struct {
	Enabled        bool     `yaml:"enabled"`
	RatePerSecond  float64  `yaml:"rate_per_second"`
	Burst          int      `yaml:"burst"`
	Mode           string   `yaml:"mode"`
	ReportInterval Duration `yaml:"report_interval"`
}

// LogConfig 控制采集器自身运行日志，不影响被采集的业务日志。
type LogConfig struct {
	Level      string `yaml:"level"`
	Format     string `yaml:"format"`
	File       string `yaml:"file"`
	AlsoStdout bool   `yaml:"also_stdout"`
	MaxSizeMB  int    `yaml:"max_size_mb"`
	MaxBackups int    `yaml:"max_backups"`
	MaxAgeDays int    `yaml:"max_age_days"`
	Compress   bool   `yaml:"compress"`
}

// StateConfig 定义日志读取位点的持久化位置。
type StateConfig struct {
	Path string `yaml:"path"`
}

// PerformanceConfig 控制发现、读取、持久化频率以及单轮资源上限。
type PerformanceConfig struct {
	DiscoveryInterval     Duration `yaml:"discovery_interval"`
	ReadInterval          Duration `yaml:"read_interval"`
	PositionFlushInterval Duration `yaml:"position_flush_interval"`
	WorkerCount           int      `yaml:"worker_count"`
	MaxReadBytesPerFile   int64    `yaml:"max_read_bytes_per_file"`
	MaxLogSize            int      `yaml:"max_log_size"`
	MaxMultilineSize      int      `yaml:"max_multiline_size"`
	MaxMultilineLines     int      `yaml:"max_multiline_lines"`
	CacheTTL              Duration `yaml:"cache_ttl"`
}

// SourcesConfig 将日志源分为进程发现、自定义文件和容器标准输出三类。
type SourcesConfig struct {
	Processes  []ProcessRule `yaml:"processes"`
	Files      []FileRule    `yaml:"files"`
	Containers []FileRule    `yaml:"containers"`
}

// ProcessRule 定义“通过 gopsutil 匹配进程，再从进程打开文件中发现日志”的规则。
// CommRegex 与 CmdlineRegex 同时配置时采用 AND 关系。
type ProcessRule struct {
	Name         string                     `yaml:"name"`
	CommRegex    string                     `yaml:"comm_regex"`
	CmdlineRegex string                     `yaml:"cmdline_regex"`
	IncludeRegex string                     `yaml:"include_regex"`
	ExcludeRegex string                     `yaml:"exclude_regex"`
	MaxFiles     int                        `yaml:"max_files"`
	StartAt      string                     `yaml:"start_at"`
	Multiline    MultilineConfig            `yaml:"multiline"`
	Attributes   map[string]string          `yaml:"attributes"`
	Extractors   []AttributeExtractorConfig `yaml:"attribute_extractors"`
	TraceID      TraceIDExtractorConfig     `yaml:"trace_id_extractor"`
	DropLevels   []string                   `yaml:"drop_levels"`
}

// FileRule 定义基于文件 glob 的日志源，同时用于普通文件和容器标准输出。
type FileRule struct {
	Name       string                     `yaml:"name"`
	Include    []string                   `yaml:"include"`
	Exclude    []string                   `yaml:"exclude"`
	StartAt    string                     `yaml:"start_at"`
	Format     string                     `yaml:"format"`
	Multiline  MultilineConfig            `yaml:"multiline"`
	Attributes map[string]string          `yaml:"attributes"`
	Extractors []AttributeExtractorConfig `yaml:"attribute_extractors"`
	TraceID    TraceIDExtractorConfig     `yaml:"trace_id_extractor"`
	DropLevels []string                   `yaml:"drop_levels"`
}

// AttributeExtractorConfig 使用正则表达式的第一个捕获组生成日志记录属性。
type AttributeExtractorConfig struct {
	Key     string `yaml:"key"`
	Pattern string `yaml:"pattern"`
}

// TraceIDExtractorConfig 使用正则表达式的第一个捕获组生成 OTLP LogRecord.TraceId。
type TraceIDExtractorConfig struct {
	Pattern    string `yaml:"pattern"`
	Completion bool   `yaml:"completion"`
}

// MultilineConfig 定义多行合并规则；匹配 StartPattern 的行开始一条新日志，其他行续接上一条。
type MultilineConfig struct {
	StartPattern string   `yaml:"start_pattern"`
	FlushAfter   Duration `yaml:"flush_after"`
}

// ExportConfig 定义 OTLP/HTTP 批量上报、内存队列及重试参数。
type ExportConfig struct {
	Enabled       bool              `yaml:"enabled"`
	Endpoint      string            `yaml:"endpoint"`
	Headers       map[string]string `yaml:"headers"`
	Compression   string            `yaml:"compression"`
	Timeout       Duration          `yaml:"timeout"`
	BatchSize     int               `yaml:"batch_size"`
	MaxBatchBytes int64             `yaml:"max_batch_bytes"`
	FlushInterval Duration          `yaml:"flush_interval"`
	QueueSize     int               `yaml:"queue_size"`
	MaxQueueBytes int64             `yaml:"max_queue_bytes"`
	Retry         RetryConfig       `yaml:"retry"`
}

// WebhookConfig 定义企业微信群机器人错误日志推送配置。
type WebhookConfig struct {
	Enabled           bool     `yaml:"enabled"`
	URL               string   `yaml:"url"`
	Title             string   `yaml:"title"`
	Environment       string   `yaml:"environment"`
	SeverityLevels    []string `yaml:"severity_levels"`
	IgnoreKeywords    []string `yaml:"ignore_keywords"`
	ErrorTypeKeywords []string `yaml:"error_type_keywords"`
	Timeout           Duration `yaml:"timeout"`
	MaxContentLength  int      `yaml:"max_content_length"`
}

// RetryConfig 定义指数退避策略；MaxElapsed 为 0 表示不限制总重试时间。
type RetryConfig struct {
	Enabled     bool     `yaml:"enabled"`
	Initial     Duration `yaml:"initial_interval"`
	MaxInterval Duration `yaml:"max_interval"`
	MaxElapsed  Duration `yaml:"max_elapsed_time"`
}

// Duration 包装 time.Duration，使 YAML 可以直接使用 200ms、3s、5m 等写法。
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	v, err := time.ParseDuration(value.Value)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Load 先装载默认值，再用 YAML 覆盖，最后统一校验配置。
func Load(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := defaults()
	decoder := yaml.NewDecoder(bytes.NewReader(b))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func defaults() Config {
	return Config{
		Log: LogConfig{Level: "info", Format: "json", File: "./logs/log-collector.log", AlsoStdout: true, MaxSizeMB: 100, MaxBackups: 10, MaxAgeDays: 7, Compress: true}, State: StateConfig{Path: "./data/positions.json"},
		Performance: PerformanceConfig{DiscoveryInterval: Duration{5 * time.Second}, ReadInterval: Duration{time.Second}, PositionFlushInterval: Duration{15 * time.Second}, WorkerCount: 4, MaxReadBytesPerFile: 4 * 1024 * 1024, MaxLogSize: 1024 * 1024, MaxMultilineSize: 4 * 1024 * 1024, MaxMultilineLines: 1000, CacheTTL: Duration{24 * time.Hour}},
		FlowControl: FlowControlConfig{RatePerSecond: 1000, Burst: 2000, Mode: "block", ReportInterval: Duration{10 * time.Second}},
		Export: ExportConfig{Enabled: true, Timeout: Duration{10 * time.Second}, BatchSize: 500, MaxBatchBytes: 4 * 1024 * 1024, FlushInterval: Duration{time.Second}, QueueSize: 10000, MaxQueueBytes: 64 * 1024 * 1024,
			Retry: RetryConfig{Enabled: true, Initial: Duration{time.Second}, MaxInterval: Duration{30 * time.Second}, MaxElapsed: Duration{5 * time.Minute}}},
		Webhook: WebhookConfig{Title: "日志异常告警", SeverityLevels: []string{"ERROR", "FATAL"}, Timeout: Duration{5 * time.Second}, MaxContentLength: 4000},
	}
}

// Validate 在程序启动前拒绝缺失字段、非法枚举和错误的正则表达式。
func (c Config) Validate() error {
	if c.Log.Level != "debug" && c.Log.Level != "info" && c.Log.Level != "warn" && c.Log.Level != "error" {
		return errors.New("log.level must be debug, info, warn or error")
	}
	if c.Log.Format != "json" && c.Log.Format != "console" {
		return errors.New("log.format must be json or console")
	}
	if c.Log.File == "" && !c.Log.AlsoStdout {
		return errors.New("log.file and log.also_stdout cannot both be disabled")
	}
	if c.Log.MaxSizeMB <= 0 || c.Log.MaxBackups < 0 || c.Log.MaxAgeDays < 0 {
		return errors.New("log rotation values are invalid")
	}
	if !c.Export.Enabled && !c.Webhook.Enabled {
		return errors.New("export and wechat_webhook cannot both be disabled")
	}
	if c.State.Path == "" {
		return errors.New("state.path is required")
	}
	if c.Export.Enabled {
		if c.Export.Endpoint == "" {
			return errors.New("export.endpoint is required when export is enabled")
		}
		if c.Export.Compression != "" && c.Export.Compression != "gzip" {
			return errors.New("export.compression must be empty or gzip")
		}
		if c.Export.BatchSize <= 0 || c.Export.QueueSize <= 0 || c.Export.MaxBatchBytes <= 0 || c.Export.MaxQueueBytes <= 0 {
			return errors.New("export batch_size, max_batch_bytes, queue_size and max_queue_bytes must be positive")
		}
		if c.Export.MaxQueueBytes < c.Export.MaxBatchBytes {
			return errors.New("export.max_queue_bytes must be greater than or equal to max_batch_bytes")
		}
		if c.Export.FlushInterval.Duration <= 0 || c.Export.Timeout.Duration <= 0 {
			return errors.New("export flush_interval and timeout must be positive")
		}
	}
	if c.Webhook.Enabled {
		if c.Webhook.URL == "" {
			return errors.New("wechat_webhook.url is required when webhook is enabled")
		}
		if c.Webhook.Timeout.Duration <= 0 || c.Webhook.MaxContentLength <= 0 {
			return errors.New("wechat_webhook timeout and max_content_length must be positive")
		}
		if len(c.Webhook.SeverityLevels) == 0 {
			return errors.New("wechat_webhook.severity_levels must not be empty")
		}
		if err := validateSeverityLevels("wechat_webhook.severity_levels", c.Webhook.SeverityLevels); err != nil {
			return err
		}
		if err := validateKeywordList("wechat_webhook.ignore_keywords", c.Webhook.IgnoreKeywords); err != nil {
			return err
		}
		if err := validateKeywordList("wechat_webhook.error_type_keywords", c.Webhook.ErrorTypeKeywords); err != nil {
			return err
		}
	}
	if c.Performance.DiscoveryInterval.Duration <= 0 || c.Performance.ReadInterval.Duration <= 0 || c.Performance.PositionFlushInterval.Duration <= 0 {
		return errors.New("performance intervals must be positive")
	}
	if c.Performance.WorkerCount <= 0 || c.Performance.MaxReadBytesPerFile <= 0 || c.Performance.MaxLogSize <= 0 || c.Performance.MaxMultilineSize <= 0 || c.Performance.MaxMultilineLines <= 0 || c.Performance.CacheTTL.Duration <= 0 {
		return errors.New("performance limits must be positive")
	}
	if c.Performance.WorkerCount > 1024 {
		return errors.New("performance.worker_count must not exceed 1024")
	}
	if c.FlowControl.Enabled {
		if c.FlowControl.RatePerSecond <= 0 || c.FlowControl.Burst <= 0 || c.FlowControl.ReportInterval.Duration <= 0 {
			return errors.New("flow_control rate, burst and report_interval must be positive")
		}
		if c.FlowControl.Mode != "block" && c.FlowControl.Mode != "drop" {
			return errors.New("flow_control.mode must be block or drop")
		}
	}
	for _, r := range c.Sources.Processes {
		if r.Name == "" || (r.CommRegex == "" && r.CmdlineRegex == "") || r.IncludeRegex == "" {
			return fmt.Errorf("invalid process rule %q", r.Name)
		}
		if err := validateStartAt(r.StartAt); err != nil {
			return fmt.Errorf("process rule %q: %w", r.Name, err)
		}
		for field, expression := range map[string]string{"comm_regex": r.CommRegex, "cmdline_regex": r.CmdlineRegex, "include_regex": r.IncludeRegex, "exclude_regex": r.ExcludeRegex, "start_pattern": r.Multiline.StartPattern} {
			if expression != "" {
				if _, err := regexp.Compile(expression); err != nil {
					return fmt.Errorf("process rule %q: invalid %s: %w", r.Name, field, err)
				}
			}
		}
		if err := validateAttributeExtractors(r.Extractors); err != nil {
			return fmt.Errorf("process rule %q: %w", r.Name, err)
		}
		if err := validateTraceIDExtractor(r.TraceID); err != nil {
			return fmt.Errorf("process rule %q: %w", r.Name, err)
		}
		if err := validateDropLevels(r.DropLevels); err != nil {
			return fmt.Errorf("process rule %q: %w", r.Name, err)
		}
	}
	for _, group := range [][]FileRule{c.Sources.Files, c.Sources.Containers} {
		for _, r := range group {
			if r.Name == "" || len(r.Include) == 0 {
				return fmt.Errorf("file rule name and include are required")
			}
			if err := validateStartAt(r.StartAt); err != nil {
				return fmt.Errorf("file rule %q: %w", r.Name, err)
			}
			if r.Format != "" && r.Format != "cri" {
				return fmt.Errorf("file rule %q: format must be empty or cri", r.Name)
			}
			for field, expression := range map[string]string{"start_pattern": r.Multiline.StartPattern} {
				if expression != "" {
					if _, err := regexp.Compile(expression); err != nil {
						return fmt.Errorf("file rule %q: invalid %s: %w", r.Name, field, err)
					}
				}
			}
			if err := validateAttributeExtractors(r.Extractors); err != nil {
				return fmt.Errorf("file rule %q: %w", r.Name, err)
			}
			if err := validateTraceIDExtractor(r.TraceID); err != nil {
				return fmt.Errorf("file rule %q: %w", r.Name, err)
			}
			if err := validateDropLevels(r.DropLevels); err != nil {
				return fmt.Errorf("file rule %q: %w", r.Name, err)
			}
		}
	}
	return nil
}

func validateKeywordList(field string, keywords []string) error {
	seen := make(map[string]struct{}, len(keywords))
	for _, keyword := range keywords {
		if strings.TrimSpace(keyword) == "" {
			return fmt.Errorf("%s must not contain empty values", field)
		}
		if _, exists := seen[keyword]; exists {
			return fmt.Errorf("duplicate %s value %q", field, keyword)
		}
		seen[keyword] = struct{}{}
	}
	return nil
}

func validateDropLevels(levels []string) error {
	return validateSeverityLevels("drop_levels", levels)
}

func validateSeverityLevels(field string, levels []string) error {
	seen := make(map[string]struct{}, len(levels))
	for _, level := range levels {
		normalized := strings.ToUpper(level)
		switch normalized {
		case "TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL":
		default:
			return fmt.Errorf("%s value %q must be TRACE, DEBUG, INFO, WARN, ERROR or FATAL", field, level)
		}
		if _, exists := seen[normalized]; exists {
			return fmt.Errorf("duplicate %s value %q", field, level)
		}
		seen[normalized] = struct{}{}
	}
	return nil
}

func validateAttributeExtractors(extractors []AttributeExtractorConfig) error {
	keys := make(map[string]struct{}, len(extractors))
	for _, extractor := range extractors {
		if extractor.Key == "" || extractor.Pattern == "" {
			return errors.New("attribute extractor key and pattern are required")
		}
		if _, exists := keys[extractor.Key]; exists {
			return fmt.Errorf("duplicate attribute extractor key %q", extractor.Key)
		}
		keys[extractor.Key] = struct{}{}
		compiled, err := regexp.Compile(extractor.Pattern)
		if err != nil {
			return fmt.Errorf("attribute extractor %q has invalid pattern: %w", extractor.Key, err)
		}
		if compiled.NumSubexp() < 1 {
			return fmt.Errorf("attribute extractor %q pattern must contain a capture group", extractor.Key)
		}
	}
	return nil
}

func validateTraceIDExtractor(extractor TraceIDExtractorConfig) error {
	if extractor.Pattern == "" {
		return nil
	}
	compiled, err := regexp.Compile(extractor.Pattern)
	if err != nil {
		return fmt.Errorf("trace_id_extractor has invalid pattern: %w", err)
	}
	if compiled.NumSubexp() < 1 {
		return errors.New("trace_id_extractor pattern must contain a capture group")
	}
	return nil
}

func validateStartAt(v string) error {
	if v != "" && v != "beginning" && v != "end" {
		return fmt.Errorf("start_at must be beginning or end")
	}
	return nil
}
