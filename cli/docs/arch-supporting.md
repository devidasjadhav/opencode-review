# Supporting Packages

These packages provide utilities used across the codebase. They have no dependencies on each other (except `review` depending on `langdetect`) and no dependencies on `cmd`.

---

## `internal/types`

Shared data types. No logic — just struct definitions. Every package imports this.

```go
// ModelInfo identifies a model returned by the opencode server
type ModelInfo struct {
    ProviderID   string
    ProviderName string
    ModelID      string
    ModelName    string
}

// Finding is one review finding, parsed from the model's response
type Finding struct {
    Severity    string  // CRITICAL | HIGH | MEDIUM | LOW
    File        string  // repo-relative path
    LineRange   string  // "45" or "45-60"
    Title       string  // short summary
    Description string  // full explanation
    Diff        string  // suggested code change
    AgentPrompt string  // self-contained instruction for auto-fix agent
    IssueNumber int     // GitHub issue number if filed (0 = not filed)
    Fingerprint string  // sha256(lower(file)|lower(title)|lower(desc[:50]))
    Confidence  string  // HIGH | MEDIUM | LOW | "" (empty = treat as MEDIUM)
}

// IssueSummary is a lightweight view of an open GitHub issue
type IssueSummary struct {
    Number int
    Title  string
}
```

**Adding a field to `Finding`**: update `internal/types/types.go`, then update the parser in `internal/review/parser.go` to populate it, and optionally the fingerprint formula if the new field is semantically identifying.

---

## `internal/git`

Thin wrappers around `git` shell commands. All functions take `repoRoot string` as first argument.

### Key Functions

```go
// Run executes a git command and returns trimmed combined stdout+stderr
func Run(dir string, args ...string) (string, error)

// ResolveRepoRoot finds the repo root from any subdirectory
func ResolveRepoRoot(dir string) (string, error)
// → git rev-parse --show-toplevel

// GetCommitInfo returns structured info about a commit ref
func GetCommitInfo(root, ref string) (hash, subject, body, diff string, err error)
// hash = full 40-char SHA, diff = full git show output

// DiffChangedFiles lists repo-relative paths changed in a commit
func DiffChangedFiles(root, ref string) ([]string, error)
// → git diff-tree --no-commit-id -r --name-only <ref>

// DiffHead returns the diff of HEAD vs its parent (for fix verification)
func DiffHead(repoRoot string) (string, error)
// → git show --stat --patch HEAD

// RevertHead reverts the most recent commit (used when fix verification fails)
func RevertHead(repoRoot string) error
// → git revert --no-edit HEAD && git push origin <branch>

// StatusSnapshot captures the working tree state before/after fix application
func StatusSnapshot(root string) (StatusSnapshotMap, error)
// → git status --porcelain=v1

// FixerStagePaths compares before/after snapshots and stages changed files
func FixerStagePaths(before, after StatusSnapshotMap) []string
// Returns repo-relative paths of files that changed between snapshots
```

### Error Handling

`Run()` returns a combined error that includes both stderr output and the exit code. Callers use `wrapErr()` in `cmd/run_bootstrap.go` to add context before propagating.

---

## `internal/logger`

Structured JSONL logger. One log file per run, auto-rotated.

### Usage

```go
log := logger.New(repoRoot)
defer log.Close()

log.Write(map[string]any{
    "event":     "review_iteration",
    "iteration": 1,
    "verdict":   "REQUEST CHANGES",
})
```

Each `Write` call appends one JSON line to the log file. The timestamp (`ts`) field is added by the caller in `cmd/run_loop.go:logEvent()`.

### Log File Location

```
<repoRoot>/logs/review-YYYYMMDD-HHMMSS.jsonl
```

### Rotation

`New()` calls `rotateLogs()` before creating the new file. It lists all `review-*.jsonl` files in the logs directory, sorts by name (which is time-ordered), and deletes the oldest files beyond `MaxLogFiles = 20`.

### Methods

```go
func New(repoRoot string) *Logger
func (l *Logger) Write(record any) error  // JSON-encodes and appends
func (l *Logger) LogPath() string         // for RunSummary output
func (l *Logger) Close()                  // flush and close
```

### Log Events

| Event | Fields | When |
|-------|--------|------|
| `review_iteration` | `iteration`, `commit`, `subject`, `verdict` | After each review step |
| `auto_fix` | `findings_fixed` | After successful fix application |
| `auto_fix_error` | `error`, `iteration` | When fix session fails |
| `verify_fix` | `iteration`, `verify_verdict` | After verification step |
| `verify_fix_reverted` | `iteration` | When fix is reverted |

---

## `internal/apperr`

Error classification for structured error handling.

### Error Kinds

```go
type Kind int

const (
    KindPermanent Kind = iota  // Logic/config error — fail fast, no retry
    KindTransient              // Network/rate-limit — safe to retry
    KindWarning                // Non-fatal — log and continue
)
```

### `AppError`

```go
type AppError struct {
    Kind    Kind
    Context string  // human-readable prefix
    Err     error   // underlying error
}

func (e *AppError) Error() string  // "Context: underlying error message"
func (e *AppError) Unwrap() error  // supports errors.Is/As
```

### Constructors

```go
func Permanent(context string, err error) *AppError
func Transient(context string, err error) *AppError
func Warning(context string, err error) *AppError
```

### Classification Helpers

```go
func IsTransient(err error) bool  // true if AppError.Kind == KindTransient
func IsWarning(err error) bool    // true if AppError.Kind == KindWarning
```

### GitHub Error Classification

```go
func ClassifyGitHub(context string, err error) *AppError
```

Maps HTTP status codes:
- **429** (rate limit), **502**, **503**, **504** (gateway/availability) → `KindTransient`
- All others → `KindPermanent`

### Usage Pattern

```go
if err := gh.CreateIssue(ctx, finding); err != nil {
    appErr := apperr.ClassifyGitHub("create issue", err)
    if apperr.IsTransient(appErr) {
        // log and continue rather than abort
        log.Write(map[string]any{"event": "github_transient_error", "error": err.Error()})
        continue
    }
    return appErr
}
```

---

## `internal/langdetect`

Detects the primary programming language of a repository for audit scoping.

### `Language` Type

```go
type Language int
const (
    Unknown Language = iota
    Go
    Python
    TypeScript
    Rust
)
```

### `Detect`

```go
func Detect(repoRoot string) Language
```

Checks for marker files in priority order:
1. `go.mod` → Go
2. `requirements.txt` / `pyproject.toml` / `setup.py` → Python
3. `package.json` → TypeScript
4. `Cargo.toml` → Rust
5. No marker found → Unknown

### `HasExtension`

```go
func (l Language) HasExtension(path string) bool
```

Returns true if `path` matches the file extension(s) for the language:
- Go: `.go`
- Python: `.py`
- TypeScript: `.ts`, `.tsx`, `.js`, `.jsx`
- Rust: `.rs`
- Unknown: any non-binary file (`.go`, `.py`, `.ts`, `.js`, `.rs`, `.md`, `.json`, `.yaml`, `.toml`, `.txt`)

### `IsTestFile`

```go
func (l Language) IsTestFile(path string) bool
```

Detects test files by convention:
- Go: `_test.go` suffix
- Python: `test_` prefix or `_test.py` suffix
- TypeScript: `.test.ts`, `.test.tsx`, `.spec.ts`, `.spec.tsx`
- Rust: `tests/` directory prefix

Test files are excluded from audit prompts to keep context focused on production code.

### `SkipDir`

```go
func SkipDir(name string) bool
```

Returns true for directories that should never be walked during audit:

```
.git  node_modules  vendor  __pycache__  target  build  dist
.next  .nuxt  coverage  .cache  tmp  temp  logs
```

---

## Adding a New Language

1. Add a constant to the `Language` type in `detect.go`
2. Add a marker file check in `Detect()`
3. Add extension matching in `HasExtension()`
4. Add test file detection in `IsTestFile()` if applicable
5. Add a test case in `detect_test.go`
