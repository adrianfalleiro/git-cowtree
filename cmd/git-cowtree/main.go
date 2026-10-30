// Command git-cowtree is `git worktree add`, but the new worktree is a
// copy-on-write clone of an existing one: tracked files, node_modules, build
// caches, .env files and everything else gitignored come along instantly and
// take almost no extra disk.
//
//	git cowtree add [<git worktree add options>] <path> [<commit-ish>]
//	git cowtree install-shim [<dir>]  # make plain `git worktree add` use it
//	git cowtree version               # print the version
//	git cowtree <anything else>       # passed straight to `git worktree`
//
// Installed as `git` (a symlink named git ahead of the real one on PATH), it
// handles `git worktree add` itself, syncs pnpm dependencies after
// `git checkout` and `git switch`, and execs the real git for everything else.
//
// It works on macOS (APFS clonefile) and Linux (btrfs, XFS and bcachefs
// reflinks). If the filesystem can't clone, it falls back to a normal checkout.
//
// Environment:
//
//	COWTREE_SOURCE=<dir>        worktree to clone from (default: the current one)
//	COWTREE_KEEP_UNTRACKED=1    keep untracked, non-ignored files from the source
//	COWTREE_DISABLE=1           shim: behave exactly like plain git
//	COWTREE_REAL_GIT=<path>     git binary to use (default: the next git on PATH)
//	COWTREE_WORKERS=<n>         parallel clone and file operations
//	COWTREE_QUIET=1             no progress output
//	COWTREE_TRACE=1             print how long each step takes
//
// Git config:
//
//	git config --add cowtree.exclude .venv   # never clone these paths (repeatable)
//	git config cowtree.install never         # pnpm: auto (default) | always | never
//	git config cowtree.installCommand '...'  # default: pnpm install
//	                                         #   --frozen-lockfile --prefer-offline
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"

	"github.com/adrianfalleiro/git-cowtree/internal/git"
	"github.com/adrianfalleiro/git-cowtree/internal/report"
	"github.com/adrianfalleiro/git-cowtree/internal/worktree"
)

const usage = `usage:
  git cowtree add [<git worktree add options>] <path> [<commit-ish>]
  git cowtree install-shim [<dir>]  make plain 'git worktree add' use copy-on-write
  git cowtree version               print the version
  git cowtree <anything else>       passed straight to 'git worktree'

env:    COWTREE_SOURCE, COWTREE_KEEP_UNTRACKED, COWTREE_DISABLE, COWTREE_REAL_GIT,
        COWTREE_WORKERS, COWTREE_QUIET, COWTREE_TRACE
config: cowtree.exclude, cowtree.install, cowtree.installCommand
`

// version is set at release build time with -ldflags "-X main.version=v1.2.3".
var version = ""

// buildVersion is the release version, or the module version that
// `go install ...@<version>` recorded, or "dev".
func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

func main() {
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == "git" {
		runAsShim(args)
		return
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprint(os.Stderr, usage)
		return
	}
	switch args[0] {
	case "version", "--version":
		fmt.Println(report.Prog, buildVersion())
	case "add":
		_, err := worktree.Add(args[1:], optionsFromEnv())
		exit(err)
	case "install-shim":
		exit(installShim(args[1:]))
	default:
		cmd := exec.Command(git.Bin, append([]string{"worktree"}, args...)...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		exit(cmd.Run())
	}
}

func optionsFromEnv() worktree.Options {
	return worktree.Options{
		Source:        os.Getenv("COWTREE_SOURCE"),
		KeepUntracked: os.Getenv("COWTREE_KEEP_UNTRACKED") != "",
	}
}

// exit ends the program, passing git's exit status through when err came from
// a git command.
func exit(err error) {
	if err == nil {
		os.Exit(0)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	}
	report.Fatalf("%v", err)
}

func selfPath() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(self)
}
