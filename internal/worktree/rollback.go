package worktree

import (
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"

	"github.com/adrianfalleiro/git-cowtree/internal/git"
	"github.com/adrianfalleiro/git-cowtree/internal/report"
)

// ErrInterrupted is returned by Add when SIGINT or SIGTERM arrived while the
// new worktree was being filled. The worktree has been removed again.
var ErrInterrupted = errors.New("interrupted")

// catchInterrupt makes the first SIGINT or SIGTERM set the returned flag
// instead of killing the program, so a half-filled worktree can be cleaned up.
// A second signal kills as usual. Call release once the worktree is complete.
func catchInterrupt() (stop *atomic.Bool, release func()) {
	stop = new(atomic.Bool)
	sigs := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-sigs:
			stop.Store(true)
			signal.Stop(sigs) // the next one gets the default behavior
			report.Errorf("interrupted; removing the incomplete worktree (again to force quit)")
		case <-done:
		}
	}()
	var once bool
	return stop, func() {
		if !once {
			once = true
			signal.Stop(sigs)
			close(done)
		}
	}
}

// localBranches lists refs/heads/* by full ref name, or returns nil if git
// can't, so that rollback deletes no branch.
func localBranches(dir string) map[string]bool {
	out, err := git.Output(dir, "for-each-ref", "--format=%(refname)", "refs/heads/")
	if err != nil {
		return nil
	}
	branches := map[string]bool{}
	for _, ref := range strings.Split(out, "\n") {
		if ref != "" {
			branches[ref] = true
		}
	}
	return branches
}

// rollback undoes a `git worktree add` whose worktree couldn't be filled: it
// removes the worktree dst (registered as id) and deletes the branch it's on
// if that branch didn't exist before (it was created by -b, or by git's
// guess from the path). A branch that existed is kept.
func rollback(src source, id, dst string, branchesBefore map[string]bool) {
	report.Errorf("removing the incomplete worktree %s", dst)
	ref, _ := git.Output(dst, "symbolic-ref", "-q", "HEAD")
	if err := git.Run(src.top, "worktree", "remove", "--force", "--force", dst); err != nil {
		// git refuses when, say, it can't read the half-written index: do
		// what it would have done.
		os.RemoveAll(dst)
		os.RemoveAll(filepath.Join(src.commonDir, "worktrees", id))
	}
	if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok && branchesBefore != nil && !branchesBefore[ref] {
		if err := git.Run(src.top, "branch", "-q", "-D", branch); err != nil {
			report.Errorf("could not delete the new branch %s: %v", branch, err)
		}
	}
}
