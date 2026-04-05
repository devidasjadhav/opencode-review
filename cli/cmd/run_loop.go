package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/talk/opencode-client/internal/git"
	gh "github.com/talk/opencode-client/internal/github"
	"github.com/talk/opencode-client/internal/logger"
	occ "github.com/talk/opencode-client/internal/opencode"
	"github.com/talk/opencode-client/internal/review"
	"github.com/talk/opencode-client/internal/types"
)

type iterationResult struct {
	hash       string
	subject    string
	reviewText string
	verdict    string
}

func executeReviewLoop(env runEnvironment, ghCtx githubContext, selected, verifier types.ModelInfo, cfg runConfig) error {
	runner := newLoopRunner(env, ghCtx, selected, verifier, cfg, []LoopStep{
		reviewStep{},
		postReviewStep{},
		issueStep{},
		mergeStep{},
		autoFixStep{},
		verifyFixStep{},
		waitStep{},
	})
	return runner.Run()
}

type LoopStep interface {
	Run(state *loopState) (continueLoop bool, err error)
}

type loopState struct {
	env          runEnvironment
	ghCtx        githubContext
	selected     types.ModelInfo
	verifier     types.ModelInfo
	cfg          runConfig
	iteration    int
	result       iterationResult
	openIssues   []int
	// changedFiles is written by autoFixStep and read by reviewStep.
	// Steps execute sequentially in a single goroutine — no synchronisation needed.
	changedFiles map[string]bool
	lastFixPaths []string
	stop         bool
}

type loopRunner struct {
	state loopState
	steps []LoopStep
}

func newLoopRunner(env runEnvironment, ghCtx githubContext, selected, verifier types.ModelInfo, cfg runConfig, steps []LoopStep) *loopRunner {
	return &loopRunner{
		state: loopState{env: env, ghCtx: ghCtx, selected: selected, verifier: verifier, cfg: cfg},
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
	result, err := runReviewIteration(state.env, state.ghCtx, state.selected, state.cfg, state.iteration, state.changedFiles)
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
	if state.cfg.mergeOnApprove && !state.cfg.autoFix && !canMergeOnApprove(state.cfg, state.result.verdict) {
		state.stop = true
	}
	return false, nil
}

type autoFixStep struct{}

func (autoFixStep) Run(state *loopState) (bool, error) {
	state.lastFixPaths = nil
	if !state.cfg.autoFix {
		return false, nil
	}
	findings := review.ParseFindings(state.result.reviewText)
	fixable := filterFixableFindings(findings, state.cfg.minConfidence)
	if len(findings) > 0 && len(fixable) == 0 {
		fmt.Fprintln(os.Stderr, "\nAuto-fix: no fixable findings at required confidence — cannot auto-fix. Manual intervention required.")
		state.stop = true
		return false, nil
	}
	cont, changedPaths, err := runAutoFixIfNeeded(state.env, state.selected, state.cfg, state.result.reviewText, state.iteration)
	if err != nil {
		return false, err
	}
	if len(changedPaths) > 0 {
		state.changedFiles = pathsToRelMap(changedPaths, state.env.repoRoot)
		state.lastFixPaths = changedPaths
	}
	return cont, nil
}

type verifyFixStep struct{}

func (verifyFixStep) Run(state *loopState) (bool, error) {
	if state.verifier.ModelID == "" || len(state.lastFixPaths) == 0 {
		return false, nil
	}
	diff, err := git.DiffHead(state.env.repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nVerifier: could not get diff: %v — skipping verification\n", err)
		return false, nil
	}
	findings := review.ParseFindings(state.result.reviewText)
	fixable := filterFixableFindings(findings, state.cfg.minConfidence)
	prompt := review.BuildVerifyPrompt(diff, buildFindingSummary(fixable))

	fmt.Println("\n--- Independent Verification ---")
	verifyText, err := occ.RunVerify(state.env.client, state.env.ctx, state.env.repoRoot, state.verifier, prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Verifier: session error: %v — treating as FAIL\n", err)
		verifyText = "FAIL — verifier session error"
	}

	verdict := review.ExtractVerifyVerdict(verifyText)
	logEvent(state.env.log, map[string]any{
		"event":          "verify_fix",
		"iteration":      state.iteration,
		"verify_verdict": verdict,
	})

	if verdict == "PASS" {
		fmt.Println("\nVerification: PASS — fix accepted.")
		return false, nil
	}

	fmt.Fprintln(os.Stderr, "\nVerification: FAIL — reverting fix commit.")
	if err := git.RevertHead(state.env.repoRoot); err != nil {
		return false, fmt.Errorf("verifier: revert failed: %w", err)
	}
	logEvent(state.env.log, map[string]any{"event": "verify_fix_reverted", "iteration": state.iteration})
	state.lastFixPaths = nil
	state.changedFiles = nil
	return true, nil
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

func runReviewIteration(env runEnvironment, ghCtx githubContext, selected types.ModelInfo, cfg runConfig, iteration int, changedFiles map[string]bool) (iterationResult, error) {
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
		prompt, err = review.BuildAuditPrompt(env.repoRoot, changedFiles)
		if err != nil {
			return iterationResult{}, wrapErr("Audit prompt error", err)
		}
	} else {
		rctx := buildReviewContext(env, ghCtx, cfg, hash)
		prompt = review.BuildReviewPrompt(hash, subject, body, diff, rctx)
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

// buildReviewContext gathers full file contents and existing issues to enrich
// the review prompt. Failures are non-fatal — missing context degrades gracefully.
func buildReviewContext(env runEnvironment, ghCtx githubContext, cfg runConfig, hash string) review.ReviewContext {
	var rctx review.ReviewContext

	// Embed full content of every file touched by this commit.
	if files, err := git.DiffChangedFiles(env.repoRoot, hash); err == nil && len(files) > 0 {
		rctx.ChangedFiles = readFileContents(env.repoRoot, files)
	}

	// Tell the model which issues are already tracked so it doesn't re-report them.
	if ghCtx.client != nil {
		if summaries, err := gh.ListOpenIssueSummaries(env.ctx, ghCtx.client, ghCtx.owner, ghCtx.repo); err == nil {
			rctx.ExistingIssues = summaries
		}
	}

	return rctx
}

// readFileContents reads the current on-disk content of each repo-relative path.
func readFileContents(repoRoot string, relPaths []string) map[string]string {
	m := make(map[string]string, len(relPaths))
	for _, p := range relPaths {
		data, err := os.ReadFile(filepath.Join(repoRoot, p))
		if err != nil {
			continue
		}
		m[p] = string(data)
	}
	return m
}

func runAutoFixIfNeeded(env runEnvironment, selected types.ModelInfo, cfg runConfig, reviewText string, iteration int) (bool, []string, error) {
	if !cfg.autoFix {
		return false, nil, nil
	}
	findings := review.ParseFindings(reviewText)
	fixable := filterFixableFindings(findings, cfg.minConfidence)
	if len(fixable) == 0 {
		return false, nil, nil
	}
	fmt.Printf("\n--- Auto-fixing %d finding(s) ---\n", len(fixable))
	n, changedPaths, err := occ.RunFix(env.client, env.ctx, env.repoRoot, selected, fixable, iteration, occ.NewGitFixPersister())
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return false, nil, nil
	}
	logEvent(env.log, map[string]any{"event": "auto_fix", "findings_fixed": n})
	return n > 0, changedPaths, nil
}

func filterFixableFindings(findings []types.Finding, minConfidence string) []types.Finding {
	fixable := make([]types.Finding, 0, len(findings))
	for _, f := range findings {
		if f.AgentPrompt != "" && review.MeetsConfidence(f.Confidence, minConfidence) {
			fixable = append(fixable, f)
		}
	}
	return fixable
}

func buildFindingSummary(findings []types.Finding) string {
	var sb strings.Builder
	for _, f := range findings {
		fmt.Fprintf(&sb, "[%s] %s:%s — %s\n", f.Severity, f.File, f.LineRange, f.Title)
		if f.Description != "" {
			fmt.Fprintf(&sb, "  %s\n", strings.TrimSpace(f.Description))
		}
	}
	return sb.String()
}

func pathsToRelMap(paths []string, repoRoot string) map[string]bool {
	m := make(map[string]bool, len(paths))
	for _, p := range paths {
		rel, err := filepath.Rel(repoRoot, p)
		if err != nil {
			rel = p
		}
		m[rel] = true
	}
	return m
}

func logEvent(log *logger.Logger, fields map[string]any) {
	fields["ts"] = time.Now().Format(time.RFC3339)
	log.Write(fields)
}
