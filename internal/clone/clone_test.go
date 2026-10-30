package clone

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTree(t *testing.T) {
	dir := t.TempDir()
	if !Possible(dir, dir) {
		t.Skip("this filesystem can't clone")
	}
	src := filepath.Join(dir, "src")
	mtime := time.Date(2020, 1, 2, 3, 4, 5, 123456789, time.UTC)
	for _, f := range []string{"a.txt", "sub/b.txt", "sub/deeper/c.txt"} {
		p := filepath.Join(src, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "dst")
	if err := Tree(src, dst); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.txt", "sub/b.txt", "sub/deeper/c.txt"} {
		p := filepath.Join(dst, f)
		data, err := os.ReadFile(p)
		if err != nil || string(data) != f {
			t.Errorf("%s = %q, %v", f, data, err)
			continue
		}
		info, _ := os.Stat(p)
		if !info.ModTime().Equal(mtime) {
			t.Errorf("%s mtime = %v, want %v (git's index relies on it)", f, info.ModTime(), mtime)
		}
		if info.Mode().Perm() != 0o640 {
			t.Errorf("%s mode = %v, want 0640", f, info.Mode().Perm())
		}
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "a.txt" {
		t.Errorf("symlink = %q, %v; want it cloned as a link to a.txt", target, err)
	}

	// The clone is independent: writing to it leaves the source alone.
	if err := os.WriteFile(filepath.Join(dst, "a.txt"), []byte("changed"), 0o640); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(src, "a.txt")); string(data) != "a.txt" {
		t.Errorf("source changed to %q after writing the clone", data)
	}
}
