package github

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
func ExistingIssueTitles(ctx context.Context, gh *gogithub.Client, owner, repo string) (map[string]bool, error) {
	issues, err := ListOpenIssues(ctx, gh, owner, repo)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(issues))
	for _, i := range issues {
		seen[i.GetTitle()] = true
	}
	return seen, nil
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

// CloseIssue closes issue state only.
func CloseIssue(ctx context.Context, gh *gogithub.Client, owner, repo string, num int) error {
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
	if err != nil && event != "COMMENT" && shouldFallbackToComment(err) {
		fmt.Fprintln(os.Stderr, "(falling back to COMMENT review)")
		comment := "COMMENT"
		_, _, err = gh.PullRequests.CreateReview(ctx, owner, repo, prNum, &gogithub.PullRequestReviewRequest{
			Body:  &body,
			Event: &comment,
		})
	}
	return err
}

func shouldFallbackToComment(err error) bool {
	var ghErr *gogithub.ErrorResponse
	if !errors.As(err, &ghErr) {
		return false
	}
	if ghErr.Response == nil || ghErr.Response.StatusCode != 422 {
		return false
	}
	return true
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

// EnsureOpenPR returns the open PR number for the current branch, creating one if none exists.
func EnsureOpenPR(ctx context.Context, gh *gogithub.Client, owner, repo, repoRoot, base string) (int, bool, error) {
	num, err := OpenPRNumber(ctx, gh, owner, repo, repoRoot)
	if err == nil {
		return num, false, nil
	}
	branch, err2 := git.Run(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err2 != nil {
		return 0, false, err2
	}
	if branch == base {
		return 0, false, fmt.Errorf("cannot create PR: already on base branch %q", base)
	}
	title := fmt.Sprintf("fix: changes on %s", branch)
	body := ""
	pr, _, err2 := gh.PullRequests.Create(ctx, owner, repo, &gogithub.NewPullRequest{
		Title: &title,
		Head:  &branch,
		Base:  &base,
		Body:  &body,
	})
	if err2 != nil {
		return 0, false, fmt.Errorf("failed to create PR for branch %q: %w", branch, err2)
	}
	return pr.GetNumber(), true, nil
}

// issueLocRe parses "[Severity] file:startLine[-endLine] — title" from issue titles.
var issueLocRe = regexp.MustCompile(`\]\s+(\S+):(\d+)`)

// IssueValidity reports whether a GitHub issue is still applicable to the current code.
type IssueValidity struct {
	Number int
	Title  string
	Valid  bool
	Reason string
}

// ValidateIssues checks each open opencode-review issue against the current codebase.
// An issue is considered stale if its reported file no longer exists or the reported
// start line no longer exists in the file.
func ValidateIssues(ctx context.Context, gh *gogithub.Client, owner, repo, repoRoot string) ([]IssueValidity, error) {
	issues, err := ListOpenIssues(ctx, gh, owner, repo)
	if err != nil {
		return nil, err
	}

	var results []IssueValidity
	for _, issue := range issues {
		body := issue.GetBody()
		if !strings.Contains(body, "_Reported by opencode-review_") {
			continue
		}
		v := IssueValidity{Number: issue.GetNumber(), Title: issue.GetTitle()}

		m := issueLocRe.FindStringSubmatch(issue.GetTitle())
		if m == nil {
			v.Valid = true
			v.Reason = "no location in title — assuming still valid"
			results = append(results, v)
			continue
		}

		relFile := m[1]
		startLine, _ := strconv.Atoi(m[2])

		absFile := filepath.Join(repoRoot, relFile)
		f, err := os.Open(absFile)
		if err != nil {
			v.Valid = false
			v.Reason = fmt.Sprintf("file %q no longer exists", relFile)
			results = append(results, v)
			continue
		}

		lineCount := 0
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			lineCount++
		}
		f.Close()

		if startLine > lineCount {
			v.Valid = false
			v.Reason = fmt.Sprintf("reported line %d exceeds current file length (%d lines)", startLine, lineCount)
		} else {
			v.Valid = true
			v.Reason = fmt.Sprintf("file exists and line %d is present (%d lines total)", startLine, lineCount)
		}
		results = append(results, v)
	}
	return results, nil
}
