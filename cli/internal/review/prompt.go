package review

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/talk/opencode-client/internal/langdetect"
	"github.com/talk/opencode-client/internal/types"
)

// ReviewContext carries enriched context gathered before building a review prompt.
// All fields are optional; absent fields are silently omitted from the prompt.
type ReviewContext struct {
	// ChangedFiles maps repo-relative paths to their full current content.
	// Embedding whole files (not just diff hunks) lets the model understand
	// surrounding code, types, and imports without guessing.
	ChangedFiles map[string]string
	// ExistingIssues lists open GitHub issues already tracking findings.
	// The model is instructed not to re-report these.
	ExistingIssues []types.IssueSummary
	// LSPEnabled instructs the model to use LSP navigation tools before
	// reporting findings. Only effective when opencode is started with
	// OPENCODE_EXPERIMENTAL_LSP_TOOL=true.
	LSPEnabled bool
}

// lspPreamble returns the LSP usage instruction block for prompts.
// The model is explicitly told which tools to call and when, so it navigates
// the codebase instead of guessing at types and call sites.
func lspPreamble() string {
	return `## LSP Navigation — Use Before Reporting Findings
You have access to LSP (Language Server Protocol) tools. Use them BEFORE filing any finding:
- **goToDefinition** — verify a symbol's definition before claiming a type contract is violated
- **findReferences** — confirm a function/type is actually duplicated before flagging DRY violations
- **hover** — check the inferred type of an expression before flagging a type mismatch
- **goToImplementation** — check all concrete implementations of an interface before flagging ISP violations
- **workspaceSymbol** — search for existing helpers before flagging that one should be extracted

Do NOT report a finding if LSP navigation shows your assumption is wrong.

`
}

func findingsFormat(sb *strings.Builder) {
	sb.WriteString("For each finding use EXACTLY this format:\n\n")
	sb.WriteString("### `[🔴 Critical|🟠 High|🟡 Medium|🔵 Low]` file:line-range — Short title\n")
	sb.WriteString("Clear description referencing the exact code and which principle it violates.\n\n")
	sb.WriteString("**Confidence:** HIGH (certain defect) | MEDIUM (likely issue) | LOW (style/preference)\n\n")
	sb.WriteString("```diff\n- old code\n+ fixed code\n```\n\n")
	sb.WriteString("**AI agent fix prompt:** One-paragraph instruction a coding agent can execute directly to fix this issue.\n\n")
	sb.WriteString("Only report HIGH and MEDIUM confidence findings. Omit LOW confidence findings entirely.\n\n")
	sb.WriteString("If there are no issues: write `_No issues found._` and skip subsections.\n\n")
}

// BuildVerifyPrompt builds a short focused prompt asking a second model to
// verify whether a fix diff correctly addresses the stated findings.
func BuildVerifyPrompt(diff, findings string) string {
	var sb strings.Builder
	sb.WriteString("You are an independent code reviewer. A previous model applied automated fixes.\n")
	sb.WriteString("Your job: verify whether those fixes are correct and complete.\n\n")
	sb.WriteString("Respond with EXACTLY one of:\n\n")
	sb.WriteString("  PASS — <one sentence: why the fix is correct>\n")
	sb.WriteString("  FAIL — <one sentence: what is wrong or missing>\n\n")
	sb.WriteString("Rules:\n")
	sb.WriteString("- Output only the verdict line above. No prose, no headings, no lists.\n")
	sb.WriteString("- PASS only if the fix fully addresses ALL findings below without introducing new defects.\n")
	sb.WriteString("- FAIL if the fix is incomplete, incorrect, introduces a regression, or makes unrelated changes.\n\n")
	sb.WriteString("--- Findings that were supposed to be fixed ---\n")
	sb.WriteString(findings)
	sb.WriteString("\n\n--- Fix diff (git show HEAD) ---\n")
	sb.WriteString(diff)
	return sb.String()
}

// ExtractVerifyVerdict parses PASS or FAIL from a verifier response.
// Defaults to FAIL (fail-safe) if neither token is found.
func ExtractVerifyVerdict(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.ToUpper(line))
		if strings.HasPrefix(line, "PASS") {
			return "PASS"
		}
		if strings.HasPrefix(line, "FAIL") {
			return "FAIL"
		}
	}
	return "FAIL"
}

// BuildReviewPrompt builds a commit review prompt with SOLID/DRY checks.
// rctx enriches the prompt with full file contents and existing issues so the
// model has real context rather than only the diff.
func BuildReviewPrompt(hash, subject, body, diff string, rctx ReviewContext) string {
	var sb strings.Builder
	if rctx.LSPEnabled {
		sb.WriteString(lspPreamble())
	}
	sb.WriteString("Review the following git commit for correctness, SOLID, and DRY violations.\n")
	sb.WriteString("The full current content of every changed file is provided below the diff.\n")
	sb.WriteString("Use those file contents — not just the diff hunks — to understand types, imports, and call sites.\n\n")
	sb.WriteString("Respond ONLY with this exact structure (no prose outside it):\n\n")

	sb.WriteString("## Summary\n")
	sb.WriteString("One sentence: what this commit does and why.\n\n")

	sb.WriteString("## Walkthrough\n")
	sb.WriteString("Per-file bullet list: `file` — what changed and the intent.\n\n")

	sb.WriteString("## Findings\n")
	sb.WriteString("Report real, verifiable issues. Check for:\n")
	sb.WriteString("- **S** Single Responsibility: functions/types doing more than one job\n")
	sb.WriteString("- **O** Open/Closed: logic that must be edited (not extended) to add behaviour\n")
	sb.WriteString("- **L** Liskov: interface implementations that break caller assumptions\n")
	sb.WriteString("- **I** Interface Segregation: fat interfaces forcing unused method stubs\n")
	sb.WriteString("- **D** Dependency Inversion: high-level code depending on concrete low-level details\n")
	sb.WriteString("- **DRY**: duplicated logic that should be extracted into a shared helper\n\n")
	findingsFormat(&sb)

	sb.WriteString("## Verdict\n")
	sb.WriteString("Exactly one token on its own line, followed by one sentence reason:\n")
	sb.WriteString("`APPROVE` — safe to merge.\n")
	sb.WriteString("`REQUEST CHANGES` — must fix before merge.\n")
	sb.WriteString("`COMMENT` — observations only.\n\n")

	// Commit metadata + diff
	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "Commit: %s\n", hash)
	fmt.Fprintf(&sb, "Subject: %s\n", subject)
	if body != "" {
		fmt.Fprintf(&sb, "Body:\n%s\n", body)
	}
	sb.WriteString("\n## Diff\n```diff\n")
	sb.WriteString(diff)
	sb.WriteString("\n```\n")

	// Full file contents — the key context enrichment
	if len(rctx.ChangedFiles) > 0 {
		sb.WriteString("\n## Current File Contents\n")
		sb.WriteString("These are the complete current versions of each changed file.\n\n")
		// Sort for deterministic output
		paths := make([]string, 0, len(rctx.ChangedFiles))
		for p := range rctx.ChangedFiles {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(&sb, "### %s\n```\n%s\n```\n\n", p, rctx.ChangedFiles[p])
		}
	}

	// Existing issues — prevents re-reporting already tracked findings
	if len(rctx.ExistingIssues) > 0 {
		sb.WriteString("## Already Tracked Issues — Do NOT Re-Report\n")
		sb.WriteString("These findings are already filed as GitHub issues. Do not include them in your Findings section.\n\n")
		for _, iss := range rctx.ExistingIssues {
			fmt.Fprintf(&sb, "- #%d %s\n", iss.Number, iss.Title)
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// auditBudgetChars is the maximum total characters of file content embedded in
// an audit prompt. Keeps the prompt within model context limits (~75K tokens).
const auditBudgetChars = 300_000

// auditFile holds metadata for a candidate source file during prompt assembly.
type auditFile struct {
	relPath string
	size    int64
	modTime time.Time
}

// BuildAuditPrompt builds a full SOLID/DRY audit prompt.
// The project language is auto-detected from repoRoot marker files so the
// audit works for Go, Python, TypeScript, Rust, or any unknown language.
// changedFiles, when non-nil and non-empty, restricts embedding to only those
// files (relative to repoRoot). Pass nil for a full first-iteration audit.
// Files are sorted by modification time (most recent first) and embedded until
// the context budget is reached; a note is added when files are omitted.
// BuildAuditPrompt builds a full SOLID/DRY audit prompt.
// lspEnabled instructs the model to use LSP tools before reporting findings.
func BuildAuditPrompt(repoRoot string, changedFiles map[string]bool, lspEnabled bool) (string, error) {
	lang := langdetect.Detect(repoRoot)

	var sb strings.Builder
	if lspEnabled {
		sb.WriteString(lspPreamble())
	}
	fmt.Fprintf(&sb, "Perform a SOLID and DRY audit of the %s files provided below.\n", lang.Name)
	if len(changedFiles) > 0 {
		sb.WriteString("Note: Only showing files changed since last audit. Unchanged files have already been reviewed.\n\n")
	}
	sb.WriteString("Read each file below in full. For every violation report EXACTLY:\n\n")
	sb.WriteString("## Findings\n")
	sb.WriteString("For EVERY violation use EXACTLY this format (no list markers, use ### headers):\n\n")
	findingsFormat(&sb)

	sb.WriteString("## Verdict\n")
	sb.WriteString("Exactly one token on its own line:\n")
	sb.WriteString("`APPROVE` — no violations found.\n")
	sb.WriteString("`REQUEST CHANGES` — violations must be fixed.\n\n")
	sb.WriteString("---\n")

	// Collect candidate files respecting changedFiles filter.
	var candidates []auditFile
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if langdetect.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !lang.HasExtension(path) || lang.IsTestFile(path) {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		if len(changedFiles) > 0 && !changedFiles[rel] {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		candidates = append(candidates, auditFile{relPath: rel, size: info.Size(), modTime: info.ModTime()})
		return nil
	})
	if err != nil {
		return "", err
	}

	// Sort most-recently-modified first so the freshest code is embedded
	// when the budget forces omissions.
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})

	// Embed files until the context budget is exhausted.
	used, omitted := 0, 0
	for _, f := range candidates {
		if used+int(f.size) > auditBudgetChars {
			omitted++
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(repoRoot, f.relPath))
		if readErr != nil {
			continue
		}
		tag := lang.FenceTag
		if tag == "" {
			tag = strings.TrimPrefix(filepath.Ext(f.relPath), ".")
		}
		fmt.Fprintf(&sb, "\n### File: %s\n```%s\n%s\n```\n", f.relPath, tag, string(content))
		used += len(content)
	}
	if omitted > 0 {
		fmt.Fprintf(&sb, "\n_Note: %d file(s) omitted — context budget reached (%d KB). Re-run with focused --commit mode to review those files._\n",
			omitted, auditBudgetChars/1024)
	}

	return sb.String(), nil
}
