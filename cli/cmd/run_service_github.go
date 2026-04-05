package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	gogithub "github.com/google/go-github/v67/github"

	"github.com/talk/opencode-client/internal/git"
	gh "github.com/talk/opencode-client/internal/github"
	"github.com/talk/opencode-client/internal/logger"
	"github.com/talk/opencode-client/internal/review"
	"github.com/talk/opencode-client/internal/types"
)

type githubContext struct {
	client *gogithub.Client
	owner  string
	repo   string
	prNum  int
}

func initGitHubContext(env runEnvironment, cfg runConfig) (githubContext, error) {
	if !cfg.needsGitHubClient() {
		return githubContext{}, nil
	}
	ghClient, err := gh.NewClient(env.ctx)
	if err != nil {
		return githubContext{}, wrapErr("GitHub", err)
	}
	ghOwner, ghRepo, err := gh.RepoOwnerName(env.repoRoot)
	if err != nil {
		return githubContext{}, wrapErr("GitHub", err)
	}

	ghCtx := githubContext{client: ghClient, owner: ghOwner, repo: ghRepo}
	if cfg.needsPRContext() {
		ghCtx.prNum, err = ensurePRWithOutput(env.ctx, ghClient, ghOwner, ghRepo, env.repoRoot, cfg.baseBranch, cfg.requiresPRSetup())
		if err != nil {
			return githubContext{}, err
		}
	}
	return ghCtx, nil
}

func ensurePRWithOutput(ctx context.Context, ghClient *gogithub.Client, owner, repo, repoRoot, baseBranch string, required bool) (int, error) {
	prNum, created, err := gh.EnsureOpenPR(ctx, ghClient, owner, repo, repoRoot, baseBranch)
	if err != nil {
		if required {
			return 0, wrapErr("GitHub PR", err)
		}
		fmt.Fprintf(os.Stderr, "Warning: GitHub PR setup failed: %v\n", err)
		return 0, nil
	}
	if created {
		fmt.Printf("GitHub: created %s/%s PR #%d\n", owner, repo, prNum)
	} else {
		fmt.Printf("GitHub: %s/%s PR #%d\n", owner, repo, prNum)
	}
	return prNum, nil
}

// validateIssuesFn is the default implementation passed to runIssueValidation.
var validateIssuesFn = gh.ValidateIssues

func runIssueValidation(ctx context.Context, validate bool, ghClient *gogithub.Client,
	owner, repo, repoRoot string,
	validateFn func(context.Context, *gogithub.Client, string, string, string) ([]gh.IssueValidity, error)) {

	if !validate || ghClient == nil {
		return
	}
	fmt.Println("Validating open issues against current codebase...")
	validations, err := validateFn(ctx, ghClient, owner, repo, repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: issue validation failed: %v\n", err)
		return
	}
	printIssueValidation(validations)
	reconcileStaleIssues(ctx, ghClient, owner, repo, validations)
}

func printIssueValidation(validations []gh.IssueValidity) {
	for _, v := range validations {
		if v.Valid {
			fmt.Printf("  #%d ✓ %s\n    %s\n", v.Number, v.Title, v.Reason)
			continue
		}
		fmt.Printf("  #%d ✗ STALE — %s\n    Closing: %s\n", v.Number, v.Title, v.Reason)
	}
}

func reconcileStaleIssues(ctx context.Context, ghClient *gogithub.Client, owner, repo string, validations []gh.IssueValidity) {
	for _, v := range validations {
		if v.Valid {
			continue
		}
		_ = closeIssueWithReporting(ctx, ghClient, owner, repo, v.Number, issueCloseOptions{
			preCloseComment: fmt.Sprintf("Closing as stale: %s", v.Reason),
			onCommentError: func(issueNum int, err error) {
				fmt.Fprintf(os.Stderr, "  Warning: failed to comment on #%d: %v\n", issueNum, err)
			},
			onCloseError: func(issueNum int, err error) {
				fmt.Fprintf(os.Stderr, "  Warning: failed to close #%d: %v\n", issueNum, err)
			},
		})
	}
}

func postPRReviewIfRequested(ctx context.Context, ghCtx githubContext, cfg runConfig, reviewText, verdict string) {
	if !cfg.postPR || reviewText == "" {
		return
	}
	fmt.Printf("Posting PR review (%s)...\n", verdict)
	if err := gh.PostPRReview(ctx, ghCtx.client, ghCtx.owner, ghCtx.repo, ghCtx.prNum, reviewText, verdict); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to post PR review: %v\n", err)
		return
	}
	fmt.Println("Posted.")
}

func maybeMergeOnApprove(env runEnvironment, ghCtx githubContext, cfg runConfig, result iterationResult, openIssues []int) (bool, error) {
	if result.verdict != "APPROVE" {
		return false, nil
	}
	if err := mergeAndClose(env.ctx, ghCtx.client, ghCtx.owner, ghCtx.repo, ghCtx.prNum, env.repoRoot, result.hash, cfg.mergeStrategy, cfg.deleteBranch, env.log); err != nil {
		return false, wrapErr("Failed to merge", err)
	}
	if cfg.createIssues {
		if err := closeRunIssues(env.ctx, ghCtx.client, ghCtx.owner, ghCtx.repo, openIssues, env.log); err != nil {
			return false, wrapErr("Failed to close issues", err)
		}
	}
	return true, nil
}

func fileIssues(ctx context.Context, ghClient *gogithub.Client, owner, repo string, prNum int,
	reviewText string, openIssues []int, log *logger.Logger) ([]int, error) {

	findings := review.ParseFindings(reviewText)
	seen, err := gh.ExistingIssueTitles(ctx, ghClient, owner, repo)
	if err != nil {
		return openIssues, wrapErr("failed to list existing issues", err)
	}
	newIssues, updatedOpen, err := createIssuesForFindings(ctx, ghClient, owner, repo, prNum, findings, seen, openIssues, log)
	if err != nil {
		return openIssues, err
	}
	emitIssueSummary(findings, newIssues)
	if err := linkIssuesIfNeeded(ctx, ghClient, owner, repo, prNum, newIssues); err != nil {
		fmt.Fprintf(os.Stderr, "  Failed to link issues to PR: %v\n", err)
	}
	return updatedOpen, nil
}

func createIssuesForFindings(ctx context.Context, ghClient *gogithub.Client, owner, repo string, prNum int,
	findings []types.Finding, seen map[string]bool, openIssues []int, log *logger.Logger) ([]int, []int, error) {
	var newIssues []int
	updatedOpen := openIssues
	for _, f := range findings {
		num, created, err := createIssueForFinding(ctx, ghClient, owner, repo, f, seen)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Failed to create issue for %q: %v\n", f.Title, err)
			continue
		}
		if !created {
			continue
		}
		emitIssueCreated(log, f, num)
		newIssues = append(newIssues, num)
		updatedOpen = append(updatedOpen, num)
		commentIssueWithPRIfNeeded(ctx, ghClient, owner, repo, prNum, num)
	}
	return newIssues, updatedOpen, nil
}

func createIssueForFinding(ctx context.Context, ghClient *gogithub.Client, owner, repo string,
	f types.Finding, seen map[string]bool) (int, bool, error) {
	ghTitle := gh.FindingIssueTitle(f)
	if seen[ghTitle] {
		fmt.Printf("  Skipping (already open): %s\n", ghTitle)
		return 0, false, nil
	}
	num, err := gh.CreateIssue(ctx, ghClient, owner, repo, f)
	if err != nil {
		return 0, false, err
	}
	seen[ghTitle] = true
	return num, true, nil
}

func commentIssueWithPRIfNeeded(ctx context.Context, ghClient *gogithub.Client, owner, repo string, prNum, issueNum int) {
	if prNum <= 0 {
		return
	}
	prRef := fmt.Sprintf("Tracking in PR #%d.", prNum)
	if err := gh.CommentOnIssue(ctx, ghClient, owner, repo, issueNum, prRef); err != nil {
		fmt.Fprintf(os.Stderr, "  Warning: could not comment on issue #%d: %v\n", issueNum, err)
	}
}

func emitIssueCreated(log *logger.Logger, finding types.Finding, issueNum int) {
	fmt.Printf("  #%d [%s] %s:%s — %s\n", issueNum, finding.Severity, finding.File, finding.LineRange, finding.Title)
	logEvent(log, map[string]any{
		"event":     "issue_created",
		"issue":     issueNum,
		"severity":  finding.Severity,
		"file":      finding.File,
		"lineRange": finding.LineRange,
		"title":     finding.Title,
	})
}

func emitIssueSummary(findings []types.Finding, newIssues []int) {
	if len(newIssues) == 0 && len(findings) > 0 {
		fmt.Println("  No new findings (all already tracked).")
	}
}

func linkIssuesIfNeeded(ctx context.Context, ghClient *gogithub.Client, owner, repo string, prNum int, newIssues []int) error {
	if prNum <= 0 || len(newIssues) == 0 {
		return nil
	}
	if err := gh.LinkIssuesToPR(ctx, ghClient, owner, repo, prNum, newIssues); err != nil {
		return err
	}
	fmt.Println("  Issues linked to PR.")
	return nil
}

func mergeAndClose(ctx context.Context, ghClient *gogithub.Client, owner, repo string, prNum int,
	repoRoot, hash, strategy string, deleteBranch bool, log *logger.Logger) error {
	fmt.Println("\nAll issues resolved — merging PR...")
	if err := verifyReviewedHead(repoRoot, hash); err != nil {
		return err
	}
	if err := executePRMerge(ctx, ghClient, owner, repo, prNum, strategy, deleteBranch); err != nil {
		return err
	}
	fmt.Println("Merged.")
	logEvent(log, map[string]any{"event": "pr_merged", "strategy": strategy})
	return nil
}

func verifyReviewedHead(repoRoot, hash string) error {
	currentHead, err := git.Run(repoRoot, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if hash != currentHead {
		return fmt.Errorf("refusing to merge: reviewed commit is not current HEAD")
	}
	return nil
}

func executePRMerge(ctx context.Context, ghClient *gogithub.Client, owner, repo string, prNum int, strategy string, deleteBranch bool) error {
	return gh.MergePR(ctx, ghClient, owner, repo, prNum, strategy, deleteBranch)
}

func closeRunIssues(ctx context.Context, ghClient *gogithub.Client, owner, repo string, openIssues []int, log *logger.Logger) error {
	closeNums := map[int]bool{}
	for _, n := range openIssues {
		closeNums[n] = true
	}

	var closeErr error
	if len(closeNums) > 0 {
		fmt.Printf("Closing %d opencode-review issue(s)...\n", len(closeNums))
		for n := range closeNums {
			closeErr = errors.Join(closeErr, closeIssueWithReporting(ctx, ghClient, owner, repo, n, issueCloseOptions{
				onCloseError: func(issueNum int, err error) {
					fmt.Fprintf(os.Stderr, "  Failed to close #%d: %v\n", issueNum, err)
				},
				onClosed: func(issueNum int) {
					fmt.Printf("  Closed #%d\n", issueNum)
					logEvent(log, map[string]any{"event": "issue_closed", "issue": issueNum})
				},
			}))
		}
	}
	if closeErr != nil {
		fmt.Fprintln(os.Stderr, "Warning: some issues could not be closed.")
		return closeErr
	}
	return nil
}

type issueCloseOptions struct {
	preCloseComment string
	onCommentError  func(issueNum int, err error)
	onCloseError    func(issueNum int, err error)
	onClosed        func(issueNum int)
}

func closeIssueWithReporting(ctx context.Context, ghClient *gogithub.Client, owner, repo string, issueNum int, opts issueCloseOptions) error {
	if opts.preCloseComment != "" {
		if err := gh.CommentOnIssue(ctx, ghClient, owner, repo, issueNum, opts.preCloseComment); err != nil && opts.onCommentError != nil {
			opts.onCommentError(issueNum, err)
		}
	}
	if err := gh.CloseIssue(ctx, ghClient, owner, repo, issueNum); err != nil {
		if opts.onCloseError != nil {
			opts.onCloseError(issueNum, err)
		}
		return fmt.Errorf("issue #%d: %w", issueNum, err)
	}
	if opts.onClosed != nil {
		opts.onClosed(issueNum)
	}
	return nil
}
