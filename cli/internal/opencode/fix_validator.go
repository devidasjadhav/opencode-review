package opencode

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/talk/opencode-client/internal/git"
)

type FixValidator interface {
	Validate(repoRoot string, changedPaths []string) error
}

type goBuildFixValidator struct{}

func NewGoBuildFixValidator() FixValidator {
	return goBuildFixValidator{}
}

func (goBuildFixValidator) Validate(repoRoot string, changedPaths []string) error {
	pkgs := changedPathsToPackages(repoRoot, changedPaths)
	if len(pkgs) == 0 {
		pkgs = []string{"./..."}
	}
	args := append([]string{"build"}, pkgs...)
	out, err := git.RunInDir(repoRoot, "go", args...)
	if err != nil {
		return fmt.Errorf("go build %s failed:\n%s: %w", strings.Join(pkgs, " "), out, err)
	}
	return nil
}

// changedPathsToPackages converts file paths to unique ./relative/pkg patterns
// suitable for passing to go build. Falls back to ./... if no Go files are found.
func changedPathsToPackages(repoRoot string, paths []string) []string {
	seen := make(map[string]bool)
	var pkgs []string
	for _, p := range paths {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		abs := p
		if !filepath.IsAbs(p) {
			abs = filepath.Join(repoRoot, p)
		}
		dir := filepath.Dir(abs)
		rel, err := filepath.Rel(repoRoot, dir)
		if err != nil || rel == "" {
			continue
		}
		pkg := "./" + filepath.ToSlash(rel)
		if !seen[pkg] {
			seen[pkg] = true
			pkgs = append(pkgs, pkg)
		}
	}
	return pkgs
}
