package tailer

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"log-collector/internal/config"
	"log-collector/internal/logging"
	"log-collector/internal/model"
	"log-collector/internal/state"
)

// Tailer 通过固定 worker pool 增量读取文件，并限制单轮、单行和多行内存用量。
type Tailer struct {
	state                                  *state.Store
	log                                    *logging.Logger
	emit                                   func(context.Context, model.Record) error
	workers                                int
	maxReadBytes                           int64
	maxLogSize, maxMultilineSize, maxLines int
	mu                                     sync.Mutex
	pending                                map[string]*pending
	jobs                                   chan readJob
	startOnce                              sync.Once
	stopOnce                               sync.Once
	workerWG                               sync.WaitGroup
}

type readJob struct {
	ctx    context.Context
	target model.FileTarget
	done   chan error
}

type pending struct {
	body        strings.Builder
	size, lines int
	target      model.FileTarget
	timestamp   time.Time
	attributes  map[string]string
	updated     time.Time
	start       *regexp.Regexp
}

var severityPattern = regexp.MustCompile(`(?i)(?:^|[\s\[\]():=\-])(TRACE|DEBUG|INFO|NOTICE|WARN(?:ING)?|ERROR|ERR|FATAL|CRITICAL|CRIT|ALERT|EMERG|PANIC)(?:$|[\s\[\]():=\-])`)

func New(store *state.Store, performance config.PerformanceConfig, logger *logging.Logger, emit func(context.Context, model.Record) error) *Tailer {
	return &Tailer{state: store, log: logger, emit: emit, workers: performance.WorkerCount, maxReadBytes: performance.MaxReadBytesPerFile, maxLogSize: performance.MaxLogSize, maxMultilineSize: performance.MaxMultilineSize, maxLines: performance.MaxMultilineLines, pending: make(map[string]*pending), jobs: make(chan readJob)}
}

// Poll 并发读取不同文件；同一轮中每个路径只会进入一个 worker。
func (t *Tailer) Poll(ctx context.Context, targets []model.FileTarget) error {
	t.startWorkers()
	done := make(chan error, len(targets))
	sent := 0
	for _, target := range targets {
		select {
		case t.jobs <- readJob{ctx: ctx, target: target, done: done}:
			sent++
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for range sent {
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.log.Warn("read log file", "error", err)
		}
	}
	return t.flushExpired(ctx, time.Now())
}

func (t *Tailer) startWorkers() {
	t.startOnce.Do(func() {
		for range t.workers {
			t.workerWG.Add(1)
			go func() {
				defer t.workerWG.Done()
				for job := range t.jobs {
					err := t.read(job.ctx, job.target)
					if err != nil {
						err = fmt.Errorf("path %s rule %s: %w", job.target.Path, job.target.Rule, err)
					}
					job.done <- err
				}
			}()
		}
	})
}

func (t *Tailer) Close(ctx context.Context) error {
	t.stopOnce.Do(func() {
		close(t.jobs)
		t.workerWG.Wait()
	})
	t.mu.Lock()
	records := make([]model.Record, 0, len(t.pending))
	for key, p := range t.pending {
		if p.body.Len() > 0 {
			records = append(records, preparedRecord(p.target, p.body.String(), p.timestamp, p.attributes))
		}
		delete(t.pending, key)
	}
	t.mu.Unlock()
	for _, record := range records {
		if err := t.emitPrepared(ctx, record); err != nil {
			return err
		}
	}
	return t.state.Save()
}

func (t *Tailer) read(ctx context.Context, target model.FileTarget) error {
	f, err := os.Open(target.Path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	key, err := identity(info)
	if err != nil {
		return err
	}
	pos, ok := t.state.Get(key)
	offset := pos.Offset
	if !ok && target.StartAt == "end" {
		offset = info.Size()
	}
	if offset > info.Size() {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return err
	}
	initialOffset, committedOffset := pos.Offset, offset
	// 一轮读取只在结束时更新一次共享位点；仅提交已成功交给下游的完整日志。
	defer func() {
		if committedOffset != initialOffset {
			t.state.Set(key, state.Position{Path: target.Path, Offset: committedOffset, UpdatedAt: time.Now().UTC()})
		}
	}()

	r := bufio.NewReaderSize(f, 64*1024)
	var readBytes int64
	for readBytes < t.maxReadBytes {
		line, consumed, complete, readErr := readBoundedLine(r, t.maxLogSize)
		if consumed == 0 && readErr != nil {
			break
		}
		// 文件末尾半行不推进位点，下一轮从该行开头重新读取。
		if !complete {
			break
		}
		offset += consumed
		readBytes += consumed
		if err := t.accept(ctx, key, target, line); err != nil {
			return err
		}
		committedOffset = offset
		if readErr != nil {
			break
		}
	}
	return nil
}

// readBoundedLine 会消费完整一行，但最多保留 maxSize 个 rune。
// UTF-8 单个 rune 最多占 4 字节，因此原始字节缓冲最多保留 maxSize*4 字节。
func readBoundedLine(r *bufio.Reader, maxSize int) (string, int64, bool, error) {
	var kept bytes.Buffer
	var consumed int64
	for {
		part, err := r.ReadSlice('\n')
		consumed += int64(len(part))
		remaining := maxSize*4 - kept.Len()
		if remaining > len(part) {
			remaining = len(part)
		}
		if remaining > 0 {
			_, _ = kept.Write(part[:remaining])
		}
		if err == nil {
			return truncateRunes(strings.TrimSuffix(strings.TrimSuffix(kept.String(), "\n"), "\r"), maxSize), consumed, true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return truncateRunes(kept.String(), maxSize), consumed, false, err
		}
		return truncateRunes(kept.String(), maxSize), consumed, false, err
	}
}

func (t *Tailer) accept(ctx context.Context, key string, target model.FileTarget, line string) error {
	timestamp := time.Time{}
	var attrs map[string]string
	if target.Format == "cri" {
		body, ts, stream, flag, ok := parseCRI(line)
		if ok {
			line, timestamp, attrs = body, ts, map[string]string{"log.iostream": stream, "log.cri.flag": flag}
		}
	}
	if target.Multiline.StartPattern == "" {
		return t.emitRecord(ctx, target, line, timestamp, attrs)
	}
	if len(line) > t.maxMultilineSize {
		line = truncateRunes(line, t.maxMultilineSize)
	}
	t.mu.Lock()
	p := t.pending[key]
	if p == nil {
		p = &pending{target: target, updated: time.Now()}
		p.start, _ = regexp.Compile(target.Multiline.StartPattern)
		t.pending[key] = p
	}
	boundary := p.start.MatchString(line)
	overLimit := p.body.Len() > 0 && (p.size+len(line)+1 > t.maxMultilineSize || p.lines >= t.maxLines)
	if (boundary || overLimit) && p.body.Len() > 0 {
		// 发送可能因限流或满队列阻塞，必须在全局 pending 锁之外执行。
		record := preparedRecord(p.target, p.body.String(), p.timestamp, p.attributes)
		t.mu.Unlock()
		if err := t.emitPrepared(ctx, record); err != nil {
			return err
		}
		t.mu.Lock()
		p.body.Reset()
		p.size, p.lines = 0, 0
	}
	if p.body.Len() == 0 {
		p.timestamp, p.attributes = timestamp, attrs
	} else {
		_ = p.body.WriteByte('\n')
		p.size++
	}
	_, _ = p.body.WriteString(line)
	p.size += len(line)
	p.lines++
	p.updated = time.Now()
	t.mu.Unlock()
	return nil
}

// truncateRunes 先转换成 []rune，再按字符数截断。
// string([]rune(...)) 天然保证输出是合法 UTF-8，非法原始字节会转换为 RuneError。
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func (t *Tailer) flushExpired(ctx context.Context, now time.Time) error {
	type expired struct {
		key    string
		value  *pending
		record model.Record
	}
	t.mu.Lock()
	records := make([]expired, 0)
	for key, p := range t.pending {
		after := p.target.Multiline.FlushAfter
		if after <= 0 {
			after = 5 * time.Second
		}
		if now.Sub(p.updated) >= after && p.body.Len() > 0 {
			records = append(records, expired{key: key, value: p, record: preparedRecord(p.target, p.body.String(), p.timestamp, p.attributes)})
		}
	}
	t.mu.Unlock()
	for _, item := range records {
		if err := t.emitPrepared(ctx, item.record); err != nil {
			return err
		}
		t.mu.Lock()
		if t.pending[item.key] == item.value {
			delete(t.pending, item.key)
		}
		t.mu.Unlock()
	}
	return nil
}
func (t *Tailer) emitRecord(ctx context.Context, target model.FileTarget, body string, timestamp time.Time, attrs map[string]string) error {
	return t.emitPrepared(ctx, preparedRecord(target, body, timestamp, attrs))
}

func (t *Tailer) emitPrepared(ctx context.Context, record model.Record) error {
	if record.Dropped {
		return nil
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now()
	}
	record.ObservedTimestamp = time.Now()
	return t.emit(ctx, record)
}

func preparedRecord(target model.FileTarget, body string, timestamp time.Time, attrs map[string]string) model.Record {
	// 正常目标在发现阶段已经预计算；该兜底也方便独立测试或其他调用方直接构造目标。
	if target.ResourceAttributes == nil {
		resource := make(map[string]string, len(target.Attributes)+3)
		for key, value := range target.Attributes {
			resource[key] = value
		}
		resource["log.file.path"] = target.ReportedPath
		if resource["log.file.path"] == "" {
			resource["log.file.path"] = target.Path
		}
		resource["log.source.type"] = target.SourceType
		resource["log.source.rule"] = target.Rule
		target.ResourceAttributes = resource
	}
	severityText, severityNumber := parseSeverity(body)
	if severityText != "" || len(target.Extractors) > 0 {
		if attrs == nil {
			attrs = make(map[string]string, len(target.Extractors)+1)
		}
	}
	if severityText != "" {
		attrs["log.level"] = severityText
	}
	for _, extractor := range target.Extractors {
		match := extractor.Pattern.FindStringSubmatch(body)
		if len(match) > 1 && match[1] != "" {
			attrs[extractor.Key] = match[1]
		}
	}
	_, dropped := target.DropLevels[severityText]
	return model.Record{Body: body, Timestamp: timestamp, SeverityText: severityText, SeverityNumber: severityNumber, Dropped: dropped, Attributes: attrs, ResourceAttributes: target.ResourceAttributes, ResourceKey: target.ResourceKey}
}

// parseSeverity 从日志首行识别常见级别，并映射到 OTLP SeverityNumber 的基础档位。
// 多行异常只读取首行，避免堆栈正文中的 ERROR 等单词覆盖真实级别。
func parseSeverity(body string) (string, int32) {
	firstLine := body
	if index := strings.IndexByte(firstLine, '\n'); index >= 0 {
		firstLine = firstLine[:index]
	}
	match := severityPattern.FindStringSubmatch(firstLine)
	if len(match) < 2 {
		return "", 0
	}
	switch strings.ToUpper(match[1]) {
	case "TRACE":
		return "TRACE", 1
	case "DEBUG":
		return "DEBUG", 5
	case "INFO", "NOTICE":
		return "INFO", 9
	case "WARN", "WARNING":
		return "WARN", 13
	case "ERROR", "ERR":
		return "ERROR", 17
	case "FATAL", "CRITICAL", "CRIT", "ALERT", "EMERG", "PANIC":
		return "FATAL", 21
	default:
		return "", 0
	}
}
func identity(info os.FileInfo) (string, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("unsupported file identity")
	}
	return strconv.FormatUint(uint64(stat.Dev), 10) + ":" + strconv.FormatUint(stat.Ino, 10), nil
}
func parseCRI(line string) (string, time.Time, string, string, bool) {
	parts := strings.SplitN(line, " ", 4)
	if len(parts) != 4 || (parts[1] != "stdout" && parts[1] != "stderr") || (parts[2] != "P" && parts[2] != "F") {
		return "", time.Time{}, "", "", false
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return "", time.Time{}, "", "", false
	}
	return parts[3], ts, parts[1], parts[2], true
}
