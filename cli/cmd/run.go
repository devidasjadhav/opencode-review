package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	gogithub "github.com/google/go-github/v67/github"
	sdk "github.com/sst/opencode-sdk-go"

	"github.com/talk/opencode-client/internal/git"
	gh "github.com/talk/opencode-client/internal/github"
	"github.com/talk/opencode-client/internal/logger"
	occ "github.com/talk/opencode-client/internal/opencode"
	"github.com/talk/opencode-client/internal/review"
	"github.com/talk/opencode-client/internal/types"
)

type runConfig struct {
	serverURL      string
	dir            string
	modelNum       int
	commitRef      string
	auditMode      bool
	postPR         bool
	createIssues   bool
	loopMode       bool
	autoFix        bool
	mergeOnApprove bool
	mergeStrategy  string
	deleteBranch   bool
	loopInterval   time.Duration
	baseBranch     string
	createBranch   bool
	validateIssues bool
}

type runEnvironment struct {
	ctx      context.Context
	repoRoot string
	client   *sdk.Client
	log      *logger.Logger
}

type githubContext struct {
	client *gogithub.Client
	owner  string
	repo   string
	prNum  int
}

type iterationResult struct {
	hash       string
	subject    string
	reviewText string
	verdict    string
}

// Run parses flags and executes the review/fix/merge loop.
func Run() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg := parseFlags()
	normalizeLoopFlags(&cfg.loopMode, &cfg.mergeOnApprove, &cfg.autoFix, &cfg.loopInterval)
	env, err := initEnvironment(cfg)
	if err != nil {
		return err
	}
	defer env.log.Close()
	if err := mustCreateFixBranchIfRequested(env.repoRoot, &cfg); err != nil {
		return err
	}
	ghCtx, err := initGitHubContext(env, cfg)
	if err != nil {
		return err
	}
	runIssueValidation(env.ctx, cfg.validateIssues, ghCtx.client, ghCtx.owner, ghCtx.repo, env.repoRoot, gh.ValidateIssues)
	if cfg.validateIssues && ghCtx.client != nil {
		fmt.Println()
	}
	selected, err := selectModel(env, cfg)
	if err != nil {
		return err
	}
	if err := executeReviewLoop(env, ghCtx, selected, cfg); err != nil {
		return err
	}
	return nil
}

func parseFlags() runConfig {
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
	mergeStrategy := flag.String("merge-strategy", types.DefaultMergeStrategy, fmt.Sprintf("Merge strategy: %s", types.AllowedMergeStrategies()))
	deleteBranch := flag.Bool("delete-branch", false, "Delete branch after merge")
	loopInterval := flag.Duration("loop-interval", 30*time.Second, "Wait between loop iterations")
	baseBranch := flag.String("base", "master", "Base branch for auto-created PRs")
	createBranch := flag.Bool("create-branch", false, "Create a new fix branch from current HEAD and push before opening PR")
	validateIssues := flag.Bool("validate-issues", false, "Check open issues against current code and close stale ones before fixing")
	flag.Parse()

	return runConfig{
		serverURL:      *serverURL,
		dir:            *dirFlag,
		modelNum:       *modelNum,
		commitRef:      *commitRef,
		auditMode:      *auditMode,
		postPR:         *postPR,
		createIssues:   *createIssues,
		loopMode:       *loopMode,
		autoFix:        *autoFix,
		mergeOnApprove: *mergeOnApprove,
		mergeStrategy:  *mergeStrategy,
		deleteBranch:   *deleteBranch,
		loopInterval:   *loopInterval,
		baseBranch:     *baseBranch,
		createBranch:   *createBranch,
		validateIssues: *validateIssues,
	}
}

func initEnvironment(cfg runConfig) (runEnvironment, error) {
	dir := cfg.dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return runEnvironment{}, wrapErr("Cannot get working directory", err)
		}
		dir = wd
	}
	absDir, err := filepath.Abs(dir)
	if err == nil {
		dir = absDir
	}
	repoRoot, err := git.ResolveRepoRoot(dir)
	if err != nil {
		return runEnvironment{}, wrapErr("", err)
	}
	return runEnvironment{
		ctx:      context.Background(),
		repoRoot: repoRoot,
		client:   occ.NewClient(cfg.serverURL),
		log:      logger.New(repoRoot),
	}, nil
}

func mustCreateFixBranchIfRequested(repoRoot string, cfg *runConfig) error {
	if !cfg.createBranch {
		return nil
	}
	newBranch := fmt.Sprintf("fix/opencode-%s", time.Now().Format("20060102-150405"))
	_, err := git.Run(repoRoot, "checkout", "-b", newBranch)
	if err != nil {
		return wrapErr(fmt.Sprintf("Failed to create branch %q", newBranch), err)
	}
	_, err = git.Run(repoRoot, "commit", "--allow-empty", "-m", "chore: open fix branch for opencode-review")
	if err != nil {
		return wrapErr("Failed to create empty commit", err)
	}
	_, err = git.Run(repoRoot, "push", "-u", "origin", newBranch)
	if err != nil {
		return wrapErr(fmt.Sprintf("Failed to push branch %q", newBranch), err)
	}
	fmt.Printf("Created and pushed branch %q\n", newBranch)
	cfg.auditMode = true
	return nil
}

func initGitHubContext(env runEnvironment, cfg runConfig) (githubContext, error) {
	if !needsGitHubClient(cfg.postPR, cfg.createIssues, cfg.loopMode, cfg.mergeOnApprove, cfg.validateIssues) {
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
	if cfg.postPR || cfg.loopMode || cfg.mergeOnApprove || cfg.createIssues {
		ghCtx.prNum, err = ensurePRWithOutput(env.ctx, ghClient, ghOwner, ghRepo, env.repoRoot, cfg.baseBranch, cfg.postPR || cfg.loopMode || cfg.mergeOnApprove)
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

func selectModel(env runEnvironment, cfg runConfig) (types.ModelInfo, error) {
	models, err := occ.ListModels(env.client, env.ctx, env.repoRoot)
	if err != nil {
		return types.ModelInfo{}, wrapErr("Error fetching models", err)
	}
	if len(models) == 0 {
		return types.ModelInfo{}, fmt.Errorf("no models available. Is opencode running?")
	}
	var selected types.ModelInfo
	if cfg.modelNum >= 1 && cfg.modelNum <= len(models) {
		selected = models[cfg.modelNum-1]
	} else {
		selected, err = occ.SelectModel(models)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return types.ModelInfo{}, fmt.Errorf("model selection canceled")
			}
			return types.ModelInfo{}, wrapErr("Error selecting model", err)
		}
	}
	fmt.Printf("\nUsing: %s / %s\n\n", selected.ProviderName, selected.ModelName)
	return selected, nil
}

func executeReviewLoop(env runEnvironment, ghCtx githubContext, selected types.ModelInfo, cfg runConfig) error {
	runner := newLoopRunner(env, ghCtx, selected, cfg, []LoopStep{
		reviewStep{},
		postReviewStep{},
		issueStep{},
		mergeStep{},
		autoFixStep{},
		waitStep{},
	})
	return runner.Run()
}

type LoopStep interface {
	Run(state *loopState) (continueLoop bool, err error)
}

type loopState struct {
	env        runEnvironment
	ghCtx      githubContext
	selected   types.ModelInfo
	cfg        runConfig
	iteration  int
	result     iterationResult
	openIssues []int
	stop       bool
}

type loopRunner struct {
	state loopState
	steps []LoopStep
}

func newLoopRunner(env runEnvironment, ghCtx githubContext, selected types.ModelInfo, cfg runConfig, steps []LoopStep) *loopRunner {
	return &loopRunner{
		state: loopState{env: env, ghCtx: ghCtx, selected: selected, cfg: cfg},
		steps: steps,
	}
}

func (r *loopRunner) Run() error {
	for {
		r.state.iteration++
		r.state.stop = false
		continueLoop := false
		for _, step := range r.steps {
			stepContinue, err := step.Run(&r.state)
			if err != nil {
				return err
			}
			if r.state.stop {
				return nil
			}
			if stepContinue {
				continueLoop = true
				break
			}
		}
		if !continueLoop {
			return nil
		}
	}
}

type reviewStep struct{}

func (reviewStep) Run(state *loopState) (bool, error) {
	result, err := runReviewIteration(state.env, state.selected, state.cfg, state.iteration)
	if err != nil {
		return false, err
	}
	state.result = result
	return false, nil
}

type postReviewStep struct{}

func (postReviewStep) Run(state *loopState) (bool, error) {
	postPRReviewIfRequested(state.env.ctx, state.ghCtx, state.cfg, state.result.reviewText, state.result.verdict)
	logEvent(state.env.log, map[string]any{
		"event":     "review_iteration",
		"iteration": state.iteration,
		"commit":    state.result.hash,
		"subject":   state.result.subject,
		"verdict":   state.result.verdict,
	})
	return false, nil
}

type issueStep struct{}

func (issueStep) Run(state *loopState) (bool, error) {
	if !state.cfg.createIssues || state.result.reviewText == "" {
		return false, nil
	}
	openIssues, err := fileIssues(state.env.ctx, state.ghCtx.client, state.ghCtx.owner, state.ghCtx.repo, state.ghCtx.prNum, state.result.reviewText, state.openIssues, state.env.log)
	if err != nil {
		return false, err
	}
	state.openIssues = openIssues
	return false, nil
}

type mergeStep struct{}

func (mergeStep) Run(state *loopState) (bool, error) {
	fmt.Printf("\nVerdict: %s\n", state.result.verdict)
	if !state.cfg.loopMode {
		state.stop = true
		return false, nil
	}
	merged, err := maybeMergeOnApprove(state.env, state.ghCtx, state.cfg, state.result, state.openIssues)
	if err != nil {
		return false, err
	}
	if merged {
		state.stop = true
		return false, nil
	}
	if err := ensureMergeConstraints(state.cfg, state.result.verdict); err != nil {
		return false, err
	}
	return false, nil
}

type autoFixStep struct{}

func (autoFixStep) Run(state *loopState) (bool, error) {
	if !state.cfg.autoFix {
		return false, nil
	}
	// If there are findings but none have an agent prompt, no fix can be applied.
	// Break the loop instead of sleeping and re-reviewing identical code forever.
	findings := review.ParseFindings(state.result.reviewText)
	hasFixable := false
	for _, f := range findings {
		if f.AgentPrompt != "" {
			hasFixable = true
			break
		}
	}
	if len(findings) > 0 && !hasFixable {
		fmt.Fprintln(os.Stderr, "\nAuto-fix: no agent prompts in findings — cannot auto-fix. Manual intervention required.")
		state.stop = true
		return false, nil
	}
	return runAutoFixIfNeeded(state.env, state.selected, state.cfg, state.result.reviewText, state.iteration)
}

type waitStep struct{}

func (waitStep) Run(state *loopState) (bool, error) {
	if !state.cfg.loopMode || state.stop {
		state.stop = true
		return false, nil
	}
	if state.cfg.loopInterval > 0 {
		fmt.Printf("\nWaiting %s for fixes before next review...\n", state.cfg.loopInterval)
		time.Sleep(state.cfg.loopInterval)
	}
	return true, nil
}

func runReviewIteration(env runEnvironment, selected types.ModelInfo, cfg runConfig, iteration int) (iterationResult, error) {
	ref := cfg.commitRef
	if cfg.loopMode && iteration > 1 {
		ref = "HEAD"
	}
	hash, subject, body, diff, err := git.GetCommitInfo(env.repoRoot, ref)
	if err != nil {
		return iterationResult{}, wrapErr("Error reading commit", err)
	}

	fmt.Printf("─── Iteration %d — reviewing %s\n  %s\n\n", iteration, hash[:12], subject)
	fmt.Println("--- Code Review ---")
	fmt.Println()

	var prompt string
	if cfg.auditMode {
		prompt, err = review.BuildAuditPrompt(env.repoRoot)
		if err != nil {
			return iterationResult{}, wrapErr("Audit prompt error", err)
		}
	} else {
		prompt = review.BuildReviewPrompt(hash, subject, body, diff)
	}
	reviewText, err := occ.RunReview(env.client, env.ctx, env.repoRoot, selected, prompt)
	if err != nil {
		return iterationResult{}, wrapErr("Review failed", err)
	}

	return iterationResult{
		hash:       hash,
		subject:    subject,
		reviewText: reviewText,
		verdict:    review.ExtractVerdict(reviewText),
	}, nil
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

func ensureMergeConstraints(cfg runConfig, verdict string) error {
	if cfg.mergeOnApprove && !cfg.autoFix && verdict != "APPROVE" {
		return fmt.Errorf("not merging: verdict is not APPROVE")
	}
	return nil
}

func runAutoFixIfNeeded(env runEnvironment, selected types.ModelInfo, cfg runConfig, reviewText string, iteration int) (bool, error) {
	if !cfg.autoFix {
		return false, nil
	}
	findings := review.ParseFindings(reviewText)
	var fixable []types.Finding
	for _, f := range findings {
		if f.AgentPrompt != "" {
			fixable = append(fixable, f)
		}
	}
	if len(fixable) == 0 {
		return false, nil
	}
	fmt.Printf("\n--- Auto-fixing %d finding(s) ---\n", len(fixable))
	n, err := occ.RunFix(env.client, env.ctx, env.repoRoot, selected, fixable, iteration, occ.NewGitFixPersister())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return false, nil
	}
	logEvent(env.log, map[string]any{"event": "auto_fix", "findings_fixed": n})
	return n > 0, nil
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

// fileIssues creates GitHub issues for new findings, deduplicating via live API.
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

// mergeAndClose verifies and merges the PR.
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

func wrapErr(context string, err error) error {
	if err == nil {
		return nil
	}
	if context == "" {
		return err
	}
	return fmt.Errorf("%s: %w", context, err)
}

func logEvent(log *logger.Logger, fields map[string]any) {
	fields["ts"] = time.Now().Format(time.RFC3339)
	log.Write(fields)
}
