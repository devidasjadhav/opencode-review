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
	"time"

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

const (
	streamMaxAttempts = 3
	streamBackoffBase = 5 * time.Second
)

// streamSession creates an opencode session, sends prompt, streams the response
// and returns the full text when the session goes idle.
// Retries up to streamMaxAttempts times with exponential backoff on transient
// errors; does not retry if the caller context is cancelled.
// renderer receives streamed text deltas when non-nil.
func streamSession(client *sdk.Client, ctx context.Context, repoRoot string, selected types.ModelInfo, prompt string, renderer io.Writer) (string, error) {
	var lastErr error
	backoff := streamBackoffBase
	for attempt := 1; attempt <= streamMaxAttempts; attempt++ {
		result, err := tryStreamSession(client, ctx, repoRoot, selected, prompt, renderer)
		if err == nil {
			return result, nil
		}
		lastErr = err
		// Don't retry if the caller cancelled the context.
		if ctx.Err() != nil {
			return "", err
		}
		if attempt < streamMaxAttempts {
			fmt.Fprintf(os.Stderr, "  Session error (attempt %d/%d): %v — retrying in %s\n", attempt, streamMaxAttempts, err, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return "", ctx.Err()
			}
			backoff *= 2
		}
	}
	return "", lastErr
}

// tryStreamSession is a single attempt at streaming a session.
func tryStreamSession(client *sdk.Client, ctx context.Context, repoRoot string, selected types.ModelInfo, prompt string, renderer io.Writer) (string, error) {
	session, err := client.Session.New(ctx, sdk.SessionNewParams{
		Directory: sdk.F(repoRoot),
	})
	if err != nil {
		return "", fmt.Errorf("error creating session: %w", err)
	}
	sessionID := session.ID
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		client.Session.Delete(cleanupCtx, sessionID, sdk.SessionDeleteParams{}) //nolint
	}()

	// Buffer of 3 ensures consumeSessionEvents never blocks on send:
	// idle event + stream-error fallback + edge-case rapid session-error then stream-close.
	idleCh := make(chan streamResult, 3)
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream := client.Event.ListStreaming(streamCtx, sdk.EventListParams{
		Directory: sdk.F(repoRoot),
	})

	go consumeSessionEvents(stream, sessionID, renderer, idleCh, cancel)

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
		cancel()
		return "", fmt.Errorf("error sending prompt: %w", err)
	}
	res := <-idleCh
	return res.text, res.err
}

// streamResult carries the outcome of a streamed session to the caller goroutine.
type streamResult struct {
	text string
	err  error
}

func consumeSessionEvents(stream sessionEventStream, sessionID string, renderer io.Writer, idleCh chan<- streamResult, cancel context.CancelFunc) {
	defer cancel()
	var buf strings.Builder
	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "message.part.delta":
			decodeAndRenderMessageDelta(event.JSON.RawJSON(), renderer, &buf)
		case sdk.EventListResponseTypeSessionIdle:
			idle, ok := event.AsUnion().(sdk.EventListResponseEventSessionIdle)
			if ok && idle.Properties.SessionID == sessionID {
				if renderer != nil {
					_, _ = io.WriteString(renderer, "\n\n")
				}
				idleCh <- streamResult{text: buf.String()}
			}
		case sdk.EventListResponseTypeSessionError:
			raw := event.JSON.RawJSON()
			fmt.Fprintf(os.Stderr, "\n[session error] %s\n", raw)
			idleCh <- streamResult{err: fmt.Errorf("session error: %s", raw)}
		}
	}
	if err := stream.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "\nStream error: %v\n", err)
		idleCh <- streamResult{err: fmt.Errorf("stream error: %w", err)}
	}
}

func decodeAndRenderMessageDelta(rawJSON string, renderer io.Writer, buf *strings.Builder) {
	var envelope struct {
		Properties struct {
			Field string `json:"field"`
			Delta string `json:"delta"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(rawJSON), &envelope); err != nil {
		return
	}
	if envelope.Properties.Field != "text" {
		return
	}
	if renderer != nil {
		_, _ = io.WriteString(renderer, envelope.Properties.Delta)
	}
	buf.WriteString(envelope.Properties.Delta)
}

// RunReview sends a review prompt and returns the full streamed response.
func RunReview(client *sdk.Client, ctx context.Context, repoRoot string, selected types.ModelInfo, prompt string) (string, error) {
	return streamSession(client, ctx, repoRoot, selected, prompt, os.Stdout)
}

// RunVerify sends a verification prompt to the verifier model and returns the full response.
func RunVerify(client *sdk.Client, ctx context.Context, repoRoot string, verifier types.ModelInfo, prompt string) (string, error) {
	return streamSession(client, ctx, repoRoot, verifier, prompt, os.Stdout)
}

// RunFix sends fix prompts for all findings, commits and pushes any changes.
// Returns the number of findings sent for fixing.
type FixPersister interface {
	Persist(repoRoot string, findings []types.Finding, iteration int, stagePaths []string) (fixPersistResult, error)
}

type gitFixPersister struct {
	validator FixValidator
}

func NewGitFixPersister() FixPersister {
	return gitFixPersister{validator: NewGoBuildFixValidator()}
}

func NewGitFixPersisterWithValidator(validator FixValidator) FixPersister {
	return gitFixPersister{validator: validator}
}

func (p gitFixPersister) Persist(repoRoot string, findings []types.Finding, iteration int, stagePaths []string) (fixPersistResult, error) {
	return stageCommitPushFixes(repoRoot, findings, iteration, stagePaths, p.validator)
}

func RunFix(client *sdk.Client, ctx context.Context, repoRoot string, selected types.ModelInfo, findings []types.Finding, iteration int, persister FixPersister) (int, []string, error) {
	if len(findings) == 0 {
		return 0, nil, nil
	}
	if persister == nil {
		persister = NewGitFixPersister()
	}
	prompt := buildFixPrompt(findings, repoRoot)
	applyResult, err := applyFixPrompt(client, ctx, repoRoot, selected, prompt)
	if err != nil {
		return 0, nil, err
	}
	if applyResult.Outcome != fixApplyChanged {
		fmt.Println("  (no file changes detected after fix)")
		return 0, nil, nil
	}
	persistResult, err := persister.Persist(repoRoot, findings, iteration, applyResult.StagePaths)
	if err != nil {
		return 0, nil, err
	}
	if persistResult.Outcome != fixPersistCommitted {
		return 0, nil, nil
	}
	fmt.Printf("  Committed and pushed fixes: %q\n", persistResult.CommitMessage)
	return len(findings), applyResult.StagePaths, nil
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

type sessionEventStream interface {
	Next() bool
	Current() sdk.EventListResponse
	Err() error
}

func (e runFixError) Error() string {
	return fmt.Sprintf("%s: %v", e.Context, e.Err)
}

// buildFixPrompt constructs the prompt sent to the AI fixer.
// repoRoot is used to read the current on-disk content of each file
// referenced in findings so the model works from accurate code, not
// a potentially stale mental model built from the original review diff.
func buildFixPrompt(findings []types.Finding, repoRoot string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "You are an automated code fixer. Apply ALL of the following fixes to the codebase.\n")
	fmt.Fprintf(&sb, "Make minimal, targeted changes. Do not refactor unrelated code.\n")
	fmt.Fprintf(&sb, "IMPORTANT: Only modify the specific file(s) referenced in each finding. Do NOT touch any other files.\n")
	fmt.Fprintf(&sb, "After applying all fixes, stop — do not explain or summarise.\n\n")

	// Embed current file contents so the fixer has the real code, not
	// just the review diff which may be stale if other fixes ran first.
	if contents := collectFixFileContents(repoRoot, findings); len(contents) > 0 {
		sb.WriteString("## Current File Contents\n")
		sb.WriteString("Use these as the authoritative source of truth when applying fixes.\n\n")
		paths := make([]string, 0, len(contents))
		for p := range contents {
			paths = append(paths, p)
		}
		sort.Strings(paths)
		for _, p := range paths {
			fmt.Fprintf(&sb, "### %s\n```\n%s\n```\n\n", p, contents[p])
		}
	}

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

// collectFixFileContents reads the current on-disk content of every unique
// file referenced in findings. Paths are validated to stay within repoRoot
// (guards against model-produced absolute paths or ../ traversal).
// Missing, unreadable, or out-of-bounds files are silently skipped.
func collectFixFileContents(repoRoot string, findings []types.Finding) map[string]string {
	seen := make(map[string]bool)
	contents := make(map[string]string)
	for _, f := range findings {
		if f.File == "" || seen[f.File] {
			continue
		}
		seen[f.File] = true
		clean := filepath.Clean(f.File)
		if filepath.IsAbs(clean) {
			continue
		}
		abs := filepath.Join(repoRoot, clean)
		rel, err := filepath.Rel(repoRoot, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		contents[f.File] = string(data)
	}
	return contents
}

func applyFixPrompt(client *sdk.Client, ctx context.Context, repoRoot string, selected types.ModelInfo, prompt string) (fixApplyResult, error) {
	beforeStatus, err := git.StatusSnapshot(repoRoot)
	if err != nil {
		return fixApplyResult{}, runFixError{Context: "Fix status snapshot error", Err: err}
	}
	if _, err := streamSession(client, ctx, repoRoot, selected, prompt, os.Stdout); err != nil {
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

func stageCommitPushFixes(repoRoot string, findings []types.Finding, iteration int, stagePaths []string, validator FixValidator) (fixPersistResult, error) {
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
	if validator == nil {
		validator = NewGoBuildFixValidator()
	}
	if err := validator.Validate(repoRoot, validPaths); err != nil {
		restoreArgs := append([]string{"restore", "--staged", "--worktree", "--"}, validPaths...)
		_, _ = git.Run(repoRoot, restoreArgs...)
		return fixPersistResult{}, runFixError{Context: "  validation failed after fix — reverted fix paths", Err: err}
	}

	msg := fmt.Sprintf("fix: auto-fix %d finding(s) from review iteration %d", len(findings), iteration)
	commitArgs := append([]string{"commit", "-m", msg, "--"}, validPaths...)
	if _, err := git.Run(repoRoot, commitArgs...); err != nil {
		return fixPersistResult{}, runFixError{Context: "  git commit failed", Err: err}
	}
	branch, err := git.Run(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fixPersistResult{}, runFixError{Context: "  git branch resolution failed", Err: err}
	}
	if _, err := git.Run(repoRoot, "push", "origin", branch); err != nil {
		return fixPersistResult{}, runFixError{Context: "  git push failed", Err: err}
	}
	return fixPersistResult{Outcome: fixPersistCommitted, CommitMessage: msg}, nil
}
