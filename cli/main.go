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

func getLatestCommitInfo(root string) (hash, subject, body, diff string, err error) {
	hash, err = gitRun(root, "rev-parse", "HEAD")
	if err != nil {
		return
	}
	subject, err = gitRun(root, "log", "-1", "--format=%s")
	if err != nil {
		return
	}
	body, err = gitRun(root, "log", "-1", "--format=%b")
	if err != nil {
		return
	}
	// Full diff with context
	diff, err = gitRun(root, "show", "--stat", "--patch", "HEAD")
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
	sb.WriteString("Please do a thorough code review of the following git commit.\n\n")
	sb.WriteString("Use the full project context (all files, imports, types) and LSP information ")
	sb.WriteString("(type definitions, references, diagnostics) to give a precise review. ")
	sb.WriteString("Do not limit the review to the diff alone — consider how the changes interact ")
	sb.WriteString("with the rest of the codebase.\n\n")
	sb.WriteString("Focus on:\n")
	sb.WriteString("- Correctness and logic errors\n")
	sb.WriteString("- Type safety and interface contracts\n")
	sb.WriteString("- Security issues\n")
	sb.WriteString("- Performance concerns\n")
	sb.WriteString("- Missing error handling\n")
	sb.WriteString("- Code style and consistency with the rest of the codebase\n\n")
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

func streamResponse(client *opencode.Client, ctx context.Context, sessionID, dir string, idleCh chan struct{}) {
	streamCtx, cancel := context.WithCancel(ctx)

	stream := client.Event.ListStreaming(streamCtx, opencode.EventListParams{
		Directory: opencode.F(dir),
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
					if envelope.Properties.Field == "text" && envelope.Properties.Delta != "" {
						fmt.Print(envelope.Properties.Delta)
					}
				}
			case opencode.EventListResponseTypeSessionIdle:
				idle, ok := event.AsUnion().(opencode.EventListResponseEventSessionIdle)
				if ok && idle.Properties.SessionID == sessionID {
					fmt.Println()
					fmt.Println()
					idleCh <- struct{}{}
				}
			case opencode.EventListResponseTypeSessionError:
				fmt.Fprintf(os.Stderr, "\n[session error] %s\n", event.JSON.RawJSON())
				idleCh <- struct{}{}
			}
		}
		if err := stream.Err(); err != nil && streamCtx.Err() == nil {
			fmt.Fprintf(os.Stderr, "\nStream error: %v\n", err)
		}
	}()
}

func main() {
	serverURL := flag.String("url", "http://localhost:4096", "Opencode server URL")
	dirFlag := flag.String("dir", "", "Git repo directory to review (default: current directory)")
	modelNum := flag.Int("model", 0, "Model number from the list (skips interactive selection)")
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

	// Get latest commit
	hash, subject, body, diff, err := getLatestCommitInfo(repoRoot)
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
	idleCh := make(chan struct{}, 2)
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
	<-idleCh
}
