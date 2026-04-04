package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	sdk "github.com/sst/opencode-sdk-go"
	"github.com/sst/opencode-sdk-go/option"

	"github.com/talk/opencode-client/internal/git"
	"github.com/talk/opencode-client/internal/types"
)

// NewClient creates a new opencode SDK client pointing at serverURL.
func NewClient(serverURL string) *sdk.Client {
	return sdk.NewClient(option.WithBaseURL(serverURL))
}

// ListModels returns all available models from the opencode server.
func ListModels(client *sdk.Client, ctx context.Context, dir string) ([]types.ModelInfo, error) {
	providers, err := client.App.Providers(ctx, sdk.AppProvidersParams{
		Directory: sdk.F(dir),
	})
	if err != nil {
		return nil, err
	}
	var models []types.ModelInfo
	for _, provider := range providers.Providers {
		ids := make([]string, 0, len(provider.Models))
		for id := range provider.Models {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			m := provider.Models[id]
			models = append(models, types.ModelInfo{
				ProviderID:   provider.ID,
				ProviderName: provider.Name,
				ModelID:      m.ID,
				ModelName:    m.Name,
			})
		}
	}
	return models, nil
}

// SelectModel prompts the user interactively to choose a model.
func SelectModel(models []types.ModelInfo) (types.ModelInfo, error) {
	fmt.Println("Available models:")
	for i, m := range models {
		fmt.Printf("  [%2d] %-20s %s\n", i+1, m.ProviderName, m.ModelName)
	}
	scanner := bufio.NewScanner(os.Stdin)
	const maxInvalidAttempts = 5
	invalidAttempts := 0
	for {
		fmt.Print("\nSelect model number: ")
		if !scanner.Scan() {
			return types.ModelInfo{}, io.EOF
		}
		n, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
		if err != nil || n < 1 || n > len(models) {
			invalidAttempts++
			if invalidAttempts >= maxInvalidAttempts {
				return types.ModelInfo{}, fmt.Errorf("failed to parse model selection after %d attempts", maxInvalidAttempts)
			}
			fmt.Printf("  Enter a number between 1 and %d\n", len(models))
			continue
		}
		return models[n-1], nil
	}
}

// streamSession creates an opencode session, sends prompt, streams the response
// and returns the full text when the session goes idle.
// printText controls whether text deltas are printed to stdout.
func streamSession(client *sdk.Client, ctx context.Context, repoRoot string, selected types.ModelInfo, prompt string, printText bool) (string, error) {
	session, err := client.Session.New(ctx, sdk.SessionNewParams{
		Directory: sdk.F(repoRoot),
	})
	if err != nil {
		return "", fmt.Errorf("error creating session: %w", err)
	}
	sessionID := session.ID
	defer func() {
		client.Session.Delete(ctx, sessionID, sdk.SessionDeleteParams{}) //nolint
	}()

	idleCh := make(chan string, 2)
	streamCtx, cancel := context.WithCancel(ctx)
	stream := client.Event.ListStreaming(streamCtx, sdk.EventListParams{
		Directory: sdk.F(repoRoot),
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
					if envelope.Properties.Field == "text" {
						if printText {
							fmt.Print(envelope.Properties.Delta)
						}
						buf.WriteString(envelope.Properties.Delta)
					}
					// "reasoning" field tokens are suppressed
				}
			case sdk.EventListResponseTypeSessionIdle:
				idle, ok := event.AsUnion().(sdk.EventListResponseEventSessionIdle)
				if ok && idle.Properties.SessionID == sessionID {
					if printText {
						fmt.Println()
						fmt.Println()
					}
					idleCh <- buf.String()
				}
			case sdk.EventListResponseTypeSessionError:
				fmt.Fprintf(os.Stderr, "\n[session error] %s\n", event.JSON.RawJSON())
				idleCh <- ""
			}
		}
		if err := stream.Err(); err != nil && streamCtx.Err() == nil {
			fmt.Fprintf(os.Stderr, "\nStream error: %v\n", err)
			idleCh <- ""
		}
	}()

	_, err = client.Session.Prompt(ctx, sessionID, sdk.SessionPromptParams{
		Directory: sdk.F(repoRoot),
		Parts: sdk.F([]sdk.SessionPromptParamsPartUnion{
			sdk.TextPartInputParam{
				Text: sdk.F(prompt),
				Type: sdk.F(sdk.TextPartInputTypeText),
			},
		}),
		Model: sdk.F(sdk.SessionPromptParamsModel{
			ModelID:    sdk.F(selected.ModelID),
			ProviderID: sdk.F(selected.ProviderID),
		}),
	})
	if err != nil {
		return "", fmt.Errorf("error sending prompt: %w", err)
	}
	return <-idleCh, nil
}

// RunReview sends a review prompt and returns the full streamed response.
func RunReview(client *sdk.Client, ctx context.Context, repoRoot string, selected types.ModelInfo, prompt string) (string, error) {
	return streamSession(client, ctx, repoRoot, selected, prompt, true)
}

// RunFix sends fix prompts for all findings, commits and pushes any changes.
// Returns the number of findings sent for fixing.
type FixPersister interface {
	Persist(repoRoot string, findings []types.Finding, iteration int, stagePaths []string) (fixPersistResult, error)
}

type gitFixPersister struct{}

func NewGitFixPersister() FixPersister {
	return gitFixPersister{}
}

func (gitFixPersister) Persist(repoRoot string, findings []types.Finding, iteration int, stagePaths []string) (fixPersistResult, error) {
	return stageCommitPushFixes(repoRoot, findings, iteration, stagePaths)
}

func RunFix(client *sdk.Client, ctx context.Context, repoRoot string, selected types.ModelInfo, findings []types.Finding, iteration int, persister FixPersister) (int, error) {
	if len(findings) == 0 {
		return 0, nil
	}
	if persister == nil {
		persister = NewGitFixPersister()
	}
	prompt := buildFixPrompt(findings)
	applyResult, err := applyFixPrompt(client, ctx, repoRoot, selected, prompt)
	if err != nil {
		return 0, err
	}
	if applyResult.Outcome != fixApplyChanged {
		fmt.Println("  (no file changes detected after fix)")
		return 0, nil
	}
	persistResult, err := persister.Persist(repoRoot, findings, iteration, applyResult.StagePaths)
	if err != nil {
		return 0, err
	}
	if persistResult.Outcome != fixPersistCommitted {
		return 0, nil
	}
	fmt.Printf("  Committed and pushed fixes: %q\n", persistResult.CommitMessage)
	return len(findings), nil
}

type fixApplyOutcome int

const (
	fixApplyNoChanges fixApplyOutcome = iota
	fixApplyChanged
)

type fixApplyResult struct {
	Outcome    fixApplyOutcome
	StagePaths []string
}

type fixPersistOutcome int

const (
	fixPersistNoop fixPersistOutcome = iota
	fixPersistCommitted
)

type fixPersistResult struct {
	Outcome       fixPersistOutcome
	CommitMessage string
}

type runFixError struct {
	Context string
	Err     error
}

func (e runFixError) Error() string {
	return fmt.Sprintf("%s: %v", e.Context, e.Err)
}

func buildFixPrompt(findings []types.Finding) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are an automated code fixer. Apply ALL of the following fixes to the codebase.\n")
	fmt.Fprintf(&sb, "Make minimal, targeted changes. Do not refactor unrelated code.\n")
	fmt.Fprintf(&sb, "IMPORTANT: Only modify the specific file(s) referenced in each finding. Do NOT touch any other files.\n")
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
	return sb.String()
}

func applyFixPrompt(client *sdk.Client, ctx context.Context, repoRoot string, selected types.ModelInfo, prompt string) (fixApplyResult, error) {
	beforeStatus, err := git.StatusSnapshot(repoRoot)
	if err != nil {
		return fixApplyResult{}, runFixError{Context: "Fix status snapshot error", Err: err}
	}
	if _, err := streamSession(client, ctx, repoRoot, selected, prompt, true); err != nil {
		return fixApplyResult{}, runFixError{Context: "Fix session error", Err: err}
	}
	afterStatus, err := git.StatusSnapshot(repoRoot)
	if err != nil {
		return fixApplyResult{}, runFixError{Context: "Fix status snapshot error", Err: err}
	}
	stagePaths := git.FixerStagePaths(beforeStatus, afterStatus)
	if len(stagePaths) == 0 {
		return fixApplyResult{Outcome: fixApplyNoChanges}, nil
	}
	return fixApplyResult{Outcome: fixApplyChanged, StagePaths: stagePaths}, nil
}

func stageCommitPushFixes(repoRoot string, findings []types.Finding, iteration int, stagePaths []string) (fixPersistResult, error) {
	var validPaths []string
	for _, p := range stagePaths {
		abs := filepath.Join(repoRoot, p)
		if _, err := os.Stat(abs); err == nil {
			validPaths = append(validPaths, p)
		} else {
			fmt.Fprintf(os.Stderr, "  Warning: skipping invalid stage path %q: %v\n", p, err)
		}
	}
	if len(validPaths) == 0 {
		fmt.Println("  (no valid paths to stage after fix)")
		return fixPersistResult{Outcome: fixPersistNoop}, nil
	}
	addArgs := append([]string{"add", "-u", "--"}, validPaths...)
	if _, err := git.Run(repoRoot, addArgs...); err != nil {
		return fixPersistResult{}, runFixError{Context: "  git add failed", Err: err}
	}
	stagedArgs := append([]string{"diff", "--cached", "--name-only", "--"}, validPaths...)
	staged, _ := git.Run(repoRoot, stagedArgs...)
	if staged == "" {
		fmt.Println("  (no file changes detected after fix)")
		return fixPersistResult{Outcome: fixPersistNoop}, nil
	}
	// Build gate: verify the codebase compiles before committing.
	if out, err := git.RunInDir(repoRoot, "go", "build", "./..."); err != nil {
		// Restore changes so the working tree is clean for the next iteration.
		git.Run(repoRoot, "checkout", "--", ".")  //nolint
		return fixPersistResult{}, runFixError{Context: fmt.Sprintf("  build failed after fix — changes reverted:\n%s", out), Err: err}
	}

	msg := fmt.Sprintf("fix: auto-fix %d finding(s) from review iteration %d", len(findings), iteration)
	commitArgs := append([]string{"commit", "-m", msg, "--"}, validPaths...)
	if _, err := git.Run(repoRoot, commitArgs...); err != nil {
		return fixPersistResult{}, runFixError{Context: "  git commit failed", Err: err}
	}
	branch, _ := git.Run(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if _, err := git.Run(repoRoot, "push", "origin", branch); err != nil {
		return fixPersistResult{}, runFixError{Context: "  git push failed", Err: err}
	}
	return fixPersistResult{Outcome: fixPersistCommitted, CommitMessage: msg}, nil
}
