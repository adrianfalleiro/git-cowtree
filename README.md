# git-cowtree

Instant git worktrees, dependencies included.

`git worktree add` checks out every file from scratch, and the new worktree
starts without your `node_modules`, build caches or `.env` files. git-cowtree
creates the worktree as a copy-on-write clone of the one you're in instead.
Everything is there straight away, gitignored files included, and the copy shares
disk space with the original until one side changes a file.

It works on macOS (APFS) and on Linux filesystems that support reflinks, such as
btrfs and XFS. Anywhere else it falls back to a normal checkout.

## Install

Download a release for macOS or Linux (arm64 or amd64) with the GitHub CLI:

```bash
os=$(uname -s | tr A-Z a-z); arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
gh release download --repo adrianfalleiro/git-cowtree --pattern "*_${os}_${arch}.tar.gz" --output - \
  | tar -xz --strip-components 1 -C ~/.local/bin git-cowtree_*/git-cowtree
```

Or build it with Go. The repo is private, so tell Go to fetch it directly:

```bash
GOPRIVATE=github.com/adrianfalleiro GOBIN=~/.local/bin go install github.com/adrianfalleiro/git-cowtree/cmd/git-cowtree@latest
```

Either way, the directory you install to must be on your `PATH`.

## Usage

```bash
git cowtree add ../feature -b feature origin/main
```

`git cowtree add` takes the same arguments as `git worktree add`. The result is
an ordinary git worktree: commit, switch branches and remove it as usual.
With `--no-checkout` or `--orphan` there is nothing to clone, so it runs plain
`git worktree add`. If filling the worktree fails, or you press Ctrl-C,
the half-made worktree is removed again, along with the branch if git created
one for it.

### With plain `git`

To make `git worktree add` itself use copy-on-write, install the shim and add the
line it prints to your shell profile:

```bash
git-cowtree install-shim
```

The shim is the same binary, linked as `git` ahead of the real one on your
`PATH`. It handles `git worktree add` and hands every other command to the real
git. Tools that run `git` from your `PATH`, such as editors and worktree
managers, pick it up too. Set `COWTREE_DISABLE=1` to bypass it for a command.

### With Claude Code

Claude Code can create its worktrees (`claude --worktree`, subagents with
`isolation: "worktree"`, background sessions) through git-cowtree, so agents
start with your dependencies already in place. Add this to
`.claude/settings.json` (or `~/.claude/settings.json` for every project).
`git-cowtree claude-hook` prints the same snippet:

```json
{
  "hooks": {
    "WorktreeCreate": [
      { "hooks": [{ "type": "command", "command": "git-cowtree claude-hook create" }] }
    ],
    "WorktreeRemove": [
      { "hooks": [{ "type": "command", "command": "git-cowtree claude-hook remove" }] }
    ]
  }
}
```

Worktrees go where Claude Code puts its own, `.claude/worktrees/<name>`, on a
branch `worktree-<name>` that starts from `origin`'s default branch (set
`git config cowtree.claudeBase <commit-ish>` to change that). Add
`.claude/worktrees/` to your `.gitignore`. When Claude Code removes the
worktree, its branch is deleted too, unless it has commits that aren't merged.



A cloned `node_modules` is adjusted to work from its new location. If the branch
you're creating the worktree for has a different `pnpm-lock.yaml` from the one
your dependencies were installed with, git-cowtree runs `pnpm install` instead
of cloning them. With the shim installed, it does the same after `git checkout`
and `git switch` when the lockfile changes.

## How it works

1. **Create the worktree.** git registers the new worktree and its branch, without
   checking anything out.
2. **Clone the source.** Your current worktree is cloned into it using the
   filesystem's copy-on-write support, a whole directory at a time. Cloning only
   records which disk blocks each file uses, so its cost depends on how many files
   there are, not how big they are. It skips `.git`, other worktrees, submodules
   and untracked files.
3. **Match the commit.** Tracked files that differ from the commit you asked for
   are restored from git: your uncommitted changes, plus anything that differs
   between your branch and the new one. Everything else is already correct.
4. **Tell git the files are unchanged.** git's index is updated so it recognises
   the cloned files, and the first `git status` doesn't have to re-read them.
5. **Fix up dependencies.** pnpm's `node_modules` contains paths to the original
   location, which are rewritten. If its lockfile doesn't match, dependencies are
   installed instead.

The new worktree contains:

| From your current worktree | In the new worktree |
|---|---|
| Tracked files | Exactly as in the commit you asked for |
| Uncommitted changes | Not carried over |
| Untracked files | Not copied (set `COWTREE_KEEP_UNTRACKED=1` to keep them) |
| Gitignored files (`node_modules`, `.env`, build output) | Cloned |

## Configuration

| Setting | Effect |
|---|---|
| `git config --add cowtree.exclude <path>` | Never clone this path, e.g. a Python `.venv` that stores absolute paths. Repeatable. |
| `git config cowtree.install <auto\|always\|never>` | When to run `pnpm install`. Default `auto`: only when the lockfile differs. |
| `git config cowtree.installCommand '<command>'` | Command to install dependencies with, instead of `pnpm install`. |
| `git config cowtree.claudeBase <commit-ish>` | Where `claude-hook create` starts new branches. Default: `origin`'s default branch, else `HEAD`. |
| `COWTREE_SOURCE=<dir>` | Worktree to clone from, instead of the current one. |
| `COWTREE_KEEP_UNTRACKED=1` | Also copy untracked files. |
| `COWTREE_DISABLE=1` | Make the shim behave exactly like plain git. |
| `COWTREE_QUIET=1` | No progress output. |
| `COWTREE_TRACE=1` | Print how long each step takes. |

`git-cowtree --help` lists everything, and `git cowtree version` prints the
installed version.

## Development

```bash
go test ./...
```

Tests need `git` on your `PATH`. Pushing a tag like `v0.1.0` publishes a release
with binaries for macOS and Linux.
