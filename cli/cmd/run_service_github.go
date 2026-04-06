package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/talk/opencode-client/internal/git"
	gh "github.com/talk/opencode-client/internal/github"
	"github.com/talk/opencode-client/internal/logger"
	"github.com/talk/opencode-client/internal/review"
	"github.com/talk/opencode-client/internal/types"
)

type githubContext struct {
	gh    gh.Port // nil when no GitHub integration is needed
	owner string  // retained for display ("owner/repo PR #N")
	repo  string
	prNum int
}

func initGitHubContext(env runEnvironment, cfg runConfig) (githubContext, error) {
	if !cfg.needsGitHubClient() {
		return githubContext{}, nil
	}

	owner, repo, err := gh.RepoOwnerName(env.repoRoot)
	if err != nil {
		return githubContext{}, wrapErr("GitHub", err)
	}

	var port gh.Port
	if cfg.dryRun {
		port = gh.NoOpClient{}
	} else {
		rc, err := gh.NewRealClient(env.ctx, owner, repo)
		if err != nil {
			return githubContext{}, wrapErr("GitHub", err)
		}
		port = rc
	}

	ghCtx := githubContext{gh: port, owner: owner, repo: repo}
	if cfg.needsPRContext() {
		ghCtx.prNum, err = ensurePRWithOutput(env.ctx, port, owner, repo, env.repoRoot, cfg.baseBranch, cfg.requiresPRSetup())
		if err != nil {
			return githubContext{}, err
		}
	}
	return ghCtx, nil
}

func ensurePRWithOutput(ctx context.Context, port gh.PRPort, owner, repo, repoRoot, baseBranch string, required bool) (int, error) {
	prNum, created, err := port.EnsureOpenPR(ctx, repoRoot, baseBranch)
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

func runIssueValidation(ctx context.Context, validate bool, port gh.ValidationPort, repoRoot string) {
	if !validate || port == nil {
		return
	}
	fmt.Println("Validating open issues against current codebase...")
	validations, err := port.ValidateIssues(ctx, repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: issue validation failed: %v\n", err)
		return
	}
	printIssueValidation(validations)
	reconcileStaleIssues(ctx, port, validations)
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

func reconcileStaleIssues(ctx context.Context, port gh.IssueCloserPort, validations []gh.IssueValidity) {
	for _, v := range validations {
		if v.Valid {
			continue
		}
		_ = closeIssueWithReporting(ctx, port, v.Number, issueCloseOptions{
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
	if err := ghCtx.gh.PostPRReview(ctx, ghCtx.prNum, reviewText, verdict); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to post PR review: %v\n", err)
		return
	}
	fmt.Println("Posted.")
}

func canMergeOnApprove(cfg runConfig, verdict string) bool {
	return cfg.mergeOnApprove && verdict == "APPROVE"
}

func maybeMergeOnApprove(env runEnvironment, ghCtx githubContext, cfg runConfig, result iterationResult, openIssues []int) (bool, error) {
	if !canMergeOnApprove(cfg, result.verdict) {
		return false, nil
	}
	if err := mergeAndClose(env.ctx, ghCtx.gh, ghCtx.prNum, env.repoRoot, result.hash, cfg.mergeStrategy, cfg.deleteBranch, env.log); err != nil {
		return false, wrapErr("Failed to merge", err)
	}
	if cfg.createIssues {
		if err := closeRunIssues(env.ctx, ghCtx.gh, openIssues, env.log); err != nil {
			return false, wrapErr("Failed to close issues", err)
		}
	}
	return true, nil
}

func fileIssues(ctx context.Context, port gh.IssueFilerPort, prNum int,
	reviewText string, openIssues []int, log *logger.Logger) ([]int, error) {

	findings := review.ParseFindings(reviewText)
	idx, err := port.FetchIssueIndex(ctx)
	if err != nil {
		return openIssues, wrapErr("failed to fetch issue index", err)
	}
	newIssues, updatedOpen, err := createIssuesForFindings(ctx, port, prNum, findings, idx.Titles, idx.Fingerprints, openIssues, log)
	if err != nil {
		return openIssues, err
	}
	emitIssueSummary(findings, newIssues)
	if err := linkIssuesIfNeeded(ctx, port, prNum, newIssues); err != nil {
		fmt.Fprintf(os.Stderr, "  Failed to link issues to PR: %v\n", err)
	}
	return updatedOpen, nil
}

func createIssuesForFindings(ctx context.Context, port gh.IssueFilerPort, prNum int,
	findings []types.Finding, seen map[string]bool, fingerprints map[string]int, openIssues []int, log *logger.Logger) ([]int, []int, error) {
	var newIssues []int
	updatedOpen := openIssues
	for _, f := range findings {
		num, created, err := createIssueForFinding(ctx, port, f, seen, fingerprints)
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
		commentIssueWithPRIfNeeded(ctx, port, prNum, num)
	}
	return newIssues, updatedOpen, nil
}

func createIssueForFinding(ctx context.Context, port gh.IssueFilerPort,
	f types.Finding, seen map[string]bool, fingerprints map[string]int) (int, bool, error) {
	ghTitle := gh.FindingIssueTitle(f)
	// Fingerprint check first (more robust), title as fallback.
	if fingerprints[f.Fingerprint] != 0 {
		fmt.Printf("  Skipping (fingerprint match #%d): %s\n", fingerprints[f.Fingerprint], ghTitle)
		return 0, false, nil
	}
	if seen[ghTitle] {
		fmt.Printf("  Skipping (already open): %s\n", ghTitle)
		return 0, false, nil
	}
	num, err := port.CreateIssue(ctx, f)
	if err != nil {
		return 0, false, err
	}
	seen[ghTitle] = true
	return num, true, nil
}

func commentIssueWithPRIfNeeded(ctx context.Context, port gh.IssueFilerPort, prNum, issueNum int) {
	if prNum <= 0 {
		return
	}
	prRef := fmt.Sprintf("Tracking in PR #%d.", prNum)
	if err := port.CommentOnIssue(ctx, issueNum, prRef); err != nil {
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

func linkIssuesIfNeeded(ctx context.Context, port gh.IssueFilerPort, prNum int, newIssues []int) error {
	if prNum <= 0 || len(newIssues) == 0 {
		return nil
	}
	if err := port.LinkIssuesToPR(ctx, prNum, newIssues); err != nil {
		return err
	}
	fmt.Println("  Issues linked to PR.")
	return nil
}

func mergeAndClose(ctx context.Context, port gh.PRPort, prNum int,
	repoRoot, hash, strategy string, deleteBranch bool, log *logger.Logger) error {
	fmt.Println("\nAll issues resolved — merging PR...")
	if err := verifyReviewedHead(repoRoot, hash); err != nil {
		return err
	}
	if err := port.MergePR(ctx, prNum, strategy, deleteBranch); err != nil {
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

func closeRunIssues(ctx context.Context, port gh.IssueCloserPort, openIssues []int, log *logger.Logger) error {
	closeNums := map[int]bool{}
	for _, n := range openIssues {
		closeNums[n] = true
	}

	var closeErr error
	if len(closeNums) > 0 {
		fmt.Printf("Closing %d opencode-review issue(s)...\n", len(closeNums))
		for n := range closeNums {
			closeErr = errors.Join(closeErr, closeIssueWithReporting(ctx, port, n, issueCloseOptions{
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

func closeIssueWithReporting(ctx context.Context, port gh.IssueCloserPort, issueNum int, opts issueCloseOptions) error {
	if opts.preCloseComment != "" {
		if err := port.CommentOnIssue(ctx, issueNum, opts.preCloseComment); err != nil && opts.onCommentError != nil {
			opts.onCommentError(issueNum, err)
		}
	}
	if err := port.CloseIssue(ctx, issueNum); err != nil {
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
