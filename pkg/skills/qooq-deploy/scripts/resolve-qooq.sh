#!/usr/bin/env bash
# Find the qooq CLI. Sourced by the other scripts, not run on its own.
#
#   . "$(dirname "$0")/resolve-qooq.sh"
#   qooq_resolve || { echo "$QOOQ_RESOLVE_ERROR"; exit 1; }
#   "$QOOQ_CLI" component ls
#
# Sets QOOQ_CLI, and QOOQ_BIN_DIR when the daemons come from a source checkout
# (`qooq up` needs that to find qooqd and qooq-watcher, which are built into a
# different repository's bin/).
#
# The order matters more than it looks. Anyone who installed qooq has the three
# binaries together on their PATH and no source anywhere; the author of this
# skill has two checkouts and often no installed copy at all. Looking on PATH
# first means the common case costs one lookup, and the development machine
# still works because it falls through.

# qooq_resolve prints nothing on success. On failure it sets QOOQ_RESOLVE_ERROR
# and returns 1, leaving the decision (install? tell the user? give up?) to the
# caller — only qooq-env.sh is entitled to install things.
qooq_resolve() {
  QOOQ_RESOLVE_ERROR=""

  # 1. Told explicitly. Anything else would make the variable a lie.
  if [ -n "${QOOQ_CLI:-}" ] && [ -x "$QOOQ_CLI" ]; then
    return 0
  fi

  # 2. Installed: the normal case for everyone who is not developing qooq.
  local found
  if found=$(command -v qooq 2>/dev/null) && [ -x "$found" ]; then
    QOOQ_CLI="$found"
    return 0
  fi

  # 3. A source checkout, built. Two repositories side by side, because
  #    qooq-cli's go.mod replaces qooq with ../qooq.
  local repo="${QOOQ_SRC:-$HOME/projects/qooqapp/qooq}"
  local cli_repo="${QOOQ_CLI_SRC:-$HOME/projects/qooqapp/qooq-cli}"
  if [ -x "$cli_repo/bin/qooq" ]; then
    QOOQ_CLI="$cli_repo/bin/qooq"
    [ -d "$repo/bin" ] && export QOOQ_BIN_DIR="$repo/bin"
    return 0
  fi

  # 4. A checkout that has not been built yet: buildable, but building is a
  #    decision for qooq-env.sh, not for a script that was asked for a port.
  if [ -d "$cli_repo" ] && [ -d "$repo" ]; then
    QOOQ_RESOLVE_ERROR="found source checkouts at ${repo} and ${cli_repo} but no built binary; run qooq-env.sh first (it builds them)"
    return 1
  fi

  QOOQ_RESOLVE_ERROR="qooq is not installed (nothing named qooq on PATH, and no source checkout at ${repo}); run the skill's qooq-env.sh, which installs it"
  return 1
}

# qooq_installer prints the path to install.sh when one is reachable locally.
#
# Note for callers: install.sh reads QOOQ_REPO too, and means something else by
# it — the GitHub slug to download from, not a directory. Anything invoking it
# has to keep this skill's QOOQ_SRC out of its environment, or a path ends up
# where a repository name belongs and the error talks about a release that was
# never requested.
#
# Installed as a Claude Code plugin, the whole repository is on disk and
# CLAUDE_PLUGIN_ROOT points at it; no other agent sets anything equivalent, so
# that candidate is simply absent there. Installed with `qooq skill install`,
# only this directory was copied — but that path required the CLI to already
# exist, so nothing needs installing and the absence never matters.
qooq_installer() {
  local candidates=(
    "${CLAUDE_PLUGIN_ROOT:-}/install.sh"
    "${QOOQ_SRC:-$HOME/projects/qooqapp/qooq}/install.sh"
  )
  local c
  for c in "${candidates[@]}"; do
    case "$c" in /install.sh) continue ;; esac
    [ -f "$c" ] && { printf '%s' "$c"; return 0; }
  done
  return 1
}
