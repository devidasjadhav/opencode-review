package review

import (
	"regexp"
	"strings"

	"github.com/talk/opencode-client/internal/types"
)

var findingHeader = regexp.MustCompile(
	`(?i)###\s+` + "`" + `\[([^\]]+)\]` + "`" + `\s+(\S+):(\S*)\s+—\s+(.+)`,
)

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
			sev := strings.TrimSpace(m[1])
			switch {
			case strings.Contains(sev, "Critical"):
				sev = "Critical"
			case strings.Contains(sev, "High"):
				sev = "High"
			case strings.Contains(sev, "Medium"):
				sev = "Medium"
			default:
				sev = "Low"
			}
			current = &types.Finding{
				Severity:  sev,
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
