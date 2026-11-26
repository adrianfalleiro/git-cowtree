package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/adrianfalleiro/git-cowtree/internal/git"
	"github.com/adrianfalleiro/git-cowtree/internal/pathutil"
	"github.com/adrianfalleiro/git-cowtree/internal/report"
	"github.com/adrianfalleiro/git-cowtree/internal/worktree"
)

// claudeSettings is the Claude Code settings snippet that routes Claude's
// worktrees (--worktree, isolation: "worktree", background sessions) through
// git-cowtree.
const claudeSettings = `{
  "hooks": {
    "WorktreeCreate": [
      { "hooks": [{ "type": "command", "command": "git-cowtree claude-hook create" }] }
    ],
    "WorktreeRemove": [
      { "hooks": [{ "type": "command", "command": "git-cowtree claude-hook remove" }] }
    ]
  }
}
`

// claudeBranchPrefix is what Claude Code itself names its worktree branches.
const claudeBranchPrefix = "worktree-"

// hookInput is the part of a Claude Code WorktreeCreate/WorktreeRemove hook's
// JSON input we use.
type hookInput struct {
	Cwd          string `json:"cwd"`
	Name         string `json:"name"`          // WorktreeCreate
	WorktreePath string `json:"worktree_path"` // WorktreeRemove
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// claudeHook runs `git cowtree claude-hook create|remove`, reading the hook's
// JSON from stdin. create prints the new worktree's path on stdout, which is
// how Claude Code learns where it is; everything else goes to stderr.
func claudeHook(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 1 || (args[0] != "create" && args[0] != "remove") {
		fmt.Fprint(os.Stderr, "usage: git cowtree claude-hook create|remove  (JSON from Claude Code on stdin)\n\nAdd this to .claude/settings.json or ~/.claude/settings.json:\n\n")
		fmt.Print(claudeSettings)
		if len(args) == 0 {
			return nil
		}
		return errors.New("unknown claude-hook command")
	}
	var in hookInput
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return fmt.Errorf("reading the hook input: %w", err)
	}
	if args[0] == "create" {
		path, err := claudeCreate(in)
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, path)
		return nil
	}
	return claudeRemove(in)
}

// claudeCreate makes the worktree Claude Code asked for where Claude would put
// it, <top>/.claude/worktrees/<name> on a branch worktree-<name>, as a
// copy-on-write clone of the worktree the session runs in. The branch starts
// at cowtree.claudeBase if set, else origin's default branch, else HEAD, like
// Claude's own default of branching from the default branch.
func claudeCreate(in hookInput) (string, error) {
	if !safeName.MatchString(in.Name) {
		return "", fmt.Errorf("unexpected worktree name %q", in.Name)
	}
	if in.Cwd != "" {
		if err := os.Chdir(in.Cwd); err != nil {
			return "", err
		}
	}
	top, err := git.Output(".", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	dst := filepath.Join(top, ".claude", "worktrees", in.Name)
	branch := claudeBranchPrefix + in.Name

	var args []string
	if _, err := git.Output(top, "rev-parse", "-q", "--verify", "refs/heads/"+branch); err == nil {
		args = []string{dst, branch} // left over from an earlier session: reuse it
	} else {
		args = []string{"-b", branch, dst, claudeBase(top)}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	path, err := worktree.Add(append([]string{"-q"}, args...), optionsFromEnv())
	if err != nil {
		return "", err
	}
	if path == "" {
		return "", errors.New("no worktree was created")
	}
	return path, nil
}

func claudeBase(top string) string {
	if base, err := git.Output(top, "config", "cowtree.claudeBase"); err == nil && base != "" {
		return base
	}
	if ref, err := git.Output(top, "symbolic-ref", "-q", "refs/remotes/origin/HEAD"); err == nil {
		return ref
	}
	return "HEAD"
}

// claudeRemove deletes a worktree claudeCreate made. Claude Code has already
// decided it can go (it checks for changes with git first); the branch is
// deleted only if that loses no commits, since Claude never deletes it.
func claudeRemove(in hookInput) error {
	dst, err := pathutil.Canon(in.WorktreePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ref, _ := git.Output(dst, "symbolic-ref", "-q", "HEAD")
	// Run git from the repository's git dir, which outlives the worktree.
	commonDir, _ := git.Output(dst, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if commonDir == "" {
		return os.RemoveAll(dst) // not a git worktree at all
	}
	if err := git.Run(commonDir, "worktree", "remove", "--force", "--force", dst); err != nil {
		// One git can't read: delete it anyway, as Claude Code expects.
		if err := os.RemoveAll(dst); err != nil {
			return err
		}
		git.Run(commonDir, "worktree", "prune")
	}
	branch, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok || !strings.HasPrefix(branch, claudeBranchPrefix) {
		return nil
	}
	// -d, not -D: a branch with commits that aren't merged is kept.
	if _, err := git.Output(commonDir, "branch", "-q", "-d", branch); err != nil {
		report.Errorf("kept branch %s, which has commits not merged anywhere", branch)
	}
	return nil
}
