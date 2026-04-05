package langdetect

import (
	"os"
	"path/filepath"
	"strings"
)

// Language describes a detected project language.
type Language struct {
	Name       string   // human-readable name
	Extensions []string // source file extensions; nil = accept any non-binary file
	FenceTag   string   // markdown code fence language tag
}

// Known languages detected by marker files.
var (
	Go = Language{
		Name:       "Go",
		Extensions: []string{".go"},
		FenceTag:   "go",
	}
	Python = Language{
		Name:       "Python",
		Extensions: []string{".py"},
		FenceTag:   "python",
	}
	TypeScript = Language{
		Name:       "TypeScript/JavaScript",
		Extensions: []string{".ts", ".tsx", ".js", ".jsx"},
		FenceTag:   "typescript",
	}
	Rust = Language{
		Name:       "Rust",
		Extensions: []string{".rs"},
		FenceTag:   "rust",
	}
	// Unknown is returned when no language can be detected.
	// HasExtension returns true for any non-binary extension.
	Unknown = Language{Name: "Unknown"}
)

// markerFiles maps a well-known filename to the language it indicates.
// Order matters: earlier entries take priority.
var markerFiles = []struct {
	file string
	lang Language
}{
	{"go.mod", Go},
	{"Cargo.toml", Rust},
	{"package.json", TypeScript},
	{"requirements.txt", Python},
	{"pyproject.toml", Python},
	{"setup.py", Python},
}

// Detect inspects repoRoot for marker files and returns the primary language.
// Returns Unknown if no language can be determined.
func Detect(repoRoot string) Language {
	for _, m := range markerFiles {
		if _, err := os.Stat(filepath.Join(repoRoot, m.file)); err == nil {
			return m.lang
		}
	}
	return Unknown
}

// HasExtension reports whether path is a source file for this language.
// For Unknown languages all non-binary extensions are accepted.
func (l Language) HasExtension(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	if len(l.Extensions) == 0 {
		// Unknown: exclude known binary/asset extensions
		switch ext {
		case ".exe", ".bin", ".so", ".dylib", ".dll", ".o", ".a",
			".png", ".jpg", ".jpeg", ".gif", ".ico", ".svg", ".webp",
			".pdf", ".zip", ".tar", ".gz", ".wasm", ".lock":
			return false
		}
		return ext != ""
	}
	for _, e := range l.Extensions {
		if ext == e {
			return true
		}
	}
	return false
}

// IsTestFile reports whether path is a test file for this language.
func (l Language) IsTestFile(path string) bool {
	base := filepath.Base(path)
	switch l.Name {
	case "Go":
		return strings.HasSuffix(base, "_test.go")
	case "Python":
		return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py")
	case "TypeScript/JavaScript":
		return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.")
	}
	return false
}

// SkipDir reports whether a directory should be skipped during source walks.
func SkipDir(name string) bool {
	switch name {
	case ".git", "vendor", "node_modules", ".venv", "venv",
		"__pycache__", "target", "dist", "build", "logs",
		".idea", ".vscode", "coverage", ".cache":
		return true
	}
	// Skip other hidden directories
	return len(name) > 1 && strings.HasPrefix(name, ".")
}
