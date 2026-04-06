package review

import (
	"strings"
	"testing"

	"github.com/talk/opencode-client/internal/types"
)

func TestBuildReviewPromptEmbeddsFileContents(t *testing.T) {
	rctx := ReviewContext{
		ChangedFiles: map[string]string{
			"cmd/run.go": "package cmd\n\nfunc Run() {}",
		},
	}
	prompt := BuildReviewPrompt("abc123", "fix: something", "", "diff --git a/cmd/run.go", rctx)

	if !strings.Contains(prompt, "## Current File Contents") {
		t.Error("prompt missing 'Current File Contents' section")
	}
	if !strings.Contains(prompt, "### cmd/run.go") {
		t.Error("prompt missing file header for cmd/run.go")
	}
	if !strings.Contains(prompt, "func Run() {}") {
		t.Error("prompt missing actual file content")
	}
}

func TestBuildReviewPromptExistingIssues(t *testing.T) {
	rctx := ReviewContext{
		ExistingIssues: []types.IssueSummary{
			{Number: 42, Title: "[High] cmd/run.go:10 — missing error check"},
			{Number: 43, Title: "[Medium] internal/git/git.go:50 — no timeout"},
		},
	}
	prompt := BuildReviewPrompt("abc123", "fix: something", "", "diff", rctx)

	if !strings.Contains(prompt, "Already Tracked Issues") {
		t.Error("prompt missing 'Already Tracked Issues' section")
	}
	if !strings.Contains(prompt, "#42") {
		t.Error("prompt missing issue #42")
	}
	if !strings.Contains(prompt, "#43") {
		t.Error("prompt missing issue #43")
	}
	if !strings.Contains(prompt, "Do NOT Re-Report") {
		t.Error("prompt missing do-not-re-report instruction")
	}
}

func TestBuildReviewPromptEmptyContext(t *testing.T) {
	// Empty ReviewContext must not crash and must not emit empty sections.
	prompt := BuildReviewPrompt("abc123", "fix: something", "", "diff", ReviewContext{})

	if strings.Contains(prompt, "Current File Contents") {
		t.Error("prompt should not emit file section when ChangedFiles is empty")
	}
	if strings.Contains(prompt, "Already Tracked Issues") {
		t.Error("prompt should not emit issues section when ExistingIssues is empty")
	}
}

func TestBuildReviewPromptLSPPreambleIncluded(t *testing.T) {
	rctx := ReviewContext{LSPEnabled: true}
	prompt := BuildReviewPrompt("abc123", "feat: x", "", "diff", rctx)
	if !strings.Contains(prompt, "LSP Navigation") {
		t.Error("LSP preamble missing when LSPEnabled=true")
	}
	if !strings.Contains(prompt, "goToDefinition") {
		t.Error("prompt should mention goToDefinition when LSPEnabled=true")
	}
	if !strings.Contains(prompt, "findReferences") {
		t.Error("prompt should mention findReferences when LSPEnabled=true")
	}
}

func TestBuildReviewPromptLSPPreambleOmitted(t *testing.T) {
	rctx := ReviewContext{LSPEnabled: false}
	prompt := BuildReviewPrompt("abc123", "feat: x", "", "diff", rctx)
	if strings.Contains(prompt, "LSP Navigation") {
		t.Error("LSP preamble should be absent when LSPEnabled=false")
	}
}

func TestBuildAuditPromptLSPEnabled(t *testing.T) {
	dir := t.TempDir()
	prompt, err := BuildAuditPrompt(dir, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "LSP Navigation") {
		t.Error("LSP preamble missing when lspEnabled=true in audit prompt")
	}
}

func TestBuildAuditPromptLSPDisabled(t *testing.T) {
	dir := t.TempDir()
	prompt, err := BuildAuditPrompt(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "LSP Navigation") {
		t.Error("LSP preamble should be absent when lspEnabled=false in audit prompt")
	}
}

func TestBuildReviewPromptFilesSortedDeterministically(t *testing.T) {
	rctx := ReviewContext{
		ChangedFiles: map[string]string{
			"z_file.go": "z content",
			"a_file.go": "a content",
			"m_file.go": "m content",
		},
	}
	prompt := BuildReviewPrompt("abc123", "subj", "", "diff", rctx)

	ia := strings.Index(prompt, "a_file.go")
	im := strings.Index(prompt, "m_file.go")
	iz := strings.Index(prompt, "z_file.go")
	if !(ia < im && im < iz) {
		t.Error("files not sorted alphabetically in prompt")
	}
}
