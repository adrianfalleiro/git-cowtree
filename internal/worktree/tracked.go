package worktree

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/adrianfalleiro/git-cowtree/internal/git"
	"github.com/adrianfalleiro/git-cowtree/internal/report"
)

// maxTrackedFix caps how many paths we restore one by one; past that a plain
// `reset --hard` over the whole tree is just as fast.
const maxTrackedFix = 5000

// trackedFix is the set of tracked paths whose cloned content or index entry
// can differ from the target commit: the source's unstaged and staged changes,
// plus everything that differs between the source's HEAD and the target.
// Every other tracked path is already right, since it was cloned from a file
// the source's index vouched for at its HEAD.
type trackedFix struct {
	target string
	paths  []string
	err    error
}

// planTrackedFix asks the source what differs. It runs while the clone is in
// progress, so it costs nothing on the critical path. skipped lists
// repo-relative paths that were deliberately not cloned (user excludes,
// submodules, skipped node_modules); any tracked files under them must be
// restored too.
func planTrackedFix(srcTop, dst string, skipped []string) trackedFix {
	defer report.Trace(time.Now(), "plan tracked-file fix")
	var (
		mu                          sync.Mutex
		wg                          sync.WaitGroup
		errFirst                    error
		srcHead, target             string
		unstaged, staged, inSkipped string
	)
	run := func(f func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := f(); err != nil {
				mu.Lock()
				if errFirst == nil {
					errFirst = err
				}
				mu.Unlock()
			}
		}()
	}
	run(func() (err error) { srcHead, err = git.Output(srcTop, "rev-parse", "-q", "--verify", "HEAD"); return })
	run(func() (err error) { target, err = git.Output(dst, "rev-parse", "-q", "--verify", "HEAD"); return })
	run(func() (err error) { unstaged, err = git.Output(srcTop, "diff-files", "--name-only", "-z"); return })
	run(func() (err error) {
		staged, err = git.Output(srcTop, "diff-index", "--cached", "--name-only", "-z", "HEAD")
		return
	})
	if len(skipped) > 0 {
		run(func() (err error) {
			inSkipped, err = git.Output(srcTop, append([]string{"--literal-pathspecs", "ls-files", "-z", "--"}, skipped...)...)
			return
		})
	}
	wg.Wait()
	if errFirst != nil {
		return trackedFix{err: errFirst}
	}
	fix := trackedFix{target: target}
	moved := ""
	if srcHead != target {
		var err error
		if moved, err = git.Output(srcTop, "diff-tree", "-r", "--name-only", "-z", "--no-renames", srcHead, target); err != nil {
			return trackedFix{err: err}
		}
	}
	seen := map[string]bool{}
	for _, list := range []string{unstaged, staged, moved, inSkipped} {
		for _, p := range strings.Split(list, "\x00") {
			if p != "" && !seen[p] {
				seen[p] = true
				fix.paths = append(fix.paths, p)
			}
		}
	}
	if len(fix.paths) > maxTrackedFix {
		fix.err = fmt.Errorf("%d paths differ", len(fix.paths))
	}
	return fix
}

// applyTrackedFix restores just the planned paths from the target commit, in
// both the index and the worktree, then checks the whole index against the
// target tree (cheap: it compares object ids, not files). Any doubt falls back
// to a full `reset --hard`.
func applyTrackedFix(dst string, fix trackedFix) error {
	defer report.Trace(time.Now(), fmt.Sprintf("restore %d differing tracked paths", len(fix.paths)))
	if fix.err == nil && len(fix.paths) > 0 {
		fix.err = git.RunInput(dst, strings.Join(fix.paths, "\x00"),
			"restore", "--source="+fix.target, "--staged", "--worktree",
			"--pathspec-from-file=-", "--pathspec-file-nul")
	}
	if fix.err == nil {
		_, fix.err = git.Output(dst, "diff-index", "--cached", "--quiet", fix.target)
	}
	if fix.err == nil {
		return nil
	}
	report.Trace(time.Now(), fmt.Sprintf("  targeted restore not usable (%v); full reset", fix.err))
	return git.Run(dst, "-c", "core.checkStat=minimal", "-c", "core.trustCtime=false", "reset", "-q", "--hard")
}
