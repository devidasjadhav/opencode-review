package cmd

import (
	"flag"
	"fmt"
	"os"
	"time"

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
	minConfidence  string
}

func (c runConfig) needsGitHubClient() bool {
	return c.postPR || c.createIssues || c.loopMode || c.mergeOnApprove || c.validateIssues
}

func (c runConfig) needsPRContext() bool {
	return c.postPR || c.loopMode || c.mergeOnApprove || c.createIssues
}

func (c runConfig) requiresPRSetup() bool {
	return c.postPR || c.loopMode || c.mergeOnApprove
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
	runIssueValidation(env.ctx, cfg.validateIssues, ghCtx.client, ghCtx.owner, ghCtx.repo, env.repoRoot, validateIssuesFn)
	if cfg.validateIssues && ghCtx.client != nil {
		fmt.Println()
	}
	selected, err := selectModel(env, cfg)
	if err != nil {
		return err
	}
	return executeReviewLoop(env, ghCtx, selected, cfg)
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
	minConfidence := flag.String("min-confidence", "MEDIUM", "Minimum confidence level to auto-fix: HIGH, MEDIUM, or LOW")
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
		minConfidence:  *minConfidence,
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
