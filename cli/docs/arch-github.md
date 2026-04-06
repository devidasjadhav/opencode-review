# Package: internal/github

The `github` package manages all communication with the GitHub API: issues, pull requests, merges, and issue validation. It uses the Interface Segregation Principle — callers depend only on the methods they actually use.

---

## Files

| File | Responsibility |
|------|---------------|
| `port.go` | Role-based interfaces (`PRPort`, `ValidationPort`, `IssueFilerPort`, `Port`) |
| `client.go` | Helper functions: `BuildIssueIndex`, `CreateIssue`, `FindingIssueTitle`, `issueLabels` |
| `real_client.go` | Live GitHub API implementation (`realClient`) |
| `noop_client.go` | Dry-run stub (`noOpClient`) |

---

## Interface Design

### Role Interfaces

```go
// PRPort — PR lifecycle
type PRPort interface {
    EnsureOpenPR(ctx context.Context, repoRoot, baseBranch string) (prNum int, created bool, err error)
    PostPRReview(ctx context.Context, prNum int, body, verdict string) error
    MergePR(ctx context.Context, prNum int, strategy string, deleteBranch bool) error
}

// ValidationPort — issue health checks
type ValidationPort interface {
    ValidateIssues(ctx context.Context, repoRoot string) ([]IssueValidity, error)
    CommentOnIssue(ctx context.Context, issueNum int, body string) error
    CloseIssue(ctx context.Context, issueNum int) error
}

// IssueFilerPort — finding → issue lifecycle
type IssueFilerPort interface {
    FetchIssueIndex(ctx context.Context) (IssueIndex, error)
    CreateIssue(ctx context.Context, f types.Finding) (int, error)
    CommentOnIssue(ctx context.Context, issueNum int, body string) error
    LinkIssuesToPR(ctx context.Context, prNum int, issueNums []int) error
}

// Port — full interface used by githubContext
type Port interface {
    PRPort
    ValidationPort
    IssueFilerPort
}
```

Functions in `cmd/run_service_github.go` accept the narrowest role interface they need, making them easy to test independently.

---

## `IssueIndex`

A single paginated fetch replaces three separate API calls per loop iteration:

```go
type IssueIndex struct {
    Titles       map[string]int    // canonical title → issue number
    Fingerprints map[string]int    // sha256 fingerprint → issue number
    Summaries    []types.IssueSummary  // (Number, Title) for prompt context
}
```

`FetchIssueIndex()` calls `BuildIssueIndex()` which:
1. Paginates through all open issues (100 per page)
2. Parses the `<!-- opencode-fingerprint: HASH -->` HTML comment from each body
3. Populates all three maps in one pass

This is the only GitHub API call made per loop iteration for issue management.

---

## Issue Creation

### `FindingIssueTitle`

```go
func FindingIssueTitle(f types.Finding) string
// → "[HIGH] internal/review/parser.go:45-60 — Missing nil check"
```

Canonical title format used both for issue creation and deduplication.

### `issueLabels`

```go
func issueLabels(severity string) []string
```

Maps severity to GitHub label set:

| Severity | Labels |
|----------|--------|
| CRITICAL / HIGH | `code-review`, `critical`/`high`, `bug` |
| MEDIUM | `code-review`, `medium`, `enhancement` |
| LOW | `code-review`, `low`, `good first issue` |

### Issue Body Format

```markdown
## [HIGH] internal/review/parser.go:45-60 — Missing nil check

**Description:** ...

**Suggested fix:**
```diff
- old
+ new
```

_Reported by opencode-review_

<!-- opencode-fingerprint: a1b2c3...64hex chars... -->
```

The fingerprint comment is machine-readable and invisible in the GitHub UI.

---

## `realClient`

Uses `google/go-github/v67` with OAuth2 token transport:

```go
type realClient struct {
    gh    *gogithub.Client
    owner string
    repo  string
}
```

Created by `NewRealClient(token, owner, repo)`. All API calls go through the `go-github` client.

### PR Operations

**`EnsureOpenPR`**: Searches for an open PR with `base=baseBranch` and `head=currentBranch`. If none exists, creates one titled `"opencode-review: <branch>"`. Returns the PR number and a `created` flag.

**`PostPRReview`**: Submits a PR review event. Maps verdict:
- `APPROVE` → `APPROVE` event
- `REQUEST CHANGES` → `REQUEST_CHANGES` event
- anything else → `COMMENT` event

**`MergePR`**: Merges with the specified strategy (`merge`, `squash`, `rebase`). Optionally deletes the branch after merge.

### Issue Operations

**`CreateIssue`**: Creates with title, body (including fingerprint), and labels. Returns the new issue number.

**`CommentOnIssue`**: Posts a comment to an existing issue.

**`CloseIssue`**: Sets issue state to `"closed"`.

**`LinkIssuesToPR`**: Posts a comment on each issue referencing the PR number: `"This issue is being addressed in PR #N."` Also posts on the PR listing all linked issues.

### Issue Validation

**`ValidateIssues`**: For each open issue with the `code-review` label:
1. Parses the file path and line range from the issue title
2. Checks if the file still exists on disk
3. Checks if the reported line range is still present in the file
4. Returns `[]IssueValidity` with `Valid`, `Reason`, `IssueNum` fields

---

## `noOpClient`

Implements `Port` but performs no real API calls. Used when `--dry-run` is set.

All mutating methods (`CreateIssue`, `PostPRReview`, `MergePR`, etc.) print what they would do and return zero values. Read methods (`FetchIssueIndex`) return empty results.

This lets the full loop run in preview mode without touching GitHub.

---

## Authentication

GitHub token is read from `GITHUB_TOKEN` environment variable. If not set:
- `gh` field in `githubContext` is `nil`
- All GitHub operations are silently skipped
- `--issues`, `--pr`, `--merge` have no effect without a token

---

## Error Handling

GitHub errors are classified via `internal/apperr`:
- HTTP 429 (rate limit), 502, 503, 504 → `KindTransient` (safe to retry)
- All other HTTP errors → `KindPermanent` (fail fast)

The loop runner does not currently auto-retry transient errors, but the classification is in place for future retry logic.

---

## Testing

`noop_client.go` doubles as the test stub — tests in `cmd/` pass it as the `Port` to exercise the full service layer without network calls.

To add a test for a new GitHub operation:
1. Add the method to the relevant role interface in `port.go`
2. Implement it in `real_client.go`
3. Add a no-op stub in `noop_client.go`
4. Write the test using `noOpClient` or a custom mock struct
