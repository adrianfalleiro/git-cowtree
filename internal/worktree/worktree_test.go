package worktree

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrianfalleiro/git-cowtree/internal/clone"
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

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// snapshot maps every file under dir (except .git and skip) to its content,
// and every symlink to its target.
func snapshot(t *testing.T, dir string, skip ...string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == ".git" || (d.IsDir() && contains(skip, rel)) {
			return fs.SkipDir
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			files[rel] = "-> " + target
		case d.Type().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			files[rel] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestAddMatchesPlainCheckout dirties a source worktree every way we can, then
// checks that the copy-on-write worktree for each target is byte-identical to
// a plain `git worktree add` of the same commit, that git sees it as clean
// straight away, and that gitignored files came along.
func TestAddMatchesPlainCheckout(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	root, err := pathutil.Canon(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "src")

	gitT(t, root, "init", "-q", "-b", "main", src)
	for _, f := range []string{"a", "b", "c", "d", "e"} {
		write(t, filepath.Join(src, "dir", f+".txt"), f+" v1\n")
	}
	write(t, filepath.Join(src, "dir", "[x]*.txt"), "glob v1\n") // glob characters in the name
	write(t, filepath.Join(src, "local.cnf"), "local v1\n")
	write(t, filepath.Join(src, "vendor", "lib", "keep.js"), "vendored\n")
	write(t, filepath.Join(src, "top.txt"), "top\n")
	write(t, filepath.Join(src, ".gitignore"), "ignored/\nworktrees/\n")
	if err := os.Symlink("top.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}
	gitT(t, src, "add", "-A")
	gitT(t, src, "commit", "-q", "-m", "base")
	gitT(t, src, "checkout", "-q", "-b", "target")
	gitT(t, src, "mv", "dir/a.txt", "dir/a-renamed.txt")
	gitT(t, src, "rm", "-q", "dir/b.txt")
	write(t, filepath.Join(src, "dir", "c.txt"), "c target\n")
	write(t, filepath.Join(src, "dir", "n.txt"), "new in target\n")
	write(t, filepath.Join(src, "local.cnf"), "local target\n")
	gitT(t, src, "add", "-A")
	gitT(t, src, "commit", "-q", "-m", "target")
	gitT(t, src, "checkout", "-q", "main")
	gitT(t, src, "config", "cowtree.exclude", "vendor") // excluded, but holds a tracked file

	// Dirty the source every way we can.
	write(t, filepath.Join(src, "dir", "c.txt"), "c staged\n")
	gitT(t, src, "add", "dir/c.txt")
	write(t, filepath.Join(src, "dir", "staged-new.txt"), "brand new\n")
	gitT(t, src, "add", "dir/staged-new.txt")
	gitT(t, src, "rm", "-q", "--cached", "dir/d.txt")
	gitT(t, src, "rm", "-q", "dir/b.txt")
	gitT(t, src, "update-index", "--skip-worktree", "local.cnf")
	write(t, filepath.Join(src, "local.cnf"), "local edit git ignores\n")
	gitT(t, src, "update-index", "--assume-unchanged", "top.txt")
	os.Remove(filepath.Join(src, "dir", "e.txt"))
	write(t, filepath.Join(src, "dir", "[x]*.txt"), "glob dirty\n")
	write(t, filepath.Join(src, "top.txt"), "top dirty\n")
	write(t, filepath.Join(src, "untracked.txt"), "untracked\n")
	write(t, filepath.Join(src, "ignored", "file"), "ignored\n")
	// Another worktree nested inside the source.
	gitT(t, src, "worktree", "add", "-q", "--detach", filepath.Join(src, "worktrees", "other"), "main")

	t.Chdir(src)
	cow := clone.Possible(src, root)
	// "full-reset" forces the fallback taken when too many paths differ to
	// restore one by one.
	for _, mode := range []string{"targeted", "full-reset"} {
		for _, target := range []string{"main", "target"} {
			t.Run(mode+"/"+target, func(t *testing.T) {
				if mode == "full-reset" {
					defer func(n int) { maxTrackedFix = n }(maxTrackedFix)
					maxTrackedFix = -1
				}
				testAddMatchesPlainCheckout(t, src, root, mode, target, cow)
			})
		}
	}
}

func testAddMatchesPlainCheckout(t *testing.T, src, root, mode, target string, cow bool) {
	got := filepath.Join(root, "cow-"+mode+"-"+target)
	want := filepath.Join(root, "plain-"+mode+"-"+target)
	path, err := Add([]string{"-q", "--detach", got, target}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if path != got {
		t.Errorf("Add returned %q, want %q", path, got)
	}
	gitT(t, src, "worktree", "add", "-q", "--detach", want, target)

	// Status straight away, without letting git refresh the index first.
	if dirty := gitT(t, got, "diff-files", "--name-only"); dirty != "" {
		t.Errorf("index not trusted for: %s", dirty)
	}
	for _, e := range strings.Split(gitT(t, got, "ls-files", "-v"), "\n") {
		if !strings.HasPrefix(e, "H ") {
			t.Errorf("index entry kept a flag from the source: %s", e)
		}
	}
	if status := gitT(t, got, "status", "--porcelain"); status != "" {
		t.Errorf("git status not clean:\n%s", status)
	}

	gotFiles, wantFiles := snapshot(t, got, "ignored"), snapshot(t, want)
	for p, w := range wantFiles {
		if g, ok := gotFiles[p]; !ok {
			t.Errorf("missing %s", p)
		} else if g != w {
			t.Errorf("%s = %q, want %q", p, g, w)
		}
	}
	for p := range gotFiles {
		if _, ok := wantFiles[p]; !ok {
			t.Errorf("extra file %s", p)
		}
	}

	ignored, err := os.ReadFile(filepath.Join(got, "ignored", "file"))
	if cow && !bytes.Equal(ignored, []byte("ignored\n")) {
		t.Errorf("gitignored file not cloned: %q, %v", ignored, err)
	}
	if !cow && err == nil {
		t.Errorf("fallback checkout should not copy gitignored files")
	}
}

// register fakes `git worktree add` registering worktree id at path.
func register(t *testing.T, commonDir, id, path string) {
	t.Helper()
	write(t, filepath.Join(commonDir, "worktrees", id, "gitdir"), filepath.Join(path, ".git")+"\n")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestNewWorktreePicksRequestedPathAmongConcurrentAdds(t *testing.T) {
	root, err := pathutil.Canon(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(root, ".git")
	register(t, common, "main", filepath.Join(root, "main"))
	before := adminDirs(common)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("wt%d", i)
		register(t, common, name, filepath.Join(root, name))
	}
	t.Chdir(root)
	for _, want := range []string{"wt3", "wt17"} {
		// "main" is also an existing worktree's path, but not a new one.
		id, dst := newWorktree(common, before, []string{"-b", "branch-" + want, want, "main"})
		if id != want || dst != filepath.Join(root, want) {
			t.Errorf("args for %s picked %s at %s", want, id, dst)
		}
	}
}

// TestNewWorktreeReusedID covers `git worktree add -f` over a registered but
// missing worktree, which deletes its admin dir and registers it again under
// the same id.
func TestNewWorktreeReusedID(t *testing.T) {
	root, err := pathutil.Canon(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(root, ".git")
	path := filepath.Join(root, "w")
	register(t, common, "w", path)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	before := adminDirs(common)
	if err := os.RemoveAll(filepath.Join(common, "worktrees", "w")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // a distinct mtime, even if the inode is reused
	register(t, common, "w", path)
	t.Chdir(root)
	if id, dst := newWorktree(common, before, []string{"-f", "w"}); id != "w" || dst != path {
		t.Errorf("picked %q at %q, want the re-registered w", id, dst)
	}
	if id, _ := newWorktree(common, adminDirs(common), []string{"-f", "w"}); id != "" {
		t.Errorf("picked %q when nothing changed", id)
	}
}

// TestRemoveLeftovers covers the last-resort path, where read-tree has already
// replaced the cloned index: files it no longer tracks must go, along with
// directories that leaves empty, while tracked and ignored files stay.
func TestRemoveLeftovers(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir, err := pathutil.Canon(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "keep", "tracked.txt"), "tracked\n")
	gitT(t, dir, "add", "-A")
	write(t, filepath.Join(dir, "keep", "gone.txt"), "leftover\n")
	write(t, filepath.Join(dir, "only", "deep", "gone.txt"), "leftover\n")
	write(t, filepath.Join(dir, "keep", "ignored.log"), "ignored\n")

	if err := removeLeftovers(dir, []string{"keep/tracked.txt", "keep/gone.txt", "only/deep/gone.txt", "missing.txt"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"keep/gone.txt", "only"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists (%v)", p, err)
		}
	}
	for _, p := range []string{"keep/tracked.txt", "keep/ignored.log"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

// newRepo makes a repository with one commit on main, in a fresh directory.
func newRepo(t *testing.T) (root, src string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	root, err := pathutil.Canon(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src = filepath.Join(root, "src")
	gitT(t, root, "init", "-q", "-b", "main", src)
	write(t, filepath.Join(src, "a.txt"), "a\n")
	write(t, filepath.Join(src, "dir", "b.txt"), "b\n")
	gitT(t, src, "add", "-A")
	gitT(t, src, "commit", "-q", "-m", "base")
	t.Chdir(src)
	return root, src
}

// checkClean fails unless the worktree at dir has exactly the files of commit,
// and git sees no changes.
func checkClean(t *testing.T, dir, commit string) {
	t.Helper()
	if status := gitT(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("git status not clean:\n%s", status)
	}
	if head, want := gitT(t, dir, "rev-parse", "HEAD"), gitT(t, dir, "rev-parse", commit); head != want {
		t.Errorf("HEAD = %s, want %s", head, want)
	}
}

func TestAddFromConflictedSource(t *testing.T) {
	root, src := newRepo(t)
	gitT(t, src, "checkout", "-q", "-b", "other")
	write(t, filepath.Join(src, "a.txt"), "other\n")
	gitT(t, src, "commit", "-q", "-am", "other")
	gitT(t, src, "checkout", "-q", "main")
	write(t, filepath.Join(src, "a.txt"), "main\n")
	gitT(t, src, "commit", "-q", "-am", "main")
	if exec.Command(git.Bin, "-C", src, "merge", "-q", "other").Run() == nil {
		t.Fatal("merge should conflict")
	}
	for _, target := range []string{"main", "other"} {
		dst := filepath.Join(root, "wt-"+target)
		if _, err := Add([]string{"-q", "--detach", dst, target}, Options{}); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		checkClean(t, dst, target)
		if data, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(data) != target+"\n" {
			t.Errorf("%s: a.txt = %q", target, data)
		}
	}
}

func TestAddFromSplitIndex(t *testing.T) {
	root, src := newRepo(t)
	gitT(t, src, "config", "core.splitIndex", "true")
	gitT(t, src, "update-index", "--split-index")
	write(t, filepath.Join(src, "a.txt"), "staged\n")
	gitT(t, src, "add", "a.txt")
	dst := filepath.Join(root, "wt")
	if _, err := Add([]string{"-q", "--detach", dst, "main"}, Options{}); err != nil {
		t.Fatal(err)
	}
	checkClean(t, dst, "main")
}

func TestAddReusingMissingWorktree(t *testing.T) {
	root, _ := newRepo(t)
	dst := filepath.Join(root, "wt")
	if _, err := Add([]string{"-q", "--detach", dst, "main"}, Options{}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dst); err != nil {
		t.Fatal(err)
	}
	if _, err := Add([]string{"-q", "-f", "--detach", dst, "main"}, Options{}); err != nil {
		t.Fatal(err)
	}
	checkClean(t, dst, "main")
}

func TestAddLeftToPlainGit(t *testing.T) {
	root, _ := newRepo(t)
	orphan := filepath.Join(root, "orphan")
	if _, err := Add([]string{"-q", "--orphan", "-b", "fresh", orphan}, Options{}); err != nil {
		t.Fatal(err)
	}
	if out := gitT(t, orphan, "ls-files"); out != "" {
		t.Errorf("orphan worktree tracks files: %s", out)
	}
	bare := filepath.Join(root, "no-checkout")
	if _, err := Add([]string{"-q", "--no-checkout", "--detach", bare, "main"}, Options{}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(bare); len(entries) != 1 || entries[0].Name() != ".git" {
		t.Errorf("--no-checkout worktree has files: %v", entries)
	}
}

func TestAddRejectsSourceFromAnotherRepo(t *testing.T) {
	root, src := newRepo(t)
	other := filepath.Join(root, "other")
	gitT(t, root, "init", "-q", "-b", "main", other)
	gitT(t, other, "commit", "-q", "--allow-empty", "-m", "o")
	t.Chdir(other)
	dst := filepath.Join(root, "wt")
	if _, err := Add([]string{"-q", "--detach", dst, "main"}, Options{Source: src}); err == nil {
		t.Fatal("Add from another repository's worktree succeeded")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Errorf("a worktree was created at %s", dst)
	}
}
