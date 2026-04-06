# Contributing Guide

Welcome to `opencode-review`. This guide covers everything a new collaborator needs to get oriented, build, test, and contribute effectively.

---

## Prerequisites

| Tool | Version | Purpose |
|------|---------|---------|
| Go | 1.22+ | Build and test |
| [opencode](https://opencode.ai) | Latest | AI model server (required for live runs) |
| git | Any | VCS operations |
| `GITHUB_TOKEN` | — | Required only for GitHub integration features |

---

## Getting Started

```bash
# Clone
git clone <repo-url>
cd opencode_client/cli

# Build
go build -o opencode-review .

# Run tests
go test ./...

# Verify no vet issues
go vet ./...
```

---

## Project Layout

```
cli/
├── main.go                    # Entry point
├── cmd/                       # CLI layer — owns flags, loop, GitHub calls
├── internal/
│   ├── types/                 # Shared structs (Finding, ModelInfo)
│   ├── opencode/              # opencode SDK wrapper (stream, fix, verify)
│   ├── review/                # Prompt builders + response parsers
│   ├── github/                # GitHub API (Port interfaces + implementations)
│   ├── git/                   # Git shell wrappers
│   ├── logger/                # JSONL logger with rotation
│   ├── apperr/                # Error kind classification
│   └── langdetect/            # Repo language detection for audit scope
└── docs/                      # Architecture documents (you are here)
```

The `internal/` boundary is enforced by Go: no package outside `cli/` can import these. Within `cli/`, `cmd/` imports `internal/*` but `internal/*` packages never import `cmd/`.

---

## Architecture at a Glance

See [architecture.md](architecture.md) for the full picture. The short version:

1. `cmd/run.go` parses flags into `runConfig`
2. `cmd/run_bootstrap.go` resolves the environment (repo root, opencode client, GitHub client)
3. `cmd/run_loop.go` drives a 7-step sequential loop: **review → post → issue → merge → fix → verify → wait**
4. Each step calls into `internal/*` packages
5. The loop continues until verdict is APPROVE (or `--loop` is false)

---

## Development Workflow

### Making a change

```bash
# 1. Create a feature branch
git checkout -b feat/my-change

# 2. Make changes

# 3. Build must pass
go build ./...

# 4. Tests must pass
go test ./...

# 5. Vet must pass
go vet ./...

# 6. Commit
git add <files>
git commit -m "feat: short description of change"
```

### Running against a real repo

```bash
# Start opencode server first
opencode

# In another terminal, review the latest commit of this repo
./opencode-review --model 416

# Full audit with issues (dry-run to preview without filing)
GITHUB_TOKEN=ghp_... ./opencode-review --model 416 --audit --issues --dry-run
```

---

## Testing

### Run all tests

```bash
go test ./...
```

### Run a specific package

```bash
go test ./internal/review/...
go test ./cmd/...
```

### Run with verbose output

```bash
go test -v ./internal/review/...
```

### Test coverage

```bash
go test -cover ./...
```

---

## Key Conventions

### Adding a new CLI flag

1. Add a field to `runConfig` in `cmd/run.go`
2. Register the flag in the `flag.StringVar`/`flag.BoolVar` block in `run()`
3. Add an env key entry in the `flag` block comment and `loadEnvFile()` mapping in `cmd/envfile.go`
4. Thread the new config field into whatever step needs it via `loopState`

### Adding a new loop step

1. Create a new struct type (e.g. `myStep struct{}`) in `cmd/run_loop.go`
2. Implement `Run(state *loopState) (bool, error)` — read from and write to `state`
3. Register it in the step slice in `executeReviewLoop()`
4. Steps execute sequentially — no locking needed

### Adding a new GitHub operation

1. Add the method to the relevant role interface in `internal/github/port.go`
2. Implement it in `internal/github/real_client.go`
3. Add a no-op stub in `internal/github/noop_client.go`
4. Narrow the function signature in `cmd/run_service_github.go` to accept the smallest interface needed

### Adding a new Finding field

1. Add the field to `types.Finding` in `internal/types/types.go`
2. Update `review.ParseFindings()` in `internal/review/parser.go` to populate it from the model output
3. Update the finding format instruction in `review.findingsFormat()` so the model knows to emit it
4. Update `computeFingerprint()` if the field is semantically identifying (should be part of dedup key)
5. Add test coverage in `internal/review/parser_test.go`

---

## Common Mistakes

| Mistake | Correct approach |
|---------|-----------------|
| Importing `cmd/` from `internal/` | Never. `internal/` packages must not import `cmd/` |
| Accepting `github.Port` when only `IssueFilerPort` is needed | Accept the narrowest interface |
| Using `state.changedFiles` to pass data between non-adjacent steps | Only `autoFixStep` writes it; only `reviewStep` reads it — document if you add a new reader |
| Forgetting to update `noop_client.go` after adding a Port method | The compiler will tell you — `noOpClient` must implement all of `Port` |
| Hard-coding the `repoRoot`-relative path assumption | Always use `filepath.Rel(repoRoot, absPath)` when converting absolute paths |

---

## Log Files

Logs are written to `<repo>/logs/review-YYYYMMDD-HHMMSS.jsonl`. They're excluded from git via `.gitignore`. To inspect a log:

```bash
# Pretty-print last run
cat logs/review-*.jsonl | tail -1 | python3 -m json.tool

# All events from a run
cat logs/review-20260406-120000.jsonl | jq .

# Filter for auto-fix events only
cat logs/review-20260406-120000.jsonl | jq 'select(.event == "auto_fix")'
```

---

## Release Process

1. Ensure all tests pass: `go test ./...`
2. Tag the commit: `git tag v0.X && git push origin v0.X`
3. Build a release binary: `go build -o opencode-review .`

---

## Getting Help

- Read [architecture.md](architecture.md) for the full data flow
- Per-package deep dives: [arch-cmd.md](arch-cmd.md), [arch-review.md](arch-review.md), [arch-github.md](arch-github.md), [arch-opencode.md](arch-opencode.md), [arch-supporting.md](arch-supporting.md)
- Check the JSONL logs for runtime behaviour
- Open an issue on the repository
