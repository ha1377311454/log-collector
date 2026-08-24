package tailer

import (
	"bufio"
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"log-collector/internal/model"
)

func TestParseCRI(t *testing.T) {
	body, ts, stream, flag, ok := parseCRI("2026-08-22T10:20:30.123456789Z stderr F failed")
	if !ok || body != "failed" || stream != "stderr" || flag != "F" {
		t.Fatalf("unexpected parse: %q %q %q %v", body, stream, flag, ok)
	}
	if !ts.Equal(time.Date(2026, 8, 22, 10, 20, 30, 123456789, time.UTC)) {
		t.Fatalf("timestamp = %s", ts)
	}
}

func TestReadBoundedLineConsumesButTruncatesOversizedLine(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("123456789\nnext\n"), 4)
	line, consumed, complete, err := readBoundedLine(r, 5)
	if err != nil || !complete || line != "12345" || consumed != 10 {
		t.Fatalf("line=%q consumed=%d complete=%v err=%v", line, consumed, complete, err)
	}
	next, _, _, _ := readBoundedLine(r, 5)
	if next != "next" {
		t.Fatalf("next line = %q", next)
	}
}

func TestReadBoundedLineDoesNotCommitPartialLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("partial"))
	_, _, complete, _ := readBoundedLine(r, 1024)
	if complete {
		t.Fatal("line without newline must remain incomplete")
	}
}

func TestReadBoundedLineTruncatesAtRuneBoundary(t *testing.T) {
	r := bufio.NewReaderSize(strings.NewReader("你好世界\n"), 4)
	line, consumed, complete, err := readBoundedLine(r, 2)
	if err != nil || !complete || line != "你好" || consumed != int64(len("你好世界\n")) {
		t.Fatalf("line=%q consumed=%d complete=%v err=%v", line, consumed, complete, err)
	}
}

func TestTruncateRunesReplacesInvalidBytes(t *testing.T) {
	value := string([]byte{'A', 0xff, 'B'})
	if got := truncateRunes(value, 10); got != "A�B" {
		t.Fatalf("truncateRunes() = %q", got)
	}
}

func TestParseCRIRejectsPlainText(t *testing.T) {
	if _, _, _, _, ok := parseCRI("plain text"); ok {
		t.Fatal("plain text must not parse as CRI")
	}
}

func TestParseSeverity(t *testing.T) {
	tests := []struct {
		body   string
		text   string
		number int32
	}{
		{"2026-08-24 10:00:00.000 [main] INFO application started", "INFO", 9},
		{"time=2026-08-24T10:00:00Z level=warning msg=slow", "WARN", 13},
		{"ERROR request failed\n\tat demo.Service.call(Service.java:10)", "ERROR", 17},
		{"panic: unrecoverable", "FATAL", 21},
		{"request completed successfully", "", 0},
	}
	for _, tt := range tests {
		text, number := parseSeverity(tt.body)
		if text != tt.text || number != tt.number {
			t.Errorf("parseSeverity(%q) = (%q, %d), want (%q, %d)", tt.body, text, number, tt.text, tt.number)
		}
	}
}

func TestPreparedRecordAddsLogLevelAttribute(t *testing.T) {
	record := preparedRecord(model.FileTarget{Path: "/tmp/app.log", SourceType: "file", Rule: "app"}, "[DEBUG] detail", time.Time{}, nil)
	if record.SeverityText != "DEBUG" || record.SeverityNumber != 5 || record.Attributes["log.level"] != "DEBUG" {
		t.Fatalf("unexpected severity record: %+v", record)
	}
}

func TestPreparedRecordExtractsConfiguredAttribute(t *testing.T) {
	target := model.FileTarget{
		Path:       "/tmp/app.log",
		SourceType: "file",
		Rule:       "app",
		Extractors: []model.AttributeExtractor{{
			Key:     "request.id",
			Pattern: regexp.MustCompile(`\b(?:TRACE|DEBUG|INFO|WARN|ERROR|FATAL)\s+-\s+(\S+)\s+-`),
		}},
	}
	body := "10:14:58.604 - demo M:acceptInterval L:34- T:[worker] DEBUG - qzJFsZlazjUljpmV - [acceptInterval]"
	record := preparedRecord(target, body, time.Time{}, nil)
	if record.Attributes["request.id"] != "qzJFsZlazjUljpmV" {
		t.Fatalf("request.id = %q", record.Attributes["request.id"])
	}
}

func TestConfiguredSeverityIsDroppedBeforeEmit(t *testing.T) {
	target := model.FileTarget{
		Path:       "/tmp/app.log",
		SourceType: "file",
		Rule:       "app",
		DropLevels: map[string]struct{}{"DEBUG": {}},
	}
	record := preparedRecord(target, "DEBUG diagnostic detail", time.Time{}, nil)
	if !record.Dropped {
		t.Fatal("DEBUG record must be marked as dropped")
	}
	called := false
	tailer := &Tailer{emit: func(context.Context, model.Record) error {
		called = true
		return nil
	}}
	if err := tailer.emitPrepared(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if called {
		t.Fatal("dropped record must not reach downstream emitter")
	}
}
