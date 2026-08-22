package tailer

import (
	"bufio"
	"strings"
	"testing"
	"time"
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
