package types

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
	IssueNumber int // set after GitHub issue is created
}
