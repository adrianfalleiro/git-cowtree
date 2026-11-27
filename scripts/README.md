# scripts/git-cowtree.sh

A single-file Bash version of `git cowtree add`. It predates the Go binary in
`cmd/git-cowtree`, which is the version to install and the only one CI builds or
tests. The script is kept as a short, readable sketch of the idea, and as
something you can drop on a machine without Go.

```bash
scripts/git-cowtree.sh add [<git worktree add options>] <path> [<commit-ish>]
scripts/git-cowtree.sh <anything else>    # passed straight to `git worktree`
scripts/git-cowtree.sh --help
```

Copy it onto your `PATH` as `git-cowtree` and `git cowtree add` works too.

## How it works

1. `git worktree add --no-checkout` registers the new worktree.
2. The source worktree is cloned into it, skipping `.git`, other worktrees,
   submodules and `cowtree.exclude` paths. On macOS each directory is a single
   `clonefile()` call, through the system `perl`. On Linux it runs
   `cp -a --reflink=always` on each entry.
3. The source's index is copied over and `git reset --hard` brings every
   tracked file to the target commit.
4. `git clean -fdq` removes the untracked files that came along, unless
   `COWTREE_KEEP_UNTRACKED` is set. Gitignored files stay.
5. The `post-checkout` hook runs, as it would after a normal add.

If the filesystem can't clone, it does a normal checkout instead.

## Settings

| Setting | Effect |
|---|---|
| `git config --add cowtree.exclude <path>` | Never clone this path. Repeatable. |
| `COWTREE_SOURCE=<dir>` | Worktree to clone from, instead of the current one. |
| `COWTREE_KEEP_UNTRACKED=1` | Keep untracked files. |
| `COWTREE_QUIET=1` | No progress output. |

## Compared with the Go binary

The script doesn't have:

- the `git` shim, pnpm `node_modules` handling, or the Claude Code hooks
- removing the worktree again when something fails or you press Ctrl-C
- the index update that makes git trust the cloned files, so the first
  `git status` may re-read every file
- restoring only the files that differ, so `reset --hard` checks every file

It also gets these cases wrong, which the Go binary handles:

- A file marked `--skip-worktree` in the source keeps that flag and its local
  edit in the new worktree, instead of matching the commit.
- With a split index (`core.splitIndex`) in the source, the new worktree's
  index can't be read and the add fails.
- `--orphan` fails, because git refuses it together with `--no-checkout`.

Use the Go binary unless you can't install it.
