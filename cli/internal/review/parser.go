package review

import (
	"crypto/sha256"
	"encoding/hex"
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
