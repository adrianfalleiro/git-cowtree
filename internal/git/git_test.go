package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/adrianfalleiro/git-cowtree/internal/pathutil"
)

func TestBlobIDMatchesGit(t *testing.T) {
	for _, content := range []string{"", "hello\n", strings.Repeat("x", 10000)} {
		path := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(Bin, "hash-object", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		if got, want := BlobID([]byte(content), "sha1"), strings.TrimSpace(string(out)); got != want {
			t.Errorf("BlobID(%d bytes) = %s, git hash-object says %s", len(content), got, want)
		}
	}
}

func TestLinkedWorktrees(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root, err := pathutil.Canon(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", repo, "worktree", "add", "-q", "--detach", filepath.Join(root, "wt")},
	} {
		if out, err := exec.Command(Bin, args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	wts := LinkedWorktrees(filepath.Join(repo, ".git"))
	if len(wts) != 1 || wts["wt"] != filepath.Join(root, "wt") {
		t.Errorf("LinkedWorktrees = %v, want map[wt:%s]", wts, filepath.Join(root, "wt"))
	}
}
