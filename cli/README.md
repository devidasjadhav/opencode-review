# opencode-review

A Go CLI that reviews the latest git commit of a project using [opencode](https://opencode.ai).

It sends the full `git show` diff to opencode, which uses the project's LSP, file tree, and
codebase context to produce a thorough code review — not just a diff summary.

## Requirements

- Go 1.22+
- opencode running locally (`opencode` CLI)

## Build

```bash
go build -o opencode-review .
```

## Usage

```bash
# Review latest commit in current directory (interactive model selection)
./opencode-review

# Specify a repo and skip model selection
./opencode-review --dir /path/to/repo --model 419

# Custom opencode server URL
./opencode-review --url http://localhost:4096 --dir /path/to/repo --model 419
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--url` | `http://localhost:4096` | Opencode server URL |
| `--dir` | current directory | Git repo to review |
| `--model` | 0 (prompts) | Model number from the list (skips prompt) |

## How it works

1. Resolves the git repo root from `--dir`
2. Runs `git show HEAD` to get the latest commit diff
3. Lists available models from the opencode server
4. Creates an opencode session scoped to the repo directory
5. Sends a structured review prompt asking the model to use full codebase context and LSP
6. Streams the response token by token via SSE

## Notes

- The model list numbers change depending on which providers are configured in opencode.
  Use `--model 0` (or omit the flag) to see the current list and pick interactively.
- The opencode server must have the target project's directory accessible.
