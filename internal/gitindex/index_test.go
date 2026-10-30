package gitindex

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adrianfalleiro/git-cowtree/internal/git"
)

func TestReadOffsetVarint(t *testing.T) {
	for _, tc := range []struct {
		in        []byte
		want      uint64
		wantBytes int
	}{
		{[]byte{0x00}, 0, 1},
		{[]byte{0x7f}, 127, 1},
		{[]byte{0x80, 0x00}, 128, 2},
		{[]byte{0xff, 0x7f}, 16511, 2},
		{[]byte{0x80}, 0, 0}, // truncated
		{nil, 0, 0},
	} {
		got, n := readOffsetVarint(tc.in)
		if got != tc.want || n != tc.wantBytes {
			t.Errorf("readOffsetVarint(%x) = %d, %d; want %d, %d", tc.in, got, n, tc.want, tc.wantBytes)
		}
	}
}

// replaceInode swaps path for a copy with the same content, mode and mtime but
// a new inode, which is what a copy-on-write clone looks like to git.
func replaceInode(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, info.Mode().Perm()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// statDirty lists the paths whose cached stat data no longer matches the file,
// without letting git refresh the index.
func statDirty(t *testing.T, repo string) []string {
	t.Helper()
	out, err := exec.Command(git.Bin, "-C", repo, "diff-files", "--name-only").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(out))
}

func TestRefreshStat(t *testing.T) {
	if _, ok := lstat("."); !ok {
		t.Skip("no lstat on this platform")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, version := range []string{"2", "3", "4"} {
		t.Run("v"+version, func(t *testing.T) {
			repo := t.TempDir()
			files := []string{"a.txt", "dir/b.txt", "dir/sub/c.txt", "edited.txt"}
			for _, f := range files {
				p := filepath.Join(repo, f)
				os.MkdirAll(filepath.Dir(p), 0o755)
				if err := os.WriteFile(p, []byte(f+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{
				{"init", "-q"},
				{"add", "."},
				{"update-index", "--index-version", version},
			} {
				if out, err := exec.Command(git.Bin, append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}

			// Give every file a new inode with the same content and mtime, as a
			// clone has, and really edit one file.
			for _, f := range files {
				replaceInode(t, filepath.Join(repo, f))
			}
			time.Sleep(10 * time.Millisecond)
			if err := os.WriteFile(filepath.Join(repo, "edited.txt"), []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if got := statDirty(t, repo); len(got) != len(files) {
				t.Fatalf("before refresh, stat-dirty = %v; want all of %v", got, files)
			}

			n, err := RefreshStat(repo, filepath.Join(repo, ".git", "index"), "sha1", time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if n != len(files)-1 {
				t.Errorf("RefreshStat updated %d entries, want %d", n, len(files)-1)
			}
			if got := statDirty(t, repo); !slices.Equal(got, []string{"edited.txt"}) {
				t.Errorf("after refresh, stat-dirty = %v; want only the edited file", got)
			}
			// git must still accept the rewritten index (its checksum included).
			if out, err := exec.Command(git.Bin, "-C", repo, "status", "--porcelain").CombinedOutput(); err != nil ||
				strings.TrimSpace(string(out)) != "A  a.txt\nA  dir/b.txt\nA  dir/sub/c.txt\nAM edited.txt" {
				t.Errorf("git status after refresh: %v\n%s", err, out)
			}
		})
	}
}

func TestRefreshStatLeavesUnverifiedEntries(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}} {
		if out, err := exec.Command(git.Bin, append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// A cutoff before the file was written: the source never verified it.
	n, err := RefreshStat(repo, filepath.Join(repo, ".git", "index"), "sha1", time.Now().Add(-time.Hour))
	if err != nil || n != 0 {
		t.Errorf("RefreshStat = %d, %v; want 0 updates", n, err)
	}
}
