package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewCreatesLogFile(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	defer l.Close()

	if l.LogPath() == "" {
		t.Fatal("LogPath() is empty — log file was not created")
	}
	if _, err := os.Stat(l.LogPath()); err != nil {
		t.Fatalf("log file does not exist: %v", err)
	}
}

func TestWriteProducesValidJSONL(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	l.Write(map[string]any{"event": "test", "value": 42})
	l.Close()

	data, err := os.ReadFile(l.LogPath())
	if err != nil {
		t.Fatalf("cannot read log file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}
	if m["event"] != "test" {
		t.Errorf("event = %v, want %q", m["event"], "test")
	}
}

func TestWriteMultipleEntriesAllAppended(t *testing.T) {
	dir := t.TempDir()
	l := New(dir)
	for i := 0; i < 5; i++ {
		l.Write(map[string]any{"i": i})
	}
	l.Close()

	data, err := os.ReadFile(l.LogPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 5 {
		t.Errorf("expected 5 log lines, got %d", len(lines))
	}
}

func TestNoOpLoggerDoesNotPanic(t *testing.T) {
	l := &Logger{} // no-op (f is nil)
	l.Write(map[string]any{"x": 1})
	l.Close()
	if l.LogPath() != "" {
		t.Error("no-op logger should have empty LogPath()")
	}
}

func TestRotateLogsKeepsMaxFiles(t *testing.T) {
	dir := t.TempDir()
	logsDir := filepath.Join(dir, "logs")
	_ = os.MkdirAll(logsDir, 0o755)

	// Pre-create MaxLogFiles + 5 old log files.
	total := MaxLogFiles + 5
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("review-2026010%02d-120000.jsonl", i)
		f, err := os.Create(filepath.Join(logsDir, name))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	// Creating a new Logger triggers rotation.
	l := New(dir)
	l.Close()

	entries, err := os.ReadDir(logsDir)
	if err != nil {
		t.Fatal(err)
	}
	var logFiles []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "review-") && strings.HasSuffix(e.Name(), ".jsonl") {
			logFiles = append(logFiles, e.Name())
		}
	}
	// Should have at most MaxLogFiles (the new file counts as one of them).
	if len(logFiles) > MaxLogFiles {
		t.Errorf("after rotation: %d log files, want ≤ %d", len(logFiles), MaxLogFiles)
	}
}

func TestRotateLogsDoesNotDeleteWhenUnderLimit(t *testing.T) {
	dir := t.TempDir()
	logsDir := filepath.Join(dir, "logs")
	_ = os.MkdirAll(logsDir, 0o755)

	// Create only 3 files — well under the limit.
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("review-2026010%d-120000.jsonl", i)
		f, _ := os.Create(filepath.Join(logsDir, name))
		f.Close()
	}

	l := New(dir)
	l.Close()

	entries, _ := os.ReadDir(logsDir)
	var logFiles []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "review-") {
			logFiles = append(logFiles, e.Name())
		}
	}
	// 3 old + 1 new = 4, all kept.
	if len(logFiles) != 4 {
		t.Errorf("expected 4 log files (3 old + 1 new), got %d", len(logFiles))
	}
}

func TestOldestFilesRemovedFirst(t *testing.T) {
	dir := t.TempDir()
	logsDir := filepath.Join(dir, "logs")
	_ = os.MkdirAll(logsDir, 0o755)

	// Create MaxLogFiles + 2 files; the two lexicographically smallest should be removed.
	for i := 0; i < MaxLogFiles+2; i++ {
		name := fmt.Sprintf("review-202601%02d-120000.jsonl", i+1)
		f, _ := os.Create(filepath.Join(logsDir, name))
		f.Close()
	}

	l := New(dir)
	l.Close()

	entries, _ := os.ReadDir(logsDir)
	for _, e := range entries {
		// The two oldest ("review-202601-01..." and "review-202601-02...") should be gone.
		if e.Name() == "review-20260101-120000.jsonl" || e.Name() == "review-20260102-120000.jsonl" {
			t.Errorf("oldest file %q should have been rotated out", e.Name())
		}
	}
}
