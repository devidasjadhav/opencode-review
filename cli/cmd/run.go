package cmd

import (
	"flag"
	"fmt"
	"os"
	"time"

	occ "github.com/talk/opencode-client/internal/opencode"
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
	minConfidence    string
	verifierModelNum int
	dryRun           bool
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
	runIssueValidation(env.ctx, cfg.validateIssues, ghCtx.gh, env.repoRoot)
	if cfg.validateIssues && ghCtx.gh != nil {
		fmt.Println()
	}
	selected, err := selectModel(env, cfg)
	if err != nil {
		return err
	}
	verifier, err := resolveVerifierModel(env, cfg)
	if err != nil {
		return err
	}
	summary := newRunSummary()
	loopErr := executeReviewLoop(env, ghCtx, selected, verifier, cfg, summary)
	summary.print(env.log.LogPath())
	return loopErr
}

func parseFlags() runConfig {
	// Load .env defaults before registering flags so CLI flags override them.
	env := loadEnvFile(resolveRootForEnv(prescanDir()))

	serverURL := flag.String("url", envOr(env, "URL", "http://localhost:4096"), "Opencode server URL")
	dirFlag := flag.String("dir", "", "Git repo directory (default: current directory)")
	modelNum := flag.Int("model", envInt(env, "MODEL", 0), "Model number from list (skips interactive selection)")
	commitRef := flag.String("commit", "HEAD", "Git ref to review (hash, branch, tag)")
	auditMode := flag.Bool("audit", envBool(env, "AUDIT", false), "Full SOLID/DRY audit of entire codebase instead of single commit review")
	postPR := flag.Bool("pr", envBool(env, "PR", false), "Post review as GitHub PR comment")
	createIssues := flag.Bool("issues", envBool(env, "ISSUES", false), "Create GitHub issues for each finding")
	loopMode := flag.Bool("loop", envBool(env, "LOOP", false), "Keep reviewing latest HEAD until APPROVE, then merge and close issues")
	autoFix := flag.Bool("auto-fix", envBool(env, "AUTO_FIX", false), "Automatically apply AI fixes between loop iterations (requires --loop)")
	mergeOnApprove := flag.Bool("merge", envBool(env, "MERGE", false), "Merge PR immediately if this review is APPROVE (one-shot)")
	mergeStrategy := flag.String("merge-strategy", envOr(env, "MERGE_STRATEGY", types.DefaultMergeStrategy), fmt.Sprintf("Merge strategy: %s", types.AllowedMergeStrategies()))
	deleteBranch := flag.Bool("delete-branch", envBool(env, "DELETE_BRANCH", false), "Delete branch after merge")
	loopInterval := flag.Duration("loop-interval", envDuration(env, "LOOP_INTERVAL", 30*time.Second), "Wait between loop iterations")
	baseBranch := flag.String("base", envOr(env, "BASE", "master"), "Base branch for auto-created PRs")
	createBranch := flag.Bool("create-branch", envBool(env, "CREATE_BRANCH", false), "Create a new fix branch from current HEAD and push before opening PR")
	validateIssues := flag.Bool("validate-issues", envBool(env, "VALIDATE_ISSUES", false), "Check open issues against current code and close stale ones before fixing")
	minConfidence := flag.String("min-confidence", envOr(env, "MIN_CONFIDENCE", "MEDIUM"), "Minimum confidence level to auto-fix: HIGH, MEDIUM, or LOW")
	verifierModelNum := flag.Int("verifier-model", envInt(env, "VERIFIER_MODEL", 0), "Model number to use as independent fix verifier (0 = disabled)")
	dryRun := flag.Bool("dry-run", envBool(env, "DRY_RUN", false), "Print GitHub actions without executing them (no issues, no PR posts, no merges)")
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
		minConfidence:    *minConfidence,
		verifierModelNum: *verifierModelNum,
		dryRun:           *dryRun,
	}
}

// resolveVerifierModel returns the ModelInfo for the verifier if --verifier-model
// is set, or a zero-value ModelInfo (disabled) if not.
func resolveVerifierModel(env runEnvironment, cfg runConfig) (types.ModelInfo, error) {
	if cfg.verifierModelNum == 0 {
		return types.ModelInfo{}, nil
	}
	models, err := occ.ListModels(env.client, env.ctx, env.repoRoot)
	if err != nil {
		return types.ModelInfo{}, fmt.Errorf("verifier model: %w", err)
	}
	if cfg.verifierModelNum < 1 || cfg.verifierModelNum > len(models) {
		return types.ModelInfo{}, fmt.Errorf("verifier model: invalid number %d (have %d models)", cfg.verifierModelNum, len(models))
	}
	m := models[cfg.verifierModelNum-1]
	fmt.Printf("Verifier: [%d] %s / %s\n\n", cfg.verifierModelNum, m.ProviderName, m.ModelName)
	return m, nil
}

func normalizeLoopFlags(loopMode, mergeOnApprove, autoFix *bool, loopInterval *time.Duration) {
	if *mergeOnApprove {
		*loopMode = true
		if !*autoFix {
			*loopInterval = 0
		}
	}
}
