#!/usr/bin/env bash
# Make sure qooq is ready to run: docker is up, the CLI exists, qooqd and the
# watcher are running. Idempotent throughout — whatever is already running is
# left alone.
#
# Usage: qooq-env.sh [--no-watcher]
#   --no-watcher   only qooqd is needed (one-shot build mode does not use the watcher)
#
# Output: one line per check, "OK/STARTED/BUILT/INSTALLED/FAIL: detail", then
# READY or BLOCKED. Exit code: 0 = ready, 1 = blocked by something it cannot fix
# (docker not running, no way to obtain qooq).

set -uo pipefail

. "$(cd "$(dirname "$0")" && pwd)/resolve-qooq.sh"

QOOQ_HOME="${QOOQ_HOME:-$HOME/.qooq}"
NEED_WATCHER=1
[ "${1:-}" = "--no-watcher" ] && NEED_WATCHER=0

fail() { echo "FAIL: $1"; echo BLOCKED; exit 1; }

mkdir -p "$QOOQ_HOME" || fail "could not create $QOOQ_HOME"

# --- docker ---
if ! docker info >/dev/null 2>&1; then
  fail "docker is not running. Start Docker Desktop / OrbStack and retry — this step cannot be automated"
fi
echo "OK: docker is available"

# --- the CLI ---------------------------------------------------------------
# Three ways to end up with one, in the order that costs least.
if qooq_resolve; then
  echo "OK: qooq CLI at $QOOQ_CLI"
else
  repo="${QOOQ_SRC:-$HOME/projects/qooqapp/qooq}"
  cli_repo="${QOOQ_CLI_SRC:-$HOME/projects/qooqapp/qooq-cli}"

  if [ -d "$repo" ] && [ -d "$cli_repo" ]; then
    # A development machine: build rather than install, so the skill exercises
    # the code that is actually being worked on.
    for pair in "$repo:qooqd and qooq-watcher" "$cli_repo:qooq"; do
      dir="${pair%%:*}"; what="${pair#*:}"
      echo "BUILD: $what missing, running make build ($dir)"
      if ! (cd "$dir" && make build >/tmp/qooq-build.log 2>&1); then
        echo "--- make build output (last 20 lines) ---"
        tail -20 /tmp/qooq-build.log
        fail "$what failed to build, see the output above"
      fi
      echo "BUILT: $what"
    done
    qooq_resolve || fail "$QOOQ_RESOLVE_ERROR"
    echo "OK: qooq CLI at $QOOQ_CLI"
  else
    # Nothing local: install it. PREFIX is a user directory so this never asks
    # for a password — an install that stops to prompt for sudo in the middle
    # of an agent's turn is an install that hangs.
    installer=$(qooq_installer) || fail "qooq is not installed and install.sh is not on this machine.
Install it yourself and re-run: see https://github.com/qooqapp/qooq (install.sh), or set QOOQ_CLI to an existing binary"
    prefix="${QOOQ_INSTALL_PREFIX:-$HOME/.local/bin}"
    echo "INSTALL: qooq is not installed, running ${installer} (PREFIX=${prefix})"
    # QOOQ_REPO/QOOQ_CLI_REPO are install.sh's own variables (GitHub slugs) and
    # nothing to do with this skill's QOOQ_SRC; clear them so a stray value in
    # the environment cannot be read as a repository name.
    if ! env -u QOOQ_REPO -u QOOQ_CLI_REPO PREFIX="$prefix" sh "$installer" >/tmp/qooq-install.log 2>&1; then
      echo "--- install.sh output (last 20 lines) ---"
      tail -20 /tmp/qooq-install.log
      fail "install failed (output above). Check that this machine can reach github.com, or set QOOQ_DOWNLOAD_BASE to a mirror, then re-run"
    fi
    export PATH="$prefix:$PATH"
    qooq_resolve || fail "install.sh reported success but no qooq binary is on PATH under ${prefix}"
    echo "INSTALLED: qooq CLI at $QOOQ_CLI"
    echo "NOTE: ${prefix} is on PATH for this run only — add it to your shell profile to use qooq yourself"
  fi
fi

# --- daemons ---------------------------------------------------------------
# `qooq up` rather than starting the binaries here: it is the same code path the
# user's own `qooq up` takes, so there is one description of how qooqd is
# started (data under QOOQ_HOME, the docker endpoint resolved from the active
# context, the watcher's sqlite kept out of the user's project directory).
# Starting them by hand here would be a third one, and the one nobody runs by
# hand is the one that would be wrong.
up_args=()
[ "$NEED_WATCHER" = 0 ] && up_args+=(--no-watcher)

if ! out=$(QOOQ_HOME="$QOOQ_HOME" "$QOOQ_CLI" up "${up_args[@]}" 2>&1); then
  echo "--- qooq up output ---"
  echo "$out" | tail -20
  fail "the daemons would not start, see the output above"
fi
# `qooq up` pads its columns; squeeze that back down so this script's output
# stays one fact per line, the way the skill documents it.
echo "$out" | grep -E '^(started|already running)' \
  | sed -E 's/^already running +/OK: already running /; s/^started +/STARTED: /; s/ +/ /g'

echo READY
