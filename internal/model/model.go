package model

import (
	"regexp"
	"time"
)

// FileTarget 是发现模块交给读取模块的统一日志文件描述。
// 不同来源在这里被归一化，Tailer 无需了解进程或容器发现细节。
type FileTarget struct {
	Path string
	// ReportedPath 保留目标进程看到的原始路径；Path 是 Tailer 实际读取的路径。
	ReportedPath      string
	SourceType        string
	Rule              string
	StartAt           string
	Format            string
	Multiline         Multiline
	Attributes        map[string]string
	Extractors        []AttributeExtractor
	TraceIDExtractor  *regexp.Regexp
	TraceIDCompletion bool
	DropLevels        map[string]struct{}
	// ResourceAttributes 和 ResourceKey 在发现阶段预计算，避免每条日志重复复制和排序。
	ResourceAttributes map[string]string
	ResourceKey        string
}

// AttributeExtractor 从日志正文的第一个正则捕获组生成一个 LogRecord 属性。
type AttributeExtractor struct {
	Key     string
	Pattern *regexp.Regexp
}

// Multiline 是已经转换成运行时 duration 的多行合并配置。
type Multiline struct {
	StartPattern string
	FlushAfter   time.Duration
}

// Record 是 Tailer 与 OTLP Exporter 之间的内部日志模型。
type Record struct {
	Body               string
	Timestamp          time.Time
	ObservedTimestamp  time.Time
	SeverityText       string
	SeverityNumber     int32
	Dropped            bool
	TraceID            []byte
	Attributes         map[string]string
	ResourceAttributes map[string]string
	ResourceKey        string
}
