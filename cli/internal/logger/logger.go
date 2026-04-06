package logger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// MaxLogFiles is the number of log files kept in the logs/ directory.
// Older files beyond this limit are removed when a new Logger is created.
const MaxLogFiles = 20

// Logger writes structured JSONL log entries to a per-run file.
type Logger struct {
	f    *os.File
	path string // empty for no-op loggers
}

// LogPath returns the path of the current log file, or "" for a no-op logger.
func (l *Logger) LogPath() string { return l.path }

// New creates a Logger writing to repoRoot/logs/review-TIMESTAMP.jsonl.
// Old log files beyond MaxLogFiles are removed automatically.
// Returns a no-op Logger if the file cannot be created.
func New(repoRoot string) *Logger {
	logsDir := filepath.Join(repoRoot, "logs")
	_ = os.MkdirAll(logsDir, 0o755)
	rotateLogs(logsDir)
	logFile := filepath.Join(logsDir, fmt.Sprintf("review-%s.jsonl", time.Now().Format("20060102-150405")))
	f, err := os.Create(logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot create log file: %v\n", err)
		return &Logger{}
	}
	return &Logger{f: f, path: logFile}
}

// rotateLogs removes the oldest review-*.jsonl files when there are more than MaxLogFiles.
func rotateLogs(logsDir string) {
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		return
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "review-") && strings.HasSuffix(e.Name(), ".jsonl") {
			files = append(files, filepath.Join(logsDir, e.Name()))
		}
	}
	if len(files) < MaxLogFiles {
		return
	}
	// Sort oldest first (name is timestamp-based, lexicographic = chronological).
	sort.Strings(files)
	for _, old := range files[:len(files)-MaxLogFiles+1] {
		_ = os.Remove(old)
	}
}

// Write serialises record as JSON and appends it to the log file.
func (l *Logger) Write(record any) {
	if l.f == nil {
		return
	}
	b, err := json.Marshal(record)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: log marshal error: %v\n", err)
		return
	}
	if _, err := l.f.Write(b); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: log write error: %v\n", err)
		return
	}
	if _, err := l.f.Write([]byte("\n")); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: log write error: %v\n", err)
	}
}

// Close flushes and closes the log file.
func (l *Logger) Close() {
	if l.f != nil {
		_ = l.f.Sync()
		l.f.Close()
	}
}
