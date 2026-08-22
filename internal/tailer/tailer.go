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
}

type pending struct {
	body                strings.Builder
	size, lines         int
	target              model.FileTarget
	timestamp           time.Time
	attributes          map[string]string
	updated             time.Time
	start, continuation *regexp.Regexp
}

func New(store *state.Store, performance config.PerformanceConfig, logger *logging.Logger, emit func(context.Context, model.Record) error) *Tailer {
	return &Tailer{state: store, log: logger, emit: emit, workers: performance.WorkerCount, maxReadBytes: performance.MaxReadBytesPerFile, maxLogSize: performance.MaxLogSize, maxMultilineSize: performance.MaxMultilineSize, maxLines: performance.MaxMultilineLines, pending: make(map[string]*pending)}
}

// Poll 并发读取不同文件；同一轮中每个路径只会进入一个 worker。
func (t *Tailer) Poll(ctx context.Context, targets []model.FileTarget) error {
	jobs := make(chan model.FileTarget)
	var wg sync.WaitGroup
	for range t.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for target := range jobs {
				if err := t.read(ctx, target); err != nil && !errors.Is(err, context.Canceled) {
					t.log.Warn("read log file", "path", target.Path, "rule", target.Rule, "error", err)
				}
			}
		}()
	}
	for _, target := range targets {
		select {
		case jobs <- target:
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return ctx.Err()
		}
	}
	close(jobs)
	wg.Wait()
	return t.flushExpired(ctx, time.Now())
}

func (t *Tailer) Close(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, p := range t.pending {
		if p.body.Len() > 0 {
			if err := t.emitPending(ctx, p); err != nil {
				return err
			}
		}
		delete(t.pending, key)
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
		t.state.Set(key, state.Position{Path: target.Path, Offset: offset, UpdatedAt: time.Now().UTC()})
		if readErr != nil {
			break
		}
	}
	return nil
}

// readBoundedLine 会消费完整一行，但最多保留 maxSize 字节，避免超长日志撑爆内存。
func readBoundedLine(r *bufio.Reader, maxSize int) (string, int64, bool, error) {
	var kept bytes.Buffer
	var consumed int64
	for {
		part, err := r.ReadSlice('\n')
		consumed += int64(len(part))
		remaining := maxSize - kept.Len()
		if remaining > len(part) {
			remaining = len(part)
		}
		if remaining > 0 {
			_, _ = kept.Write(part[:remaining])
		}
		if err == nil {
			return strings.TrimSuffix(strings.TrimSuffix(kept.String(), "\n"), "\r"), consumed, true, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			return kept.String(), consumed, false, err
		}
		return kept.String(), consumed, false, err
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
	if target.Multiline.StartPattern == "" && target.Multiline.ContinuationPattern == "" {
		return t.emitRecord(ctx, target, line, timestamp, attrs)
	}
	if len(line) > t.maxMultilineSize {
		line = line[:t.maxMultilineSize]
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	p := t.pending[key]
	if p == nil {
		p = &pending{target: target, updated: time.Now()}
		p.start, _ = regexp.Compile(target.Multiline.StartPattern)
		p.continuation, _ = regexp.Compile(target.Multiline.ContinuationPattern)
		t.pending[key] = p
	}
	boundary := (p.start != nil && p.start.MatchString(line)) || (p.continuation != nil && !p.continuation.MatchString(line))
	overLimit := p.body.Len() > 0 && (p.size+len(line)+1 > t.maxMultilineSize || p.lines >= t.maxLines)
	if (boundary || overLimit) && p.body.Len() > 0 {
		if err := t.emitPending(ctx, p); err != nil {
			return err
		}
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
	return nil
}

func (t *Tailer) emitPending(ctx context.Context, p *pending) error {
	return t.emitRecord(ctx, p.target, p.body.String(), p.timestamp, p.attributes)
}
func (t *Tailer) flushExpired(ctx context.Context, now time.Time) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, p := range t.pending {
		after := p.target.Multiline.FlushAfter
		if after <= 0 {
			after = 5 * time.Second
		}
		if now.Sub(p.updated) >= after && p.body.Len() > 0 {
			if err := t.emitPending(ctx, p); err != nil {
				return err
			}
			delete(t.pending, key)
		}
	}
	return nil
}
func (t *Tailer) emitRecord(ctx context.Context, target model.FileTarget, body string, timestamp time.Time, attrs map[string]string) error {
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	resource := make(map[string]string, len(target.Attributes)+3)
	for k, v := range target.Attributes {
		resource[k] = v
	}
	resource["log.file.path"], resource["log.source.type"], resource["log.source.rule"] = target.Path, target.SourceType, target.Rule
	return t.emit(ctx, model.Record{Body: body, Timestamp: timestamp, ObservedTimestamp: time.Now(), Attributes: attrs, ResourceAttributes: resource})
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
