# Architecture Overview

`opencode-review` is a Go CLI that orchestrates AI-powered code review, automated fixing, and GitHub integration into a single repeatable loop. This document describes the top-level structure, data flow, and how the packages fit together.

---

## Repository Layout

```
cli/
├── main.go                        # Thin entry point: calls cmd.Run()
├── go.mod / go.sum                # Go 1.22+; three direct dependencies
├── .opencode-review.env           # Per-repo config (not committed)
│
├── cmd/                           # CLI orchestration layer
│   ├── run.go                     # Flag parsing, runConfig, top-level run()
│   ├── run_bootstrap.go           # Environment init, model selection
│   ├── run_loop.go                # 7-step review/fix/merge loop
│   ├── run_service_github.go      # GitHub operations called from the loop
│   ├── run_summary.go             # RunSummary: metrics printed on exit
│   └── envfile.go                 # .opencode-review.env loader
│
└── internal/                      # Business logic — no cmd imports allowed
    ├── types/          types.go   # Shared data types (Finding, ModelInfo)
    ├── opencode/       client.go  # Opencode SDK wrapper: review, fix, verify
    ├── review/         prompt.go  # Prompt builders
    │                  parser.go  # Finding parser, verdict extractor
    ├── github/         port.go    # Role interfaces (Port pattern)
    │                  client.go  # Helper functions
    │                  real_client.go  # Live GitHub API implementation
    │                  noop_client.go  # Dry-run stub
    ├── git/            git.go     # Git shell wrappers
    ├── logger/         logger.go  # JSONL structured logger with rotation
    ├── apperr/         apperr.go  # Error classification (transient/permanent)
    └── langdetect/     detect.go  # Language detection for audit scope
```

---

## External Dependencies

| Package | Purpose |
|---------|---------|
| `github.com/sst/opencode-sdk-go` | SSE session streaming to opencode server |
| `github.com/google/go-github/v67` | GitHub REST API (issues, PRs, merges) |
| `golang.org/x/oauth2` | GitHub token authentication |

---

## End-to-End Data Flow

```
┌──────────────────────────────────────────────────────────────────┐
│  STARTUP                                                          │
│                                                                   │
│  main.go → cmd.Run()                                             │
│    ├─ Load .opencode-review.env                                  │
│    ├─ Parse CLI flags (runConfig)                                │
│    ├─ initEnvironment()                                          │
│    │    ├─ Resolve git repo root                                 │
│    │    ├─ Create opencode SDK client                            │
│    │    ├─ Create JSONL logger (logs/review-*.jsonl)             │
│    │    └─ Create GitFixPersister                                │
│    ├─ initGitHubContext()                                        │
│    │    ├─ Parse owner/repo from git remote URL                  │
│    │    ├─ Instantiate realClient or noOpClient (--dry-run)      │
│    │    └─ EnsureOpenPR() if needed                              │
│    └─ selectModel() → interactive or --model N                   │
└──────────────────────────────────────────────────────────────────┘
                              ↓
┌──────────────────────────────────────────────────────────────────┐
│  REVIEW / FIX / MERGE LOOP  (executeReviewLoop)                  │
│                                                                   │
│  loopRunner.Run() iterates until stop=true                       │
│                                                                   │
│  ┌─ Step 1: reviewStep ──────────────────────────────────────┐  │
│  │  git.GetCommitInfo(ref) → hash, subject, body, diff       │  │
│  │  git.DiffChangedFiles() → changed file paths              │  │
│  │  readFileContents() → file path → content map             │  │
│  │  gh.FetchIssueIndex() → existing issue titles+fingerprints│  │
│  │  review.BuildReviewPrompt() / BuildAuditPrompt()           │  │
│  │  occ.RunReview() → streams to terminal, returns full text  │  │
│  │  review.ParseFindings() → []Finding                        │  │
│  │  review.ExtractVerdict() → APPROVE / REQUEST CHANGES       │  │
│  └────────────────────────────────────────────────────────────┘  │
│                              ↓                                    │
│  ┌─ Step 2: postReviewStep ──────────────────────────────────┐  │
│  │  gh.PostPRReview(prNum, body, verdict)   (if --pr)        │  │
│  │  logger.Write(review_iteration event)                      │  │
│  └────────────────────────────────────────────────────────────┘  │
│                              ↓                                    │
│  ┌─ Step 3: issueStep ───────────────────────────────────────┐  │
│  │  fileIssues() for each Finding:                            │  │
│  │    ├─ Dedup by fingerprint (SHA256) — skip if exists       │  │
│  │    ├─ Dedup by title — skip if similar title exists        │  │
│  │    ├─ gh.CreateIssue() → embeds fingerprint in body        │  │
│  │    └─ gh.LinkIssuesToPR(prNum, issueNums)                  │  │
│  └────────────────────────────────────────────────────────────┘  │
│                              ↓                                    │
│  ┌─ Step 4: mergeStep ───────────────────────────────────────┐  │
│  │  Print verdict                                             │  │
│  │  If APPROVE && --merge: gh.MergePR() → stop=true          │  │
│  │  If not --loop: stop=true                                  │  │
│  └────────────────────────────────────────────────────────────┘  │
│                              ↓                                    │
│  ┌─ Step 5: autoFixStep ─────────────────────────────────────┐  │
│  │  Filter findings by --min-confidence                        │  │
│  │  occ.RunFix() → model applies changes to files             │  │
│  │  GitFixPersister.Persist() → validate, git add+commit+push │  │
│  │  state.changedFiles = paths touched by fix                 │  │
│  └────────────────────────────────────────────────────────────┘  │
│                              ↓                                    │
│  ┌─ Step 6: verifyFixStep ───────────────────────────────────┐  │
│  │  occ.RunVerify() with second model (--verifier-model)      │  │
│  │  If FAIL: git.RevertHead() → changedFiles = nil            │  │
│  └────────────────────────────────────────────────────────────┘  │
│                              ↓                                    │
│  ┌─ Step 7: waitStep ────────────────────────────────────────┐  │
│  │  Sleep(--loop-interval)                                    │  │
│  │  Return continueLoop=true → back to Step 1                 │  │
│  └────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
                              ↓
┌──────────────────────────────────────────────────────────────────┐
│  SHUTDOWN                                                         │
│  RunSummary.print() — iterations, issues, fixes, verdict, elapsed│
│  logger.Close() — flush JSONL log                                │
└──────────────────────────────────────────────────────────────────┘
```

---

## Package Dependency Graph

```
cmd
 ├── internal/types        (Finding, ModelInfo)
 ├── internal/opencode     (RunReview, RunFix, RunVerify)
 ├── internal/review       (BuildPrompt, ParseFindings, ExtractVerdict)
 ├── internal/github       (Port, realClient, noOpClient)
 ├── internal/git          (GetCommitInfo, DiffChangedFiles, RevertHead)
 ├── internal/logger       (JSONL logger)
 └── internal/apperr       (error classification)

internal/opencode
 ├── internal/types
 └── internal/review       (BuildVerifyPrompt, lspPreamble)

internal/github
 └── internal/types        (Finding)

internal/review
 ├── internal/types        (Finding)
 └── internal/langdetect   (Detect, SkipDir)

internal/git              (no internal deps)
internal/logger           (no internal deps)
internal/apperr           (no internal deps)
internal/langdetect       (no internal deps)
internal/types            (no internal deps)
```

No cycles. `internal/*` packages never import `cmd`.

---

## Key Design Patterns

### 1. Port Interfaces (ISP)

`internal/github/port.go` defines three role interfaces that compose into `Port`. Functions accept only the narrowest interface they need — `fileIssues` takes `IssueFilerPort`, `mergeAndClose` takes `PRPort`. This makes unit testing straightforward and enforces single-responsibility.

### 2. Loop Step Pattern

`cmd/run_loop.go` defines `LoopStep` — a single `Run(*loopState) (continueLoop bool, err error)` method. Seven concrete step types implement it. The `loopRunner` executes them sequentially, threading shared state through `loopState`. Adding a new behaviour means adding a new step type without touching existing steps.

### 3. Streaming with Retry

`internal/opencode/client.go`'s `streamSession()` wraps the SDK with up to 3 retries using exponential backoff (5s → 10s → 20s). SSE events stream directly to stdout token-by-token while the full response is buffered for parsing.

### 4. Finding Fingerprints

Each finding gets a SHA256 fingerprint: `lower(file) | lower(title) | lower(desc[:50])`. This fingerprint is embedded in the GitHub issue body as an HTML comment and retrieved on the next run via `FetchIssueIndex`. Duplicate findings are silently skipped without a second API call.

### 5. Dependency Injection

`runEnvironment` bundles `ctx`, `client`, `log`, `repoRoot`, and `fixPersister`. It is constructed once in `initEnvironment()` and threaded through the loop. Tests substitute a fake `FixPersister` without touching the loop logic.

---

## State Machine: Loop Modes

```
                    ┌───────────────────────┐
                    │      reviewStep        │
                    └──────────┬────────────┘
                               │
                    ┌──────────▼────────────┐
                    │    postReviewStep      │
                    └──────────┬────────────┘
                               │
                    ┌──────────▼────────────┐
                    │      issueStep         │
                    └──────────┬────────────┘
                               │
              ┌────────────────▼─────────────────────┐
              │              mergeStep                │
              │  APPROVE && --merge  →  MergePR       │
              │  not --loop          →  stop=true     │
              └──────────┬───────────────────────────┘
                         │ (loop continues)
              ┌──────────▼────────────┐
              │      autoFixStep      │
              │  applies AI fixes     │
              └──────────┬────────────┘
                         │
              ┌──────────▼────────────┐
              │    verifyFixStep      │
              │  FAIL → revert        │
              └──────────┬────────────┘
                         │
              ┌──────────▼────────────┐
              │       waitStep        │
              │  sleep(interval)      │
              │  continueLoop = true  │
              └──────────┬────────────┘
                         │
                    back to reviewStep
```

---

## Detailed Package Docs

| Package | Document |
|---------|---------|
| `cmd` | [arch-cmd.md](arch-cmd.md) |
| `internal/review` | [arch-review.md](arch-review.md) |
| `internal/github` | [arch-github.md](arch-github.md) |
| `internal/opencode` | [arch-opencode.md](arch-opencode.md) |
| `internal/git`, `logger`, `apperr`, `langdetect`, `types` | [arch-supporting.md](arch-supporting.md) |
