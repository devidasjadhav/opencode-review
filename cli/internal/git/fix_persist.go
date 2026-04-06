package git

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/talk/opencode-client/internal/types"
)

// FixPersister stages, validates, commits, and pushes AI-applied fixes.
// Returns the committed paths on success, nil if no changes were committed.
type FixPersister interface {
	Persist(repoRoot string, findings []types.Finding, iteration int, stagePaths []string) (committedPaths []string, err error)
}

type gitFixPersister struct {
	validator FixValidator
}

// NewGitFixPersister creates a FixPersister that commits and pushes via git.
// It uses GoBuildFixValidator to validate changes before committing.
func NewGitFixPersister() FixPersister {
	return gitFixPersister{validator: NewGoBuildFixValidator()}
}

// NewGitFixPersisterWithValidator creates a FixPersister with a custom validator.
// Useful for testing with a no-op or stub validator.
func NewGitFixPersisterWithValidator(validator FixValidator) FixPersister {
	return gitFixPersister{validator: validator}
}

func (p gitFixPersister) Persist(repoRoot string, findings []types.Finding, iteration int, stagePaths []string) ([]string, error) {
	return stageCommitPushFixes(repoRoot, findings, iteration, stagePaths, p.validator)
}

func stageCommitPushFixes(repoRoot string, findings []types.Finding, iteration int, stagePaths []string, validator FixValidator) ([]string, error) {
	var validPaths []string
	for _, p := range stagePaths {
		abs := filepath.Join(repoRoot, p)
		if _, err := os.Stat(abs); err == nil {
			validPaths = append(validPaths, p)
		} else {
			fmt.Fprintf(os.Stderr, "  Warning: skipping invalid stage path %q: %v\n", p, err)
		}
	}
	if len(validPaths) == 0 {
		fmt.Println("  (no valid paths to stage after fix)")
		return nil, nil
	}
	addArgs := append([]string{"add", "-u", "--"}, validPaths...)
	if _, err := Run(repoRoot, addArgs...); err != nil {
		return nil, fmt.Errorf("git add failed: %w", err)
	}
	stagedArgs := append([]string{"diff", "--cached", "--name-only", "--"}, validPaths...)
	staged, _ := Run(repoRoot, stagedArgs...)
	if staged == "" {
		fmt.Println("  (no file changes detected after fix)")
		return nil, nil
	}
	if validator == nil {
		validator = NewGoBuildFixValidator()
	}
	if err := validator.Validate(repoRoot, validPaths); err != nil {
		restoreArgs := append([]string{"restore", "--staged", "--worktree", "--"}, validPaths...)
		_, _ = Run(repoRoot, restoreArgs...)
		return nil, fmt.Errorf("validation failed after fix — reverted fix paths: %w", err)
	}

	msg := fmt.Sprintf("fix: auto-fix %d finding(s) from review iteration %d", len(findings), iteration)
	commitArgs := append([]string{"commit", "-m", msg, "--"}, validPaths...)
	if _, err := Run(repoRoot, commitArgs...); err != nil {
		return nil, fmt.Errorf("git commit failed: %w", err)
	}
	branch, err := Run(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("git branch resolution failed: %w", err)
	}
	if _, err := Run(repoRoot, "push", "origin", branch); err != nil {
		return nil, fmt.Errorf("git push failed: %w", err)
	}
	fmt.Printf("  Committed and pushed fixes: %q\n", msg)
	return validPaths, nil
}
