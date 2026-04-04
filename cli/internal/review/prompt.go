package review

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func findingsFormat(sb *strings.Builder) {
	sb.WriteString("For each finding use EXACTLY this format:\n\n")
	sb.WriteString("### `[🔴 Critical|🟠 High|🟡 Medium|🔵 Low]` file:line-range — Short title\n")
	sb.WriteString("Clear description referencing the exact code and which principle it violates.\n\n")
	sb.WriteString("```diff\n- old code\n+ fixed code\n```\n\n")
	sb.WriteString("**AI agent fix prompt:** One-paragraph instruction a coding agent can execute directly to fix this issue.\n\n")
	sb.WriteString("If there are no issues: write `_No issues found._` and skip subsections.\n\n")
}

// BuildReviewPrompt builds a commit review prompt with SOLID/DRY checks.
func BuildReviewPrompt(hash, subject, body, diff string) string {
	var sb strings.Builder
	sb.WriteString("Review the following git commit using the FULL project context and LSP info.\n")
	sb.WriteString("Read all referenced files, trace types, follow imports — do not limit analysis to the diff alone.\n\n")
	sb.WriteString("Respond ONLY with this exact structure (no prose outside it):\n\n")

	sb.WriteString("## Summary\n")
	sb.WriteString("One sentence: what this commit does and why.\n\n")

	sb.WriteString("## Walkthrough\n")
	sb.WriteString("Per-file bullet list: `file` — what changed and the intent.\n\n")

	sb.WriteString("## Findings\n")
	sb.WriteString("Report real, verifiable issues including SOLID and DRY violations. Check for:\n")
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

	sb.WriteString("---\n")
	fmt.Fprintf(&sb, "Commit: %s\n", hash)
	fmt.Fprintf(&sb, "Subject: %s\n", subject)
	if body != "" {
		fmt.Fprintf(&sb, "Body:\n%s\n", body)
	}
	sb.WriteString("\n---\n")
	sb.WriteString(diff)
	return sb.String()
}

// BuildAuditPrompt builds a full SOLID/DRY audit prompt embedding all Go source files.
func BuildAuditPrompt(repoRoot string) (string, error) {
	var sb strings.Builder
	sb.WriteString("Perform a full SOLID and DRY audit of this codebase.\n")
	sb.WriteString("Read ALL files below in full. For every violation report EXACTLY:\n\n")
	sb.WriteString("## Findings\n")
	sb.WriteString("For EVERY violation use EXACTLY this format (no list markers, use ### headers):\n\n")
	findingsFormat(&sb)

	sb.WriteString("## Verdict\n")
	sb.WriteString("Exactly one token on its own line:\n")
	sb.WriteString("`APPROVE` — no violations found.\n")
	sb.WriteString("`REQUEST CHANGES` — violations must be fixed.\n\n")
	sb.WriteString("---\n")

	err := filepath.WalkDir(filepath.Join(repoRoot, "cli"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot, path)
		fmt.Fprintf(&sb, "\n### File: %s\n```go\n%s\n```\n", rel, string(content))
		return nil
	})
	if err != nil {
		return "", err
	}
	return sb.String(), nil
}
