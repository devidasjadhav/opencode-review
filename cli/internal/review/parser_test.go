package review

import (
	"strings"
	"testing"
)

func TestNormalizeSeverityAliases(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: "🔥 Critical", want: "Critical"},
		{raw: "hIgH", want: "High"},
		{raw: "MeD", want: "Medium"},
		{raw: "unknown", want: "Low"},
	}

	for _, tc := range tests {
		if got := normalizeSeverity(tc.raw); got != tc.want {
			t.Fatalf("normalizeSeverity(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestParseFindingsLenientFallback verifies that the lenient parser recovers
// findings when the model uses ## instead of ### or omits backticks.
func TestParseFindingsLenientFallback(t *testing.T) {
	// Format deviations: ## header, no backticks, en-dash separator
	reviewText := "## Findings\n" +
		"## [High] cmd/run.go:10 — Missing error check\n" +
		"The function ignores the returned error.\n" +
		"## [Medium] internal/git/git.go:50 — No timeout set\n" +
		"Context is never cancelled.\n"

	findings := ParseFindings(reviewText)
	if len(findings) != 2 {
		t.Fatalf("lenient fallback: got %d findings, want 2", len(findings))
	}
	if findings[0].Severity != "High" {
		t.Errorf("findings[0].Severity = %q, want High", findings[0].Severity)
	}
	if findings[0].File != "cmd/run.go" {
		t.Errorf("findings[0].File = %q, want cmd/run.go", findings[0].File)
	}
	if !strings.Contains(findings[0].Title, "Missing error") {
		t.Errorf("findings[0].Title = %q, want to contain 'Missing error'", findings[0].Title)
	}
}

// TestParseFindingsStrictTakesPrecedence ensures the strict parser runs first
// and the lenient fallback is not triggered when strict parsing succeeds.
func TestParseFindingsStrictTakesPrecedence(t *testing.T) {
	reviewText := "## Findings\n" +
		"### `[🟠 High]` cmd/run.go:10 — Missing error check\n" +
		"The function ignores the returned error.\n"

	findings := ParseFindings(reviewText)
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(findings))
	}
	// Strict parser sets lineRange; lenient does not always
	if findings[0].LineRange != "10" {
		t.Errorf("findings[0].LineRange = %q, want '10'", findings[0].LineRange)
	}
}

// TestParseFindingsNoFalsePositive ensures text with no findings does not
// trigger the lenient fallback and produce phantom findings.
func TestParseFindingsNoFalsePositive(t *testing.T) {
	reviewText := "## Summary\nThis commit looks fine.\n\n## Verdict\nAPPROVE — no issues found.\n"
	findings := ParseFindings(reviewText)
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for clean review, got %d", len(findings))
	}
}

func TestParseFindingsFingerprintNonEmpty(t *testing.T) {
	reviewText := "## Findings\n" +
		"### `[🟠 High]` cmd/run.go:10 — Missing error check\n" +
		"The function ignores the returned error.\n"
	findings := ParseFindings(reviewText)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Fingerprint == "" {
		t.Error("Fingerprint should be non-empty")
	}
	if len(findings[0].Fingerprint) != 64 {
		t.Errorf("Fingerprint length = %d, want 64 (sha256 hex)", len(findings[0].Fingerprint))
	}
}

func TestParseFindingsFingerprintStableAcrossDifferentLineNumbers(t *testing.T) {
	// Same file+title+desc but different line numbers → same fingerprint.
	make := func(lineRange string) string {
		return "## Findings\n" +
			"### `[🟠 High]` cmd/run.go:" + lineRange + " — Missing error check\n" +
			"The function ignores the returned error.\n"
	}
	f1 := ParseFindings(make("10"))
	f2 := ParseFindings(make("99"))
	if len(f1) != 1 || len(f2) != 1 {
		t.Fatal("expected 1 finding each")
	}
	if f1[0].Fingerprint != f2[0].Fingerprint {
		t.Errorf("fingerprint changed with line number: %q vs %q", f1[0].Fingerprint, f2[0].Fingerprint)
	}
}

func TestParseFindingsConfidenceParsed(t *testing.T) {
	reviewText := "## Findings\n" +
		"### `[🟠 High]` cmd/run.go:10 — Missing error check\n" +
		"The function ignores the returned error.\n" +
		"**Confidence:** HIGH (certain defect)\n"
	findings := ParseFindings(reviewText)
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Confidence != "HIGH" {
		t.Errorf("Confidence = %q, want HIGH", findings[0].Confidence)
	}
}

func TestMeetsConfidenceTable(t *testing.T) {
	tests := []struct {
		findingConf string
		minConf     string
		want        bool
	}{
		{"HIGH", "HIGH", true},
		{"HIGH", "MEDIUM", true},
		{"HIGH", "LOW", true},
		{"MEDIUM", "HIGH", false},
		{"MEDIUM", "MEDIUM", true},
		{"MEDIUM", "LOW", true},
		{"LOW", "HIGH", false},
		{"LOW", "MEDIUM", false},
		{"LOW", "LOW", true},
		// Empty confidence treated as MEDIUM.
		{"", "MEDIUM", true},
		{"", "HIGH", false},
		{"", "LOW", true},
	}
	for _, tc := range tests {
		got := MeetsConfidence(tc.findingConf, tc.minConf)
		if got != tc.want {
			t.Errorf("MeetsConfidence(%q, %q) = %v, want %v", tc.findingConf, tc.minConf, got, tc.want)
		}
	}
}

func TestParseFindingsNormalizesSeverity(t *testing.T) {
	reviewText := "## Findings\n" +
		"### `[🚨 CRITICAL]` file1.go:10 — broken thing\n" +
		"details\n" +
		"### `[hIgH]` file2.go:20 — risky thing\n" +
		"details\n" +
		"### `[mEd]` file3.go:30 — minor thing\n" +
		"details\n"

	findings := ParseFindings(reviewText)
	if len(findings) != 3 {
		t.Fatalf("len(findings) = %d, want 3", len(findings))
	}

	if findings[0].Severity != "Critical" {
		t.Fatalf("findings[0].Severity = %q, want %q", findings[0].Severity, "Critical")
	}
	if findings[1].Severity != "High" {
		t.Fatalf("findings[1].Severity = %q, want %q", findings[1].Severity, "High")
	}
	if findings[2].Severity != "Medium" {
		t.Fatalf("findings[2].Severity = %q, want %q", findings[2].Severity, "Medium")
	}
}
