package github

import (
	"context"

	"github.com/talk/opencode-client/internal/types"
)

// Port is the interface through which cmd/ interacts with GitHub.
// Encapsulating owner/repo and authentication into the implementation
// enables a NoOpClient for --dry-run and mock clients for unit tests
// without any change to calling code.
type Port interface {
	EnsureOpenPR(ctx context.Context, repoRoot, baseBranch string) (prNum int, created bool, err error)
	PostPRReview(ctx context.Context, prNum int, body, verdict string) error
	ValidateIssues(ctx context.Context, repoRoot string) ([]IssueValidity, error)
	FetchIssueIndex(ctx context.Context) (IssueIndex, error)
	ExistingIssueTitles(ctx context.Context) (map[string]bool, error)
	ExistingFingerprints(ctx context.Context) (map[string]int, error)
	ListOpenIssueSummaries(ctx context.Context) ([]types.IssueSummary, error)
	CreateIssue(ctx context.Context, f types.Finding) (int, error)
	CommentOnIssue(ctx context.Context, issueNum int, body string) error
	CloseIssue(ctx context.Context, issueNum int) error
	LinkIssuesToPR(ctx context.Context, prNum int, issueNums []int) error
	MergePR(ctx context.Context, prNum int, strategy string, deleteBranch bool) error
}
