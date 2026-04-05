package types

import "strings"

// DefaultMergeStrategy is used when no --merge-strategy flag is provided.
const DefaultMergeStrategy = "squash"

var allowedMergeStrategies = []string{"merge", "squash", "rebase"}

// MergeMethod validates and returns the GitHub merge method string.
func MergeMethod(strategy string) (string, bool) {
	for _, s := range allowedMergeStrategies {
		if strings.EqualFold(strategy, s) {
			return s, true
		}
	}
	return "", false
}

// AllowedMergeStrategies returns a human-readable list of valid strategies.
func AllowedMergeStrategies() string {
	return strings.Join(allowedMergeStrategies, ", ")
}

// IssueSummary is a lightweight view of an open GitHub issue used to inform
// the review model of already-tracked findings so it does not re-report them.
type IssueSummary struct {
	Number int
	Title  string
}

// ModelInfo holds provider and model identity returned by the opencode API.
type ModelInfo struct {
	ProviderID   string
	ProviderName string
	ModelID      string
	ModelName    string
}

// Finding represents a single review finding parsed from a review response.
type Finding struct {
	Severity    string // Critical / High / Medium / Low
	File        string
	LineRange   string
	Title       string
	Description string
	Diff        string
	AgentPrompt string
	IssueNumber int    // set after GitHub issue is created
	Fingerprint string // sha256(lower(file)|lower(title)|lower(desc[:50])); set by ParseFindings
	Confidence  string // HIGH | MEDIUM | LOW; set by ParseFindings; empty = unrated
}
