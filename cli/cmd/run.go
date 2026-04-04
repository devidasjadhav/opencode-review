package cmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	gogithub "github.com/google/go-github/v67/github"

	"github.com/talk/opencode-client/internal/git"
	gh "github.com/talk/opencode-client/internal/github"
	"github.com/talk/opencode-client/internal/logger"
	occ "github.com/talk/opencode-client/internal/opencode"
	"github.com/talk/opencode-client/internal/review"
	"github.com/talk/opencode-client/internal/types"
)

// Run parses flags and executes the review/fix/merge loop.
func Run() {
	serverURL := flag.String("url", "http://localhost:4096", "Opencode server URL")
	dirFlag := flag.String("dir", "", "Git repo directory (default: current directory)")
	modelNum := flag.Int("model", 0, "Model number from list (skips interactive selection)")
	commitRef := flag.String("commit", "HEAD", "Git ref to review (hash, branch, tag)")
	auditMode := flag.Bool("audit", false, "Full SOLID/DRY audit of entire codebase instead of single commit review")
	postPR := flag.Bool("pr", false, "Post review as GitHub PR comment")
	createIssues := flag.Bool("issues", false, "Create GitHub issues for each finding")
	loopMode := flag.Bool("loop", false, "Keep reviewing latest HEAD until APPROVE, then merge and close issues")
	autoFix := flag.Bool("auto-fix", false, "Automatically apply AI fixes between loop iterations (requires --loop)")
	mergeOnApprove := flag.Bool("merge", false, "Merge PR immediately if this review is APPROVE (one-shot)")
	mergeStrategy := flag.String("merge-strategy", "squash", "Merge strategy: merge, squash, rebase")
	deleteBranch := flag.Bool("delete-branch", false, "Delete branch after merge")
	loopInterval := flag.Duration("loop-interval", 30*time.Second, "Wait between loop iterations")
	baseBranch := flag.String("base", "master", "Base branch for auto-created PRs")
	createBranch := flag.Bool("create-branch", false, "Create a new fix branch from current HEAD and push before opening PR")
	validateIssues := flag.Bool("validate-issues", false, "Check open issues against current code and close stale ones before fixing")
	flag.Parse()

	dir := *dirFlag
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot get working directory: %v\n", err)
			os.Exit(1)
		}
	}
	dir, _ = filepath.Abs(dir)

	repoRoot, err := git.ResolveRepoRoot(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	client := occ.NewClient(*serverURL)
	ctx := context.Background()

	// Create and push a new fix branch if requested.
	if *createBranch {
		newBranch := fmt.Sprintf("fix/opencode-%s", time.Now().Format("20060102-150405"))
		if _, err := git.Run(repoRoot, "checkout", "-b", newBranch); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create branch %q: %v\n", newBranch, err)
			os.Exit(1)
		}
		// Create an empty commit so GitHub allows PR creation against base.
		if _, err := git.Run(repoRoot, "commit", "--allow-empty", "-m", "chore: open fix branch for opencode-review"); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to create empty commit: %v\n", err)
			os.Exit(1)
		}
		if _, err := git.Run(repoRoot, "push", "-u", "origin", newBranch); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to push branch %q: %v\n", newBranch, err)
			os.Exit(1)
		}
		fmt.Printf("Created and pushed branch %q\n", newBranch)
		// Switch to audit mode: the branch has no commit diff to review,
		// so we need a full codebase audit to find and fix existing issues.
		*auditMode = true
	}

	// GitHub client — required for --pr, --issues, --loop, --merge, --validate-issues
	var ghClient *gogithub.Client
	var ghOwner, ghRepo string
	var ghPRNum int
	if needsGitHubClient(*postPR, *createIssues, *loopMode, *mergeOnApprove, *validateIssues) {
		ghClient, err = gh.NewClient(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "GitHub: %v\n", err)
			os.Exit(1)
		}
		ghOwner, ghRepo, err = gh.RepoOwnerName(repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "GitHub: %v\n", err)
			os.Exit(1)
		}
		if *postPR || *loopMode || *mergeOnApprove {
			var created bool
			ghPRNum, created, err = gh.EnsureOpenPR(ctx, ghClient, ghOwner, ghRepo, repoRoot, *baseBranch)
			if err != nil {
				fmt.Fprintf(os.Stderr, "GitHub PR: %v\n", err)
				os.Exit(1)
			}
			if created {
				fmt.Printf("GitHub: created %s/%s PR #%d\n", ghOwner, ghRepo, ghPRNum)
			} else {
				fmt.Printf("GitHub: %s/%s PR #%d\n", ghOwner, ghRepo, ghPRNum)
			}
		} else if *createIssues {
			if n, _, err2 := gh.EnsureOpenPR(ctx, ghClient, ghOwner, ghRepo, repoRoot, *baseBranch); err2 == nil {
				ghPRNum = n
				fmt.Printf("GitHub: %s/%s PR #%d\n", ghOwner, ghRepo, ghPRNum)
			}
		}
	}

	// Validate open issues against current codebase before proceeding.
	if *validateIssues && ghClient != nil {
		runIssueValidation(ctx, *validateIssues, ghClient, ghOwner, ghRepo, repoRoot, gh.ValidateIssues)
		fmt.Println()
	}

	models, err := occ.ListModels(client, ctx, repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching models: %v\n", err)
		os.Exit(1)
	}
	if len(models) == 0 {
		fmt.Fprintln(os.Stderr, "No models available. Is opencode running?")
		os.Exit(1)
	}

	var selected types.ModelInfo
	if *modelNum >= 1 && *modelNum <= len(models) {
		selected = models[*modelNum-1]
	} else {
		selected = occ.SelectModel(models)
	}
	fmt.Printf("\nUsing: %s / %s\n\n", selected.ProviderName, selected.ModelName)

	normalizeLoopFlags(loopMode, mergeOnApprove, autoFix, loopInterval)

	log := logger.New(repoRoot)
	defer log.Close()

	var openIssues []int
	iteration := 0

	for {
		iteration++
		ref := *commitRef
		if *loopMode && iteration > 1 {
			ref = "HEAD"
		}

		hash, subject, body, diff, err := git.GetCommitInfo(repoRoot, ref)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading commit: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("─── Iteration %d — reviewing %s\n  %s\n\n", iteration, hash[:12], subject)
		fmt.Println("--- Code Review ---")
		fmt.Println()

		var prompt string
		if *auditMode {
			prompt, err = review.BuildAuditPrompt(repoRoot)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Audit prompt error: %v\n", err)
				os.Exit(1)
			}
		} else {
			prompt = review.BuildReviewPrompt(hash, subject, body, diff)
		}

		reviewText, err := occ.RunReview(client, ctx, repoRoot, selected, prompt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Review failed: %v\n", err)
			os.Exit(1)
		}
		verdict := review.ExtractVerdict(reviewText)

		if *postPR && reviewText != "" {
			fmt.Printf("Posting PR review (%s)...\n", verdict)
			if err := gh.PostPRReview(ctx, ghClient, ghOwner, ghRepo, ghPRNum, reviewText, verdict); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to post PR review: %v\n", err)
			} else {
				fmt.Println("Posted.")
			}
		}

		log.Write(map[string]any{
			"ts":        time.Now().Format(time.RFC3339),
			"iteration": iteration,
			"commit":    hash,
			"subject":   subject,
			"verdict":   verdict,
		})

		if *createIssues && reviewText != "" {
			openIssues = fileIssues(ctx, ghClient, ghOwner, ghRepo, ghPRNum, reviewText, openIssues, log)
		}

		fmt.Printf("\nVerdict: %s\n", verdict)

		if !*loopMode {
			break
		}

		if verdict == "APPROVE" {
			mergeAndClose(ctx, ghClient, ghOwner, ghRepo, ghPRNum, repoRoot, hash, *mergeStrategy, *deleteBranch, *createIssues, openIssues, log)
			break
		}

		if *mergeOnApprove && !*autoFix {
			fmt.Fprintln(os.Stderr, "Not merging: verdict is not APPROVE.")
			os.Exit(1)
		}

		if *autoFix {
			findings := review.ParseFindings(reviewText)
			var fixable []types.Finding
			for _, f := range findings {
				if f.AgentPrompt != "" {
					fixable = append(fixable, f)
				}
			}
			if len(fixable) > 0 {
				fmt.Printf("\n--- Auto-fixing %d finding(s) ---\n", len(fixable))
				n := occ.RunFix(client, ctx, repoRoot, selected, fixable, iteration)
				log.Write(map[string]any{"ts": time.Now().Format(time.RFC3339), "action": "auto_fix", "findings_fixed": n})
				if n > 0 {
					continue
				}
			}
		}

		if *loopInterval > 0 {
			fmt.Printf("\nWaiting %s for fixes before next review...\n", *loopInterval)
			time.Sleep(*loopInterval)
		}
	}
}

func normalizeLoopFlags(loopMode, mergeOnApprove, autoFix *bool, loopInterval *time.Duration) {
	if *mergeOnApprove {
		*loopMode = true
		if !*autoFix {
			*loopInterval = 0
		}
	}
}

func needsGitHubClient(postPR, createIssues, loopMode, mergeOnApprove, validateIssues bool) bool {
	return postPR || createIssues || loopMode || mergeOnApprove || validateIssues
}

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
	for _, v := range validations {
		if v.Valid {
			fmt.Printf("  #%d ✓ %s\n    %s\n", v.Number, v.Title, v.Reason)
		} else {
			fmt.Printf("  #%d ✗ STALE — %s\n    Closing: %s\n", v.Number, v.Title, v.Reason)
			comment := fmt.Sprintf("Closing as stale: %s", v.Reason)
			if err := gh.CommentOnIssue(ctx, ghClient, owner, repo, v.Number, comment); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: failed to comment on #%d: %v\n", v.Number, err)
			}
			if err := gh.CloseIssue(ctx, ghClient, owner, repo, v.Number); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: failed to close #%d: %v\n", v.Number, err)
			}
		}
	}
}

// fileIssues creates GitHub issues for new findings, deduplicating via live API.
func fileIssues(ctx context.Context, ghClient *gogithub.Client, owner, repo string, prNum int,
	reviewText string, openIssues []int, log *logger.Logger) []int {

	findings := review.ParseFindings(reviewText)
	ghSeen := gh.ExistingIssueTitles(ctx, ghClient, owner, repo)
	var newIssues []int

	for _, f := range findings {
		ghTitle := gh.FindingIssueTitle(f)
		if ghSeen[ghTitle] {
			fmt.Printf("  Skipping (already open): %s\n", ghTitle)
			continue
		}
		num, err := gh.CreateIssue(ctx, ghClient, owner, repo, f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  Failed to create issue for %q: %v\n", f.Title, err)
			continue
		}
		fmt.Printf("  #%d [%s] %s:%s — %s\n", num, f.Severity, f.File, f.LineRange, f.Title)
		newIssues = append(newIssues, num)
		openIssues = append(openIssues, num)
		ghSeen[ghTitle] = true

		if prNum > 0 {
			prRef := fmt.Sprintf("Tracking in PR #%d.", prNum)
			if err := gh.CommentOnIssue(ctx, ghClient, owner, repo, num, prRef); err != nil {
				fmt.Fprintf(os.Stderr, "  Warning: could not comment on issue #%d: %v\n", num, err)
			}
		}
		log.Write(map[string]any{
			"ts": time.Now().Format(time.RFC3339), "action": "issue_created",
			"issue": num, "severity": f.Severity, "file": f.File,
			"lineRange": f.LineRange, "title": f.Title,
		})
	}
	if len(newIssues) == 0 && len(findings) > 0 {
		fmt.Println("  No new findings (all already tracked).")
	}
	if prNum > 0 && len(newIssues) > 0 {
		if err := gh.LinkIssuesToPR(ctx, ghClient, owner, repo, prNum, newIssues); err != nil {
			fmt.Fprintf(os.Stderr, "  Failed to link issues to PR: %v\n", err)
		} else {
			fmt.Println("  Issues linked to PR.")
		}
	}
	return openIssues
}

// mergeAndClose merges the PR and closes all opencode-review issues.
func mergeAndClose(ctx context.Context, ghClient *gogithub.Client, owner, repo string, prNum int,
	repoRoot, hash, strategy string, deleteBranch, createIssues bool, openIssues []int, log *logger.Logger) {

	fmt.Println("\nAll issues resolved — merging PR...")
	currentHead, _ := git.Run(repoRoot, "rev-parse", "HEAD")
	if hash != currentHead {
		fmt.Fprintln(os.Stderr, "Refusing to merge: reviewed commit is not current HEAD.")
		os.Exit(1)
	}
	if err := gh.MergePR(ctx, ghClient, owner, repo, prNum, strategy, deleteBranch); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to merge: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Merged.")
	log.Write(map[string]any{"ts": time.Now().Format(time.RFC3339), "action": "pr_merged", "strategy": strategy})

	if !createIssues {
		return
	}

	// Only close issues filed in this run. Never sweep unrelated opencode-review
	// issues from other runs — those may not have been fixed by this PR.
	closeNums := map[int]bool{}
	for _, n := range openIssues {
		closeNums[n] = true
	}

	var closeFailed bool
	if len(closeNums) > 0 {
		fmt.Printf("Closing %d opencode-review issue(s)...\n", len(closeNums))
		for n := range closeNums {
			if err := gh.CloseIssue(ctx, ghClient, owner, repo, n); err != nil {
				fmt.Fprintf(os.Stderr, "  Failed to close #%d: %v\n", n, err)
				closeFailed = true
			} else {
				fmt.Printf("  Closed #%d\n", n)
				log.Write(map[string]any{"ts": time.Now().Format(time.RFC3339), "action": "issue_closed", "issue": n})
			}
		}
	}
	if closeFailed {
		fmt.Fprintln(os.Stderr, "Warning: some issues could not be closed.")
		os.Exit(1)
	}
}
