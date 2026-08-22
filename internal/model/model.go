package model

import "time"

// FileTarget 是发现模块交给读取模块的统一日志文件描述。
// 不同来源在这里被归一化，Tailer 无需了解进程或容器发现细节。
type FileTarget struct {
	Path       string
	SourceType string
	Rule       string
	StartAt    string
	Format     string
	Multiline  Multiline
	Attributes map[string]string
}

// Multiline 是已经转换成运行时 duration 的多行合并配置。
type Multiline struct {
	StartPattern        string
	ContinuationPattern string
	FlushAfter          time.Duration
}

// Record 是 Tailer 与 OTLP Exporter 之间的内部日志模型。
type Record struct {
	Body               string
	Timestamp          time.Time
	ObservedTimestamp  time.Time
	Attributes         map[string]string
	ResourceAttributes map[string]string
}
