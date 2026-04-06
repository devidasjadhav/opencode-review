# Package: internal/opencode

The `opencode` package wraps the opencode SDK. It handles model listing, session creation, SSE streaming with retry logic, and fix application with git persistence. Everything that talks to the opencode server lives here.

---

## Files

| File | Responsibility |
|------|---------------|
| `client.go` | `ListModels`, `SelectModel`, `RunReview`, `RunFix`, `RunVerify`, `streamSession` |
| `fix_validator.go` | `FixPersister`, `FixValidator`, `GitFixPersister`, `GoBuildFixValidator` |

---

## Model Management

### `ListModels`

```go
func ListModels(client *sdk.Client, ctx context.Context, dir string) ([]types.ModelInfo, error)
```

Calls the opencode `/models` endpoint. Returns a slice ordered by provider then model name.

### `SelectModel`

```go
func SelectModel(models []types.ModelInfo) (types.ModelInfo, error)
```

Prints a numbered list to stdout, reads a line from stdin. Returns the chosen model. Returns `io.EOF` if stdin is closed (detects non-interactive use).

---

## Streaming Sessions

### `streamSession`

```go
func streamSession(client *sdk.Client, ctx context.Context, dir string,
    selected types.ModelInfo, prompt string, renderer func(string)) (string, error)
```

Core primitive. Creates a new opencode session scoped to `dir`, sends the prompt, and reads the SSE stream:

- **`PartDelta` events**: token text is passed to `renderer(token)` for live terminal output, and accumulated in a buffer
- **`SessionIdle` event**: stream is complete — returns the full buffered text
- **Error events**: returned as errors

**Retry logic**: up to 3 attempts with exponential backoff:
```
attempt 1: immediate
attempt 2: wait 5s
attempt 3: wait 10s
```

Each retry creates a fresh session (previous partial session is abandoned).

### `RunReview`

```go
func RunReview(client *sdk.Client, ctx context.Context, repoRoot string,
    selected types.ModelInfo, prompt string) (string, error)
```

Calls `streamSession` with a live-rendering `renderer` that writes each token to stdout. Returns the full review text for parsing.

### `RunVerify`

```go
func RunVerify(client *sdk.Client, ctx context.Context, repoRoot string,
    verifier types.ModelInfo, prompt string) (string, error)
```

Same as `RunReview` but for the second model's verification step. Renders to stdout under the `--- Independent Verification ---` heading.

### `RunFix`

```go
func RunFix(client *sdk.Client, ctx context.Context, repoRoot string,
    selected types.ModelInfo, findings []types.Finding, iteration int,
    persister FixPersister, lspEnabled bool) (n int, changedPaths []string, err error)
```

1. Calls `buildFixPrompt(findings, lspEnabled)` — embeds current file contents + AI agent prompts + LSP instruction
2. Streams to the model via `streamSession` (terminal output suppressed — only result matters)
3. Calls `persister.Persist(repoRoot, findings, iteration, stagePaths)` to commit the changes
4. Returns the count of applied fixes and the paths of changed files

#### Fix Prompt Structure

```
[LSP preamble if lspEnabled]

You are an automated code-fix agent. Apply ALL of the following fixes:

Finding 1: [SEVERITY] file:lines — title
File: file/path.go (current content below)
```go
<full current file content>
```
Fix instruction: <AgentPrompt from Finding>

Finding 2: ...

Commit your changes when done. Commit message:
fix: auto-fix N finding(s) from review iteration M
```

---

## Fix Persistence

### `FixPersister` Interface

```go
type FixPersister interface {
    Persist(repoRoot string, findings []types.Finding, iteration int,
        stagePaths []string) (fixPersistResult, error)
}

type fixPersistResult struct {
    StagePaths []string  // repo-relative paths that were staged
}
```

### `GitFixPersister`

Production implementation. After the model applies changes to disk:

1. **Snapshot**: takes `git.StatusSnapshot()` before and after to detect which files changed
2. **Validate**: runs `FixValidator.Validate()` (e.g. `go build`) on changed packages — reverts if build fails
3. **Stage**: `git add <changed files>`
4. **Commit**: `git commit -m "fix: auto-fix N finding(s) from review iteration M"`
5. **Push**: `git push origin <branch>`

Returns `StagePaths` (the files that were staged), which become `changedFiles` in `loopState`.

### `FixValidator` Interface

```go
type FixValidator interface {
    Validate(repoRoot string, changedPaths []string) error
}
```

### `GoBuildFixValidator`

Detects the Go packages affected by `changedPaths` and runs `go build ./pkg/...` on each. Returns an error if any package fails to compile — causing `GitFixPersister` to discard the fix.

Only used when `langdetect.Detect(repoRoot)` returns `Go`.

---

## Session Scoping

Every session is created with `dir = repoRoot`. The opencode server uses this directory to:
- Initialise its file tree browser
- Scope LSP to the correct project
- Resolve relative file paths in the model's edits

This means the opencode server must have `repoRoot` accessible on its filesystem. For remote servers, mount the repo or use `--url` to point at a server that already has it.

---

## Error Handling

`streamSession` returns errors for:
- Network failures (connection refused, timeout)
- SDK-level errors (session creation failed, stream closed unexpectedly)

These are retried up to 3 times. Persistent failures propagate to the loop runner as fatal errors.

Fix validation failures (e.g. `go build` error) are non-fatal — the fix is discarded and the next loop iteration re-reviews the unchanged code.

---

## Testing

`fix_validator.go` is unit-tested via `fix_validator_test.go`:
- `GoBuildFixValidator` with a real temporary Go module
- `GitFixPersister` with a real temporary git repo

`client.go` is integration-tested only (requires a running opencode server). To run integration tests:
```bash
OPENCODE_URL=http://localhost:4096 go test ./internal/opencode/... -tags=integration
```
