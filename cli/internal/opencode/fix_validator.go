package opencode

import (
	"fmt"

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
	_ = changedPaths
	out, err := git.RunInDir(repoRoot, "go", "build", "./...")
	if err != nil {
		return fmt.Errorf("go build ./... failed:\n%s: %w", out, err)
	}
	return nil
}
