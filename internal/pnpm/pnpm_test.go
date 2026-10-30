package pnpm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/adrianfalleiro/git-cowtree/internal/pathutil"
)

func tempRoot(t *testing.T) string {
	t.Helper()
	root, err := pathutil.Canon(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestReplacePathInFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shim")
	writeFile(t, path, `NODE_PATH="/src/app/node_modules:/src/app2/node_modules:/src/app"`+"\n/src/app\n")
	replacePathInFile(path, "/src/app", "/dst/wt")
	want := `NODE_PATH="/dst/wt/node_modules:/src/app2/node_modules:/dst/wt"` + "\n/dst/wt\n"
	if got := readFile(t, path); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("mode not kept: %v %v", info.Mode(), err)
	}
}

func TestRelinkDir(t *testing.T) {
	root := tempRoot(t)
	src := filepath.Join(root, "repo")
	dst := filepath.Join(root, "repo", "nested", "worktrees", "deep") // three levels deeper
	store := filepath.Join(root, "store", "pkg")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	nm := filepath.Join(dst, "node_modules")
	if err := os.MkdirAll(filepath.Join(nm, "@scope"), 0o755); err != nil {
		t.Fatal(err)
	}
	// As pnpm wrote them in the source's node_modules, then cloned verbatim.
	links := map[string]string{
		"outside":       "../../store/pkg",                       // leaves the project
		"@scope/pkg":    "../../../store/pkg",                    // leaves it, in a scope dir
		"inside":        ".pnpm/pkg/node_modules/pkg",            // stays inside: untouched
		"absolute-src":  filepath.Join(src, "packages", "local"), // absolute into source
		"absolute-else": store,                                   // absolute elsewhere: untouched
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(nm, name)); err != nil {
			t.Fatal(err)
		}
	}

	relinkDir(nm, src, dst)

	for name, want := range map[string]string{
		"outside":       "../../../../../store/pkg",
		"@scope/pkg":    "../../../../../../store/pkg",
		"inside":        ".pnpm/pkg/node_modules/pkg",
		"absolute-src":  filepath.Join(dst, "packages", "local"),
		"absolute-else": store,
	} {
		got, err := os.Readlink(filepath.Join(nm, name))
		if err != nil || got != want {
			t.Errorf("%s -> %q (%v), want %q", name, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(nm, "outside")); err != nil {
		t.Errorf("relinked symlink does not resolve: %v", err)
	}
}

func TestFixVirtualStoreDir(t *testing.T) {
	root := tempRoot(t)
	src, dst := filepath.Join(root, "repo"), filepath.Join(root, "repo", "nested", "worktrees", "x")
	for _, tc := range []struct{ in, want string }{
		{`{"virtualStoreDir": "../../store/links",}`, `{"virtualStoreDir": "../../../../../store/links",}`},
		{`{"virtualStoreDir": ".pnpm",}`, `{"virtualStoreDir": ".pnpm",}`},
		{"virtualStoreDir: ../../store/links\n", "virtualStoreDir: ../../../../../store/links\n"},
	} {
		path := filepath.Join(dst, "node_modules", ".modules.yaml")
		writeFile(t, path, tc.in)
		fixVirtualStoreDir(path, filepath.Join(src, "node_modules"), filepath.Join(dst, "node_modules"), src)
		if got := readFile(t, path); got != tc.want {
			t.Errorf("%q -> %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestProjects(t *testing.T) {
	top := tempRoot(t)
	writeFile(t, filepath.Join(top, "node_modules", ".pnpm-workspace-state-v1.json"),
		`{"projects": {"`+top+`": {}, "`+filepath.Join(top, "packages", "app")+`": {}, "/elsewhere": {}}}`)
	got := Projects(top)
	if len(got) != 2 || got[0] != "." || got[1] != filepath.Join("packages", "app") {
		t.Errorf("Projects = %v, want [. packages/app]", got)
	}
}

func TestSyncInstallSkipsWhenLockfileMatches(t *testing.T) {
	dst := t.TempDir()
	writeFile(t, filepath.Join(dst, "pnpm-lock.yaml"), "lockfileVersion: 9\n")
	writeFile(t, filepath.Join(dst, "node_modules", ".modules.yaml"), "{}")
	writeFile(t, filepath.Join(dst, "node_modules", ".pnpm", "lock.yaml"), "lockfileVersion: 9\n")
	// A command that fails proves it never ran.
	if err := SyncInstall(dst, "auto", "exit 1", false); err != nil {
		t.Errorf("SyncInstall ran an install for a matching lockfile: %v", err)
	}
	if err := SyncInstall(dst, "never", "exit 1", true); err != nil {
		t.Errorf("SyncInstall ran an install with mode never: %v", err)
	}
	if err := SyncInstall(dst, "auto", "exit 1", true); err == nil {
		t.Error("SyncInstall skipped a requested fresh install")
	}
}
