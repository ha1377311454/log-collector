package discovery

import (
	"os"
	"path/filepath"
	"testing"

	"log-collector/internal/model"
)

func TestResolveProcessFileUsesTargetMountNamespace(t *testing.T) {
	procRoot := t.TempDir()
	openedPath := "/data/tingyun/logs/application.log"
	want := filepath.Join(procRoot, "123", "root", "data", "tingyun", "logs", "application.log")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(want, []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := resolveProcessFile(procRoot, 123, openedPath)
	if !ok || got != want {
		t.Fatalf("resolveProcessFile() = (%q, %v), want (%q, true)", got, ok, want)
	}
}

func TestResolveProcessFileFallsBackToDirectPath(t *testing.T) {
	openedPath := filepath.Join(t.TempDir(), "application.log")
	if err := os.WriteFile(openedPath, []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := resolveProcessFile(t.TempDir(), 123, openedPath)
	if !ok || got != openedPath {
		t.Fatalf("resolveProcessFile() = (%q, %v), want (%q, true)", got, ok, openedPath)
	}
}

func TestPrepareResourceKeepsReportedProcessPath(t *testing.T) {
	target := model.FileTarget{
		Path:         "/proc/123/root/data/tingyun/logs/application.log",
		ReportedPath: "/data/tingyun/logs/application.log",
		SourceType:   "process",
		Rule:         "application",
	}

	prepareResource(&target)
	if got := target.ResourceAttributes["log.file.path"]; got != target.ReportedPath {
		t.Fatalf("log.file.path = %q, want %q", got, target.ReportedPath)
	}
}
