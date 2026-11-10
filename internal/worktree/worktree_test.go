package worktree

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	for _, target := range []string{"main", "target"} {
		t.Run(target, func(t *testing.T) {
			got := filepath.Join(root, "cow-"+target)
			want := filepath.Join(root, "plain-"+target)
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
		})
	}
}
