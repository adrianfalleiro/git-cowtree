package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianfalleiro/git-cowtree/internal/git"
	"github.com/adrianfalleiro/git-cowtree/internal/pathutil"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command(git.Bin, append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func hook(t *testing.T, cmd, input string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := claudeHook([]string{cmd}, strings.NewReader(input), &out)
	return strings.TrimSpace(out.String()), err
}

func TestClaudeHook(t *testing.T) {
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		t.Setenv(k, v)
	}
	root, err := pathutil.Canon(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	gitT(t, root, "init", "-q", "-b", "main", repo)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-q", "-m", "base")
	t.Chdir(root)

	create := func(name string) string {
		t.Helper()
		path, err := hook(t, "create", fmt.Sprintf(`{"hook_event_name":"WorktreeCreate","cwd":%q,"name":%q}`, repo, name))
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(repo, ".claude", "worktrees", name); path != want {
			t.Fatalf("created %q, want %q", path, want)
		}
		if b := gitT(t, path, "branch", "--show-current"); b != "worktree-"+name {
			t.Errorf("on branch %q, want worktree-%s", b, name)
		}
		return path
	}
	remove := func(path string) {
		t.Helper()
		if _, err := hook(t, "remove", fmt.Sprintf(`{"hook_event_name":"WorktreeRemove","worktree_path":%q}`, path)); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists", path)
		}
	}
	hasBranch := func(b string) bool {
		return exec.Command(git.Bin, "-C", repo, "rev-parse", "-q", "--verify", "refs/heads/"+b).Run() == nil
	}

	// A branch with no new commits goes with its worktree.
	remove(create("calm-fox"))
	if hasBranch("worktree-calm-fox") {
		t.Error("unused branch worktree-calm-fox was kept")
	}

	// One with commits stays, and the same name picks it up again.
	path := create("bold-oak")
	if err := os.WriteFile(filepath.Join(path, "a.txt"), []byte("agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, path, "commit", "-q", "-am", "agent work")
	remove(path)
	if !hasBranch("worktree-bold-oak") {
		t.Fatal("branch with unmerged commits was deleted")
	}
	path = create("bold-oak")
	if data, _ := os.ReadFile(filepath.Join(path, "a.txt")); string(data) != "agent\n" {
		t.Errorf("reused branch's a.txt = %q", data)
	}

	if _, err := hook(t, "create", fmt.Sprintf(`{"cwd":%q,"name":"../escape"}`, repo)); err == nil {
		t.Error("a name with .. was accepted")
	}
}
