// Package worktree creates git worktrees as copy-on-write clones of an
// existing worktree instead of checking files out.
package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/adrianfalleiro/git-cowtree/internal/clone"
	"github.com/adrianfalleiro/git-cowtree/internal/git"
	"github.com/adrianfalleiro/git-cowtree/internal/gitindex"
	"github.com/adrianfalleiro/git-cowtree/internal/parallel"
	"github.com/adrianfalleiro/git-cowtree/internal/pathutil"
	"github.com/adrianfalleiro/git-cowtree/internal/pnpm"
	"github.com/adrianfalleiro/git-cowtree/internal/report"
)

// maxUntrackedExcludes caps how many untracked paths we skip while cloning;
// past that it's cheaper to clone them and `git clean` afterwards.
const maxUntrackedExcludes = 2000

// Options configures Add.
type Options struct {
	// Source is the worktree to clone from. Empty means the worktree that
	// contains the current directory.
	Source string
	// KeepUntracked keeps the source's untracked, non-ignored files.
	KeepUntracked bool
}

type source struct {
	top, commonDir, index, objectFormat string
}

// sourceFacts is what we learn about the source worktree while git is busy
// creating the new one.
type sourceFacts struct {
	excludes       []string // repo-relative: cowtree.exclude, submodules, untracked
	mayHoldTracked []string // the excludes that aren't untracked files (may contain tracked ones)
	needClean      bool     // untracked files will be cloned and must be cleaned
	hooksPath      string   // core.hooksPath, if set
	install        string   // cowtree.install: auto, always, never
	installCommand string   // cowtree.installCommand
}

// PlainAdd reports whether `git worktree add <args>` has nothing to clone into
// and should run as is: with --no-checkout the user wants no files, and an
// --orphan worktree starts empty (git also refuses it with --no-checkout).
func PlainAdd(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "--no-checkout", "--orphan", "-h", "--help":
			return true
		}
	}
	return false
}

// Add runs `git worktree add --no-checkout <args>` from the current directory,
// then fills the new worktree from the source worktree: everything is cloned
// copy-on-write, and only tracked files that differ from the new HEAD are
// rewritten. It returns the new worktree's path, or "" if git created none
// (for example with --help) or Add left the work to plain git (see PlainAdd).
//
// If the worktree can't be filled, or Ctrl-C arrives first, Add removes it
// again, along with its branch if git created that branch for it.
func Add(args []string, opts Options) (string, error) {
	t0 := time.Now()
	if PlainAdd(args) {
		return "", git.Run(".", append([]string{"worktree", "add"}, args...)...)
	}
	dir := opts.Source
	if dir == "" {
		dir = "."
	}
	out, err := git.Output(dir, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir", "--git-path", "index", "--show-object-format")
	lines := strings.Split(out, "\n")
	if err != nil || len(lines) != 4 {
		return "", errors.New("run inside a work tree, or set COWTREE_SOURCE")
	}
	var src source
	if src.top, err = pathutil.Canon(lines[0]); err != nil {
		return "", err
	}
	src.commonDir, src.index, src.objectFormat = lines[1], lines[2], lines[3]
	if opts.Source != "" {
		// git creates the worktree in the repository of the current directory,
		// which must be the one we clone from.
		here, err := git.Output(".", "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return "", err
		}
		a, errA := pathutil.Canon(here)
		b, errB := pathutil.Canon(src.commonDir)
		if errA != nil || errB != nil || a != b {
			return "", fmt.Errorf("COWTREE_SOURCE %s is not a worktree of the repository in the current directory", opts.Source)
		}
	}

	// From here until the worktree is filled, Ctrl-C removes it instead of
	// leaving it half-made. git, in our process group, gets the signal too.
	stop, release := catchInterrupt()
	defer release()

	before := adminDirs(src.commonDir)
	var facts sourceFacts
	var branchesBefore map[string]bool
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); facts = querySource(src.top, opts.KeepUntracked) }()
	go func() { defer wg.Done(); branchesBefore = localBranches(src.top) }()
	addErr := git.Run(".", append([]string{"worktree", "add", "--no-checkout"}, args...)...)
	wg.Wait()
	if addErr != nil {
		if stop.Load() {
			return "", ErrInterrupted
		}
		return "", addErr
	}

	id, dst := newWorktree(src.commonDir, before, args)
	if id == "" {
		return "", errors.New("cannot tell which worktree git just created; it was left without a checkout")
	}
	dstIndex := filepath.Join(src.commonDir, "worktrees", id, "index")

	plan := pnpm.PlanClone(src.top, dst, facts.install, src.objectFormat)
	facts.excludes = append(facts.excludes, plan.Skip...)
	facts.mayHoldTracked = append(facts.mayHoldTracked, plan.Skip...)
	err = populateFn(src, dst, dstIndex, facts, stop)
	if err == nil && stop.Load() {
		err = ErrInterrupted
	}
	if err != nil {
		rollback(src, id, dst, branchesBefore)
		if stop.Load() {
			return "", ErrInterrupted
		}
		return "", err
	}
	release() // complete: a Ctrl-C during the install below just stops it
	runPostCheckout(src, dst, facts.hooksPath)
	if err := pnpm.SyncInstall(dst, facts.install, facts.installCommand, plan.FreshInstall); err != nil {
		report.Errorf("warning: %v", err)
	}
	report.Trace(t0, "total")
	report.Logf("ready: %s", dst)
	return dst, nil
}

// adminDirs records the identity of each registered worktree's gitdir file
// (<commonDir>/worktrees/<id>/gitdir), keyed by worktree id.
func adminDirs(commonDir string) map[string]os.FileInfo {
	ids := map[string]os.FileInfo{}
	entries, _ := os.ReadDir(filepath.Join(commonDir, "worktrees"))
	for _, e := range entries {
		if info, err := os.Stat(filepath.Join(commonDir, "worktrees", e.Name(), "gitdir")); err == nil {
			ids[e.Name()] = info
		}
	}
	return ids
}

// newWorktree finds the worktree `git worktree add <args>` just registered:
// an id that wasn't there before, or one whose gitdir file was rewritten
// (`add -f` over a missing worktree reuses its id). If several appeared, as
// with concurrent adds, it picks the one at a path named in args.
func newWorktree(commonDir string, before map[string]os.FileInfo, args []string) (id, dst string) {
	wanted := map[string]bool{}
	for _, a := range args {
		if c, err := pathutil.Canon(a); err == nil {
			wanted[c] = true
		}
	}
	after := adminDirs(commonDir)
	var only []string
	for k, p := range git.LinkedWorktrees(commonDir) {
		if old, ok := before[k]; ok {
			if cur, ok := after[k]; !ok || (os.SameFile(old, cur) && old.ModTime().Equal(cur.ModTime())) {
				continue
			}
		}
		c, err := pathutil.Canon(p)
		if err != nil {
			continue
		}
		if wanted[c] {
			return k, c
		}
		only = append(only, k, c)
	}
	if len(only) == 2 {
		return only[0], only[1]
	}
	return "", ""
}

// querySource gathers config, submodules and untracked files from the source.
func querySource(top string, keepUntracked bool) sourceFacts {
	var facts sourceFacts
	var mu sync.Mutex
	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }

	run(func() {
		out, _ := git.Output(top, "config", "--get-regexp", `^(cowtree\.(exclude|install|installcommand)|core\.hookspath)$`)
		mu.Lock()
		defer mu.Unlock()
		for _, line := range strings.Split(out, "\n") {
			key, val, _ := strings.Cut(line, " ")
			switch key {
			case "cowtree.exclude":
				facts.excludes = append(facts.excludes, filepath.Clean(val))
				facts.mayHoldTracked = append(facts.mayHoldTracked, filepath.Clean(val))
			case "cowtree.install":
				facts.install = val
			case "cowtree.installcommand":
				facts.installCommand = val
			case "core.hookspath":
				facts.hooksPath = val
			}
		}
	})
	if _, err := os.Stat(filepath.Join(top, ".gitmodules")); err == nil {
		run(func() {
			out, _ := git.Output(top, "config", "-f", ".gitmodules", "--get-regexp", `^submodule\..*\.path$`)
			mu.Lock()
			defer mu.Unlock()
			for _, line := range strings.Split(out, "\n") {
				if _, p, ok := strings.Cut(line, " "); ok {
					facts.excludes = append(facts.excludes, filepath.Clean(p))
					facts.mayHoldTracked = append(facts.mayHoldTracked, filepath.Clean(p))
				}
			}
		})
	}
	if !keepUntracked {
		run(func() {
			out, err := git.Output(top, "ls-files", "-z", "--others", "--exclude-standard", "--directory")
			paths := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
			mu.Lock()
			defer mu.Unlock()
			if err != nil || len(paths) > maxUntrackedExcludes {
				facts.needClean = true
				return
			}
			for _, p := range paths {
				if p != "" {
					facts.excludes = append(facts.excludes, filepath.Clean(p))
				}
			}
		})
	}
	wg.Wait()
	return facts
}

// populateFn is populate, replaceable in tests.
var populateFn = populate

// populate fills the (--no-checkout) worktree dst from the source worktree.
// It gives up early, returning ErrInterrupted, once stop is set.
func populate(src source, dst, dstIndex string, facts sourceFacts, stop *atomic.Bool) error {
	if !clone.Possible(src.top, dst) {
		report.Logf("copy-on-write not available between %s and %s; doing a normal checkout", src.top, dst)
		return git.Run(dst, "reset", "-q", "--hard")
	}

	// Never clone: the source's .git, other worktrees nested inside it, the new
	// worktree itself, submodules, untracked files, and user excludes.
	excluded := map[string]bool{filepath.Join(src.top, ".git"): true}
	for _, p := range git.LinkedWorktrees(src.commonDir) {
		if c, err := pathutil.Canon(p); err == nil && c != src.top {
			excluded[c] = true
		}
	}
	for _, rel := range facts.excludes {
		excluded[filepath.Join(src.top, rel)] = true
	}

	fixCh := make(chan trackedFix, 1)
	go func() { fixCh <- planTrackedFix(src.top, dst, facts.mayHoldTracked) }()

	start := time.Now()
	pairs, err := planClones(src.top, dst, excluded)
	if err != nil {
		return err
	}
	failed := cloneAll(pairs, stop)
	if stop.Load() {
		<-fixCh // let its git commands finish before the worktree goes
		return ErrInterrupted
	}
	if failed > 0 {
		report.Errorf("%d clone(s) failed; tracked files will still be checked out normally", failed)
	}
	report.Logf("cloned %s -> %s in %s", src.top, dst, time.Since(start).Round(time.Millisecond))
	pnpm.Relocate(src.top, dst)

	// Start from the source's index, then bring only the paths that differ from
	// the target commit up to date; every other entry already describes its
	// clone exactly.
	var srcIndexTime time.Time
	if info, err := os.Stat(src.index); err == nil {
		srcIndexTime = info.ModTime()
	}
	if err := copyFile(src.index, dstIndex); err != nil {
		return err
	}
	// A split index needs its shared index, which lives beside it in the
	// per-worktree git dir.
	if shared, err := gitindex.SharedIndexID(dstIndex, src.objectFormat); err == nil && shared != "" {
		name := "sharedindex." + shared
		if err := copyFile(filepath.Join(filepath.Dir(src.index), name), filepath.Join(filepath.Dir(dstIndex), name)); err != nil {
			return err
		}
	}
	fix := <-fixCh
	if failed > 0 && fix.err == nil {
		// The index vouches for files that may not have been cloned.
		fix.err = fmt.Errorf("%d clone(s) failed", failed)
	}
	if err := applyTrackedFix(dst, fix); err != nil {
		return err
	}
	// Then make the index trust the clones under git's normal stat checks too,
	// so the worktree is fully native: no rehash on the first status.
	if n, err := gitindex.RefreshStat(dst, dstIndex, src.objectFormat, srcIndexTime); err != nil {
		report.Logf("note: index stat refresh skipped (%v); first git status will rehash", err)
	} else {
		report.Trace(time.Now(), fmt.Sprintf("trusted %d clones in the index", n))
	}
	if facts.needClean {
		return git.Run(dst, "clean", "-fdq")
	}
	return nil
}

// copyFile replaces dst with a clone of src, or a plain copy where cloning
// isn't possible. A missing src is not an error.
func copyFile(src, dst string) error {
	os.Remove(dst)
	err := clone.Tree(src, dst)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return nil
	}
	return os.WriteFile(dst, data, 0o644)
}

type clonePair struct{ src, dst string }

// planClones lists what to clone from src into dst. Whole subtrees are cloned
// in one call wherever possible (splitting them into more, smaller calls was
// measured to be slower on APFS); we descend only into directories that
// contain an excluded path.
func planClones(src, dst string, excluded map[string]bool) ([]clonePair, error) {
	holdsExcluded := func(dir string) bool {
		for e := range excluded {
			if strings.HasPrefix(e, dir+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	var pairs []clonePair
	var walk func(s, d string) error
	walk = func(s, d string) error {
		entries, err := os.ReadDir(s)
		if err != nil {
			return err
		}
		for _, e := range entries {
			sp, dp := filepath.Join(s, e.Name()), filepath.Join(d, e.Name())
			switch {
			case excluded[sp]:
			case e.IsDir() && holdsExcluded(sp):
				info, err := e.Info()
				if err != nil {
					return err
				}
				if err := os.MkdirAll(dp, info.Mode().Perm()); err != nil {
					return err
				}
				if err := walk(sp, dp); err != nil {
					return err
				}
			default:
				pairs = append(pairs, clonePair{sp, dp})
			}
		}
		return nil
	}
	return pairs, walk(src, dst)
}

// cloneAll runs the clones in parallel and returns how many failed.
func cloneAll(pairs []clonePair, stop *atomic.Bool) int {
	defer report.Trace(time.Now(), fmt.Sprintf("clone %d entries", len(pairs)))
	var failed atomic.Int64
	parallel.ForEach(pairs, func(p clonePair) {
		if stop.Load() {
			return
		}
		start := time.Now()
		err := clone.Tree(p.src, p.dst)
		if time.Since(start) > 200*time.Millisecond {
			report.Trace(start, "  clone "+filepath.Base(p.src))
		}
		if err != nil {
			failed.Add(1)
			report.Errorf("clone %s: %v", p.src, err)
		}
	})
	return int(failed.Load())
}

// runPostCheckout runs the post-checkout hook that git skips under
// --no-checkout, but only spawns git when such a hook actually exists.
func runPostCheckout(src source, dst, hooksPath string) {
	hooks := filepath.Join(src.commonDir, "hooks")
	if hooksPath != "" {
		hooks = hooksPath
		if !filepath.IsAbs(hooks) {
			hooks = filepath.Join(dst, hooks)
		}
	}
	if info, err := os.Stat(filepath.Join(hooks, "post-checkout")); err != nil || info.Mode()&0o111 == 0 {
		return
	}
	if head, err := git.Output(dst, "rev-parse", "HEAD"); err == nil {
		git.Run(dst, "hook", "run", "--ignore-missing", "post-checkout", "--", strings.Repeat("0", len(head)), head, "1")
	}
}
