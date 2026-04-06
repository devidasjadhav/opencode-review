package git

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"os/exec"
)

func Run(dir string, args ...string) (string, error) {
	return RunInDir(dir, "git", args...)
}

// RunInDir runs an arbitrary command in dir and returns trimmed stdout+stderr output.
func RunInDir(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out.String())
	}
	return strings.TrimSpace(out.String()), nil
}

func ResolveRepoRoot(dir string) (string, error) {
	root, err := Run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("not a git repository: %w", err)
	}
	return root, nil
}

func GetCommitInfo(root, ref string) (hash, subject, body, diff string, err error) {
	hash, err = Run(root, "rev-parse", ref)
	if err != nil {
		err = fmt.Errorf("unknown ref %q: %w", ref, err)
		return
	}
	subject, err = Run(root, "log", "-1", "--format=%s", hash)
	if err != nil {
		return
	}
	body, err = Run(root, "log", "-1", "--format=%b", hash)
	if err != nil {
		return
	}
	diff, err = Run(root, "show", "--stat", "--patch", hash)
	return
}

// StatusSnapshotEntry captures the git status and content hash of a file.
type StatusSnapshotEntry struct {
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
	hash, err := Run(root, "hash-object", "--", path)
	if err != nil {
		return "<non-blob>", nil
	}
	return hash, nil
}

func StatusSnapshot(root string) (map[string]StatusSnapshotEntry, error) {
	out, err := Run(root, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	snapshot := map[string]StatusSnapshotEntry{}
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
		snapshot[path] = StatusSnapshotEntry{Status: status, WorktreeHash: worktreeHash}
	}
	return snapshot, nil
}

func IsOperationalArtifact(path string) bool {
	p := filepath.ToSlash(path)
	return strings.HasPrefix(p, "logs/")
}

// DiffHead returns the stat+patch output of the current HEAD commit.
func DiffHead(repoRoot string) (string, error) {
	return Run(repoRoot, "show", "--stat", "--patch", "HEAD")
}

// RevertHead creates a revert commit for HEAD and pushes it to origin.
func RevertHead(repoRoot string) error {
	if _, err := Run(repoRoot, "revert", "HEAD", "--no-edit"); err != nil {
		return fmt.Errorf("git revert HEAD: %w", err)
	}
	branch, err := Run(repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fmt.Errorf("git branch name: %w", err)
	}
	if _, err := Run(repoRoot, "push", "origin", branch); err != nil {
		return fmt.Errorf("git push after revert: %w", err)
	}
	return nil
}

// DiffChangedFiles returns repo-relative paths of files modified in the given commit ref.
func DiffChangedFiles(root, ref string) ([]string, error) {
	out, err := Run(root, "diff-tree", "--no-commit-id", "-r", "--name-only", ref)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// ReadFileContents reads the current on-disk content of each repo-relative path.
// Paths that escape the repo root via symlinks or ".." are silently skipped.
func ReadFileContents(repoRoot string, relPaths []string) map[string]string {
	m := make(map[string]string, len(relPaths))
	rootWithSep := repoRoot + string(filepath.Separator)
	for _, p := range relPaths {
		abs := filepath.Join(repoRoot, p)
		// Resolve symlinks so we can enforce the boundary check.
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			resolved = abs
		}
		if resolved != repoRoot && !strings.HasPrefix(resolved, rootWithSep) {
			continue // path traversal — skip
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		m[p] = string(data)
	}
	return m
}

func FixerStagePaths(before, after map[string]StatusSnapshotEntry) []string {
	var paths []string
	for path, afterStatus := range after {
		if IsOperationalArtifact(path) {
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
