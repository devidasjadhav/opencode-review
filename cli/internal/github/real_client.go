package github

import (
	"context"

	gogithub "github.com/google/go-github/v67/github"
	"github.com/talk/opencode-client/internal/types"
)

// RealClient implements Port against the live GitHub API.
// It holds owner/repo so callers never need to pass them explicitly.
type RealClient struct {
	client *gogithub.Client
	owner  string
	repo   string
}

// NewRealClient creates a RealClient authenticated via GITHUB_TOKEN.
func NewRealClient(ctx context.Context, owner, repo string) (*RealClient, error) {
	client, err := NewClient(ctx)
	if err != nil {
		return nil, err
	}
	return &RealClient{client: client, owner: owner, repo: repo}, nil
}

func (c *RealClient) EnsureOpenPR(ctx context.Context, repoRoot, baseBranch string) (int, bool, error) {
	return EnsureOpenPR(ctx, c.client, c.owner, c.repo, repoRoot, baseBranch)
}

func (c *RealClient) PostPRReview(ctx context.Context, prNum int, body, verdict string) error {
	return PostPRReview(ctx, c.client, c.owner, c.repo, prNum, body, verdict)
}

func (c *RealClient) ValidateIssues(ctx context.Context, repoRoot string) ([]IssueValidity, error) {
	return ValidateIssues(ctx, c.client, c.owner, c.repo, repoRoot)
}

func (c *RealClient) ExistingIssueTitles(ctx context.Context) (map[string]bool, error) {
	return ExistingIssueTitles(ctx, c.client, c.owner, c.repo)
}

func (c *RealClient) ExistingFingerprints(ctx context.Context) (map[string]int, error) {
	return ExistingFingerprints(ctx, c.client, c.owner, c.repo)
}

func (c *RealClient) ListOpenIssueSummaries(ctx context.Context) ([]types.IssueSummary, error) {
	return ListOpenIssueSummaries(ctx, c.client, c.owner, c.repo)
}

func (c *RealClient) CreateIssue(ctx context.Context, f types.Finding) (int, error) {
	return CreateIssue(ctx, c.client, c.owner, c.repo, f)
}

func (c *RealClient) CommentOnIssue(ctx context.Context, issueNum int, body string) error {
	return CommentOnIssue(ctx, c.client, c.owner, c.repo, issueNum, body)
}

func (c *RealClient) CloseIssue(ctx context.Context, issueNum int) error {
	return CloseIssue(ctx, c.client, c.owner, c.repo, issueNum)
}

func (c *RealClient) LinkIssuesToPR(ctx context.Context, prNum int, issueNums []int) error {
	return LinkIssuesToPR(ctx, c.client, c.owner, c.repo, prNum, issueNums)
}

func (c *RealClient) MergePR(ctx context.Context, prNum int, strategy string, deleteBranch bool) error {
	return MergePR(ctx, c.client, c.owner, c.repo, prNum, strategy, deleteBranch)
}
