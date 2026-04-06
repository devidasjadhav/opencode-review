# Package: internal/review

The `review` package owns two things: building prompts sent to the AI model, and parsing the model's structured response into `Finding` values and a verdict. It has no side effects — all functions are pure transformations of text.

---

## Files

| File | Responsibility |
|------|---------------|
| `prompt.go` | Prompt builders for review, audit, and fix verification |
| `parser.go` | Finding parser, verdict extractor, fingerprint computation |

---

## Prompt Building

### `ReviewContext`

Enriches the review prompt with live codebase state:

```go
type ReviewContext struct {
    ChangedFiles   map[string]string  // repo-relative path → full file content
    ExistingIssues []types.IssueSummary  // already-tracked issues (suppress re-reports)
    LSPEnabled     bool
}
```

`ChangedFiles` is populated by reading every file touched by the commit. `ExistingIssues` comes from `gh.FetchIssueIndex()`. Both are optional — a missing context degrades gracefully.

---

### `BuildReviewPrompt`

```go
func BuildReviewPrompt(hash, subject, body, diff string, rctx ReviewContext) string
```

Builds a commit review prompt. Structure:

1. **LSP preamble** (if `LSPEnabled`) — instructs the model to use goToDefinition, findReferences, hover, workspaceSymbol before reporting any finding
2. **Role instruction** — "You are an expert code reviewer. Apply SOLID and DRY principles."
3. **Existing issues** — list of open GitHub issue titles so the model doesn't re-report them
4. **Commit metadata** — hash, subject, body
5. **Full file contents** — every file changed in the commit (not just the diff)
6. **Diff** — the raw `git show` output
7. **Output format** — strict finding format + verdict instruction

---

### `BuildAuditPrompt`

```go
func BuildAuditPrompt(repoRoot string, changedFiles map[string]bool, lspEnabled bool) (string, error)
```

Builds a full-codebase audit prompt for `--audit` mode. Key behaviours:

- **Changed-files filter**: if `changedFiles` is non-nil and non-empty (set after an auto-fix), only files in that map are included. This prevents re-reviewing unchanged code on iteration 2+.
- **Budget cap**: total included file content is capped at `auditBudgetChars` (300,000 characters) to stay within context limits.
- **Language detection**: uses `internal/langdetect` to skip non-source files, test files, vendor directories, etc.
- Adds a note to the prompt when filtering is active: `"Only showing files changed since last audit."`

---

### `BuildVerifyPrompt`

```go
func BuildVerifyPrompt(diff, findingSummary string) string
```

Builds a short prompt for the verifier model (used by `verifyFixStep`). Includes:
- The diff of the applied fix (`git show HEAD`)
- A structured summary of the findings the fix was supposed to address
- Instruction to respond with only `PASS` or `FAIL` and a brief rationale

---

### LSP Preamble: `lspPreamble()`

When `--lsp` is enabled, this preamble is prepended to every prompt:

```
You have access to LSP tools. Before reporting any finding, verify it using:
- goToDefinition — confirm the symbol definition
- findReferences — check all call sites
- hover — verify type signatures
- goToImplementation — confirm interface implementations
- workspaceSymbol — check for existing helpers before suggesting new ones

Do not report findings you cannot confirm with LSP.
```

This instruction significantly reduces hallucinated findings because the model must verify each claim against the live codebase before reporting it.

---

### Finding Format Instruction: `findingsFormat()`

The model is instructed to use this exact format for each finding:

```markdown
### `[Severity]` file/path.go:10-15 — Short descriptive title

**Description:** What the problem is and why it matters.

**Confidence:** HIGH (certain defect) | MEDIUM (likely issue) | LOW (style/preference)

**Suggested diff:**
` `` `diff
- old code
+ new code
` `` `

**AI agent fix prompt:** A self-contained instruction for an AI agent to fix this finding.
```

Severity values: `CRITICAL`, `HIGH`, `MEDIUM`, `LOW`.

Confidence values control auto-fix eligibility (see `--min-confidence`).

---

### Verdict Instruction

The model must end every response with exactly one of:

```
**Verdict:** APPROVE
**Verdict:** REQUEST CHANGES
**Verdict:** COMMENT
```

---

## Response Parsing

### `ParseFindings`

```go
func ParseFindings(reviewText string) []types.Finding
```

Two-pass parser:

**Pass 1 — Strict parser**: Looks for headings matching:
```
### `[SEVERITY]` file:line-range — title
```
Uses regex: `` ### `\[(\w+)\]` ([^\s:]+):(\S+) — (.+) ``

For each heading, accumulates subsequent lines into fields:
- `**Description:**` → `Finding.Description`
- `**Confidence:**` → `Finding.Confidence` (HIGH/MEDIUM/LOW)
- ` ```diff ` block → `Finding.Diff`
- `**AI agent fix prompt:**` → `Finding.AgentPrompt`

**Pass 2 — Lenient parser**: Falls back only if strict parsing yields 0 findings. Accepts looser heading formats to handle models that deviate slightly from the format.

After parsing, `flush()` computes the fingerprint for each finding.

---

### `computeFingerprint`

```go
func computeFingerprint(file, title, desc string) string
```

Returns the hex SHA256 of `lower(file) + "|" + lower(title) + "|" + lower(desc[:50])`.

Line numbers are deliberately excluded so that the same logical finding isn't re-filed just because surrounding code was refactored and line numbers shifted.

---

### `MeetsConfidence`

```go
func MeetsConfidence(findingConf, minConf string) bool
```

Confidence ranking: `LOW (1) < MEDIUM (2) < HIGH (3)`

Empty `findingConf` (old findings without a Confidence field) is treated as MEDIUM for backward compatibility.

---

### `ExtractVerdict`

```go
func ExtractVerdict(review string) string
```

Scans the review text for the last occurrence of `**Verdict:**` and returns the word following it. Returns `"REQUEST CHANGES"` if no verdict is found (safe default — avoids false APPROVEs).

---

### `ExtractVerifyVerdict`

```go
func ExtractVerifyVerdict(text string) string
```

Looks for `PASS` or `FAIL` (case-insensitive) in the verifier response. Returns `"FAIL"` if neither is found.

---

## Testing

`parser_test.go` and `prompt_test.go` cover:

- Strict parser: single finding, multiple findings, missing fields
- Lenient fallback: triggered when strict yields 0 results
- Confidence parsing: HIGH/MEDIUM/LOW extraction
- Fingerprint stability: same finding = same fingerprint across runs
- `MeetsConfidence`: all threshold combinations
- `ExtractVerdict`: APPROVE, REQUEST CHANGES, COMMENT, missing verdict
- `BuildAuditPrompt`: file budget cap, changedFiles filter
- LSP preamble: injected when `LSPEnabled = true`
