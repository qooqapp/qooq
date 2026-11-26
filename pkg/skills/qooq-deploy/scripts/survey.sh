#!/usr/bin/env bash
# What is already deployed on this machine, grouped the way the user thinks about it:
# apps running by themselves, and apps deployed together as a stack.
#
# Usage: survey.sh
#
# Read this before deciding whether a new project is standalone or part of a stack,
# and before addressing an existing component from a new one. It changes nothing.
#
# Output: DAEMON, then STACKS / COMPONENTS / TARGETS. An empty machine says "(none)"
# rather than printing a blank section.

set -uo pipefail

. "$(cd "$(dirname "$0")" && pwd)/resolve-qooq.sh"
qooq_resolve || { echo "FAIL: $QOOQ_RESOLVE_ERROR" >&2; exit 1; }
ADDR="${QOOQ_ADDR:-http://127.0.0.1:7777}"
WADDR="${QOOQ_WATCHER_ADDR:-http://127.0.0.1:7778}"

if ! "$QOOQ_CLI" system health --addr "$ADDR" >/dev/null 2>&1; then
  echo "DAEMON: down"
  echo "Nothing is deployed yet, or qooqd is not running — qooq-env.sh starts it."
  exit 0
fi
echo "DAEMON: up"

# A listing is "empty" when it is blank or only the header row survived.
show() {
  local label="$1" body="$2"
  echo
  echo "$label"
  if [ -z "${body//[[:space:]]/}" ] || [ "$(printf '%s\n' "$body" | wc -l | tr -d ' ')" -le 1 ]; then
    echo "  (none)"
  else
    printf '%s\n' "$body" | sed 's/^/  /'
  fi
}

show "STACKS"     "$("$QOOQ_CLI" stack ls --watcher-addr "$WADDR" 2>/dev/null || true)"
show "COMPONENTS" "$("$QOOQ_CLI" component ls --addr "$ADDR" 2>/dev/null || true)"
show "TARGETS"    "$("$QOOQ_CLI" watch ls --watcher-addr "$WADDR" 2>/dev/null || true)"

echo
echo "NAMESPACE 'default' means standalone. Any other namespace is the name of the stack"
echo "that owns those components, and they deploy together in dependency order."
