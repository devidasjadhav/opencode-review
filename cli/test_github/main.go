package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/google/go-github/v67/github"
	"golang.org/x/oauth2"
)

func ghClient(ctx context.Context) (*github.Client, error) {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("GITHUB_TOKEN not set")
	}
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	return github.NewClient(oauth2.NewClient(ctx, ts)), nil
}

func gitRun(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = ".."
	var out, errBuf strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func main() {
	ctx := context.Background()

	gh, err := ghClient(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth: %v\n", err)
		os.Exit(1)
	}

	// Get remote URL to find owner/repo
	remote, _ := gitRun("remote", "get-url", "origin")
	remote = strings.TrimSuffix(remote, ".git")
	var owner, repo string
	if idx := strings.Index(remote, "github.com/"); idx >= 0 {
		parts := strings.SplitN(remote[idx+len("github.com/"):], "/", 2)
		owner, repo = parts[0], parts[1]
	} else if idx := strings.Index(remote, "github.com:"); idx >= 0 {
		parts := strings.SplitN(remote[idx+len("github.com:"):], "/", 2)
		owner, repo = parts[0], parts[1]
	}
	fmt.Printf("Repo: %s/%s\n", owner, repo)

	me, _, err := gh.Users.Get(ctx, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get user: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Authenticated as: %s\n\n", me.GetLogin())

	// 1. Create a test branch and push it
	testBranch := "test/github-api-dummy-pr"
	fmt.Println("Step 1: Creating test branch...")
	masterRef, _, err := gh.Git.GetRef(ctx, owner, repo, "refs/heads/master")
	if err != nil {
		fmt.Fprintf(os.Stderr, "get master ref: %v\n", err)
		os.Exit(1)
	}
	sha := masterRef.Object.GetSHA()
	ref := "refs/heads/" + testBranch
	_, _, err = gh.Git.CreateRef(ctx, owner, repo, &github.Reference{
		Ref: &ref,
		Object: &github.GitObject{SHA: &sha},
	})
	if err != nil && !strings.Contains(err.Error(), "already exists") {
		fmt.Fprintf(os.Stderr, "create ref: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Branch %q created at %s\n", testBranch, sha[:8])

	// 2. Create a dummy commit on that branch (update a file)
	fmt.Println("Step 2: Creating dummy commit...")
	content := []byte("# dummy\nThis file was created by the GitHub API test.\n")
	path := "test_dummy_file.md"
	fileOpts := &github.RepositoryContentFileOptions{
		Message: github.String("test: dummy commit for PR API test"),
		Content: content,
		Branch:  github.String(testBranch),
	}
	// Try create; if exists, get SHA and update
	_, resp, err := gh.Repositories.CreateFile(ctx, owner, repo, path, fileOpts)
	if err != nil && resp != nil && resp.StatusCode == 422 {
		// File exists on branch, get its SHA
		fc, _, _, gerr := gh.Repositories.GetContents(ctx, owner, repo, path, &github.RepositoryContentGetOptions{Ref: testBranch})
		if gerr == nil {
			fileOpts.SHA = github.String(fc.GetSHA())
			_, _, err = gh.Repositories.UpdateFile(ctx, owner, repo, path, fileOpts)
		}
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "create/update file: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  Dummy commit pushed.")

	// 3. Create PR
	fmt.Println("Step 3: Creating dummy PR...")
	base := "master"
	prTitle := "test: dummy PR for GitHub API validation"
	prBody := "This PR was created by the opencode-review GitHub API test. Safe to close."
	pr, _, err := gh.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: &prTitle,
		Body:  &prBody,
		Head:  &testBranch,
		Base:  &base,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create PR: %v\n", err)
		os.Exit(1)
	}
	prNum := pr.GetNumber()
	fmt.Printf("  Created PR #%d: %s\n", prNum, pr.GetHTMLURL())

	// 4. Add a comment to the PR
	fmt.Println("Step 4: Adding comment to PR...")
	commentBody := "Test comment from opencode-review GitHub API integration test."
	comment, _, err := gh.Issues.CreateComment(ctx, owner, repo, prNum, &github.IssueComment{
		Body: &commentBody,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create comment: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Comment #%d added.\n", comment.GetID())

	// 5. Post a PR review comment
	fmt.Println("Step 5: Posting PR review...")
	reviewEvent := "COMMENT"
	reviewBody := "Test review from opencode-review GitHub API integration test."
	review, _, err := gh.PullRequests.CreateReview(ctx, owner, repo, prNum, &github.PullRequestReviewRequest{
		Body:  &reviewBody,
		Event: &reviewEvent,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "create review: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Review #%d posted.\n", review.GetID())

	// 6. Close the PR (do NOT merge)
	fmt.Println("Step 6: Closing PR (not merging)...")
	state := "closed"
	_, _, err = gh.PullRequests.Edit(ctx, owner, repo, prNum, &github.PullRequest{State: &state})
	if err != nil {
		fmt.Fprintf(os.Stderr, "close PR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  PR #%d closed.\n", prNum)

	// 7. Delete test branch
	fmt.Println("Step 7: Deleting test branch...")
	_, err = gh.Git.DeleteRef(ctx, owner, repo, "refs/heads/"+testBranch)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delete branch: %v\n", err)
	} else {
		fmt.Printf("  Branch %q deleted.\n", testBranch)
	}

	fmt.Println("\nAll GitHub API steps passed.")
}
