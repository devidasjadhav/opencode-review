package github

import (
	"context"
	"fmt"

	"github.com/talk/opencode-client/internal/types"
)

// NoOpClient implements Port without making any real API calls.
// Every write operation prints a "[dry-run] Would ..." line to stdout.
// Read operations return safe empty values so the rest of the loop
// runs normally and prints what it would do.
type NoOpClient struct{}

func (NoOpClient) EnsureOpenPR(_ context.Context, _, _ string) (int, bool, error) {
	fmt.Println("[dry-run] Would ensure open PR (skipped)")
	return 0, false, nil
}

func (NoOpClient) PostPRReview(_ context.Context, prNum int, _, verdict string) error {
	fmt.Printf("[dry-run] Would post PR #%d review: %s\n", prNum, verdict)
	return nil
}

func (NoOpClient) ValidateIssues(_ context.Context, _ string) ([]IssueValidity, error) {
	fmt.Println("[dry-run] Would validate open issues (skipped)")
	return nil, nil
}

func (NoOpClient) ExistingIssueTitles(_ context.Context) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (NoOpClient) ExistingFingerprints(_ context.Context) (map[string]int, error) {
	return map[string]int{}, nil
}

func (NoOpClient) ListOpenIssueSummaries(_ context.Context) ([]types.IssueSummary, error) {
	return nil, nil
}

func (NoOpClient) CreateIssue(_ context.Context, f types.Finding) (int, error) {
	fmt.Printf("[dry-run] Would create issue: [%s] %s:%s — %s\n", f.Severity, f.File, f.LineRange, f.Title)
	return 0, nil
}

func (NoOpClient) CommentOnIssue(_ context.Context, issueNum int, body string) error {
	fmt.Printf("[dry-run] Would comment on #%d: %s\n", issueNum, body)
	return nil
}

func (NoOpClient) CloseIssue(_ context.Context, issueNum int) error {
	fmt.Printf("[dry-run] Would close issue #%d\n", issueNum)
	return nil
}

func (NoOpClient) LinkIssuesToPR(_ context.Context, prNum int, issueNums []int) error {
	fmt.Printf("[dry-run] Would link issues %v to PR #%d\n", issueNums, prNum)
	return nil
}

func (NoOpClient) MergePR(_ context.Context, prNum int, strategy string, _ bool) error {
	fmt.Printf("[dry-run] Would merge PR #%d with strategy %q\n", prNum, strategy)
	return nil
}
