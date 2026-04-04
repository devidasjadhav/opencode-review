# opencode-review — Usage Guide

## Prerequisites

- [opencode](https://opencode.ai) running locally (default: `http://localhost:4096`)
- `GITHUB_TOKEN` exported in your shell (a GitHub PAT with `repo` scope)

```bash
export GITHUB_TOKEN=ghp_...
```

## Build

```bash
cd cli
go build -o opencode-review .
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--url` | `http://localhost:4096` | Opencode server URL |
| `--dir` | current directory | Git repo to review |
| `--model` | interactive | Model number from the listed models |
| `--commit` | `HEAD` | Git ref to review (hash, branch, tag) |
| `--audit` | false | Full SOLID/DRY audit of entire codebase |
| `--pr` | false | Post review as GitHub PR comment |
| `--issues` | false | Create GitHub issues for each finding |
| `--loop` | false | Keep reviewing until APPROVE, then merge |
| `--auto-fix` | false | Auto-apply AI fixes between loop iterations (requires `--loop`) |
| `--merge` | false | Merge PR immediately on APPROVE (one-shot) |
| `--merge-strategy` | `squash` | `merge`, `squash`, or `rebase` |
| `--delete-branch` | false | Delete branch after merge |
| `--loop-interval` | `30s` | Wait between loop iterations |

## Examples

### Review latest commit interactively
```bash
./opencode-review
```

### Review with a specific model (skip prompt)
```bash
./opencode-review --model 416
```

### Post review to an open PR
```bash
./opencode-review --model 416 --pr
```

### Create GitHub issues for findings
```bash
./opencode-review --model 416 --issues
```

### Full SOLID/DRY audit of codebase
```bash
./opencode-review --model 416 --audit --issues
```

### Auto-fix loop until APPROVE, then merge
```bash
./opencode-review --model 416 --loop --auto-fix --pr --issues --merge --delete-branch
```

### One-shot merge on APPROVE
```bash
./opencode-review --model 416 --merge --pr
```

## Package Structure

```
cli/
├── main.go                  # Entry point — calls cmd.Run()
├── cmd/
│   └── run.go               # CLI flags, review/fix/merge loop
├── internal/
│   ├── types/
│   │   └── types.go         # Shared data types (ModelInfo, Finding)
│   ├── git/
│   │   └── git.go           # Git helpers (run, diff, status)
│   ├── github/
│   │   └── client.go        # GitHub API (issues, PRs, reviews)
│   ├── opencode/
│   │   └── client.go        # Opencode SDK (models, streaming, fix)
│   ├── review/
│   │   ├── prompt.go        # Prompt builders (review + audit)
│   │   └── parser.go        # Finding parser + verdict extractor
│   └── logger/
│       └── logger.go        # JSONL run logger
└── docs/
    └── usage.md             # This file
```

## Logs

Each run writes a JSONL log to `<repo>/logs/review-YYYYMMDD-HHMMSS.jsonl` with entries for each iteration, issue created, fix applied, and PR merged.
