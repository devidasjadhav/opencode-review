package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/google/go-github/v67/github"
	opencode "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"
)

// ── Types ─────────────────────────────────────────────────────────────────────

type ModelInfo struct {
	ProviderID   string
	ProviderName string
	ModelID      string
	ModelName    string
}

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

// ── Git helpers ───────────────────────────────────────────────────────────────

func gitRun(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func resolveRepoRoot(dir string) (string, error) {
	root, err := gitRun(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	return root, nil
}

func getCommitInfo(root, ref string) (hash, subject, body, diff string, err error) {
	hash, err = gitRun(root, "rev-parse", ref)
	if err != nil {
		err = fmt.Errorf("unknown ref %q: %w", ref, err)
		return
	}
	subject, err = gitRun(root, "log", "-1", "--format=%s", hash)
	if err != nil {
		return
	}
	body, err = gitRun(root, "log", "-1", "--format=%b", hash)
	if err != nil {
		return
	}
	diff, err = gitRun(root, "show", "--stat", "--patch", hash)
	return
}

type statusSnapshotEntry struct {
	Status       string
	WorktreeHash string
}

func worktreeContentFingerprint(root, path, status string) (string, error) {
	if strings.HasPrefix(status, "??") || strings.HasPrefix(status, "!!") {
		return "", nil
	}
	if strings.Contains(status, "D") {
		return "<deleted>", nil
	}
	abs := filepath.Join(root, path)
	if fi, statErr := os.Lstat(abs); statErr == nil && fi.IsDir() {
		return "<directory>", nil
	}
	hash, err := gitRun(root, "hash-object", "--", path)
	if err != nil {
		// Non-blob entries (for example submodules) cannot be hashed with hash-object.
		return "<non-blob>", nil
	}
	return hash, nil
}

func gitStatusSnapshot(root string) (map[string]statusSnapshotEntry, error) {
	out, err := gitRun(root, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	snapshot := map[string]statusSnapshotEntry{}
	if out == "" {
		return snapshot, nil
	}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		status := line[:2]
		path := strings.TrimSpace(line[3:])
		if i := strings.Index(path, " -> "); i >= 0 {
			path = path[i+4:]
		}
		worktreeHash, err := worktreeContentFingerprint(root, path, status)
		if err != nil {
			return nil, err
		}
		snapshot[path] = statusSnapshotEntry{Status: status, WorktreeHash: worktreeHash}
	}
	return snapshot, nil
}

func isOperationalArtifact(path string) bool {
	p := filepath.ToSlash(path)
	return strings.HasPrefix(p, "logs/")
}

func fixerStagePaths(before, after map[string]statusSnapshotEntry) []string {
	var paths []string
	for path, afterStatus := range after {
		if isOperationalArtifact(path) {
			continue
		}
		if strings.HasPrefix(afterStatus.Status, "??") {
			continue
		}
		beforeEntry, existed := before[path]
		if !existed || beforeEntry.Status != afterStatus.Status || beforeEntry.WorktreeHash != afterStatus.WorktreeHash {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

// ── Model helpers ─────────────────────────────────────────────────────────────

func listModels(client *opencode.Client, ctx context.Context, dir string) ([]ModelInfo, error) {
	providers, err := client.App.Providers(ctx, opencode.AppProvidersParams{
		Directory: opencode.F(dir),
	})
	if err != nil {
		return nil, err
	}
	var models []ModelInfo
	for _, provider := range providers.Providers {
		ids := make([]string, 0, len(provider.Models))
		for id := range provider.Models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			m := provider.Models[id]
			models = append(models, ModelInfo{
				ProviderID:   provider.ID,
				ProviderName: provider.Name,
				ModelID:      m.ID,
				ModelName:    m.Name,
			})
		}
	}
	return models, nil
}

func selectModel(models []ModelInfo) ModelInfo {
	fmt.Println("Available models:")
	for i, m := range models {
		fmt.Printf("  [%2d] %-20s %s\n", i+1, m.ProviderName, m.ModelName)
	}
	scanner := bufio.NewScanner(os.Stdin)
	for {
		fmt.Print("\nSelect model number: ")
		if !scanner.Scan() {
			os.Exit(0)
		}
		n, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if err != nil || n < 1 || n > len(models) {
			fmt.Printf("  Enter a number between 1 and %d\n", len(models))
			continue
		}
		return models[n-1]
	}
}

// ── Prompt builder ────────────────────────────────────────────────────────────

func buildAuditPrompt(repoRoot string) (string, error) {
	var sb strings.Builder
	sb.WriteString("Perform a full SOLID and DRY audit of this codebase.\n")
	sb.WriteString("Read ALL files below in full. For every violation report EXACTLY:\n\n")
	sb.WriteString("### `[🔴 Critical|🟠 High|🟡 Medium|🔵 Low]` file:line-range — Short title\n")
	sb.WriteString("Which principle is violated and why, referencing exact code.\n\n")
	sb.WriteString("```diff\n- old code\n+ fixed code\n```\n\n")
	sb.WriteString("**AI agent fix prompt:** One-paragraph instruction to fix this.\n\n")
	sb.WriteString("## Findings\n")
	sb.WriteString("(list all findings above — if none write `_No issues found._`)\n\n")
	sb.WriteString("## Verdict\n")
	sb.WriteString("`APPROVE` — no violations found.\n")
	sb.WriteString("`REQUEST CHANGES` — violations must be fixed.\n\n")
	sb.WriteString("---\n")

	// Append all .go source files (excluding test files)
	matches, err := filepath.Glob(filepath.Join(repoRoot, "cli", "*.go"))
	if err != nil {
		return "", err
	}
	for _, f := range matches {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		content, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(repoRoot, f)
		fmt.Fprintf(&sb, "\n### File: %s\n```go\n%s\n```\n", rel, string(content))
	}
	return sb.String(), nil
}

func buildReviewPrompt(hash, subject, body, diff string) string {
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
	sb.WriteString("For each finding use EXACTLY this format:\n\n")
	sb.WriteString("### `[🔴 Critical|🟠 High|🟡 Medium|🔵 Low]` file:line-range — Short title\n")
	sb.WriteString("Clear description referencing the exact code and which principle it violates.\n\n")
	sb.WriteString("```diff\n- old code\n+ fixed code\n```\n\n")
	sb.WriteString("**AI agent fix prompt:** One-paragraph instruction a coding agent can execute directly to fix this issue.\n\n")
	sb.WriteString("If there are no issues: write `_No issues found._` and skip subsections.\n\n")

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

// ── Review parser ─────────────────────────────────────────────────────────────

var findingHeader = regexp.MustCompile(
	`(?i)###\s+` + "`" + `\[([^\]]+)\]` + "`" + `\s+(\S+):(\S*)\s+—\s+(.+)`,
)

func parseFindings(reviewText string) []Finding {
	var findings []Finding
	var current *Finding
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

		// Track top-level sections
		if strings.HasPrefix(trimmed, "## ") {
			flush()
			section = strings.TrimPrefix(trimmed, "## ")
			continue
		}

		if section != "Findings" {
			continue
		}

		// New finding
		if m := findingHeader.FindStringSubmatch(trimmed); m != nil {
			flush()
			sev := strings.TrimSpace(m[1])
			// Normalise severity emoji → word
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
			current = &Finding{
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

		// Diff block
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

		// AI agent fix prompt
		if strings.HasPrefix(trimmed, "**AI agent fix prompt:**") {
			rest := strings.TrimPrefix(trimmed, "**AI agent fix prompt:**")
			agentLines = append(agentLines, strings.TrimSpace(rest))
			continue
		}
		if len(agentLines) > 0 && trimmed != "" {
			agentLines = append(agentLines, line)
			continue
		}

		// Description
		if trimmed != "" {
			current.Description += line + "\n"
		}
	}
	flush()
	return findings
}

func extractVerdict(review string) string {
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

// ── GitHub client ─────────────────────────────────────────────────────────────

// ghClient builds a GitHub API client from GITHUB_TOKEN env var.
func ghClient(ctx context.Context) (*github.Client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN environment variable not set")
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	return github.NewClient(oauth2.NewClient(ctx, ts)), nil
}

// repoOwnerName parses "owner" and "repo" from `git remote get-url origin`.
func repoOwnerName(repoRoot string) (owner, repo string, err error) {
	out, err := gitRun(repoRoot, "remote", "get-url", "origin")
	if err != nil {
		return "", "", fmt.Errorf("cannot read git remote: %w", err)
	}
	// Handles https://github.com/owner/repo.git and git@github.com:owner/repo.git
	out = strings.TrimSuffix(out, ".git")
	if idx := strings.Index(out, "github.com/"); idx >= 0 {
		parts := strings.SplitN(out[idx+len("github.com/"):], "/", 2)
		if len(parts) == 2 {
			return parts[0], parts[1], nil
		}
	}
	if idx := strings.Index(out, "github.com:"); idx >= 0 {
		parts := strings.SplitN(out[idx+len("github.com:"):], "/", 2)
		if len(parts) == 2 {
			return parts[0], parts[1], nil
		}
	}
	return "", "", fmt.Errorf("cannot parse GitHub owner/repo from remote %q", out)
}

// ── GitHub helpers ────────────────────────────────────────────────────────────

func createIssue(ctx context.Context, gh *github.Client, owner, repo string, f Finding) (int, error) {
	title := fmt.Sprintf("[%s] %s:%s — %s", f.Severity, f.File, f.LineRange, f.Title)
	var body strings.Builder
	fmt.Fprintf(&body, "## %s\n\n", f.Title)
	fmt.Fprintf(&body, "**Severity:** %s  \n", f.Severity)
	fmt.Fprintf(&body, "**Location:** `%s:%s`\n\n", f.File, f.LineRange)
	body.WriteString("### Description\n")
	body.WriteString(strings.TrimSpace(f.Description))
	body.WriteString("\n\n")
	if f.Diff != "" {
		body.WriteString("### Suggested fix\n```diff\n")
		body.WriteString(f.Diff)
		body.WriteString("\n```\n\n")
	}
	if f.AgentPrompt != "" {
		body.WriteString("### AI agent fix prompt\n")
		body.WriteString(f.AgentPrompt)
		body.WriteString("\n")
	}
	body.WriteString("\n---\n_Reported by opencode-review_\n")

	bodyStr := body.String()
	issue, _, err := gh.Issues.Create(ctx, owner, repo, &github.IssueRequest{
		Title: &title,
		Body:  &bodyStr,
	})
	if err != nil {
		return 0, err
	}
	return issue.GetNumber(), nil
}

func closeIssue(ctx context.Context, gh *github.Client, owner, repo string, num int) error {
	comment := "Fixed and merged. Closing."
	if _, _, err := gh.Issues.CreateComment(ctx, owner, repo, num, &github.IssueComment{Body: &comment}); err != nil {
		return err
	}
	state := "closed"
	_, _, err := gh.Issues.Edit(ctx, owner, repo, num, &github.IssueRequest{State: &state})
	return err
}

// existingIssues fetches open issue titles from GitHub for deduplication.
func existingIssues(ctx context.Context, gh *github.Client, owner, repo string) map[string]bool {
	seen := map[string]bool{}
	opts := &github.IssueListByRepoOptions{State: "open", ListOptions: github.ListOptions{PerPage: 100}}
	for {
		issues, resp, err := gh.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return seen
		}
		for _, issue := range issues {
			if issue.PullRequestLinks == nil { // skip PRs
				seen[issue.GetTitle()] = true
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return seen
}

func linkIssuesToPR(ctx context.Context, gh *github.Client, owner, repo string, prNum int, issueNums []int) error {
	if len(issueNums) == 0 {
		return nil
	}
	pr, _, err := gh.PullRequests.Get(ctx, owner, repo, prNum)
	if err != nil {
		return err
	}
	var closes []string
	for _, n := range issueNums {
		closes = append(closes, fmt.Sprintf("Closes #%d", n))
	}
	newBody := pr.GetBody() + "\n\n" + strings.Join(closes, "\n")
	_, _, err = gh.PullRequests.Edit(ctx, owner, repo, prNum, &github.PullRequest{Body: &newBody})
	return err
}

func postGitHubPRReview(ctx context.Context, gh *github.Client, owner, repo string, prNum int, body, verdict string) error {
	event := "COMMENT"
	switch {
	case strings.Contains(verdict, "REQUEST CHANGES"):
		event = "REQUEST_CHANGES"
	case strings.Contains(verdict, "APPROVE"):
		event = "APPROVE"
	}

	_, _, err := gh.PullRequests.CreateReview(ctx, owner, repo, prNum, &github.PullRequestReviewRequest{
		Body:  &body,
		Event: &event,
	})
	if err != nil && event != "COMMENT" {
		// Cannot approve/request-changes on own PR — fall back to comment
		if strings.Contains(err.Error(), "Can not approve your own") ||
			strings.Contains(err.Error(), "own pull request") {
			fmt.Fprintln(os.Stderr, "(falling back to COMMENT: cannot approve/request-changes own PR)")
			comment := "COMMENT"
			_, _, err = gh.PullRequests.CreateReview(ctx, owner, repo, prNum, &github.PullRequestReviewRequest{
				Body:  &body,
				Event: &comment,
			})
		}
	}
	return err
}

func mergePR(ctx context.Context, gh *github.Client, owner, repo string, prNum int, strategy string, deleteBranch bool) error {
	method := map[string]string{"merge": "merge", "squash": "squash", "rebase": "rebase"}[strategy]
	if method == "" {
		return fmt.Errorf("invalid merge strategy %q: must be merge, squash, or rebase", strategy)
	}
	_, _, err := gh.PullRequests.Merge(ctx, owner, repo, prNum, "", &github.PullRequestOptions{
		MergeMethod: method,
	})
	if err != nil {
		return err
	}
	if deleteBranch {
		pr, _, err2 := gh.PullRequests.Get(ctx, owner, repo, prNum)
		if err2 == nil && pr.Head != nil && pr.Head.Ref != nil {
			gh.Git.DeleteRef(ctx, owner, repo, "heads/"+pr.Head.GetRef()) //nolint
		}
	}
	return nil
}

// openPRNumber returns the number of the open PR for the current branch.
func openPRNumber(ctx context.Context, gh *github.Client, owner, repo, repoRoot string) (int, error) {
	branch, err := gitRun(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return 0, err
	}
	// Resolve the authenticated user's login for the head filter (handles orgs and forks).
	headUser := owner
	if me, _, err2 := gh.Users.Get(ctx, ""); err2 == nil {
		headUser = me.GetLogin()
	}
	prs, _, err := gh.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State: "open", Head: headUser + ":" + branch,
	})
	if err != nil {
		return 0, err
	}
	if len(prs) == 0 {
		return 0, fmt.Errorf("no open PR found for branch %q", branch)
	}
	return prs[0].GetNumber(), nil
}

// ── Opencode stream ───────────────────────────────────────────────────────────

func runReview(client *opencode.Client, ctx context.Context, repoRoot string, selected ModelInfo, prompt string) (string, error) {

	session, err := client.Session.New(ctx, opencode.SessionNewParams{
		Directory: opencode.F(repoRoot),
	})
	if err != nil {
		return "", fmt.Errorf("error creating session: %w", err)
	}
	sessionID := session.ID
	// Clean up session when review is done to avoid orphan sessions in loop mode.
	defer func() {
		client.Session.Delete(ctx, sessionID, opencode.SessionDeleteParams{}) //nolint
	}()

	idleCh := make(chan string, 2)
	streamCtx, cancel := context.WithCancel(ctx)

	stream := client.Event.ListStreaming(streamCtx, opencode.EventListParams{
		Directory: opencode.F(repoRoot),
	})

	go func() {
		defer cancel()
		var buf strings.Builder
		for stream.Next() {
			event := stream.Current()
			switch event.Type {
			case "message.part.delta":
				var envelope struct {
					Properties struct {
						Field string `json:"field"`
						Delta string `json:"delta"`
					} `json:"properties"`
				}
				if err := json.Unmarshal([]byte(event.JSON.RawJSON()), &envelope); err == nil {
					switch envelope.Properties.Field {
					case "text":
						fmt.Print(envelope.Properties.Delta)
						buf.WriteString(envelope.Properties.Delta)
					case "reasoning":
						// Suppress reasoning/thinking tokens from output and parsing
					}
				}
			case opencode.EventListResponseTypeSessionIdle:
				idle, ok := event.AsUnion().(opencode.EventListResponseEventSessionIdle)
				if ok && idle.Properties.SessionID == sessionID {
					fmt.Println()
					fmt.Println()
					idleCh <- buf.String()
				}
			case opencode.EventListResponseTypeSessionError:
				fmt.Fprintf(os.Stderr, "\n[session error] %s\n", event.JSON.RawJSON())
				idleCh <- ""
			}
		}
		if err := stream.Err(); err != nil && streamCtx.Err() == nil {
			fmt.Fprintf(os.Stderr, "\nStream error: %v\n", err)
			idleCh <- "" // unblock caller so it doesn't hang
		}
	}()

	_, err = client.Session.Prompt(ctx, sessionID, opencode.SessionPromptParams{
		Directory: opencode.F(repoRoot),
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Text: opencode.F(prompt),
				Type: opencode.F(opencode.TextPartInputTypeText),
			},
		}),
		Model: opencode.F(opencode.SessionPromptParamsModel{
			ModelID:    opencode.F(selected.ModelID),
			ProviderID: opencode.F(selected.ProviderID),
		}),
	})
	if err != nil {
		return "", fmt.Errorf("error sending prompt: %w", err)
	}
	return <-idleCh, nil
}

// runFix sends each finding's agent fix prompt to opencode and waits for completion.
// Returns the number of findings that were sent for fixing.
func runFix(client *opencode.Client, ctx context.Context, repoRoot string, selected ModelInfo,
	findings []Finding, iteration int) int {

	if len(findings) == 0 {
		return 0
	}

	// Build a single combined fix prompt for all findings so opencode has full context.
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are an automated code fixer. Apply ALL of the following fixes to the codebase.\n")
	fmt.Fprintf(&sb, "Make minimal, targeted changes. Do not refactor unrelated code.\n")
	fmt.Fprintf(&sb, "After applying all fixes, stop — do not explain or summarise.\n\n")
	for i, f := range findings {
		if f.AgentPrompt == "" {
			continue
		}
		fmt.Fprintf(&sb, "--- Fix %d/%d [%s] %s:%s — %s ---\n", i+1, len(findings), f.Severity, f.File, f.LineRange, f.Title)
		sb.WriteString(f.AgentPrompt)
		sb.WriteString("\n\n")
		if f.Diff != "" {
			sb.WriteString("Suggested diff:\n```diff\n")
			sb.WriteString(f.Diff)
			sb.WriteString("\n```\n\n")
		}
	}
	prompt := sb.String()
	beforeStatus, err := gitStatusSnapshot(repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fix status snapshot error: %v\n", err)
		return 0
	}

	session, err := client.Session.New(ctx, opencode.SessionNewParams{
		Directory: opencode.F(repoRoot),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fix session error: %v\n", err)
		return 0
	}
	sessionID := session.ID
	defer func() {
		client.Session.Delete(ctx, sessionID, opencode.SessionDeleteParams{}) //nolint
	}()

	idleCh := make(chan struct{}, 2)
	streamCtx, cancel := context.WithCancel(ctx)
	stream := client.Event.ListStreaming(streamCtx, opencode.EventListParams{
		Directory: opencode.F(repoRoot),
	})

	go func() {
		defer cancel()
		for stream.Next() {
			event := stream.Current()
			switch event.Type {
			case "message.part.delta":
				var envelope struct {
					Properties struct {
						Field string `json:"field"`
						Delta string `json:"delta"`
					} `json:"properties"`
				}
				if err := json.Unmarshal([]byte(event.JSON.RawJSON()), &envelope); err == nil {
					if envelope.Properties.Field == "text" {
						fmt.Print(envelope.Properties.Delta)
					}
				}
			case opencode.EventListResponseTypeSessionIdle:
				idle, ok := event.AsUnion().(opencode.EventListResponseEventSessionIdle)
				if ok && idle.Properties.SessionID == sessionID {
					fmt.Println()
					idleCh <- struct{}{}
				}
			case opencode.EventListResponseTypeSessionError:
				fmt.Fprintf(os.Stderr, "\n[fix session error] %s\n", event.JSON.RawJSON())
				idleCh <- struct{}{}
			}
		}
		if err := stream.Err(); err != nil && streamCtx.Err() == nil {
			fmt.Fprintf(os.Stderr, "\nFix stream error: %v\n", err)
			idleCh <- struct{}{}
		}
	}()

	_, err = client.Session.Prompt(ctx, sessionID, opencode.SessionPromptParams{
		Directory: opencode.F(repoRoot),
		Parts: opencode.F([]opencode.SessionPromptParamsPartUnion{
			opencode.TextPartInputParam{
				Text: opencode.F(prompt),
				Type: opencode.F(opencode.TextPartInputTypeText),
			},
		}),
		Model: opencode.F(opencode.SessionPromptParamsModel{
			ModelID:    opencode.F(selected.ModelID),
			ProviderID: opencode.F(selected.ProviderID),
		}),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fix prompt error: %v\n", err)
		return 0
	}
	<-idleCh

	// Commit only tracked, fix-related changes from this fixer session.
	afterStatus, err := gitStatusSnapshot(repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fix status snapshot error: %v\n", err)
		return 0
	}
	stagePaths := fixerStagePaths(beforeStatus, afterStatus)
	if len(stagePaths) == 0 {
		fmt.Println("  (no file changes detected after fix)")
		return 0
	}
	addArgs := append([]string{"add", "-u", "--"}, stagePaths...)
	if _, err := gitRun(repoRoot, addArgs...); err != nil {
		fmt.Fprintf(os.Stderr, "  git add failed: %v\n", err)
		return 0
	}
	stagedArgs := append([]string{"diff", "--cached", "--name-only", "--"}, stagePaths...)
	staged, _ := gitRun(repoRoot, stagedArgs...)
	if staged == "" {
		fmt.Println("  (no file changes detected after fix)")
		return 0
	}
	msg := fmt.Sprintf("fix: auto-fix %d finding(s) from review iteration %d", len(findings), iteration)
	commitArgs := append([]string{"commit", "-m", msg, "--"}, stagePaths...)
	if _, err := gitRun(repoRoot, commitArgs...); err != nil {
		fmt.Fprintf(os.Stderr, "  git commit failed: %v\n", err)
		return 0
	}
	branch, _ := gitRun(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if _, err := gitRun(repoRoot, "push", "origin", branch); err != nil {
		fmt.Fprintf(os.Stderr, "  git push failed: %v\n", err)
		return 0
	}
	fmt.Printf("  Committed and pushed fixes: %q\n", msg)
	return len(findings)
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	serverURL := flag.String("url", "http://localhost:4096", "Opencode server URL")
	dirFlag := flag.String("dir", "", "Git repo directory (default: current directory)")
	modelNum := flag.Int("model", 0, "Model number from list (skips interactive selection)")
	commitRef := flag.String("commit", "HEAD", "Git ref to review (hash, branch, tag)")
	auditMode := flag.Bool("audit", false, "Full SOLID/DRY audit of entire codebase instead of single commit review")
	postPR := flag.Bool("pr", false, "Post review as GitHub PR comment")
	createIssues := flag.Bool("issues", false, "Create GitHub issues for each finding")
	loopMode := flag.Bool("loop", false, "Keep reviewing latest HEAD until APPROVE, then merge and close issues")
	autoFix := flag.Bool("auto-fix", false, "Automatically apply AI fixes between loop iterations (requires --loop)")
	mergeOnApprove := flag.Bool("merge", false, "Merge PR immediately if this review is APPROVE (one-shot, alias for single-pass --loop)")
	mergeStrategy := flag.String("merge-strategy", "squash", "Merge strategy: merge, squash, rebase")
	deleteBranch := flag.Bool("delete-branch", false, "Delete branch after merge")
	loopInterval := flag.Duration("loop-interval", 30*time.Second, "How long to wait between loop iterations")
	flag.Parse()

	// Resolve directory
	dir := *dirFlag
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot get working directory: %v\n", err)
			os.Exit(1)
		}
	}
	dir, _ = filepath.Abs(dir)

	repoRoot, err := resolveRepoRoot(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	client := opencode.NewClient(option.WithBaseURL(*serverURL))
	ctx := context.Background()

	// GitHub client (required for --pr, --issues, --merge, --loop)
	var gh *github.Client
	var ghOwner, ghRepo string
	var ghPRNum int
	if *postPR || *createIssues || *loopMode || *mergeOnApprove {
		var err error
		gh, err = ghClient(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "GitHub: %v\n", err)
			os.Exit(1)
		}
		ghOwner, ghRepo, err = repoOwnerName(repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "GitHub: %v\n", err)
			os.Exit(1)
		}
		if *postPR || *loopMode || *mergeOnApprove {
			// Fatal: PR is required for review posting, loop, and merge.
			ghPRNum, err = openPRNumber(ctx, gh, ghOwner, ghRepo, repoRoot)
			if err != nil {
				fmt.Fprintf(os.Stderr, "GitHub PR: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("GitHub: %s/%s PR #%d\n", ghOwner, ghRepo, ghPRNum)
		} else if *createIssues {
			// Non-fatal: try to find a PR for issue linking/commenting; continue without one.
			if n, err2 := openPRNumber(ctx, gh, ghOwner, ghRepo, repoRoot); err2 == nil {
				ghPRNum = n
				fmt.Printf("GitHub: %s/%s PR #%d\n", ghOwner, ghRepo, ghPRNum)
			}
		}
	}

	models, err := listModels(client, ctx, repoRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error fetching models: %v\n", err)
		os.Exit(1)
	}
	if len(models) == 0 {
		fmt.Fprintln(os.Stderr, "No models available. Is opencode running?")
		os.Exit(1)
	}

	var selected ModelInfo
	if *modelNum >= 1 && *modelNum <= len(models) {
		selected = models[*modelNum-1]
	} else {
		selected = selectModel(models)
	}
	fmt.Printf("\nUsing: %s / %s\n\n", selected.ProviderName, selected.ModelName)

	// --merge is a one-shot alias: review once, merge if APPROVE
	if *mergeOnApprove {
		*loopMode = true
		*loopInterval = 0
	}

	// Ensure logs directory exists
	logsDir := filepath.Join(repoRoot, "logs")
	_ = os.MkdirAll(logsDir, 0o755)
	logFile := filepath.Join(logsDir, fmt.Sprintf("review-%s.jsonl", time.Now().Format("20060102-150405")))
	logF, logErr := os.Create(logFile)
	if logErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: cannot create log file: %v\n", logErr)
		logF = nil
	}
	writeLog := func(record any) {
		if logF == nil {
			return
		}
		b, _ := json.Marshal(record)
		logF.Write(b)
		logF.Write([]byte("\n"))
	}
	defer func() {
		if logF != nil {
			logF.Close()
		}
	}()

	// Track open issues across loop iterations (deduplicated via GitHub API)
	var openIssues []int
	iteration := 0

	for {
		iteration++
		ref := *commitRef
		if *loopMode && iteration > 1 {
			ref = "HEAD"
		}

		hash, subject, body, diff, err := getCommitInfo(repoRoot, ref)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading commit: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("─── Iteration %d — reviewing %s\n  %s\n\n", iteration, hash[:12], subject)
		fmt.Println("--- Code Review ---\n")

		var reviewPrompt string
		if *auditMode {
			reviewPrompt, err = buildAuditPrompt(repoRoot)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Audit prompt error: %v\n", err)
				os.Exit(1)
			}
		} else {
			reviewPrompt = buildReviewPrompt(hash, subject, body, diff)
		}

		reviewText, err := runReview(client, ctx, repoRoot, selected, reviewPrompt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Review failed: %v\n", err)
			os.Exit(1)
		}
		verdict := extractVerdict(reviewText)

		// Post PR review comment
		if *postPR && reviewText != "" {
			fmt.Printf("Posting PR review (%s)...\n", verdict)
			if err := postGitHubPRReview(ctx, gh, ghOwner, ghRepo, ghPRNum, reviewText, verdict); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to post PR review: %v\n", err)
			} else {
				fmt.Println("Posted.")
			}
		}

		// Write per-iteration log entry
		writeLog(map[string]any{
			"ts":        time.Now().Format(time.RFC3339),
			"iteration": iteration,
			"commit":    hash,
			"subject":   subject,
			"verdict":   verdict,
		})

		// Create GitHub issues for new findings only (deduplicated via live issue list)
		if *createIssues && reviewText != "" {
			findings := parseFindings(reviewText)
			ghSeen := existingIssues(ctx, gh, ghOwner, ghRepo)
			var newIssues []int
			for _, f := range findings {
				ghTitle := fmt.Sprintf("[%s] %s:%s — %s", f.Severity, f.File, f.LineRange, f.Title)
				// Deduplicate: skip if an open issue with this exact title already exists
				if ghSeen[ghTitle] {
					fmt.Printf("  Skipping (already open): %s\n", ghTitle)
					continue
				}
				num, err := createIssue(ctx, gh, ghOwner, ghRepo, f)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  Failed to create issue for %q: %v\n", f.Title, err)
					continue
				}
				fmt.Printf("  #%d [%s] %s:%s — %s\n", num, f.Severity, f.File, f.LineRange, f.Title)
				newIssues = append(newIssues, num)
				openIssues = append(openIssues, num)
				if ghSeen != nil {
					ghSeen[ghTitle] = true // prevent double-filing within same iteration
				}
				// Comment on the issue referencing the PR (if one exists)
				if ghPRNum > 0 {
					prRef := fmt.Sprintf("Tracking in PR #%d.", ghPRNum)
					if _, _, err2 := gh.Issues.CreateComment(ctx, ghOwner, ghRepo, num, &github.IssueComment{Body: &prRef}); err2 != nil {
						fmt.Fprintf(os.Stderr, "  Warning: could not comment on issue #%d: %v\n", num, err2)
					}
				}
				writeLog(map[string]any{
					"ts":        time.Now().Format(time.RFC3339),
					"action":    "issue_created",
					"issue":     num,
					"severity":  f.Severity,
					"file":      f.File,
					"lineRange": f.LineRange,
					"title":     f.Title,
				})
			}
			if len(newIssues) == 0 && len(findings) > 0 {
				fmt.Println("  No new findings (all already tracked).")
			}
			// Link new issues to PR (always when PR exists, not only when --pr is set)
			if ghPRNum > 0 && len(newIssues) > 0 {
				if err := linkIssuesToPR(ctx, gh, ghOwner, ghRepo, ghPRNum, newIssues); err != nil {
					fmt.Fprintf(os.Stderr, "  Failed to link issues to PR: %v\n", err)
				} else {
					fmt.Println("  Issues linked to PR.")
				}
			}
		}

		fmt.Printf("\nVerdict: %s\n", verdict)

		if !*loopMode {
			break
		}

		if verdict == "APPROVE" {
			fmt.Println("\nAll issues resolved — merging PR...")
			currentHead, _ := gitRun(repoRoot, "rev-parse", "HEAD")
			if hash != currentHead {
				fmt.Fprintln(os.Stderr, "Refusing to merge: reviewed commit is not current HEAD.")
				os.Exit(1)
			}
			if err := mergePR(ctx, gh, ghOwner, ghRepo, ghPRNum, *mergeStrategy, *deleteBranch); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to merge: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("Merged.")
			writeLog(map[string]any{"ts": time.Now().Format(time.RFC3339), "action": "pr_merged", "strategy": *mergeStrategy})

			// Close issues created by opencode-review (current run + previous runs).
			// Only closes issues whose body ends with the opencode-review signature line
			// to avoid touching unrelated human-tracked issues.
			if *createIssues {
				closeNums := map[int]bool{}
				for _, n := range openIssues {
					closeNums[n] = true
				}
				// Fetch open issues from previous runs (identified by signature footer)
				opts := &github.IssueListByRepoOptions{State: "open", ListOptions: github.ListOptions{PerPage: 100}}
				var listErr error
				for {
					issues, resp, err2 := gh.Issues.ListByRepo(ctx, ghOwner, ghRepo, opts)
					if err2 != nil {
						listErr = err2
						break
					}
					for _, issue := range issues {
						if issue.PullRequestLinks == nil &&
							strings.Contains(issue.GetBody(), "_Reported by opencode-review_") {
							closeNums[issue.GetNumber()] = true
						}
					}
					if resp.NextPage == 0 {
						break
					}
					opts.Page = resp.NextPage
				}
				if listErr != nil {
					fmt.Fprintf(os.Stderr, "Failed to list open issues: %v\n", listErr)
					os.Exit(1)
				}
				var closeFailed bool
				if len(closeNums) > 0 {
					fmt.Printf("Closing %d opencode-review issue(s)...\n", len(closeNums))
					for n := range closeNums {
						if err := closeIssue(ctx, gh, ghOwner, ghRepo, n); err != nil {
							fmt.Fprintf(os.Stderr, "  Failed to close #%d: %v\n", n, err)
							closeFailed = true
						} else {
							fmt.Printf("  Closed #%d\n", n)
							writeLog(map[string]any{"ts": time.Now().Format(time.RFC3339), "action": "issue_closed", "issue": n})
						}
					}
				}
				if closeFailed {
					fmt.Fprintln(os.Stderr, "Warning: some issues could not be closed.")
					os.Exit(1)
				}
			}
			break
		}

		if *mergeOnApprove {
			// --merge is one-shot: don't loop on non-APPROVE
			fmt.Fprintln(os.Stderr, "Not merging: verdict is not APPROVE.")
			os.Exit(1)
		}
		if *autoFix {
			findings := parseFindings(reviewText)
			var fixable []Finding
			for _, f := range findings {
				if f.AgentPrompt != "" {
					fixable = append(fixable, f)
				}
			}
			if len(fixable) > 0 {
				fmt.Printf("\n--- Auto-fixing %d finding(s) ---\n", len(fixable))
				n := runFix(client, ctx, repoRoot, selected, fixable, iteration)
				writeLog(map[string]any{"ts": time.Now().Format(time.RFC3339), "action": "auto_fix", "findings_fixed": n})
				if n > 0 {
					continue // skip sleep, re-review immediately
				}
			}
		}
		if *loopInterval > 0 {
			fmt.Printf("\nWaiting %s for fixes before next review...\n", *loopInterval)
			time.Sleep(*loopInterval)
		}
	}
}
