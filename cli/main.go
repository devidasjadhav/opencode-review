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
	"sort"
	"strconv"
	"strings"

	opencode "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"
)

type ModelInfo struct {
	ProviderID   string
	ProviderName string
	ModelID      string
	ModelName    string
}

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
	sb.WriteString("Only real, verifiable issues. For each finding use this format:\n\n")
	sb.WriteString("### `[🔴 Critical|🟠 High|🟡 Medium|🔵 Low]` file:line-range — Short title\n")
	sb.WriteString("Clear description referencing the exact code and how it interacts with the rest of the codebase.\n\n")
	sb.WriteString("```diff\n")
	sb.WriteString("- old code\n")
	sb.WriteString("+ fixed code\n")
	sb.WriteString("```\n\n")
	sb.WriteString("**AI agent fix prompt:** One-paragraph instruction a coding agent can execute directly to fix this issue.\n\n")
	sb.WriteString("If there are no issues: write `_No issues found._` and skip this section's subsections.\n\n")

	sb.WriteString("## Verdict\n")
	sb.WriteString("Exactly one of these tokens on its own line, followed by one sentence reason:\n")
	sb.WriteString("`APPROVE` — safe to merge.\n")
	sb.WriteString("`REQUEST CHANGES` — must fix before merge.\n")
	sb.WriteString("`COMMENT` — observations only, no blocking issues.\n\n")

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

// streamResponse streams the response to stdout and returns the full text via doneCh.
func streamResponse(client *opencode.Client, ctx context.Context, sessionID, dir string, idleCh chan string) {
	streamCtx, cancel := context.WithCancel(ctx)

	stream := client.Event.ListStreaming(streamCtx, opencode.EventListParams{
		Directory: opencode.F(dir),
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
}

func postGitHubPRReview(repoRoot, body, verdict string) error {
	// Try the matching review type; fall back to --comment if GitHub rejects it
	// (e.g. you cannot approve/request-changes on your own PR).
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
		// GitHub rejects approve/request-changes on own PRs — retry as comment
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

func extractVerdict(review string) string {
	// Only parse the ## Verdict section to avoid false matches in findings/summary.
	inVerdict := false
	for _, line := range strings.Split(review, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "## Verdict") {
			inVerdict = true
			continue
		}
		if inVerdict {
			if strings.HasPrefix(trimmed, "##") {
				break // next section
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

func main() {
	serverURL := flag.String("url", "http://localhost:4096", "Opencode server URL")
	dirFlag := flag.String("dir", "", "Git repo directory to review (default: current directory)")
	modelNum := flag.Int("model", 0, "Model number from the list (skips interactive selection)")
	postPR := flag.Bool("pr", false, "Post review as a GitHub PR comment (requires gh CLI)")
	commitRef := flag.String("commit", "HEAD", "Git ref to review (commit hash, branch, tag)")
	doMerge := flag.Bool("merge", false, "Merge the PR after posting review (only if verdict is APPROVE)")
	mergeStrategy := flag.String("merge-strategy", "squash", "Merge strategy: merge, squash, rebase")
	deleteBranch := flag.Bool("delete-branch", false, "Delete branch after merge")
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

	// Resolve git repo root
	repoRoot, err := resolveRepoRoot(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Get commit info
	hash, subject, body, diff, err := getCommitInfo(repoRoot, *commitRef)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading commit: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Reviewing commit: %s\n  %s\n\n", hash[:12], subject)

	client := opencode.NewClient(option.WithBaseURL(*serverURL))
	ctx := context.Background()

	// List and select model
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

	// Create session in the repo directory
	session, err := client.Session.New(ctx, opencode.SessionNewParams{
		Directory: opencode.F(repoRoot),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating session: %v\n", err)
		os.Exit(1)
	}
	sessionID := session.ID

	// Start event stream
	idleCh := make(chan string, 2)
	streamResponse(client, ctx, sessionID, repoRoot, idleCh)

	// Send review prompt
	prompt := buildReviewPrompt(hash, subject, body, diff)
	fmt.Println("--- Code Review ---\n")

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

	// Wait for response to complete
	reviewText := <-idleCh

	verdict := extractVerdict(reviewText)

	// Optionally post to GitHub PR
	if *postPR && reviewText != "" {
		fmt.Printf("Posting to GitHub PR (%s)...\n", verdict)
		if err := postGitHubPRReview(repoRoot, reviewText, verdict); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to post PR review: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Posted.")
	}

	// Optionally merge if verdict is APPROVE
	if *doMerge {
		if verdict != "APPROVE" {
			fmt.Fprintf(os.Stderr, "Not merging: verdict is %q (only merges on APPROVE)\n", verdict)
			os.Exit(1)
		}
		fmt.Printf("Merging PR (%s)...\n", *mergeStrategy)
		if err := mergePR(repoRoot, *mergeStrategy, *deleteBranch); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to merge PR: %v\n", err)
			os.Exit(1)
		}
		fmt.Println("Merged.")
	}
}

func mergePR(repoRoot, strategy string, deleteBranch bool) error {
	args := []string{"pr", "merge"}
	switch strategy {
	case "merge":
		args = append(args, "--merge")
	case "rebase":
		args = append(args, "--rebase")
	default:
		args = append(args, "--squash")
	}
	if deleteBranch {
		args = append(args, "--delete-branch")
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = repoRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
