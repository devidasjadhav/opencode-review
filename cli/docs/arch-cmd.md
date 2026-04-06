# Package: cmd

The `cmd` package is the CLI orchestration layer. It owns flag parsing, environment setup, the review/fix/merge loop, and GitHub service calls. It imports all `internal/*` packages but is never imported by them.

---

## Files

| File | Responsibility |
|------|---------------|
| `run.go` | Entry point, flag definitions, `runConfig`, top-level `run()` |
| `run_bootstrap.go` | Environment init, model selection, branch creation |
| `run_loop.go` | 7-step loop runner and all step implementations |
| `run_service_github.go` | GitHub issue/PR operations called from loop steps |
| `run_summary.go` | `RunSummary` struct and final metrics output |
| `envfile.go` | `.opencode-review.env` loader |

---

## Configuration: `runConfig`

All CLI flags map into a single `runConfig` struct, constructed in `run()`:

```go
type runConfig struct {
    serverURL        string
    dir              string
    commitRef        string
    modelNum         int
    verifierModelNum int
    auditMode        bool
    postPR           bool
    createIssues     bool
    loopMode         bool
    autoFix          bool
    mergeOnApprove   bool
    mergeStrategy    string
    deleteBranch     bool
    baseBranch       string
    createBranch     bool
    validateIssues   bool
    loopInterval     time.Duration
    dryRun           bool
    lspEnabled       bool
    minConfidence    string
}
```

Flag defaults are resolved in this order:
1. Hardcoded default (e.g. `"MEDIUM"` for `--min-confidence`)
2. `.opencode-review.env` value (loaded by `loadEnvFile`)
3. CLI flag value (always wins)

---

## Environment: `runEnvironment`

Constructed once by `initEnvironment()`, threaded through the entire run:

```go
type runEnvironment struct {
    ctx          context.Context
    repoRoot     string
    client       *sdk.Client     // opencode SDK
    log          *logger.Logger  // JSONL log for this run
    fixPersister occ.FixPersister
}
```

`fixPersister` is `occ.NewGitFixPersister()` in production and can be swapped in tests.

### `initEnvironment(cfg runConfig) (runEnvironment, error)`

1. Resolves `--dir` to an absolute path
2. Calls `git.ResolveRepoRoot()` — fails fast if not in a git repo
3. Warns if `--lsp` is set but `OPENCODE_EXPERIMENTAL_LSP_TOOL` is not
4. Creates the opencode SDK client pointed at `--url`
5. Creates the logger (rotates old logs)
6. Returns a ready `runEnvironment`

---

## GitHub Context: `githubContext`

```go
type githubContext struct {
    gh    github.Port   // nil if no GITHUB_TOKEN
    owner string
    repo  string
    prNum int
}
```

`initGitHubContext()` parses owner/repo from the git remote URL, creates a `realClient` (or `noOpClient` for `--dry-run`), and optionally calls `EnsureOpenPR()`.

---

## The Loop: `executeReviewLoop`

```go
func executeReviewLoop(env runEnvironment, ghCtx githubContext,
    selected, verifier types.ModelInfo, cfg runConfig, summary *RunSummary) error
```

Creates a `loopRunner` with 7 steps and calls `Run()`. Steps execute sequentially within each iteration. `loopState` carries mutable state between steps:

```go
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
    changedFiles map[string]bool  // written by autoFixStep, read by reviewStep
    lastFixPaths []string
    stop         bool
}
```

### LoopStep Interface

```go
type LoopStep interface {
    Run(state *loopState) (continueLoop bool, err error)
}
```

`continueLoop = true` restarts the outer loop immediately (skipping remaining steps). `state.stop = true` exits cleanly after the current step. An error exits with the error.

---

## Step Implementations

### `reviewStep`

Calls `runReviewIteration()`:
- Gets commit info via `git.GetCommitInfo()`
- Builds `ReviewContext` (changed file contents + existing GitHub issues)
- Builds prompt via `review.BuildReviewPrompt()` or `review.BuildAuditPrompt()`
- Streams through `occ.RunReview()` — live output to terminal
- Parses findings and verdict; stores in `state.result`

On iteration > 1 in loop mode, reviews `HEAD` (the latest fixed commit) rather than the original `--commit` ref.

### `postReviewStep`

- Posts PR review via `gh.PostPRReview()` if `--pr`
- Writes `review_iteration` JSONL event

### `issueStep`

- Skips if `--issues` is false or review text is empty
- Calls `fileIssues()` which deduplicates by fingerprint then by title
- Tracks created issue numbers in `state.openIssues`
- Increments `summary.issuesCreated`

### `mergeStep`

- Prints the verdict
- If not `--loop`: sets `stop=true` (one-shot mode)
- If APPROVE and `--merge`: calls `maybeMergeOnApprove()` → `gh.MergePR()`, `stop=true`
- If not APPROVE and `--merge` without `--auto-fix`: `stop=true` (cannot merge, manual fix needed)

### `autoFixStep`

- Skips if `--auto-fix` is false
- Filters findings by `--min-confidence` via `filterFixableFindings()`
- If all findings are below threshold: prints message, sets `stop=true`
- Calls `runAutoFixIfNeeded()` → `occ.RunFix()`
- Stores `changedPaths` in `state.changedFiles` (used by next iteration's `reviewStep`)
- Increments `summary.fixesApplied`

### `verifyFixStep`

- Skips if `--verifier-model` is 0 or no fix was applied
- Gets `git diff HEAD` and builds `review.BuildVerifyPrompt()`
- Streams through `occ.RunVerify()` with the verifier model
- PASS: continue to next step
- FAIL: calls `git.RevertHead()`, clears `changedFiles`, increments `summary.fixesReverted`
- Returns `continueLoop=true` on revert (triggers immediate re-review)

### `waitStep`

- Skips if not `--loop`
- Sleeps `--loop-interval` (default 30s)
- Returns `continueLoop=true` to restart the iteration

---

## GitHub Service Functions (`run_service_github.go`)

Each function accepts only the narrowest interface it needs:

| Function | Interface | Description |
|----------|-----------|-------------|
| `ensurePRWithOutput` | `PRPort` | Creates PR if none exists, prints URL |
| `postPRReviewIfRequested` | `PRPort` | Posts review verdict to GitHub PR |
| `runIssueValidation` | `ValidationPort` | Validates open issues against code, closes stale ones |
| `fileIssues` | `IssueFilerPort` | Deduplicates and creates GitHub issues |
| `maybeMergeOnApprove` | `PRPort` | Merges and closes issues on APPROVE |
| `closeRunIssues` | `issueCloser` | Closes all issues created in this run |
| `reconcileStaleIssues` | `issueCloser` | Closes issues whose reported code no longer exists |

```go
// Minimal local interface — only used within this file
type issueCloser interface {
    CommentOnIssue(ctx context.Context, issueNum int, body string) error
    CloseIssue(ctx context.Context, issueNum int) error
}
```

### Issue Deduplication in `fileIssues`

```
FetchIssueIndex() → IssueIndex {titles, fingerprints, summaries}
  For each Finding:
    1. Check fingerprint map → skip if SHA256 matches open issue
    2. Check title map → skip if canonical title matches
    3. CreateIssue() → embed fingerprint in body
    4. Track issue number
  LinkIssuesToPR(prNum, newIssueNums)
```

---

## Run Summary

```go
type RunSummary struct {
    startTime      time.Time
    iterations     int
    issuesCreated  int
    fixesApplied   int
    fixesReverted  int
    finalVerdict   string
}
```

`print(logPath string)` outputs a formatted block at the end of every run:

```
══════════════════════════════════════════
 Run Summary
══════════════════════════════════════════
 Iterations    : 3
 Issues created: 5
 Fixes applied : 2
 Fixes reverted: 0
 Final verdict : APPROVE
 Elapsed       : 4m32s
 Log           : logs/review-20260406-120000.jsonl
══════════════════════════════════════════
```

---

## Branch Creation: `mustCreateFixBranchIfRequested`

When `--create-branch` is set:
1. Creates `fix/opencode-YYYYMMDD-HHMMSS` branch
2. Commits an empty "open fix branch" commit
3. Pushes with `-u origin`
4. Sets `cfg.auditMode = true` (branch has no meaningful HEAD diff)

---

## Env File Loading: `loadEnvFile`

Searches for `.opencode-review.env` then `.env` in the current directory. Parses `KEY=VALUE` lines, trims whitespace, ignores `#` comments. Uses `os.Setenv()` only for keys not already set in the environment — so shell-exported vars always win.

---

## Testing

`run_test.go` covers:
- Flag normalization (e.g. `--merge` implies `--loop`)
- Loop state transitions (step order, `stop` flag behaviour)
- GitHub Port mock wiring

Test fixtures use a mock `github.Port` implementation that records calls without hitting the network.
