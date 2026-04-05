package review

import "testing"

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
