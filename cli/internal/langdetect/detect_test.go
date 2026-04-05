package langdetect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectGo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example"), 0644); err != nil {
		t.Fatal(err)
	}
	lang := Detect(dir)
	if lang.Name != "Go" {
		t.Errorf("Detect with go.mod = %q, want Go", lang.Name)
	}
}

func TestDetectRust(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Cargo.toml"), []byte("[package]"), 0644); err != nil {
		t.Fatal(err)
	}
	lang := Detect(dir)
	if lang.Name != "Rust" {
		t.Errorf("Detect with Cargo.toml = %q, want Rust", lang.Name)
	}
}

func TestDetectPython(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[tool]"), 0644); err != nil {
		t.Fatal(err)
	}
	lang := Detect(dir)
	if lang.Name != "Python" {
		t.Errorf("Detect with pyproject.toml = %q, want Python", lang.Name)
	}
}

func TestDetectUnknown(t *testing.T) {
	dir := t.TempDir()
	lang := Detect(dir)
	if lang.Name != "Unknown" {
		t.Errorf("Detect empty dir = %q, want Unknown", lang.Name)
	}
}

func TestHasExtensionGo(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"cmd/main.go", true},
		{"internal/foo.go", true},
		{"main_test.go", true},
		{"README.md", false},
		{"script.py", false},
	}
	for _, tc := range tests {
		if got := Go.HasExtension(tc.path); got != tc.want {
			t.Errorf("Go.HasExtension(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestHasExtensionUnknownRejectsBinaries(t *testing.T) {
	binaries := []string{"app.exe", "lib.so", "image.png", "doc.pdf", "archive.zip"}
	for _, p := range binaries {
		if Unknown.HasExtension(p) {
			t.Errorf("Unknown.HasExtension(%q) = true, want false for binary/asset", p)
		}
	}
	sources := []string{"main.rb", "app.java", "style.css", "config.yaml"}
	for _, p := range sources {
		if !Unknown.HasExtension(p) {
			t.Errorf("Unknown.HasExtension(%q) = false, want true for source", p)
		}
	}
}

func TestIsTestFileGo(t *testing.T) {
	if !Go.IsTestFile("foo_test.go") {
		t.Error("Go.IsTestFile(foo_test.go) = false, want true")
	}
	if Go.IsTestFile("foo.go") {
		t.Error("Go.IsTestFile(foo.go) = true, want false")
	}
}

func TestIsTestFilePython(t *testing.T) {
	if !Python.IsTestFile("test_foo.py") {
		t.Error("Python.IsTestFile(test_foo.py) = false, want true")
	}
	if !Python.IsTestFile("foo_test.py") {
		t.Error("Python.IsTestFile(foo_test.py) = false, want true")
	}
	if Python.IsTestFile("foo.py") {
		t.Error("Python.IsTestFile(foo.py) = true, want false")
	}
}

func TestSkipDir(t *testing.T) {
	skip := []string{".git", "vendor", "node_modules", "target", "dist", ".venv", "logs"}
	for _, d := range skip {
		if !SkipDir(d) {
			t.Errorf("SkipDir(%q) = false, want true", d)
		}
	}
	keep := []string{"cmd", "internal", "src", "pkg", "lib"}
	for _, d := range keep {
		if SkipDir(d) {
			t.Errorf("SkipDir(%q) = true, want false", d)
		}
	}
}
