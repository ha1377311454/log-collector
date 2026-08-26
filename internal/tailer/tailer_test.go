package tailer

import (
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"log-collector/internal/exporter"
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

func TestPreparedRecordExtractsOTLPTraceID(t *testing.T) {
	target := model.FileTarget{
		Path:              "/tmp/app.log",
		SourceType:        "file",
		Rule:              "app",
		TraceIDExtractor:  regexp.MustCompile(`tingyun\.trace_id:([0-9a-fA-F]+)`),
		TraceIDCompletion: true,
	}
	record := preparedRecord(target, "[tingyun.trace_id:57c52734aa944c84]", time.Time{}, nil)
	if got := fmt.Sprintf("%x", record.TraceID); got != "000000000000000057c52734aa944c84" {
		t.Fatalf("TraceID = %q", got)
	}
}

func TestPreparedRecordIgnoresInvalidOTLPTraceID(t *testing.T) {
	target := model.FileTarget{TraceIDExtractor: regexp.MustCompile(`trace_id:([^ ]+)`), TraceIDCompletion: true}
	for _, body := range []string{"trace_id:not-hex", "trace_id:0000000000000000", "trace_id:1234"} {
		if record := preparedRecord(target, body, time.Time{}, nil); len(record.TraceID) != 0 {
			t.Fatalf("body %q generated TraceID %x", body, record.TraceID)
		}
	}
}

func TestPreparedRecordWritesRawTraceIDWhenCompletionDisabled(t *testing.T) {
	target := model.FileTarget{TraceIDExtractor: regexp.MustCompile(`trace_id:([^ ]+)`)}
	record := preparedRecord(target, "trace_id:57c52734aa944c84", time.Time{}, nil)
	if got := string(record.TraceID); got != "57c52734aa944c84" {
		t.Fatalf("TraceID = %q", got)
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

func TestJavaErrorStackTraceMergesLinesWithoutTimestamp(t *testing.T) {
	const startPattern = `^(?:(?:\d{4}[-/]\d{2}[-/]\d{2})[ T])?\d{2}:\d{2}:\d{2}(?:[.,]\d{3,9})?(?:Z|[+-]\d{2}:?\d{2})?`
	continuations := []string{
		"com.example.application.QueryException: query expression is invalid",
		"\tat com.example.application.QueryService.execute(QueryService.java:72)",
		"\tat java.lang.Thread.run(Thread.java:830)",
	}
	tests := []struct {
		name   string
		first  string
		second string
	}{
		{name: "time with milliseconds", first: "11:13:10.692", second: "11:13:23.469"},
		{name: "date time with milliseconds", first: "2026-08-24 11:13:10.692", second: "2026-08-24 11:13:23.469"},
		{name: "ISO time with nanoseconds and UTC", first: "2026-08-24T11:13:10.692123456Z", second: "2026-08-24T11:13:23.469123456Z"},
		{name: "slash date with comma milliseconds", first: "2026/08/24 11:13:10,692", second: "2026/08/24 11:13:23,469"},
		{name: "date time without fraction", first: "2026-08-24 11:13:10", second: "2026-08-24 11:13:23"},
		{name: "time with timezone offset", first: "2026-08-24 11:13:10.692+08:00", second: "2026-08-24 11:13:23.469+08:00"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			firstLine := tt.first + " - c.e.a.QueryService T:[worker-1] ERROR - request-a - query failed"
			secondLine := tt.second + " - c.e.a.QueryService T:[worker-2] ERROR - request-b - query failed"
			var emitted []model.Record
			tailer := &Tailer{
				emit: func(_ context.Context, record model.Record) error {
					emitted = append(emitted, record)
					return nil
				},
				maxMultilineSize: 1024 * 1024,
				maxLines:         100,
				pending:          make(map[string]*pending),
			}
			target := model.FileTarget{
				Path:       "/tmp/query.log",
				SourceType: "file",
				Rule:       "java-query",
				Multiline:  model.Multiline{StartPattern: startPattern},
				Extractors: []model.AttributeExtractor{{
					Key:     "request.id",
					Pattern: regexp.MustCompile(`\b(?:TRACE|DEBUG|INFO|WARN|ERROR|FATAL)\s+-\s+(\S+)\s+-`),
				}},
			}
			ctx := context.Background()
			if err := tailer.accept(ctx, "file-id", target, firstLine); err != nil {
				t.Fatal(err)
			}
			for _, line := range continuations {
				if err := tailer.accept(ctx, "file-id", target, line); err != nil {
					t.Fatal(err)
				}
			}
			if err := tailer.accept(ctx, "file-id", target, secondLine); err != nil {
				t.Fatal(err)
			}

			if len(emitted) != 1 {
				t.Fatalf("emitted records = %d, want 1", len(emitted))
			}
			wantBody := strings.Join(append([]string{firstLine}, continuations...), "\n")
			if emitted[0].Body != wantBody {
				t.Fatalf("merged body mismatch\ngot:  %q\nwant: %q", emitted[0].Body, wantBody)
			}
			if strings.Contains(emitted[0].Body, secondLine) {
				t.Fatal("second log entry must not be merged into the first record")
			}
			originalLines := append([]string{firstLine}, continuations...)
			originalLines = append(originalLines, secondLine)
			mergedLines := strings.Split(emitted[0].Body, "\n")
			t.Logf(
				"before merge (%d source lines):\n%s\nafter merge (record 1 uses source lines 1-%d; source line %d starts the next record):\n%s",
				len(originalLines),
				formatNumberedLines("source", originalLines),
				len(mergedLines),
				len(mergedLines)+1,
				formatNumberedLines("record 1", mergedLines),
			)

			protocolRecord := emitted[0]
			protocolTime := time.Date(2026, 8, 24, 3, 13, 10, 692000000, time.UTC)
			protocolRecord.Timestamp = protocolTime
			protocolRecord.ObservedTimestamp = protocolTime
			otlpJSON, err := (protojson.MarshalOptions{Multiline: true, Indent: "  "}).Marshal(exporter.BuildRequest([]model.Record{protocolRecord}))
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("final OpenTelemetry ExportLogsServiceRequest:\n%s", otlpJSON)
		})
	}
}

func formatNumberedLines(label string, lines []string) string {
	var output strings.Builder
	for index, line := range lines {
		if index > 0 {
			output.WriteByte('\n')
		}
		_, _ = fmt.Fprintf(&output, "[%s line %d] %s", label, index+1, line)
	}
	return output.String()
}
