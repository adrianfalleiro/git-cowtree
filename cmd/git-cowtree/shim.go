package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/adrianfalleiro/git-cowtree/internal/git"
	"github.com/adrianfalleiro/git-cowtree/internal/pnpm"
	"github.com/adrianfalleiro/git-cowtree/internal/report"
	"github.com/adrianfalleiro/git-cowtree/internal/worktree"
)

// shimAction is what the `git` shim does with a command line.
type shimAction int

const (
	passThrough  shimAction = iota // exec the real git
	addWorktree                    // copy-on-write `git worktree add`
	checkoutSync                   // real checkout/switch, then sync pnpm deps
)

// classifyShim decides what to do with `git <args>`. Only leading -C options
// are understood; any other global option means plain git. It also returns
// the -C directories and the arguments after them.
func classifyShim(args []string, disabled bool) (shimAction, []string, []string) {
	i := 0
	var chdirs []string
	for i+1 < len(args) && args[i] == "-C" {
		chdirs = append(chdirs, args[i+1])
		i += 2
	}
	rest := args[i:]
	switch {
	case disabled || len(rest) == 0:
		return passThrough, chdirs, rest
	case len(rest) >= 2 && rest[0] == "worktree" && rest[1] == "add" && !worktree.PlainAdd(rest[2:]):
		return addWorktree, chdirs, rest
	case rest[0] == "checkout" || rest[0] == "switch":
		return checkoutSync, chdirs, rest
	}
	return passThrough, chdirs, rest
}

// runAsShim handles a command line when this binary is invoked as `git`.
func runAsShim(args []string) {
	action, chdirs, rest := classifyShim(args, os.Getenv("COWTREE_DISABLE") != "")
	switch action {
	case addWorktree:
		for _, d := range chdirs {
			if err := os.Chdir(d); err != nil {
				report.Fatalf("cannot change to '%s': %v", d, err)
			}
		}
		_, err := worktree.Add(rest[2:], optionsFromEnv())
		exit(err)
	case checkoutSync:
		os.Exit(checkoutThenSync(args, chdirs))
	}
	if err := syscall.Exec(git.Bin, append([]string{git.Bin}, args...), os.Environ()); err != nil {
		report.Fatalf("exec %s: %v", git.Bin, err)
	}
}

// checkoutThenSync runs the real `git checkout`/`git switch`, then, if the
// branch's pnpm lockfile no longer matches the installed node_modules, syncs
// it, so switching branches keeps dependencies right too.
func checkoutThenSync(args, chdirs []string) int {
	// Let Ctrl-C reach git while we wait for it. Notify, unlike Ignore, isn't
	// inherited by the child.
	signal.Notify(make(chan os.Signal, 1), os.Interrupt, syscall.SIGTERM)
	cmd := exec.Command(git.Bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exitStatus(ee)
		}
		report.Fatalf("%v", err)
	}
	dir := "."
	for _, d := range chdirs {
		dir = filepath.Join(dir, d)
		if filepath.IsAbs(d) {
			dir = d
		}
	}
	wt, err := git.Output(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return 0
	}
	if _, err := os.Stat(filepath.Join(wt, "pnpm-lock.yaml")); err != nil {
		return 0
	}
	conf, _ := git.Output(wt, "config", "--get-regexp", `^cowtree\.install(command)?$`)
	var mode, command string
	for _, line := range strings.Split(conf, "\n") {
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "cowtree.install":
			mode = val
		case "cowtree.installcommand":
			command = val
		}
	}
	if mode == "always" {
		mode = "auto" // after a checkout, only reinstall on an actual lockfile change
	}
	if err := pnpm.SyncInstall(wt, mode, command, false); err != nil {
		report.Errorf("warning: %v", err)
	}
	return 0
}

// installShim symlinks <dir>/git to this binary and prints how to put it on PATH.
func installShim(args []string) error {
	self, err := selfPath()
	if err != nil {
		return err
	}
	dir := filepath.Join(os.Getenv("HOME"), ".local", "share", "git-cowtree", "shim")
	if len(args) > 0 {
		dir = args[0]
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	link := filepath.Join(dir, "git")
	os.Remove(link)
	if err := os.Symlink(self, link); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "installed %s -> %s\n\nAdd this to your shell profile (e.g. ~/.zshrc):\n\n", link, self)
	fmt.Printf("export PATH=%q:\"$PATH\"\n", dir)
	return nil
}
