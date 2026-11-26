#!/usr/bin/env bash
# Wire a project up to local GitOps: git init (if needed) -> commit -> push to a
# local bare repo.
#
# Usage: gitops-setup.sh <project-dir> <component-name> [--commit-only]
#   --commit-only  stop after the commit (init + large-file guard + commit): no bare
#                  repo, no "qooq" remote, no push. cloud-deploy.sh uses this so the
#                  qooq Cloud path shares the one guard against large files in history.
#
# Safety position (important):
#   - The user's existing origin remote is never touched. The local bare repo is
#     always attached to a remote named "qooq", so their code never ends up on
#     their GitHub.
#   - A pre-push hook is installed so that pushing anywhere else (origin) also
#     pushes the same branches to "qooq". Without it, a user who pushes to GitHub
#     out of habit sees nothing redeploy and nothing say why. The hook leaves
#     origin's configuration alone, never blocks the push it runs for, and is
#     not installed over a pre-push hook the project already has (or when
#     core.hooksPath points the hooks somewhere else). QOOQ_NO_PUSH_HOOK=1 skips it.
#   - Only files already in the working tree get committed, and a new file over
#     QOOQ_LARGE_FILE_MB (default 50) stops the script rather than entering the
#     history, where removing it would mean a rewrite. QOOQ_ALLOW_LARGE_FILES=1
#     proceeds anyway.
#
# Output: one line per step, ending with "BARE=<bare repo path>" and "COMMIT=<sha>".

set -uo pipefail

DIR="${1:?usage: gitops-setup.sh <project-dir> <component-name>}"
NAME="${2:?usage: gitops-setup.sh <project-dir> <component-name>}"
QOOQ_HOME="${QOOQ_HOME:-$HOME/.qooq}"
BARE="$QOOQ_HOME/repos/$NAME.git"

fail() { echo "FAIL: $1"; exit 1; }

cd "$DIR" || fail "could not enter $DIR"
mkdir -p "$QOOQ_HOME/repos" || fail "could not create $QOOQ_HOME/repos"

# --- git init (if this is not a repo yet) ---
if [ ! -d .git ]; then
  git init -q -b main || fail "git init failed"
  echo "INIT: created a git repo in $DIR (branch main)"
else
  echo "OK: already a git repo"
fi

# Branch: whatever HEAD already points at, defaulting to main before the first commit
BRANCH=$(git symbolic-ref --short HEAD 2>/dev/null || echo main)

# --- commit the working tree ---
if [ -n "$(git status --porcelain)" ]; then
  git add -A || fail "git add failed"

  # A commit is forever: a large blob staged by "git add -A" stays in the
  # history of this repo and of the bare repo pushed to, and getting it back
  # out means rewriting history. That is not something to discover later, and
  # it is not something this script can ask about -- it runs unattended.
  #
  # So it stops instead, names the files, and gives both ways forward. The
  # stop costs one command; the blob costs a history rewrite. Deliberately not
  # a warning: a warning here would be read after the commit already happened.
  LARGE_MB="${QOOQ_LARGE_FILE_MB:-50}"
  large=$(git diff --cached --name-only --diff-filter=A -z 2>/dev/null \
    | while IFS= read -r -d '' f; do
        [ -f "$f" ] || continue
        sz=$(wc -c < "$f" 2>/dev/null | tr -d ' ')
        [ -n "${sz}" ] || continue
        if [ "${sz}" -gt $(( LARGE_MB * 1024 * 1024 )) ]; then
          printf '  %s (%s MB)\n' "$f" "$(( sz / 1024 / 1024 ))"
        fi
      done)
  if [ -n "${large}" ] && [ -z "${QOOQ_ALLOW_LARGE_FILES:-}" ]; then
    git reset -q
    echo "these files are new and larger than ${LARGE_MB} MB:" >&2
    echo "${large}" >&2
    echo "" >&2
    echo "committing them would put them in this repo's history permanently." >&2
    echo "either add them to .gitignore, or re-run with QOOQ_ALLOW_LARGE_FILES=1" >&2
    echo "if they really belong in the repo. nothing has been committed." >&2
    exit 1
  fi

  git -c user.email="${GIT_AUTHOR_EMAIL:-qooq@localhost}" \
      -c user.name="${GIT_AUTHOR_NAME:-qooq deploy}" \
      commit -q -m "deploy: $NAME via qooq" || fail "commit failed"
  echo "COMMITTED: working tree changes committed"
else
  if ! git rev-parse HEAD >/dev/null 2>&1; then
    fail "there is nothing in the working tree to commit"
  fi
  echo "OK: working tree is clean, using the existing commit"
fi

if [ "${3:-}" = "--commit-only" ]; then
  echo "BRANCH=$BRANCH"
  echo "COMMIT=$(git rev-parse HEAD)"
  exit 0
fi

# --- bare repo (create if absent) ---
if [ ! -d "$BARE" ]; then
  git init -q --bare "$BARE" || fail "could not create the bare repo: $BARE"
  echo "CREATED: local bare repo $BARE"
else
  echo "OK: bare repo already exists at $BARE"
fi

# --- remote: always "qooq", never origin ---
if git remote get-url qooq >/dev/null 2>&1; then
  git remote set-url qooq "$BARE" || fail "could not update remote qooq"
else
  git remote add qooq "$BARE" || fail "could not add remote qooq"
fi
echo "OK: remote qooq -> ${BARE} (origin untouched)"

# --- pre-push hook: a push to origin also updates qooq ---
HOOK_MARK="# qooq-deploy: mirror pushes to the qooq remote"
install_push_hook() {
  if [ -n "${QOOQ_NO_PUSH_HOOK:-}" ]; then
    echo "SKIPPED: pre-push hook (QOOQ_NO_PUSH_HOOK is set) -- deploy with: git push qooq"
    return
  fi
  if [ -n "$(git config --get core.hooksPath)" ]; then
    echo "SKIPPED: pre-push hook (core.hooksPath is set; this project manages its own hooks) -- deploy with: git push qooq"
    return
  fi
  local hook
  hook="$(git rev-parse --git-path hooks)/pre-push"
  if [ -f "$hook" ] && ! grep -qF "$HOOK_MARK" "$hook"; then
    echo "SKIPPED: pre-push hook (the project already has one at ${hook}) -- deploy with: git push qooq"
    return
  fi
  mkdir -p "$(dirname "$hook")"
  cat >"$hook" <<'HOOK'
#!/bin/sh
# qooq-deploy: mirror pushes to the qooq remote
#
# Installed by qooq's gitops-setup.sh. A push to any remote other than "qooq"
# also pushes the same branches to "qooq" (the local repo qooq deploys from),
# so pushing to GitHub as usual redeploys too. It never fails the push it runs
# for. Delete this file to stop it; deploy with `git push qooq` from then on.
[ "$1" = qooq ] && exit 0
git remote get-url qooq >/dev/null 2>&1 || exit 0
zero=0000000000000000000000000000000000000000
while read -r local_ref local_sha _remote_ref _remote_sha; do
  case "$local_ref" in refs/heads/*) ;; *) continue ;; esac
  [ "$local_sha" = "$zero" ] && continue
  if git push -q --force --no-verify qooq "$local_sha:$local_ref" 2>/dev/null; then
    echo "qooq: also pushed ${local_ref#refs/heads/} to the qooq remote" >&2
  else
    echo "qooq: could not push ${local_ref#refs/heads/} to the qooq remote -- run: git push qooq" >&2
  fi
done
exit 0
HOOK
  chmod +x "$hook" || { echo "WARN: could not make ${hook} executable -- deploy with: git push qooq"; return; }
  echo "OK: pre-push hook installed (a push to origin also updates qooq)"
}
install_push_hook

# --- push ---
git push -q --force qooq "$BRANCH":"$BRANCH" || fail "push to the local bare repo failed"
SHA=$(git rev-parse HEAD)
echo "PUSHED: $BRANCH @ ${SHA:0:7}"

# --- upstream: a new project's plain `git push` must go somewhere ---
# A project created for this deploy has no origin and no upstream, so the
# `git push` the report tells the user to run would fail with "no upstream
# branch". Point the branch at qooq then. A branch that already tracks
# something (its own origin) is left exactly as it was.
if ! git rev-parse --abbrev-ref --symbolic-full-name "@{u}" >/dev/null 2>&1; then
  if git remote get-url origin >/dev/null 2>&1; then
    echo "NOTE: $BRANCH has no upstream -- deploy with: git push qooq"
  elif git branch -q --set-upstream-to="qooq/$BRANCH" "$BRANCH" 2>/dev/null; then
    echo "OK: $BRANCH now tracks qooq/$BRANCH (a plain git push redeploys)"
  else
    echo "NOTE: could not set an upstream -- deploy with: git push qooq"
  fi
fi

echo "BARE=$BARE"
echo "BRANCH=$BRANCH"
echo "COMMIT=$SHA"
