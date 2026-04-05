package cmd

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/talk/opencode-client/internal/git"
)

// loadEnvFile reads KEY=VALUE pairs from .opencode-review.env (fallback: .env)
// in the given directory. Returns nil if no file is found.
func loadEnvFile(dir string) map[string]string {
	for _, name := range []string{".opencode-review.env", ".env"} {
		path := filepath.Join(dir, name)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		defer f.Close()
		return parseEnvFile(f)
	}
	return nil
}

func parseEnvFile(f *os.File) map[string]string {
	m := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m
}

// prescanDir extracts --dir / -dir value from os.Args before flag.Parse().
func prescanDir() string {
	args := os.Args[1:]
	for i, arg := range args {
		if (arg == "--dir" || arg == "-dir") && i+1 < len(args) {
			return args[i+1]
		}
		for _, prefix := range []string{"--dir=", "-dir="} {
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimPrefix(arg, prefix)
			}
		}
	}
	return ""
}

// resolveRootForEnv resolves the git repo root for .env lookup.
// Falls back to dir itself if git lookup fails.
func resolveRootForEnv(dir string) string {
	if dir == "" {
		dir, _ = os.Getwd()
	}
	root, err := git.ResolveRepoRoot(dir)
	if err != nil {
		return dir
	}
	return root
}

func envOr(env map[string]string, key, fallback string) string {
	if v, ok := env[key]; ok && v != "" {
		return v
	}
	return fallback
}

func envBool(env map[string]string, key string, fallback bool) bool {
	v, ok := env[key]
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
		return false
	}
	return fallback
}

func envInt(env map[string]string, key string, fallback int) int {
	v, ok := env[key]
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return n
}

func envDuration(env map[string]string, key string, fallback time.Duration) time.Duration {
	v, ok := env[key]
	if !ok {
		return fallback
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return d
}
