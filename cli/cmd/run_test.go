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
func (m *mockGitHub) FetchIssueIndex(_ context.Context) (gh.IssueIndex, error) {
	return gh.IssueIndex{Titles: map[string]bool{}, Fingerprints: map[string]int{}}, nil
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

func TestLSPFlagInConfig(t *testing.T) {
	cfg := runConfig{lspEnabled: true}
	if !cfg.lspEnabled {
		t.Fatal("lspEnabled field not set")
	}
}

// TestRunIssueValidationAcceptsNarrowInterface verifies that runIssueValidation
// accepts a gh.ValidationPort, not the full Port — confirming ISP compliance.
func TestRunIssueValidationAcceptsNarrowInterface(t *testing.T) {
	// mockValidation only implements ValidationPort (not full Port).
	var port gh.ValidationPort = &mockGitHub{}
	runIssueValidation(context.Background(), true, port, "/tmp/repo")
}

// TestFileIssuesAcceptsNarrowInterface verifies that fileIssues accepts
// gh.IssueFilerPort — not the full Port — confirming ISP compliance.
func TestFileIssuesAcceptsNarrowInterface(t *testing.T) {
	// mockGitHub implements IssueFilerPort; passing it directly should compile.
	var port gh.IssueFilerPort = &mockGitHub{}
	// fileIssues needs review text with findings; empty text → no findings, no GitHub calls.
	_, _ = fileIssues(context.Background(), port, 0, "", nil, nil)
}

func TestRunSummaryTracksIterations(t *testing.T) {
	s := newRunSummary()
	s.iterations = 3
	s.issuesCreated = 5
	s.fixesApplied = 2
	s.finalVerdict = "APPROVE"

	if s.iterations != 3 {
		t.Errorf("iterations = %d, want 3", s.iterations)
	}
	if s.issuesCreated != 5 {
		t.Errorf("issuesCreated = %d, want 5", s.issuesCreated)
	}
	if s.fixesApplied != 2 {
		t.Errorf("fixesApplied = %d, want 2", s.fixesApplied)
	}
	if s.finalVerdict != "APPROVE" {
		t.Errorf("finalVerdict = %q, want APPROVE", s.finalVerdict)
	}
}

func TestRunSummaryPrintDoesNotPanic(t *testing.T) {
	s := newRunSummary()
	s.iterations = 1
	s.issuesCreated = 2
	s.fixesApplied = 1
	s.fixesReverted = 0
	s.finalVerdict = "REQUEST CHANGES"
	// Should not panic regardless of logPath value.
	s.print("")
	s.print("/tmp/fake.jsonl")
}
