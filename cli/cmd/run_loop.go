package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/talk/opencode-client/internal/git"
	"github.com/talk/opencode-client/internal/logger"
	occ "github.com/talk/opencode-client/internal/opencode"
	"github.com/talk/opencode-client/internal/review"
	"github.com/talk/opencode-client/internal/types"
)

// StepSignal is the control-flow value returned by every LoopStep.
type StepSignal int

const (
	// SignalStop ends the run after the current iteration completes.
	SignalStop StepSignal = iota
	// SignalContinue lets the iteration finish all steps, then starts a new one.
	SignalContinue
	// SignalRestartNow skips all remaining steps and begins a new iteration immediately.
	SignalRestartNow
)

type iterationResult struct {
	hash       string
	subject    string
	reviewText string
	verdict    string
	findings   []types.Finding // parsed once in reviewStep; consumed by later steps
}

func executeReviewLoop(env runEnvironment, ghCtx githubContext, selected, verifier types.ModelInfo, cfg runConfig, summary *RunSummary) error {
	runner := newLoopRunner(env, ghCtx, selected, verifier, cfg, summary, []LoopStep{
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

// LoopStep is one stage in the review/fix/merge loop.
// Returning SignalRestartNow skips all remaining steps in the iteration.
// Returning SignalContinue marks the iteration as "keep looping" but still runs subsequent steps.
// Returning SignalStop (or any step returning Stop while no other step returned Continue) ends the run.
type LoopStep interface {
	Run(state *loopState) (StepSignal, error)
}

type loopState struct {
	env          runEnvironment
	ghCtx        githubContext
	selected     types.ModelInfo
	verifier     types.ModelInfo
	cfg          runConfig
	summary      *RunSummary
	iteration    int
	result       iterationResult
	openIssues   []int
	// changedFiles is written by autoFixStep and read by reviewStep.
	// Steps execute sequentially in a single goroutine — no synchronisation needed.
	changedFiles map[string]bool
	lastFixPaths []string
}

type loopRunner struct {
	state loopState
	steps []LoopStep
}

func newLoopRunner(env runEnvironment, ghCtx githubContext, selected, verifier types.ModelInfo, cfg runConfig, summary *RunSummary, steps []LoopStep) *loopRunner {
	return &loopRunner{
		state: loopState{env: env, ghCtx: ghCtx, selected: selected, verifier: verifier, cfg: cfg, summary: summary},
		steps: steps,
	}
}

// Run executes all steps sequentially each iteration.
// SignalRestartNow is the only signal that short-circuits remaining steps.
// SignalContinue from any step causes another full iteration after all steps complete.
func (r *loopRunner) Run() error {
	for {
		r.state.iteration++
		finalSignal := SignalStop
		for _, step := range r.steps {
			sig, err := step.Run(&r.state)
			if err != nil {
				return err
			}
			if sig == SignalRestartNow {
				finalSignal = SignalRestartNow
				break
			}
			if sig == SignalContinue {
				finalSignal = SignalContinue
			}
		}
		if finalSignal == SignalStop {
			return nil
		}
	}
}

type reviewStep struct{}

func (reviewStep) Run(state *loopState) (StepSignal, error) {
	result, err := runReviewIteration(state.env, state.ghCtx, state.selected, state.cfg, state.iteration, state.changedFiles)
	if err != nil {
		return SignalStop, err
	}
	state.result = result
	state.summary.iterations++
	state.summary.finalVerdict = result.verdict
	return SignalStop, nil
}

type postReviewStep struct{}

func (postReviewStep) Run(state *loopState) (StepSignal, error) {
	postPRReviewIfRequested(state.env.ctx, state.ghCtx, state.cfg, state.result.reviewText, state.result.verdict)
	logEvent(state.env.log, map[string]any{
		"event":     "review_iteration",
		"iteration": state.iteration,
		"commit":    state.result.hash,
		"subject":   state.result.subject,
		"verdict":   state.result.verdict,
	})
	return SignalStop, nil
}

type issueStep struct{}

func (issueStep) Run(state *loopState) (StepSignal, error) {
	if !state.cfg.createIssues || state.result.reviewText == "" {
		return SignalStop, nil
	}
	before := len(state.openIssues)
	openIssues, err := fileIssues(state.env.ctx, state.ghCtx.gh, state.ghCtx.prNum, state.result.reviewText, state.openIssues, state.env.log)
	if err != nil {
		return SignalStop, err
	}
	state.summary.issuesCreated += len(openIssues) - before
	state.openIssues = openIssues
	return SignalStop, nil
}

type mergeStep struct{}

func (mergeStep) Run(state *loopState) (StepSignal, error) {
	fmt.Printf("\nVerdict: %s\n", state.result.verdict)
	if !state.cfg.loopMode {
		return SignalStop, nil
	}
	merged, err := maybeMergeOnApprove(state.env, state.ghCtx, state.cfg, state.result, state.openIssues)
	if err != nil {
		return SignalStop, err
	}
	if merged {
		return SignalStop, nil
	}
	if state.cfg.mergeOnApprove && !state.cfg.autoFix && !canMergeOnApprove(state.cfg, state.result.verdict) {
		return SignalStop, nil
	}
	return SignalStop, nil
}

type autoFixStep struct{}

func (autoFixStep) Run(state *loopState) (StepSignal, error) {
	state.lastFixPaths = nil
	if !state.cfg.autoFix {
		return SignalStop, nil
	}
	fixable := filterFixableFindings(state.result.findings, state.cfg.minConfidence)
	if len(state.result.findings) > 0 && len(fixable) == 0 {
		fmt.Fprintln(os.Stderr, "\nAuto-fix: no fixable findings at required confidence — cannot auto-fix. Manual intervention required.")
		return SignalStop, nil
	}
	cont, changedPaths, err := runAutoFixIfNeeded(state.env, state.selected, state.cfg, state.result.findings, state.iteration)
	if err != nil {
		return SignalStop, err
	}
	if len(changedPaths) > 0 {
		state.changedFiles = pathsToRelMap(changedPaths, state.env.repoRoot)
		state.lastFixPaths = changedPaths
		state.summary.fixesApplied++
	}
	if cont {
		return SignalContinue, nil
	}
	return SignalStop, nil
}

type verifyFixStep struct{}

func (verifyFixStep) Run(state *loopState) (StepSignal, error) {
	if state.verifier.ModelID == "" || len(state.lastFixPaths) == 0 {
		return SignalStop, nil
	}
	diff, err := git.DiffHead(state.env.repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\nVerifier: could not get diff: %v — skipping verification\n", err)
		return SignalStop, nil
	}
	fixable := filterFixableFindings(state.result.findings, state.cfg.minConfidence)
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
		return SignalStop, nil
	}

	fmt.Fprintln(os.Stderr, "\nVerification: FAIL — reverting fix commit.")
	if err := git.RevertHead(state.env.repoRoot); err != nil {
		return SignalStop, fmt.Errorf("verifier: revert failed: %w", err)
	}
	logEvent(state.env.log, map[string]any{"event": "verify_fix_reverted", "iteration": state.iteration})
	state.summary.fixesReverted++
	state.lastFixPaths = nil
	state.changedFiles = nil
	// Restart immediately — skip the wait interval when a fix was just reverted.
	return SignalRestartNow, nil
}

type waitStep struct{}

func (waitStep) Run(state *loopState) (StepSignal, error) {
	if !state.cfg.loopMode {
		return SignalStop, nil
	}
	if state.cfg.loopInterval > 0 {
		fmt.Printf("\nWaiting %s for fixes before next review...\n", state.cfg.loopInterval)
		time.Sleep(state.cfg.loopInterval)
	}
	return SignalContinue, nil
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
		prompt, err = review.BuildAuditPrompt(env.repoRoot, changedFiles, cfg.lspEnabled)
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
		findings:   review.ParseFindings(reviewText), // parsed once here
	}, nil
}

// buildReviewContext gathers full file contents and existing issues to enrich
// the review prompt. Failures are non-fatal — missing context degrades gracefully.
func buildReviewContext(env runEnvironment, ghCtx githubContext, cfg runConfig, hash string) review.ReviewContext {
	var rctx review.ReviewContext

	// Embed full content of every file touched by this commit.
	if files, err := git.DiffChangedFiles(env.repoRoot, hash); err == nil && len(files) > 0 {
		rctx.ChangedFiles = git.ReadFileContents(env.repoRoot, files)
	}

	// Tell the model which issues are already tracked so it doesn't re-report them.
	if ghCtx.gh != nil {
		if idx, err := ghCtx.gh.FetchIssueIndex(env.ctx); err == nil {
			rctx.ExistingIssues = idx.Summaries
		}
	}

	rctx.LSPEnabled = cfg.lspEnabled
	return rctx
}

func runAutoFixIfNeeded(env runEnvironment, selected types.ModelInfo, cfg runConfig, findings []types.Finding, iteration int) (bool, []string, error) {
	if !cfg.autoFix {
		return false, nil, nil
	}
	fixable := filterFixableFindings(findings, cfg.minConfidence)
	if len(fixable) == 0 {
		return false, nil, nil
	}
	fmt.Printf("\n--- Auto-fixing %d finding(s) ---\n", len(fixable))
	n, changedPaths, err := occ.RunFix(env.client, env.ctx, env.repoRoot, selected, fixable, iteration, env.fixPersister, cfg.lspEnabled)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: auto-fix failed: %v\n", err)
		logEvent(env.log, map[string]any{"event": "auto_fix_error", "error": err.Error(), "iteration": iteration})
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
