package github

import (
	"context"

	gogithub "github.com/google/go-github/v67/github"
	"github.com/talk/opencode-client/internal/apperr"
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
	err := PostPRReview(ctx, c.client, c.owner, c.repo, prNum, body, verdict)
	if err != nil {
		return apperr.ClassifyGitHub("post PR review", err)
	}
	return nil
}

func (c *RealClient) ValidateIssues(ctx context.Context, repoRoot string) ([]IssueValidity, error) {
	return ValidateIssues(ctx, c.client, c.owner, c.repo, repoRoot)
}

func (c *RealClient) FetchIssueIndex(ctx context.Context) (IssueIndex, error) {
	return BuildIssueIndex(ctx, c.client, c.owner, c.repo)
}

func (c *RealClient) CreateIssue(ctx context.Context, f types.Finding) (int, error) {
	num, err := CreateIssue(ctx, c.client, c.owner, c.repo, f)
	if err != nil {
		return 0, apperr.ClassifyGitHub("create issue", err)
	}
	return num, nil
}

func (c *RealClient) CommentOnIssue(ctx context.Context, issueNum int, body string) error {
	err := CommentOnIssue(ctx, c.client, c.owner, c.repo, issueNum, body)
	if err != nil {
		return apperr.ClassifyGitHub("comment on issue", err)
	}
	return nil
}

func (c *RealClient) CloseIssue(ctx context.Context, issueNum int) error {
	err := CloseIssue(ctx, c.client, c.owner, c.repo, issueNum)
	if err != nil {
		return apperr.ClassifyGitHub("close issue", err)
	}
	return nil
}

func (c *RealClient) LinkIssuesToPR(ctx context.Context, prNum int, issueNums []int) error {
	err := LinkIssuesToPR(ctx, c.client, c.owner, c.repo, prNum, issueNums)
	if err != nil {
		return apperr.ClassifyGitHub("link issues to PR", err)
	}
	return nil
}

func (c *RealClient) MergePR(ctx context.Context, prNum int, strategy string, deleteBranch bool) error {
	err := MergePR(ctx, c.client, c.owner, c.repo, prNum, strategy, deleteBranch)
	if err != nil {
		return apperr.ClassifyGitHub("merge PR", err)
	}
	return nil
}
