package agentops

import (
	"os/exec"
	"strings"
	"testing"
)

// initRepoOnBranchForAssert creates a temp git repo whose checked-out branch is
// `branch`, with one commit so HEAD resolves. Returns the worktree dir.
func initRepoOnBranchForAssert(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	run("init")
	// Hermetic: the developer's global init.defaultBranch must not decide
	// which branch the test repo treats as its default.
	run("config", "init.defaultBranch", "main")
	run("symbolic-ref", "HEAD", "refs/heads/"+branch)
	run("commit", "--allow-empty", "-m", "initial")
	return dir
}

// detachHEAD puts the repo into a detached-HEAD state (HEAD points at a commit,
// not a branch ref) — symbolic-ref then returns empty, which is NOT the incident.
func detachHEAD(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "checkout", "--detach")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout --detach: %s: %v", out, err)
	}
}

// TestAssertNotOnMain_DefaultBranch covers a repo whose shared branch is not
// named "main": its default branch (origin/HEAD, else init.defaultBranch) is
// protected too, and "main" stays protected.
func TestAssertNotOnMain_DefaultBranch(t *testing.T) {
	setDefault := func(dir, how, branch string) {
		t.Helper()
		var args []string
		if how == "origin" {
			args = []string{"symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/" + branch}
		} else {
			args = []string{"config", "init.defaultBranch", branch}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s: %v", args, out, err)
		}
	}
	forkDefault := initRepoOnBranchForAssert(t, "trunk")
	setDefault(forkDefault, "origin", "trunk")
	configDefault := initRepoOnBranchForAssert(t, "master")
	setDefault(configDefault, "config", "master")
	mainStill := initRepoOnBranchForAssert(t, "main")
	setDefault(mainStill, "origin", "trunk")
	feature := initRepoOnBranchForAssert(t, "feature")
	setDefault(feature, "origin", "trunk")

	cases := []struct {
		name     string
		worktree string
		wantErr  string
	}{
		{name: "origin/HEAD default branch => error", worktree: forkDefault, wantErr: "trunk"},
		{name: "init.defaultBranch default branch => error", worktree: configDefault, wantErr: "master"},
		{name: "main still protected when default differs", worktree: mainStill, wantErr: "main"},
		{name: "feature branch => ok", worktree: feature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AssertNotOnMain(tc.worktree, "engineer")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected no error, got: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "'"+tc.wantErr+"'") {
				t.Fatalf("expected error naming %q, got: %v", tc.wantErr, err)
			}
			if err2 := AssertNotOnMain(tc.worktree, "weave"); err2 != nil {
				t.Errorf("weave must stay exempt, got: %v", err2)
			}
		})
	}
}

func TestAssertNotOnMain(t *testing.T) {
	mainRepo := initRepoOnBranchForAssert(t, "main")
	featureRepo := initRepoOnBranchForAssert(t, "dmotles/qum-837-foo")
	detachedRepo := initRepoOnBranchForAssert(t, "main")
	detachHEAD(t, detachedRepo)

	cases := []struct {
		name     string
		worktree string
		identity string
		wantErr  bool
	}{
		{name: "main + non-root agent => error", worktree: mainRepo, identity: "engineer", wantErr: true},
		{name: "main + weave => exempt", worktree: mainRepo, identity: "weave", wantErr: false},
		{name: "main + empty identity (human) => exempt", worktree: mainRepo, identity: "", wantErr: false},
		{name: "feature branch + non-root agent => ok", worktree: featureRepo, identity: "engineer", wantErr: false},
		{name: "detached HEAD + non-root agent => ok (not the incident)", worktree: detachedRepo, identity: "engineer", wantErr: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AssertNotOnMain(tc.worktree, tc.identity)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for identity %q on %s, got nil", tc.identity, tc.worktree)
				}
				if !strings.Contains(err.Error(), "main") {
					t.Errorf("expected error to mention 'main'; got: %v", err)
				}
			} else if err != nil {
				t.Fatalf("expected no error for identity %q on %s, got: %v", tc.identity, tc.worktree, err)
			}
		})
	}
}
