package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/talk/opencode-client/internal/types"
)

var findingHeader = regexp.MustCompile(
	`(?i)###\s+` + "`" + `\[([^\]]+)\]` + "`" + `\s+(\S+):(\S*)\s+—\s+(.+)`,
)

var severityAliases = map[string]string{
	"critical": "Critical",
	"crit":     "Critical",
	"high":     "High",
	"medium":   "Medium",
	"med":      "Medium",
	"low":      "Low",
}

var severityAliasOrder = []string{"critical", "crit", "high", "medium", "med", "low"}

func normalizeSeverity(raw string) string {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	for _, alias := range severityAliasOrder {
		if strings.Contains(normalized, alias) {
			return severityAliases[alias]
		}
	}
	return "Low"
}

// computeFingerprint returns a stable sha256 of a finding's identity.
// Line numbers excluded so reformatted-but-same issues still deduplicate.
func computeFingerprint(file, title, desc string) string {
	norm := strings.ToLower(strings.TrimSpace(file)) +
		"|" + strings.ToLower(strings.TrimSpace(title)) +
		"|" + strings.ToLower(strings.TrimSpace(truncate(desc, 50)))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func parseConfidence(val string) string {
	val = strings.ToUpper(strings.TrimSpace(val))
	switch {
	case strings.HasPrefix(val, "HIGH"):
		return "HIGH"
	case strings.HasPrefix(val, "MEDIUM"):
		return "MEDIUM"
	case strings.HasPrefix(val, "LOW"):
		return "LOW"
	default:
		// Unrecognised or empty — treat as MEDIUM (same as meetsConfidence backward-compat rule).
		return "MEDIUM"
	}
}

// MeetsConfidence reports whether findingConf satisfies the minimum required level.
// Empty confidence is treated as MEDIUM for backward compatibility with old issues.
func MeetsConfidence(findingConf, minConf string) bool {
	rank := map[string]int{"LOW": 1, "MEDIUM": 2, "HIGH": 3}
	fc := findingConf
	if fc == "" {
		fc = "MEDIUM"
	}
	return rank[fc] >= rank[strings.ToUpper(minConf)]
}

// looksLikeItHasFindings returns true when the text appears to contain findings
// but the strict parser produced none — signals a format deviation worth retrying.
func looksLikeItHasFindings(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "high") ||
		strings.Contains(lower, "medium") ||
		strings.Contains(lower, "critical") ||
		strings.Contains(lower, "finding")
}

// lenientFindingHeader accepts looser formatting variations:
//   - ## or ### headers
//   - severity with or without backticks/emoji
//   - en-dash or em-dash separators
var lenientFindingHeader = regexp.MustCompile(
	`(?i)#{2,3}\s+` + "`?" + `\[?([^\]` + "`" + `]+(?:critical|high|medium|low)[^\]` + "`" + `]*)\]?` + "`?" +
		`\s+(\S+?)(?::(\S*))?(?:\s+[—–-]+\s+(.+))?`,
)

// parseLenient is a best-effort fallback parser for when the strict parser
// produces no findings but the text appears to contain some.
func parseLenient(reviewText string) []types.Finding {
	var findings []types.Finding
	var current *types.Finding
	var descLines []string

	flush := func() {
		if current == nil {
			return
		}
		current.Description = strings.TrimSpace(strings.Join(descLines, "\n"))
		current.Fingerprint = computeFingerprint(current.File, current.Title, current.Description)
		findings = append(findings, *current)
		current = nil
		descLines = nil
	}

	inFindings := false
	for _, line := range strings.Split(reviewText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## Findings") {
			inFindings = true
			continue
		}
		if strings.HasPrefix(trimmed, "## ") && inFindings {
			break
		}
		if !inFindings {
			continue
		}
		if m := lenientFindingHeader.FindStringSubmatch(trimmed); m != nil {
			flush()
			title := strings.TrimSpace(m[4])
			if title == "" {
				title = "Untitled finding"
			}
			current = &types.Finding{
				Severity:  normalizeSeverity(m[1]),
				File:      m[2],
				LineRange: m[3],
				Title:     title,
			}
			continue
		}
		if current != nil && trimmed != "" {
			descLines = append(descLines, line)
		}
	}
	flush()
	return findings
}

// ParseFindings extracts structured findings from a review response.
func ParseFindings(reviewText string) []types.Finding {
	var findings []types.Finding
	var current *types.Finding
	var section string
	var diffLines, agentLines []string
	inDiff := false

	flush := func() {
		if current == nil {
			return
		}
		current.Diff = strings.TrimSpace(strings.Join(diffLines, "\n"))
		current.AgentPrompt = strings.TrimSpace(strings.Join(agentLines, "\n"))
		current.AgentPrompt = strings.TrimPrefix(current.AgentPrompt, "**AI agent fix prompt:**")
		current.AgentPrompt = strings.TrimSpace(current.AgentPrompt)
		current.Fingerprint = computeFingerprint(current.File, current.Title, current.Description)
		findings = append(findings, *current)
		current = nil
		diffLines = nil
		agentLines = nil
		inDiff = false
	}

	for _, line := range strings.Split(reviewText, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## ") {
			flush()
			section = strings.TrimPrefix(trimmed, "## ")
			continue
		}
		if section != "Findings" {
			continue
		}
		if m := findingHeader.FindStringSubmatch(trimmed); m != nil {
			flush()
			current = &types.Finding{
				Severity:  normalizeSeverity(strings.TrimSpace(m[1])),
				File:      m[2],
				LineRange: m[3],
				Title:     strings.TrimSpace(m[4]),
			}
			continue
		}
		if current == nil {
			continue
		}
		if trimmed == "```diff" {
			inDiff = true
			continue
		}
		if inDiff {
			if trimmed == "```" {
				inDiff = false
			} else {
				diffLines = append(diffLines, line)
			}
			continue
		}
		if strings.HasPrefix(trimmed, "**Confidence:**") {
			val := strings.TrimPrefix(trimmed, "**Confidence:**")
			current.Confidence = parseConfidence(val)
			continue
		}
		if strings.HasPrefix(trimmed, "**AI agent fix prompt:**") {
			rest := strings.TrimPrefix(trimmed, "**AI agent fix prompt:**")
			agentLines = append(agentLines, strings.TrimSpace(rest))
			continue
		}
		if len(agentLines) > 0 && trimmed != "" {
			agentLines = append(agentLines, line)
			continue
		}
		if trimmed != "" {
			current.Description += line + "\n"
		}
	}
	flush()

	// Fallback: if strict parsing yielded nothing but the text looks like it has
	// findings, try the lenient parser and warn so the format deviation is visible.
	if len(findings) == 0 && looksLikeItHasFindings(reviewText) {
		fmt.Fprintf(os.Stderr, "Warning: strict finding parser found 0 results — trying lenient parser\n")
		findings = parseLenient(reviewText)
		if len(findings) > 0 {
			fmt.Fprintf(os.Stderr, "  Lenient parser recovered %d finding(s). Consider fixing the model output format.\n", len(findings))
		}
	}

	return findings
}

// ExtractVerdict parses the ## Verdict section and returns APPROVE, REQUEST CHANGES, or COMMENT.
func ExtractVerdict(review string) string {
	inVerdict := false
	for _, line := range strings.Split(review, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## Verdict") {
			inVerdict = true
			continue
		}
		if inVerdict {
			if strings.HasPrefix(trimmed, "##") {
				break
			}
			if strings.Contains(trimmed, "REQUEST CHANGES") {
				return "REQUEST CHANGES"
			}
			if strings.Contains(trimmed, "APPROVE") {
				return "APPROVE"
			}
			if strings.Contains(trimmed, "COMMENT") {
				return "COMMENT"
			}
		}
	}
	return "COMMENT"
}
