#!/usr/bin/env bash
# git-cowtree — `git worktree add`, but the new worktree is a copy-on-write
# clone of an existing one: tracked files, node_modules, build caches, .env files
# and everything else gitignored come along instantly and take no extra disk.
#
#   git cowtree add [<git worktree add options>] <path> [<commit-ish>]
#   git cowtree <anything else>    # passed straight to `git worktree`
#
# Works on macOS (APFS clonefile, via the system perl) and Linux (btrfs/XFS/bcachefs
# reflinks). If the filesystem can't clone, falls back to a normal checkout.
#
# Knobs (env):
#   COWTREE_SOURCE=<dir>        worktree to clone from (default: current one)
#   COWTREE_KEEP_UNTRACKED=1    keep untracked, non-ignored files from the source
#   COWTREE_QUIET=1             no progress output
# Knobs (git config, repo-relative paths, repeatable):
#   git config --add cowtree.exclude .venv   # never clone these paths
set -euo pipefail

PROG=git-cowtree

log() { [[ -n ${COWTREE_QUIET:-} ]] || echo "$PROG: $*" >&2; }
die() { echo "$PROG: $*" >&2; exit 1; }

canon() { (cd -P -- "$1" 2>/dev/null && pwd -P); }

worktree_paths() { # $1 = any dir in the repo
  git -C "$1" worktree list --porcelain | sed -n 's/^worktree //p'
}

# Reads NUL-separated "src\0dst\0" pairs on stdin and clones each one.
# On macOS a directory is cloned in a single clonefile() call, which is
# ~100x faster than `cp -c -R` (that clones file by file).
clone_pairs() {
  case "$(uname -s)" in
    Darwin)
      /usr/bin/perl -e '
        $/ = "\0"; my $fail = 0;
        while (defined(my $s = <STDIN>)) {
          chomp $s; my $d = <STDIN>; chomp $d;
          # SYS_clonefileat = 462, AT_FDCWD = -2, CLONE_NOFOLLOW = 1
          if (syscall(462, -2, $s, -2, $d, 1) != 0) { print STDERR "clonefile $s: $!\n"; $fail = 1 }
        }
        exit $fail;' ;;
    Linux)
      local s d
      while IFS= read -r -d '' s && IFS= read -r -d '' d; do
        cp -a --reflink=always -- "$s" "$d" || return 1
      done ;;
    *) return 1 ;;
  esac
}

# Can we clone from $1 to $2 (same CoW-capable volume)?
can_clone() {
  local probe=$1/.cowtree-probe.$$
  : > "$probe" || return 1
  printf '%s\0%s\0' "$probe" "$2/.cowtree-probe.$$" | clone_pairs 2>/dev/null
  local rc=$?
  rm -f "$probe" "$2/.cowtree-probe.$$"
  return $rc
}

EXCLUDES=()
is_excluded() { local e; for e in "${EXCLUDES[@]}"; do [[ $1 == "$e" ]] && return 0; done; return 1; }
holds_excluded() { local e; for e in "${EXCLUDES[@]}"; do [[ $e == "$1"/* ]] && return 0; done; return 1; }

# Emit clone pairs for $1 -> $2, cloning whole subtrees where possible and only
# descending into directories that contain something we must skip.
emit_pairs() {
  local s d
  for s in "$1"/* "$1"/.[!.]* "$1"/..?*; do
    [[ -e $s || -L $s ]] || continue
    d=$2/${s##*/}
    if is_excluded "$s"; then
      continue
    elif [[ -d $s && ! -L $s ]] && holds_excluded "$s"; then
      mkdir -p "$d"
      emit_pairs "$s" "$d"
    else
      printf '%s\0%s\0' "$s" "$d"
    fi
  done
}

# Fill the (--no-checkout) worktree $2 from source worktree $1.
populate() {
  local src dst p rel
  src=$(canon "$1") || die "source $1 not found"
  dst=$(canon "$2")

  if ! can_clone "$src" "$dst"; then
    log "copy-on-write not available between $src and $dst; doing a normal checkout"
    git -C "$dst" reset -q --hard
    return
  fi

  # Never clone: the source's .git, other worktrees nested inside it, the new
  # worktree itself, submodules, user excludes.
  EXCLUDES=("$src/.git")
  while IFS= read -r p; do
    p=$(canon "$p") || continue
    [[ $p != "$src" ]] && EXCLUDES+=("$p")
  done < <(worktree_paths "$src")
  while IFS= read -r rel; do
    [[ -n $rel ]] && EXCLUDES+=("$src/$rel")
  done < <(git -C "$src" ls-files -s | awk '$1 == "160000" { sub(/^[^\t]*\t/, ""); print }'
           git -C "$src" config --get-all cowtree.exclude || true)

  log "cloning $src -> $dst"
  if ! emit_pairs "$src" "$dst" | clone_pairs; then
    log "clone failed part-way; falling back to a normal checkout for tracked files"
  fi

  # Start from the source's index: clones keep mtimes and sizes, so with
  # checkStat=minimal git sees every unchanged file as clean without rehashing,
  # and `reset --hard` only rewrites files that differ from the new HEAD.
  local fast=(-c core.checkStat=minimal -c core.trustCtime=false)
  local src_idx dst_idx
  src_idx=$(git -C "$src" rev-parse --path-format=absolute --git-path index)
  dst_idx=$(git -C "$dst" rev-parse --path-format=absolute --git-path index)
  [[ -f $src_idx ]] && cp "$src_idx" "$dst_idx"
  git -C "$dst" "${fast[@]}" update-index -q --refresh >/dev/null 2>&1 || true
  git -C "$dst" "${fast[@]}" reset -q --hard
  [[ -n ${COWTREE_KEEP_UNTRACKED:-} ]] || git -C "$dst" clean -fdq
}

cmd_add() {
  local src before after p dst=""
  src=${COWTREE_SOURCE:-$(git rev-parse --show-toplevel 2>/dev/null)} \
    || die "run inside a work tree, or set COWTREE_SOURCE"

  before=$(worktree_paths "$src")
  git -C "$PWD" worktree add --no-checkout "$@" >&2
  after=$(worktree_paths "$src")
  while IFS= read -r p; do
    grep -qxF -- "$p" <<<"$before" || { dst=$p; break; }
  done <<<"$after"
  [[ -n $dst ]] || return 0  # e.g. --help, or nothing was created

  populate "$src" "$dst"
  # git skips post-checkout with --no-checkout; run it like a normal add would.
  git -C "$dst" hook run --ignore-missing post-checkout -- \
    0000000000000000000000000000000000000000 "$(git -C "$dst" rev-parse HEAD)" 1 >&2 2>/dev/null || true
  log "ready: $dst"
}

case "${1:-}" in
  add)             shift; cmd_add "$@" ;;
  -h|--help|"")    sed -n '2,/^set -euo/{/^set -euo/d;s/^# \{0,1\}//;p;}' "$0" ;;
  *)               exec git worktree "$@" ;;
esac
