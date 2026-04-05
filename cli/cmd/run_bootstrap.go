package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	sdk "github.com/sst/opencode-sdk-go"

	"github.com/talk/opencode-client/internal/git"
	"github.com/talk/opencode-client/internal/logger"
	occ "github.com/talk/opencode-client/internal/opencode"
	"github.com/talk/opencode-client/internal/types"
)

type runEnvironment struct {
	ctx      context.Context
	repoRoot string
	client   *sdk.Client
	log      *logger.Logger
}

func initEnvironment(cfg runConfig) (runEnvironment, error) {
	dir := cfg.dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return runEnvironment{}, wrapErr("Cannot get working directory", err)
		}
		dir = wd
	}
	absDir, err := filepath.Abs(dir)
	if err == nil {
		dir = absDir
	}
	repoRoot, err := git.ResolveRepoRoot(dir)
	if err != nil {
		return runEnvironment{}, wrapErr("", err)
	}
	return runEnvironment{
		ctx:      context.Background(),
		repoRoot: repoRoot,
		client:   occ.NewClient(cfg.serverURL),
		log:      logger.New(repoRoot),
	}, nil
}

func mustCreateFixBranchIfRequested(repoRoot string, cfg *runConfig) error {
	if !cfg.createBranch {
		return nil
	}
	newBranch := fmt.Sprintf("fix/opencode-%s", time.Now().Format("20060102-150405"))
	_, err := git.Run(repoRoot, "checkout", "-b", newBranch)
	if err != nil {
		return wrapErr(fmt.Sprintf("Failed to create branch %q", newBranch), err)
	}
	_, err = git.Run(repoRoot, "commit", "--allow-empty", "-m", "chore: open fix branch for opencode-review")
	if err != nil {
		return wrapErr("Failed to create empty commit", err)
	}
	_, err = git.Run(repoRoot, "push", "-u", "origin", newBranch)
	if err != nil {
		return wrapErr(fmt.Sprintf("Failed to push branch %q", newBranch), err)
	}
	fmt.Printf("Created and pushed branch %q\n", newBranch)
	cfg.auditMode = true
	return nil
}

func selectModel(env runEnvironment, cfg runConfig) (types.ModelInfo, error) {
	models, err := occ.ListModels(env.client, env.ctx, env.repoRoot)
	if err != nil {
		return types.ModelInfo{}, wrapErr("Error fetching models", err)
	}
	if len(models) == 0 {
		return types.ModelInfo{}, fmt.Errorf("no models available. Is opencode running?")
	}
	var selected types.ModelInfo
	if cfg.modelNum >= 1 && cfg.modelNum <= len(models) {
		selected = models[cfg.modelNum-1]
	} else {
		selected, err = occ.SelectModel(models)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return types.ModelInfo{}, fmt.Errorf("model selection canceled")
			}
			return types.ModelInfo{}, wrapErr("Error selecting model", err)
		}
	}
	fmt.Printf("\nUsing: %s / %s\n\n", selected.ProviderName, selected.ModelName)
	return selected, nil
}

func wrapErr(context string, err error) error {
	if err == nil {
		return nil
	}
	if context == "" {
		return err
	}
	return fmt.Errorf("%s: %w", context, err)
}
