package github

import (
	"context"
	"fmt"
	"os"
	"strings"

	gogithub "github.com/google/go-github/v67/github"
	"golang.org/x/oauth2"

	"github.com/talk/opencode-client/internal/git"
	"github.com/talk/opencode-client/internal/types"
)

// NewClient builds a GitHub API client authenticated via GITHUB_TOKEN.
func NewClient(ctx context.Context) (*gogithub.Client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN environment variable not set")
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	return gogithub.NewClient(oauth2.NewClient(ctx, ts)), nil
}

// RepoOwnerName parses owner and repo from the git remote URL.
func RepoOwnerName(repoRoot string) (owner, repo string, err error) {
	out, err := git.Run(repoRoot, "remote", "get-url", "origin")
	if err != nil {
		return "", "", fmt.Errorf("cannot read git remote: %w", err)
	}
	out = strings.TrimSuffix(out, ".git")
	for _, prefix := range []string{"github.com/", "github.com:"} {
		if idx := strings.Index(out, prefix); idx >= 0 {
			parts := strings.SplitN(out[idx+len(prefix):], "/", 2)
			if len(parts) == 2 {
				return parts[0], parts[1], nil
			}
		}
	}
	return "", "", fmt.Errorf("cannot parse GitHub owner/repo from remote %q", out)
}

// ListOpenIssues fetches all open non-PR issues paginating automatically.
func ListOpenIssues(ctx context.Context, gh *gogithub.Client, owner, repo string) ([]*gogithub.Issue, error) {
	opts := &gogithub.IssueListByRepoOptions{State: "open", ListOptions: gogithub.ListOptions{PerPage: 100}}
	var all []*gogithub.Issue
	for {
		issues, resp, err := gh.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, err
		}
		for _, i := range issues {
			if i.PullRequestLinks == nil {
				all = append(all, i)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// ExistingIssueTitles returns a set of open issue titles for deduplication.
func ExistingIssueTitles(ctx context.Context, gh *gogithub.Client, owner, repo string) map[string]bool {
	issues, _ := ListOpenIssues(ctx, gh, owner, repo)
	seen := make(map[string]bool, len(issues))
	for _, i := range issues {
		seen[i.GetTitle()] = true
	}
	return seen
}

// FindingIssueTitle returns the canonical GitHub issue title for a finding.
func FindingIssueTitle(f types.Finding) string {
	return fmt.Sprintf("[%s] %s:%s — %s", f.Severity, f.File, f.LineRange, f.Title)
}

// CreateIssue opens a new GitHub issue from a Finding and returns its number.
func CreateIssue(ctx context.Context, gh *gogithub.Client, owner, repo string, f types.Finding) (int, error) {
	title := FindingIssueTitle(f)
	var body strings.Builder
	fmt.Fprintf(&body, "## %s\n\n", f.Title)
	fmt.Fprintf(&body, "**Severity:** %s  \n", f.Severity)
	fmt.Fprintf(&body, "**Location:** `%s:%s`\n\n", f.File, f.LineRange)
	body.WriteString("### Description\n")
	body.WriteString(strings.TrimSpace(f.Description))
	body.WriteString("\n\n")
	if f.Diff != "" {
		body.WriteString("### Suggested fix\n```diff\n")
		body.WriteString(f.Diff)
		body.WriteString("\n```\n\n")
	}
	if f.AgentPrompt != "" {
		body.WriteString("### AI agent fix prompt\n")
		body.WriteString(f.AgentPrompt)
		body.WriteString("\n")
	}
	body.WriteString("\n---\n_Reported by opencode-review_\n")

	bodyStr := body.String()
	issue, _, err := gh.Issues.Create(ctx, owner, repo, &gogithub.IssueRequest{
		Title: &title,
		Body:  &bodyStr,
	})
	if err != nil {
		return 0, err
	}
	return issue.GetNumber(), nil
}

// CloseIssue adds a closing comment then closes the issue.
func CloseIssue(ctx context.Context, gh *gogithub.Client, owner, repo string, num int) error {
	comment := "Fixed and merged. Closing."
	if _, _, err := gh.Issues.CreateComment(ctx, owner, repo, num, &gogithub.IssueComment{Body: &comment}); err != nil {
		return err
	}
	state := "closed"
	_, _, err := gh.Issues.Edit(ctx, owner, repo, num, &gogithub.IssueRequest{State: &state})
	return err
}

// CommentOnIssue adds a comment to an existing issue.
func CommentOnIssue(ctx context.Context, gh *gogithub.Client, owner, repo string, num int, body string) error {
	_, _, err := gh.Issues.CreateComment(ctx, owner, repo, num, &gogithub.IssueComment{Body: &body})
	return err
}

// LinkIssuesToPR appends "Closes #N" lines to the PR body.
func LinkIssuesToPR(ctx context.Context, gh *gogithub.Client, owner, repo string, prNum int, issueNums []int) error {
	if len(issueNums) == 0 {
		return nil
	}
	pr, _, err := gh.PullRequests.Get(ctx, owner, repo, prNum)
	if err != nil {
		return err
	}
	var closes []string
	for _, n := range issueNums {
		closes = append(closes, fmt.Sprintf("Closes #%d", n))
	}
	newBody := pr.GetBody() + "\n\n" + strings.Join(closes, "\n")
	_, _, err = gh.PullRequests.Edit(ctx, owner, repo, prNum, &gogithub.PullRequest{Body: &newBody})
	return err
}

// PostPRReview creates a PR review, falling back to COMMENT on own-PR restrictions.
func PostPRReview(ctx context.Context, gh *gogithub.Client, owner, repo string, prNum int, body, verdict string) error {
	event := "COMMENT"
	switch {
	case strings.Contains(verdict, "REQUEST CHANGES"):
		event = "REQUEST_CHANGES"
	case strings.Contains(verdict, "APPROVE"):
		event = "APPROVE"
	}

	_, _, err := gh.PullRequests.CreateReview(ctx, owner, repo, prNum, &gogithub.PullRequestReviewRequest{
		Body:  &body,
		Event: &event,
	})
	if err != nil && event != "COMMENT" {
		if strings.Contains(err.Error(), "Can not approve your own") ||
			strings.Contains(err.Error(), "own pull request") {
			fmt.Fprintln(os.Stderr, "(falling back to COMMENT: cannot approve/request-changes own PR)")
			comment := "COMMENT"
			_, _, err = gh.PullRequests.CreateReview(ctx, owner, repo, prNum, &gogithub.PullRequestReviewRequest{
				Body:  &body,
				Event: &comment,
			})
		}
	}
	return err
}

// MergePR merges the PR using the given strategy, optionally deleting the branch.
func MergePR(ctx context.Context, gh *gogithub.Client, owner, repo string, prNum int, strategy string, deleteBranch bool) error {
	method := map[string]string{"merge": "merge", "squash": "squash", "rebase": "rebase"}[strategy]
	if method == "" {
		return fmt.Errorf("invalid merge strategy %q: must be merge, squash, or rebase", strategy)
	}
	_, _, err := gh.PullRequests.Merge(ctx, owner, repo, prNum, "", &gogithub.PullRequestOptions{
		MergeMethod: method,
	})
	if err != nil {
		return err
	}
	if deleteBranch {
		pr, _, err2 := gh.PullRequests.Get(ctx, owner, repo, prNum)
		if err2 == nil && pr.Head != nil && pr.Head.Ref != nil {
			gh.Git.DeleteRef(ctx, owner, repo, "heads/"+pr.Head.GetRef()) //nolint
		}
	}
	return nil
}

// OpenPRNumber finds the open PR number for the current branch.
func OpenPRNumber(ctx context.Context, gh *gogithub.Client, owner, repo, repoRoot string) (int, error) {
	branch, err := git.Run(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return 0, err
	}
	headUser := owner
	if me, _, err2 := gh.Users.Get(ctx, ""); err2 == nil {
		headUser = me.GetLogin()
	}
	prs, _, err := gh.PullRequests.List(ctx, owner, repo, &gogithub.PullRequestListOptions{
		State: "open", Head: headUser + ":" + branch,
	})
	if err != nil {
		return 0, err
	}
	if len(prs) == 0 {
		return 0, fmt.Errorf("no open PR found for branch %q", branch)
	}
	return prs[0].GetNumber(), nil
}
