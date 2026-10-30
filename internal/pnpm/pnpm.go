// Package pnpm makes a node_modules cloned from another worktree work in its
// new location, and keeps node_modules in step with the checked-out lockfile.
package pnpm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/adrianfalleiro/git-cowtree/internal/git"
	"github.com/adrianfalleiro/git-cowtree/internal/parallel"
	"github.com/adrianfalleiro/git-cowtree/internal/pathutil"
	"github.com/adrianfalleiro/git-cowtree/internal/report"
)

// Plan says, before cloning, whether the source's node_modules is worth
// cloning. One installed from a different lockfile than the one being checked
// out is slower to update in place than to install fresh from the store
// (17.7s vs 3.8s measured on a 2,400-package repo), so then it's skipped and
// installed fresh instead.
type Plan struct {
	FreshInstall bool     // run a fresh install after cloning
	Skip         []string // repo-relative node_modules dirs not to clone
}

// PlanClone decides the Plan for cloning srcTop into the worktree dst, whose
// HEAD is already the target commit. mode is the cowtree.install setting
// (auto, always or never); objectFormat is the repository's (sha1 or sha256).
func PlanClone(srcTop, dst, mode, objectFormat string) Plan {
	if mode == "never" {
		return Plan{}
	}
	if _, err := os.Stat(filepath.Join(srcTop, "node_modules", ".modules.yaml")); err != nil {
		return Plan{}
	}
	want, err := git.Output(dst, "rev-parse", "-q", "--verify", "HEAD:pnpm-lock.yaml")
	if err != nil {
		return Plan{} // target has no lockfile
	}
	if have, err := os.ReadFile(filepath.Join(srcTop, "node_modules", ".pnpm", "lock.yaml")); err == nil &&
		git.BlobID(have, objectFormat) == want {
		return Plan{} // installed from this exact lockfile: clone it
	}
	plan := Plan{FreshInstall: true}
	for _, rel := range Projects(srcTop) {
		plan.Skip = append(plan.Skip, filepath.Join(rel, "node_modules"))
	}
	return plan
}

// Projects lists the workspace projects of the pnpm project at top
// (repo-relative, "." first), from pnpm's own record of them in
// node_modules/.pnpm-workspace-state-v1.json.
func Projects(top string) []string {
	projects := []string{"."}
	data, err := os.ReadFile(filepath.Join(top, "node_modules", ".pnpm-workspace-state-v1.json"))
	if err != nil {
		return projects
	}
	var state struct {
		Projects map[string]json.RawMessage `json:"projects"`
	}
	if json.Unmarshal(data, &state) == nil {
		for p := range state.Projects {
			if rel, err := filepath.Rel(top, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
				projects = append(projects, rel)
			}
		}
	}
	return projects
}

// Relocate repairs what pnpm bakes into node_modules about where the project
// lives, after node_modules was cloned from src to dst:
//
//   - symlinks whose relative target leaves the project (pnpm's global virtual
//     store links into ~/Library/pnpm/store via ../../..) break when the
//     worktree sits at a different depth; they are re-pointed at the same target
//   - .bin shims hard-code the project path in NODE_PATH, including the ones
//     inside a classic per-project virtual store
//   - .pnpm-workspace-state-v1.json lists workspace projects by absolute path
//   - .modules.yaml records the virtual store relative to node_modules
func Relocate(src, dst string) {
	if _, err := os.Stat(filepath.Join(dst, "node_modules", ".modules.yaml")); err != nil {
		return
	}
	defer report.Trace(time.Now(), "relocate pnpm node_modules")
	replacePathInFile(filepath.Join(dst, "node_modules", ".pnpm-workspace-state-v1.json"), src, dst)

	var binDirs []string
	for _, rel := range Projects(src) {
		srcNM, dstNM := filepath.Join(src, rel, "node_modules"), filepath.Join(dst, rel, "node_modules")
		for _, dir := range []string{dstNM, filepath.Join(dstNM, ".pnpm", "node_modules")} {
			relinkDir(dir, src, dst)
			binDirs = append(binDirs, filepath.Join(dir, ".bin"))
		}
		binDirs = append(binDirs, virtualStoreBinDirs(filepath.Join(dstNM, ".pnpm"))...)
		fixVirtualStoreDir(filepath.Join(dstNM, ".modules.yaml"), srcNM, dstNM, src)
		replacePathInFile(filepath.Join(dstNM, ".modules.yaml"), src, dst)
	}
	parallel.ForEach(binDirs, func(dir string) {
		bins, _ := os.ReadDir(dir)
		for _, b := range bins {
			if b.Type().IsRegular() {
				replacePathInFile(filepath.Join(dir, b.Name()), src, dst)
			}
		}
	})
}

// SyncInstall runs `pnpm install` in dst when its node_modules doesn't match
// the checked-out lockfile: fresh (PlanClone skipped cloning it), or as an
// in-place update of a cloned one. mode is the cowtree.install setting;
// command, if set, replaces the default install command (run with sh -c).
func SyncInstall(dst, mode, command string, fresh bool) error {
	if mode == "never" {
		return nil
	}
	want, err := os.ReadFile(filepath.Join(dst, "pnpm-lock.yaml"))
	if err != nil {
		return nil // not a pnpm project
	}
	if mode != "always" && !fresh {
		if _, err := os.Stat(filepath.Join(dst, "node_modules", ".modules.yaml")); err != nil {
			return nil // nothing was cloned to keep in sync
		}
		have, err := os.ReadFile(filepath.Join(dst, "node_modules", ".pnpm", "lock.yaml"))
		if err == nil && bytes.Equal(want, have) {
			return nil
		}
	}
	if command == "" {
		command = "pnpm install --frozen-lockfile --prefer-offline"
		// Keep a safe-chain setup (normally a shell function around pnpm) in the loop.
		if _, err := exec.LookPath("safe-chain"); err == nil {
			command = "safe-chain " + command
		}
	}
	if fresh {
		report.Logf("source node_modules was installed from a different lockfile; installing fresh: %s", command)
	} else {
		report.Logf("lockfile differs from installed node_modules; running: %s", command)
	}
	defer report.Trace(time.Now(), command)
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = dst
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w (the worktree is ready; node_modules may be stale)", command, err)
	}
	return nil
}

// virtualStoreBinDirs finds the .bin directories pnpm's classic (per-project)
// virtual store generates for each package's own dependencies, at
// .pnpm/<pkg@ver>/node_modules/<name>/node_modules/.bin.
func virtualStoreBinDirs(store string) []string {
	pkgs, err := os.ReadDir(store)
	if err != nil {
		return nil
	}
	var nms []string
	for _, p := range pkgs {
		if p.IsDir() && p.Name() != "node_modules" {
			nms = append(nms, filepath.Join(store, p.Name(), "node_modules"))
		}
	}
	var mu sync.Mutex
	var found []string
	parallel.ForEach(nms, func(nm string) {
		// The package's own directory is the only real one; its siblings are
		// symlinks to other packages, which get their own visit.
		names, _ := os.ReadDir(nm)
		for _, n := range names {
			if !n.IsDir() {
				continue
			}
			dirs := []string{filepath.Join(nm, n.Name())}
			if strings.HasPrefix(n.Name(), "@") {
				dirs = nil
				scoped, _ := os.ReadDir(filepath.Join(nm, n.Name()))
				for _, s := range scoped {
					if s.IsDir() {
						dirs = append(dirs, filepath.Join(nm, n.Name(), s.Name()))
					}
				}
			}
			for _, d := range dirs {
				bin := filepath.Join(d, "node_modules", ".bin")
				if _, err := os.Lstat(bin); err == nil {
					mu.Lock()
					found = append(found, bin)
					mu.Unlock()
				}
			}
		}
	})
	return found
}

// relinkDir re-points the symlinks directly in dir (and in @scope dirs) that
// leave the project, so they resolve to the same place from the new worktree.
func relinkDir(dir, src, dst string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if e.IsDir() && strings.HasPrefix(e.Name(), "@") {
			relinkDir(p, src, dst)
			continue
		}
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(p)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(dst, filepath.Dir(p))
		srcLinkDir := filepath.Join(src, rel)
		var want string
		if filepath.IsAbs(target) {
			if r, err := filepath.Rel(src, target); err == nil && !strings.HasPrefix(r, "..") {
				want = filepath.Join(dst, r) // absolute link into the source
			}
		} else if abs := filepath.Join(srcLinkDir, target); !pathutil.Within(abs, src) {
			want, _ = filepath.Rel(filepath.Dir(p), abs) // relative link leaving the project
		}
		if want == "" || want == target {
			continue
		}
		tmp := p + ".cowtree-tmp"
		if os.Symlink(want, tmp) == nil {
			if os.Rename(tmp, p) != nil {
				os.Remove(tmp)
			}
		}
	}
}

var virtualStoreDirRe = regexp.MustCompile(`("?virtualStoreDir"?:\s*"?)([^"\n]+)`)

// fixVirtualStoreDir recomputes a relative virtualStoreDir that leaves the project.
func fixVirtualStoreDir(path, srcNM, dstNM, src string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	m := virtualStoreDirRe.FindSubmatchIndex(data)
	if m == nil {
		return
	}
	old := string(data[m[4]:m[5]])
	if filepath.IsAbs(old) {
		return // absolute paths are handled by replacePathInFile
	}
	abs := filepath.Join(srcNM, old)
	if pathutil.Within(abs, src) {
		return
	}
	if want, err := filepath.Rel(dstNM, abs); err == nil && want != old {
		out := append(append(append([]byte{}, data[:m[4]]...), want...), data[m[5]:]...)
		if info, err := os.Stat(path); err == nil {
			writeWithMode(path, out, info.Mode().Perm())
		}
	}
}

// replacePathInFile rewrites occurrences of the source project path in a small
// text file. Only whole path components are replaced, so /x/app never matches
// inside /x/app2.
func replacePathInFile(path, src, dst string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		f.Close()
		return
	}
	data := make([]byte, info.Size())
	_, err = io.ReadFull(f, data)
	f.Close()
	if err != nil || !bytes.Contains(data, []byte(src)) {
		return
	}
	out := data
	for _, suffix := range []string{"/", `"`, ":", "'", "\n"} {
		out = bytes.ReplaceAll(out, []byte(src+suffix), []byte(dst+suffix))
	}
	if !bytes.Equal(out, data) {
		writeWithMode(path, out, info.Mode().Perm())
	}
}

// writeWithMode replaces path's content atomically. The new file breaks
// copy-on-write sharing for this one file only.
func writeWithMode(path string, data []byte, perm os.FileMode) {
	tmp := path + ".cowtree-tmp"
	if os.WriteFile(tmp, data, perm) == nil {
		if os.Rename(tmp, path) != nil {
			os.Remove(tmp)
		}
	}
}
