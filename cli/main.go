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
	sb.WriteString("Only real, verifiable issues. For each finding use EXACTLY this format:\n\n")
	sb.WriteString("### `[🔴 Critical|🟠 High|🟡 Medium|🔵 Low]` file:line-range — Short title\n")
	sb.WriteString("Clear description referencing the exact code and how it interacts with the rest of the codebase.\n\n")
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

// ── GitHub helpers ────────────────────────────────────────────────────────────

func ghRun(dir string, args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh %s: %w\n%s", strings.Join(args, " "), err, errBuf.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func createIssue(repoRoot string, f Finding) (int, error) {
	title := fmt.Sprintf("[%s] %s:%s — %s", f.Severity, f.File, f.LineRange, f.Title)
	var body strings.Builder
	fmt.Fprintf(&body, "## %s\n\n", f.Title)
	fmt.Fprintf(&body, "**Severity:** %s  \n", f.Severity)
	fmt.Fprintf(&body, "**Location:** `%s:%s`\n\n", f.File, f.LineRange)
	body.WriteString("### Description\n")
	body.WriteString(strings.TrimSpace(f.Description))
	body.WriteString("\n\n")
	if f.Diff != "" {
		body.WriteString("### Suggested fix\n")
		body.WriteString("```diff\n")
		body.WriteString(f.Diff)
		body.WriteString("\n```\n\n")
	}
	if f.AgentPrompt != "" {
		body.WriteString("### AI agent fix prompt\n")
		body.WriteString(f.AgentPrompt)
		body.WriteString("\n")
	}
	body.WriteString("\n---\n_Reported by opencode-review_\n")

	out, err := ghRun(repoRoot, "issue", "create",
		"--title", title,
		"--body", body.String(),
	)
	if err != nil {
		return 0, err
	}
	// Output is the issue URL, parse number from end
	parts := strings.Split(strings.TrimRight(out, "/"), "/")
	num, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		return 0, fmt.Errorf("cannot parse issue number from %q", out)
	}
	return num, nil
}

func closeIssue(repoRoot string, num int) error {
	_, err := ghRun(repoRoot, "issue", "close", strconv.Itoa(num), "--comment", "Fixed and merged. Closing.")
	return err
}

func linkIssuesToPR(repoRoot string, issueNums []int) error {
	if len(issueNums) == 0 {
		return nil
	}
	var closes []string
	for _, n := range issueNums {
		closes = append(closes, fmt.Sprintf("Closes #%d", n))
	}
	body, err := ghRun(repoRoot, "pr", "view", "--json", "body", "--jq", ".body")
	if err != nil {
		return err
	}
	// Append closes if not already there
	newBody := body + "\n\n" + strings.Join(closes, "\n")
	_, err = ghRun(repoRoot, "pr", "edit", "--body", newBody)
	return err
}

func postGitHubPRReview(repoRoot, body, verdict string) error {
	reviewFlag := "--comment"
	switch {
	case strings.Contains(verdict, "REQUEST CHANGES"):
		reviewFlag = "--request-changes"
	case strings.Contains(verdict, "APPROVE"):
		reviewFlag = "--approve"
	}

	cmd := exec.Command("gh", "pr", "review", reviewFlag, "--body", body)
	cmd.Dir = repoRoot
	var errBuf bytes.Buffer
	cmd.Stdout = os.Stdout
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		if reviewFlag != "--comment" && strings.Contains(errBuf.String(), "own pull request") {
			fmt.Fprintln(os.Stderr, "(falling back to --comment: cannot approve/request-changes own PR)")
			cmd2 := exec.Command("gh", "pr", "review", "--comment", "--body", body)
			cmd2.Dir = repoRoot
			cmd2.Stdout = os.Stdout
			cmd2.Stderr = os.Stderr
			return cmd2.Run()
		}
		fmt.Fprint(os.Stderr, errBuf.String())
		return err
	}
	return nil
}

func mergePR(repoRoot, strategy string, deleteBranch bool) error {
	var strategyFlag string
	switch strategy {
	case "merge":
		strategyFlag = "--merge"
	case "squash":
		strategyFlag = "--squash"
	case "rebase":
		strategyFlag = "--rebase"
	default:
		return fmt.Errorf("invalid merge strategy %q: must be merge, squash, or rebase", strategy)
	}
	args := []string{"pr", "merge", strategyFlag}
	if deleteBranch {
		args = append(args, "--delete-branch")
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ── Opencode stream ───────────────────────────────────────────────────────────

func runReview(client *opencode.Client, ctx context.Context, repoRoot string, selected ModelInfo,
	hash, subject, body, diff string) string {

	session, err := client.Session.New(ctx, opencode.SessionNewParams{
		Directory: opencode.F(repoRoot),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating session: %v\n", err)
		os.Exit(1)
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
					if envelope.Properties.Field == "text" && envelope.Properties.Delta != "" {
						fmt.Print(envelope.Properties.Delta)
						buf.WriteString(envelope.Properties.Delta)
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
		}
	}()

	prompt := buildReviewPrompt(hash, subject, body, diff)
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
		fmt.Fprintf(os.Stderr, "Error sending prompt: %v\n", err)
		os.Exit(1)
	}
	return <-idleCh
}

// ── Main ──────────────────────────────────────────────────────────────────────

func main() {
	serverURL := flag.String("url", "http://localhost:4096", "Opencode server URL")
	dirFlag := flag.String("dir", "", "Git repo directory (default: current directory)")
	modelNum := flag.Int("model", 0, "Model number from list (skips interactive selection)")
	commitRef := flag.String("commit", "HEAD", "Git ref to review (hash, branch, tag)")
	postPR := flag.Bool("pr", false, "Post review as GitHub PR comment")
	createIssues := flag.Bool("issues", false, "Create GitHub issues for each finding")
	loopMode := flag.Bool("loop", false, "Keep reviewing latest HEAD until APPROVE, then merge and close issues")
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

	// Track open issues across loop iterations (deduplicated by finding key)
	var openIssues []int
	seenFindings := map[string]bool{} // key: "file:line:title"
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

		reviewText := runReview(client, ctx, repoRoot, selected, hash, subject, body, diff)
		verdict := extractVerdict(reviewText)

		// Post PR review comment
		if *postPR && reviewText != "" {
			fmt.Printf("Posting PR review (%s)...\n", verdict)
			if err := postGitHubPRReview(repoRoot, reviewText, verdict); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to post PR review: %v\n", err)
			} else {
				fmt.Println("Posted.")
			}
		}

		// Create GitHub issues for new findings only (deduplicated)
		if *createIssues && reviewText != "" {
			findings := parseFindings(reviewText)
			var newIssues []int
			for _, f := range findings {
				key := fmt.Sprintf("%s:%s:%s", f.File, f.LineRange, f.Title)
				if seenFindings[key] {
					continue // already filed
				}
				seenFindings[key] = true
				num, err := createIssue(repoRoot, f)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  Failed to create issue for %q: %v\n", f.Title, err)
					continue
				}
				fmt.Printf("  #%d [%s] %s:%s — %s\n", num, f.Severity, f.File, f.LineRange, f.Title)
				newIssues = append(newIssues, num)
				openIssues = append(openIssues, num)
			}
			if len(newIssues) == 0 && len(findings) > 0 {
				fmt.Println("  No new findings (all already tracked).")
			}
			// Link new issues to PR
			if *postPR && len(newIssues) > 0 {
				if err := linkIssuesToPR(repoRoot, newIssues); err != nil {
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
			if err := mergePR(repoRoot, *mergeStrategy, *deleteBranch); err != nil {
				fmt.Fprintf(os.Stderr, "Failed to merge: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("Merged.")

			// Close all tracked issues; exit non-zero if any fail
			var closeFailed bool
			if len(openIssues) > 0 {
				fmt.Printf("Closing %d issue(s)...\n", len(openIssues))
				for _, n := range openIssues {
					if err := closeIssue(repoRoot, n); err != nil {
						fmt.Fprintf(os.Stderr, "  Failed to close #%d: %v\n", n, err)
						closeFailed = true
					} else {
						fmt.Printf("  Closed #%d\n", n)
					}
				}
			}
			if closeFailed {
				fmt.Fprintln(os.Stderr, "Warning: some issues could not be closed.")
				os.Exit(1)
			}
			break
		}

		if *mergeOnApprove {
			// --merge is one-shot: don't loop on non-APPROVE
			fmt.Fprintln(os.Stderr, "Not merging: verdict is not APPROVE.")
			os.Exit(1)
		}
		fmt.Printf("\nWaiting %s for fixes before next review...\n", *loopInterval)
		time.Sleep(*loopInterval)
	}
}
