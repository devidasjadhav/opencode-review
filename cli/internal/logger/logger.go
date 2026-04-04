package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Logger writes structured JSONL log entries to a per-run file.
type Logger struct {
	f *os.File
}

// New creates a Logger writing to repoRoot/logs/review-TIMESTAMP.jsonl.
// Returns a no-op Logger if the file cannot be created.
func New(repoRoot string) *Logger {
	logsDir := filepath.Join(repoRoot, "logs")
	_ = os.MkdirAll(logsDir, 0o755)
	logFile := filepath.Join(logsDir, fmt.Sprintf("review-%s.jsonl", time.Now().Format("20060102-150405")))
	f, err := os.Create(logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot create log file: %v\n", err)
		return &Logger{}
	}
	return &Logger{f: f}
}

// Write serialises record as JSON and appends it to the log file.
func (l *Logger) Write(record any) {
	if l.f == nil {
		return
	}
	b, _ := json.Marshal(record)
	l.f.Write(b)
	l.f.Write([]byte("\n"))
}

// Close flushes and closes the log file.
func (l *Logger) Close() {
	if l.f != nil {
		l.f.Close()
	}
}
