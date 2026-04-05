package cmd

import (
	"context"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v67/github"
	gh "github.com/talk/opencode-client/internal/github"
)

func TestNormalizeLoopFlagsMergeAutoFixInterval(t *testing.T) {
	tests := []struct {
		name         string
		merge        bool
		autoFix      bool
		wantInterval time.Duration
	}{
		{
			name:         "merge with auto-fix keeps interval",
			merge:        true,
			autoFix:      true,
			wantInterval: 30 * time.Second,
		},
		{
			name:         "merge without auto-fix zeros interval",
			merge:        true,
			autoFix:      false,
			wantInterval: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			loopMode := false
			mergeOnApprove := tc.merge
			autoFix := tc.autoFix
			loopInterval := 30 * time.Second

			normalizeLoopFlags(&loopMode, &mergeOnApprove, &autoFix, &loopInterval)

			if !loopMode {
				t.Fatalf("loopMode = %v, want true", loopMode)
			}
			if loopInterval != tc.wantInterval {
				t.Fatalf("loopInterval = %v, want %v", loopInterval, tc.wantInterval)
			}
		})
	}
}

func TestNeedsGitHubClientValidateIssuesOnly(t *testing.T) {
	cfg := runConfig{validateIssues: true}
	if !cfg.needsGitHubClient() {
		t.Fatalf("needsGitHubClient with validateIssues=true = false, want true")
	}
}

func TestRunIssueValidationCallsValidateIssuesWhenEnabled(t *testing.T) {
	called := 0
	validateFn := func(context.Context, *gogithub.Client, string, string, string) ([]gh.IssueValidity, error) {
		called++
		return nil, nil
	}

	runIssueValidation(context.Background(), true, &gogithub.Client{}, "owner", "repo", "/tmp/repo", validateFn)

	if called != 1 {
		t.Fatalf("validateFn called %d time(s), want 1", called)
	}
}
