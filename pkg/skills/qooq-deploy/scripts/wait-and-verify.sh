#!/usr/bin/env bash
# Wait for the deployment to finish, probe the endpoint for real, and print the
# address that can be handed to the user.
#
# Usage: wait-and-verify.sh <component-name> [--target <watcher target>] [--path <path>]
#                            [--trigger [--force]]
#   --target   in GitOps mode, wait for the build first (and print why it failed)
#   --path     the path to curl, default /
#   --trigger  trigger the build from here instead of before calling this
#   --force    with --trigger: rebuild even if the remote has not changed
#
# Pass --trigger whenever there is a build to wait for. It is the only way this
# script can tell the build it is waiting for from the one before it: a build
# status of "success" and a matching deployed image describe the *previous*
# deployment just as well as the new one, right up until the new build starts.
# Reading the generation counter before triggering is what removes that
# ambiguity -- see the wait loop below.
#
# With --trigger and no --force, a remote that is already deployed is not
# rebuilt: the script prints NOTHING_TO_BUILD within one sweep and goes on to
# verify the version that is running (exit 0 if it answers).
#
# Output: status lines, then ENDPOINT_LAN / ENDPOINT_LOCAL / BODY.
# Exit code: 0 = component Running and the endpoint answered; 1 = failed (a FAIL: line,
#            and the container's last output when it has any);
#            2 = blocked on missing inputs (a question, not a failure).

set -uo pipefail

NAME="${1:?usage: wait-and-verify.sh <component-name> [--target T] [--path P]}"; shift
TARGET=""
VERIFY_PATH="/"
DO_TRIGGER=""
FORCE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --target)  TARGET="$2"; shift 2 ;;
    --path)    VERIFY_PATH="$2"; shift 2 ;;
    --trigger) DO_TRIGGER=1; shift ;;
    --force)   FORCE=1; shift ;;
    *) shift ;;
  esac
done

. "$(cd "$(dirname "$0")" && pwd)/resolve-qooq.sh"
qooq_resolve || { echo "FAIL: $QOOQ_RESOLVE_ERROR" >&2; exit 1; }
ADDR="${QOOQ_ADDR:-http://127.0.0.1:7777}"
WADDR="${QOOQ_WATCHER_ADDR:-http://127.0.0.1:7778}"

# --- talking to the daemons ---
# Both daemons may require a bearer token. The variables are the ones the qooq
# CLI reads (QOOQ_TOKEN for qooqd, QOOQ_WATCHER_TOKEN for the watcher), so this
# script authenticates exactly when the CLI would; unset means no header, for a
# daemon started without auth.
#
# The header goes to curl through a config on a file descriptor, not on the
# command line, so the token does not show up in `ps`.
auth_config() {
  [ -n "$1" ] || return 0
  local t="${1//\\/\\\\}"; t="${t//\"/\\\"}"
  printf 'header = "Authorization: Bearer %s"\n' "$t"
}

# fetch <qooqd|watcher> <url> [curl args...] -- sets RESP to the body.
#
# Anything but a 2xx stops the script. Before this, a 401 parsed as an empty
# list: the baseline generation came out "unknown", the endpoint came out
# empty, and the script went on to report on state it had never read.
# It runs in the current shell (never inside $(...)), so the exit is real.
RESP=""
fetch() {
  local who="$1" url="$2" tok var out code; shift 2
  if [ "$who" = watcher ]; then tok="${QOOQ_WATCHER_TOKEN:-}"; var=QOOQ_WATCHER_TOKEN
  else tok="${QOOQ_TOKEN:-}"; var=QOOQ_TOKEN; fi
  out=$(curl -s -m 10 -w '\n%{http_code}' -K <(auth_config "$tok") "$@" "$url" 2>/dev/null)
  code="${out##*$'\n'}"
  RESP="${out%$'\n'*}"
  case "$code" in 2??) return 0 ;; esac
  case "$code" in
    000) echo "FAIL: cannot reach the ${who} at ${url%%/v1/*} (is it running?)" >&2 ;;
    401|403)
      if [ -n "$tok" ]; then
        echo "FAIL: the ${who} rejected ${var} (HTTP ${code}) -- it does not match the token the ${who} was started with" >&2
      else
        echo "FAIL: the ${who} requires a token (HTTP ${code}) -- set ${var}" >&2
      fi ;;
    *) echo "FAIL: the ${who} answered HTTP ${code} for ${url}: ${RESP:0:200}" >&2 ;;
  esac
  exit 1
}

# --- what the daemon and the watcher each currently believe ---
# status, revision and generation -- the fields the wait is decided on.
# build_generation increments when a build *starts*, which is the only signal
# that separates "this build finished" from "the last one did".
# Then: next_check_at as epoch seconds (0 if unknown), and 1 if automatic
# deploys are paused (deploy_paused), else 0.
# Sets TARGET_STATE (empty when the target does not exist).
TARGET_STATE=""
target_state() {
  fetch watcher "$WADDR/v1/targets"
  TARGET_STATE=$(printf '%s' "$RESP" | /usr/bin/python3 -c '
import json,sys,re
from datetime import datetime
def epoch(v):
    # RFC 3339 from Go: up to nine fractional digits and maybe a Z, neither of
    # which python 3.9 fromisoformat accepts.
    if not v: return 0
    v=re.sub(r"\.\d+","",v).replace("Z","+00:00")
    try: return int(datetime.fromisoformat(v).timestamp())
    except Exception: return 0
try: ts=json.load(sys.stdin)
except Exception: sys.exit(0)
for t in ts:
    if t.get("name")==sys.argv[1]:
        print(t.get("last_build_status","") or "-",
              t.get("last_build_revision","") or "-",
              t.get("build_generation",0) or 0,
              epoch(t.get("next_check_at")),
              1 if t.get("deploy_paused") else 0)
        break
' "$1" 2>/dev/null)
}

# Sets COMPONENT_STATE to "phase image" (empty when it does not exist yet).
COMPONENT_STATE=""
component_state() {
  fetch qooqd "$ADDR/v1/components"
  COMPONENT_STATE=$(printf '%s' "$RESP" | /usr/bin/python3 -c '
import json,sys
try: cs=json.load(sys.stdin)
except Exception: sys.exit(0)
for c in cs:
    if c.get("name")==sys.argv[1]:
        print(c.get("phase","") or "-", c.get("image","") or "-")
        break
' "$1" 2>/dev/null)
}

# deployed_matches <revision> <image> -- is the running container built from
# this revision?
#
# The tag qooq builds is <component>:<first 7 of the commit>. A revision that
# is not a git sha (image-mode targets carry a digest) is not comparable, and
# the answer is "cannot tell" rather than "no" -- refusing to proceed on a
# target this check was never meant to cover would be worse than the bug.
deployed_matches() {
  case "$1" in
    -|"") return 0 ;;
    *[!0-9a-fA-F]*) return 0 ;;
  esac
  [ "${#1}" -ge 7 ] || return 0
  case "$2" in
    *:"${1:0:7}") return 0 ;;
    *) return 1 ;;
  esac
}

# crash_log -- the last lines the container printed. Every failure that is about
# the app (not about qooq) ends with these: they are almost always the answer,
# and without them the reader has to know to run `docker logs` themselves.
crash_log() {
  local out
  out=$("$QOOQ_CLI" component logs "$NAME" --tail 20 --addr "$ADDR" 2>/dev/null)
  [ -n "$out" ] || return 0
  echo "--- last output of ${NAME} ---"
  printf '%s\n' "$out"
}

# --- GitOps: wait for the build that was just triggered, not the last one ---
if [ -n "$TARGET" ]; then
  # Baseline the generation BEFORE triggering. Everything below turns on the
  # difference: until the counter moves, every field still describes the
  # previous deployment, and a status of "success" read in that window is the
  # single most misleading thing this script can report.
  BASE_GEN=-1
  if [ -n "$DO_TRIGGER" ]; then
    target_state "$TARGET"
    [ -n "$TARGET_STATE" ] || { echo "FAIL: the watcher has no target named ${TARGET}" >&2; exit 1; }
    read -r _bs _br BASE_GEN _nc paused <<<"$TARGET_STATE"
    # A target with automatic deploys paused is never picked up by a plain
    # trigger (only an explicit rebuild gets past the pause), so waiting on one
    # is ten minutes spent on a build that cannot start.
    if [ "${paused:-0}" = 1 ] && [ -z "$FORCE" ]; then
      echo "FAIL: automatic deploys are paused for ${TARGET}, so a trigger without --force will not build" >&2
      echo "  resume them (qooq watch resume ${TARGET}) or re-run with --force" >&2
      exit 1
    fi
    TRIGGER_AT=$(date +%s)
    : "${BASE_GEN:=-1}"
    url="$WADDR/v1/targets/$TARGET/trigger"
    [ -n "$FORCE" ] && url="$url?force=1"
    fetch watcher "$url" -X POST
    echo "TRIGGERED: ${TARGET} (was generation ${BASE_GEN})"
  fi

  echo "waiting for the build (up to 10 minutes)..."
  status=""; revision="-"; gen=0; image="-"; phase="-"; started=""; nothing=""
  for _ in $(seq 1 120); do
    target_state "$TARGET"
    read -r status revision gen checked _paused <<<"$TARGET_STATE"
    component_state "$NAME"
    read -r phase image <<<"$COMPONENT_STATE"
    : "${gen:=0}"; : "${checked:=0}"

    # Has the build we are waiting for started yet? With no baseline (the
    # caller triggered elsewhere) this cannot be known, so the question is
    # skipped rather than guessed at.
    if [ "$BASE_GEN" -ge 0 ] && [ "$gen" -le "$BASE_GEN" ]; then
      # Or was there nothing to build? The watcher moves next_check_at past
      # the trigger only once it has finished looking at this target, and a
      # build bumps the generation before that. So a generation that has not
      # moved, with next_check_at past the trigger, means the watcher read the
      # remote *after* the trigger and found it already deployed. A push made
      # before the trigger is therefore always seen; this cannot mistake
      # "not fetched yet" for "nothing new".
      if [ "$checked" -gt $((TRIGGER_AT + 1)) ]; then
        case "$status" in
          # The check itself reported something (the remote could not be
          # read, or inputs are missing): give that verdict below.
          failed|needs_input) ;;
          *)
            echo "NOTHING_TO_BUILD: the remote at ${revision:0:12} is already deployed; pass --force to rebuild"
            echo "  verifying the version that is running now"
            nothing=1
            break ;;
        esac
      else
        [ -z "$started" ] && echo "  build not started yet (generation still ${gen})"
        sleep 5
        continue
      fi
    fi
    started=1

    case "$status" in
      success)
        # The build finished. It is still not done until the running container
        # is the one it produced -- the old container lives on for the seconds
        # it takes to swap, and reporting on it is how a successful deploy gets
        # called a failure.
        if deployed_matches "$revision" "$image"; then
          echo "BUILD: success (revision ${revision}, generation ${gen})"
          break
        fi
        status="swapping"
        ;;
      needs_input)
        # Not a failure -- it is a question nobody has asked yet. The caller asks the
        # user, runs set-input, and re-runs this script.
        echo "BUILD: needs_input"
        echo "--- these values are missing ---"
        fetch watcher "$WADDR/v1/targets"
        printf '%s' "$RESP" | /usr/bin/python3 -c '
import json,sys
for t in json.load(sys.stdin):
    if t.get("name")!=sys.argv[1]: continue
    missing=set(t.get("missing_inputs") or [])
    for i in (t.get("inputs") or []):
        if i["name"] in missing:
            line="MISSING_INPUT="+i["name"]
            if i.get("description"): line+=" | "+i["description"]
            if i.get("example"):     line+=" | e.g. "+i["example"]
            print(line)
' "$TARGET" 2>/dev/null
        exit 2 ;;
      failed)
        # The watcher's message already carries the new version's output when
        # it was the version that failed (it rolls back and quotes it).
        echo "FAIL: the build or deploy failed"
        echo "--- why it failed ---"
        fetch watcher "$WADDR/v1/targets"
        printf '%s' "$RESP" | /usr/bin/python3 -c '
import json,sys
for t in json.load(sys.stdin):
    if t.get("name")==sys.argv[1]: print(t.get("last_build_error","(none)"))
' "$TARGET" 2>/dev/null
        # The one-line error is a summary; the compiler error or missing
        # package is in the build output. -f, not fetch: a target that failed
        # before building has no output (404), and that is not a second failure.
        blog=$(curl -sf -m 10 -K <(auth_config "${QOOQ_WATCHER_TOKEN:-}") "$WADDR/v1/targets/$TARGET/build-log" 2>/dev/null)
        if [ -n "$blog" ]; then
          echo "--- last 40 lines of the build output (all of it: qooq watch build-log ${TARGET}) ---"
          printf '%s\n' "$blog" | tail -40
        fi
        exit 1 ;;
    esac
    sleep 5
  done

  if [ "$status" = swapping ]; then
    echo "BUILD: the build succeeded but the new version never came up"
    echo "  built:    ${revision} (generation ${gen})"
    echo "  deployed: ${image}   <- still the old build"
    exit 1
  fi
  if [ -z "$nothing" ] && [ "$BASE_GEN" -ge 0 ] && [ -z "$started" ]; then
    echo "BUILD: the build never started (generation stayed at ${BASE_GEN})"
    echo "  the watcher may not be polling this target -- check: qooq watch get ${TARGET}"
    exit 1
  fi
  case "$nothing$status" in
    1*|success) ;;
    *) echo "BUILD: timed out (last status=${status:-unknown})"; exit 1 ;;
  esac
fi

# --- wait for the component to reach Running ---
# Exited is not a verdict here. An app with restart: on-failure is *expected* to
# go Exited -> Running a few times while whatever it connects to finishes
# starting, so the only terminal answers are "Running" and "ran out of time".
echo "waiting for the component to start..."
phase=""
for _ in $(seq 1 60); do
  component_state "$NAME"
  read -r phase image <<<"$COMPONENT_STATE"
  [ "$phase" = Running ] && break
  sleep 2
done

if [ "$phase" != Running ]; then
  echo "FAIL: the component did not reach Running (currently=${phase:-not found})"
  if [ -n "$TARGET" ]; then
    target_state "$TARGET"
    read -r bstatus brev bgen _bc _bp <<<"$TARGET_STATE"
    echo "  build:    ${bstatus:-unknown} (revision ${brev:--}, generation ${bgen:-0})"
    echo "  deployed: ${image:--}"
    if ! deployed_matches "${brev:--}" "${image:--}"; then
      echo "  NOTE: these disagree -- the new version is still being swapped in,"
      echo "        so everything printed below describes the OLD deployment."
    fi
  fi
  echo "--- component get ---"
  "$QOOQ_CLI" component get "$NAME" --addr "$ADDR" 2>/dev/null | grep -E '^(phase|image|last_error|restart)'
  crash_log
  exit 1
fi
echo "PHASE: Running"

# --- get the host port and the endpoint the daemon reports ---
# The endpoint is built by qooqd from its own advertise host; this script does not
# guess the LAN IP. Only the daemon knows where the container is actually bound and
# what this machine's outward address is.
fetch qooqd "$ADDR/v1/components"
read -r HOSTPORT ENDPOINT <<EOF
$(printf '%s' "$RESP" | /usr/bin/python3 -c '
import json,sys
for c in json.load(sys.stdin):
    if c.get("name")!=sys.argv[1]: continue
    port=""
    for p in (c.get("ports") or []):
        if p.get("host_port"): port=str(p["host_port"]); break
    eps=c.get("endpoints") or []
    print(port, eps[0] if eps else "")
    break
' "$NAME" 2>/dev/null)
EOF

if [ -z "$HOSTPORT" ]; then
  echo "NOTE: this component publishes no port (a background service). It is running."
  echo "ENDPOINT_LAN="
  echo "ENDPOINT_LOCAL="
  echo "BODY="
  exit 0
fi

# Older qooqd versions do not report endpoints; fall back to the local address, as before.
[ -z "$ENDPOINT" ] && ENDPOINT="http://127.0.0.1:$HOSTPORT"

# --- probe for real, against the address the daemon reported, not localhost ---
# What has to be verified is that the URL reported to the user actually works.
# 127.0.0.1 would pass and prove nothing about the phone on the same network,
# which is what the user is going to try.
BODY=$(curl -s -m 8 "$ENDPOINT$VERIFY_PATH" 2>&1)
CODE=$(curl -s -m 8 -o /dev/null -w '%{http_code}' "$ENDPOINT$VERIFY_PATH" 2>/dev/null)

echo "HTTP: $CODE"
echo "ENDPOINT_LAN=$ENDPOINT"
echo "ENDPOINT_LOCAL=http://127.0.0.1:$HOSTPORT"
echo "BODY=${BODY:0:400}"

# Running a moment ago is not running now: a process that dies on its first
# request (or its first health probe) reads Running right up to that request.
# So look again after the probe, and treat a component that has gone down
# since as the failure it is.
component_state "$NAME"
read -r phase_after _ <<<"$COMPONENT_STATE"

# 000 (nothing answered) and 5xx (the app answered with its own error) are
# failures. 4xx stays a note: the service is up and that path has nothing on
# it, which only the caller can judge.
if [ "$CODE" = "000" ] || [ "${CODE:0:1}" = 5 ] || [ "${phase_after:-}" != Running ]; then
  if [ "$CODE" = "000" ]; then
    echo "FAIL: nothing answered at ${ENDPOINT}${VERIFY_PATH}"
  elif [ "${CODE:0:1}" = 5 ]; then
    echo "FAIL: the app answered HTTP ${CODE} at ${ENDPOINT}${VERIFY_PATH}"
  fi
  if [ "${phase_after:-}" != Running ]; then
    echo "FAIL: the component is no longer running (phase=${phase_after:-not found}) -- it went down after starting"
  fi
  crash_log
  exit 1
fi
case "$CODE" in 4??) echo "NOTE: HTTP ${CODE} -- the service is up, but ${VERIFY_PATH} has nothing on it" ;; esac
exit 0
