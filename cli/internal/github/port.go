package github

import (
	"context"

	"github.com/talk/opencode-client/internal/types"
)

// PRPort covers PR lifecycle operations.
type PRPort interface {
	EnsureOpenPR(ctx context.Context, repoRoot, baseBranch string) (prNum int, created bool, err error)
	PostPRReview(ctx context.Context, prNum int, body, verdict string) error
	MergePR(ctx context.Context, prNum int, strategy string, deleteBranch bool) error
}

// ValidationPort covers open-issue validation and stale-issue reconciliation.
type ValidationPort interface {
	ValidateIssues(ctx context.Context, repoRoot string) ([]IssueValidity, error)
	CommentOnIssue(ctx context.Context, issueNum int, body string) error
	CloseIssue(ctx context.Context, issueNum int) error
}

// IssueFilerPort covers finding-to-issue filing and PR linking.
// Closing issues uses the narrower issueCloser interface in cmd/.
type IssueFilerPort interface {
	FetchIssueIndex(ctx context.Context) (IssueIndex, error)
	CreateIssue(ctx context.Context, f types.Finding) (int, error)
	CommentOnIssue(ctx context.Context, issueNum int, body string) error
	LinkIssuesToPR(ctx context.Context, prNum int, issueNums []int) error
}

// Port is the full GitHub interface used by githubContext.
// It composes the three role interfaces so individual functions can accept
// the narrowest interface they actually need.
type Port interface {
	PRPort
	ValidationPort
	IssueFilerPort
}
