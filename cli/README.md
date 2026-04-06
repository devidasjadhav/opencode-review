# opencode-review

A Go CLI that performs AI-powered code reviews on git commits and entire codebases using [opencode](https://opencode.ai). It streams the review live to your terminal, optionally files GitHub issues, auto-applies fixes, and merges PRs when the review passes.

---

## Quick Start

### Prerequisites

| Requirement | Notes |
|-------------|-------|
| Go 1.22+ | For building |
| [opencode](https://opencode.ai) | Running locally on port 4096 |
| `GITHUB_TOKEN` | Required for `--issues`, `--pr`, `--merge` |

### Build

```bash
cd cli
go build -o opencode-review .
```

### Run your first review

```bash
# Review the latest commit (interactive model selection)
./opencode-review

# Skip model selection with --model
./opencode-review --model 416

# Full audit of the entire codebase
./opencode-review --model 416 --audit

# Auto-fix loop: review → fix → re-review until APPROVE, then merge
./opencode-review --model 416 --audit --loop --auto-fix --issues --merge
```

---

## Configuration

Create `.opencode-review.env` in your repository root to persist settings:

```env
MODEL=416
AUDIT=true
LOOP=true
AUTO_FIX=true
ISSUES=true
MERGE=true
MIN_CONFIDENCE=MEDIUM
# GITHUB_TOKEN — do not commit. Export in shell or CI:
#   export GITHUB_TOKEN=ghp_...
```

CLI flags override env file values. Precedence: **flag > env file > default**.

---

## All Flags

| Flag | Default | Env Key | Description |
|------|---------|---------|-------------|
| `--url` | `http://localhost:4096` | `URL` | Opencode server URL |
| `--dir` | `.` | — | Git repo to review |
| `--model` | 0 (interactive) | `MODEL` | Model number (skips prompt) |
| `--commit` | `HEAD` | — | Git ref to review |
| `--audit` | false | `AUDIT` | Full SOLID/DRY codebase audit |
| `--pr` | false | `PR` | Post review as GitHub PR review |
| `--issues` | false | `ISSUES` | File GitHub issues for findings |
| `--loop` | false | `LOOP` | Keep reviewing until APPROVE |
| `--auto-fix` | false | `AUTO_FIX` | AI applies fixes between iterations |
| `--merge` | false | `MERGE` | Merge PR on APPROVE |
| `--merge-strategy` | `squash` | `MERGE_STRATEGY` | `merge`, `squash`, or `rebase` |
| `--delete-branch` | false | `DELETE_BRANCH` | Delete branch after merge |
| `--loop-interval` | `30s` | `LOOP_INTERVAL` | Wait between iterations |
| `--base` | `master` | `BASE` | Base branch for PRs |
| `--create-branch` | false | `CREATE_BRANCH` | Create new fix branch before PR |
| `--validate-issues` | false | `VALIDATE_ISSUES` | Close stale GitHub issues |
| `--min-confidence` | `MEDIUM` | `MIN_CONFIDENCE` | Auto-fix threshold: `HIGH`/`MEDIUM`/`LOW` |
| `--verifier-model` | 0 (off) | `VERIFIER_MODEL` | Second model to verify fixes |
| `--dry-run` | false | `DRY_RUN` | Print GitHub actions without executing |
| `--lsp` | false | `LSP` | Enable LSP tools in prompts |

---

## Common Recipes

```bash
# Review a specific commit
./opencode-review --model 416 --commit abc1234

# Post review to GitHub PR
GITHUB_TOKEN=ghp_... ./opencode-review --model 416 --pr

# File issues for findings, dry-run to preview
GITHUB_TOKEN=ghp_... ./opencode-review --model 416 --audit --issues --dry-run

# Loop until approve with dual-model verification
./opencode-review --model 416 --verifier-model 417 --loop --auto-fix

# CI usage
export GITHUB_TOKEN=${{ secrets.GITHUB_TOKEN }}
./opencode-review --model 416 --audit --issues --loop --auto-fix --merge
```

---

## How It Works

1. **Resolve repo** — finds git root from `--dir`
2. **Select model** — lists models from opencode, user picks (or `--model` skips)
3. **Build prompt** — commit diff or full codebase audit with file contents + existing issues
4. **Stream review** — sends to opencode, streams response live to terminal
5. **Parse findings** — extracts structured `[Severity] file:line — title` entries
6. **Act on verdict** — post PR review, file issues, auto-fix, verify, merge
7. **Loop** — if `--loop`, repeats from step 3 until `APPROVE`
8. **Print summary** — iterations, issues filed, fixes applied, elapsed time

See [docs/architecture.md](docs/architecture.md) for the full architecture.

---

## GitHub Token Permissions

The `GITHUB_TOKEN` needs these scopes:

| Scope | Required for |
|-------|-------------|
| `issues: write` | Filing issues |
| `pull_requests: write` | Posting PR reviews, merging |
| `contents: read` | Reading file contents for validation |

Fine-grained PATs are supported and recommended over classic tokens.

---

## LSP Mode

Start opencode with LSP tools enabled, then pass `--lsp`:

```bash
OPENCODE_EXPERIMENTAL_LSP_TOOL=true opencode
./opencode-review --model 416 --audit --lsp
```

LSP mode instructs the model to use `goToDefinition`, `findReferences`, `hover`, and `workspaceSymbol` to validate findings against the actual codebase — reducing hallucinations significantly.

---

## Log Files

Each run writes a structured JSONL log to `<repo>/logs/review-YYYYMMDD-HHMMSS.jsonl`. The 20 most recent logs are kept automatically.

---

## Further Reading

- [Architecture Overview](docs/architecture.md)
- [Contributing Guide](docs/contributing.md)
- [Package: cmd](docs/arch-cmd.md)
- [Package: internal/review](docs/arch-review.md)
- [Package: internal/github](docs/arch-github.md)
- [Package: internal/opencode](docs/arch-opencode.md)
- [Package: internal/git & utilities](docs/arch-supporting.md)
