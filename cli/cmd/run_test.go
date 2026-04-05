package cmd

import (
	"context"
	"testing"
	"time"

	gh "github.com/talk/opencode-client/internal/github"
	"github.com/talk/opencode-client/internal/types"
)

// mockGitHub is a test implementation of gh.Port that records calls.
type mockGitHub struct {
	validateCalled int
	validateResult []gh.IssueValidity
	validateErr    error
}

func (m *mockGitHub) EnsureOpenPR(_ context.Context, _, _ string) (int, bool, error) {
	return 0, false, nil
}
func (m *mockGitHub) PostPRReview(_ context.Context, _ int, _, _ string) error { return nil }
func (m *mockGitHub) ValidateIssues(_ context.Context, _ string) ([]gh.IssueValidity, error) {
	m.validateCalled++
	return m.validateResult, m.validateErr
}
func (m *mockGitHub) ExistingIssueTitles(_ context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}
func (m *mockGitHub) ExistingFingerprints(_ context.Context) (map[string]int, error) {
	return map[string]int{}, nil
}
func (m *mockGitHub) ListOpenIssueSummaries(_ context.Context) ([]types.IssueSummary, error) {
	return nil, nil
}
func (m *mockGitHub) CreateIssue(_ context.Context, _ types.Finding) (int, error) { return 0, nil }
func (m *mockGitHub) CommentOnIssue(_ context.Context, _ int, _ string) error     { return nil }
func (m *mockGitHub) CloseIssue(_ context.Context, _ int) error                   { return nil }
func (m *mockGitHub) LinkIssuesToPR(_ context.Context, _ int, _ []int) error      { return nil }
func (m *mockGitHub) MergePR(_ context.Context, _ int, _ string, _ bool) error    { return nil }

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
	mock := &mockGitHub{}
	runIssueValidation(context.Background(), true, mock, "/tmp/repo")
	if mock.validateCalled != 1 {
		t.Fatalf("ValidateIssues called %d time(s), want 1", mock.validateCalled)
	}
}

func TestRunIssueValidationSkipsWhenDisabled(t *testing.T) {
	mock := &mockGitHub{}
	runIssueValidation(context.Background(), false, mock, "/tmp/repo")
	if mock.validateCalled != 0 {
		t.Fatalf("ValidateIssues called %d time(s) when disabled, want 0", mock.validateCalled)
	}
}

func TestRunIssueValidationSkipsWhenPortNil(t *testing.T) {
	// Should not panic when port is nil (no GitHub client configured).
	runIssueValidation(context.Background(), true, nil, "/tmp/repo")
}

func TestDryRunFlagInConfig(t *testing.T) {
	cfg := runConfig{dryRun: true}
	if !cfg.dryRun {
		t.Fatal("dryRun field not set")
	}
}
