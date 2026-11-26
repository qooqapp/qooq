#!/usr/bin/env bash
# Deploy a project to qooq Cloud instead of a local qooq.
#
# Usage:
#   cloud-deploy.sh detect
#   cloud-deploy.sh commit      <project-dir> <name>
#   cloud-deploy.sh remote      <project-dir>
#   cloud-deploy.sh create-repo <project-dir> <repo-name> private|public
#   cloud-deploy.sh ship        <project-dir> <name> [-n <namespace>]
#   cloud-deploy.sh upload      <project-dir> <name> [-n <namespace>] [--force]
#
# Two ways in: commit/remote/create-repo/ship build from GitHub (a push
# redeploys), upload sends the directory itself (no git; re-run to update).
#
# Each step is its own subcommand because the agent has to stop and ask the
# user between some of them (creating a GitHub repository, its visibility).
# Nothing here decides those for the user: `remote` reports and exits 3,
# `create-repo` only runs when the agent has already asked.
#
# Exit codes: 0 ok, 1 failed, 2 needs_input (a question for the user, not an
# error), 3 a decision or an action by the user is required first.
#
# This script never prints a token. Credentials live in the CLI's own store and
# in gh's; neither is read here.

set -euo pipefail

. "$(cd "$(dirname "$0")" && pwd)/resolve-qooq.sh"
HERE="$(cd "$(dirname "$0")" && pwd)"

die() { echo "FAIL: $1" >&2; exit 1; }

need_cli() {
  qooq_resolve || die "${QOOQ_RESOLVE_ERROR}"
}

# github_url prints the https form of a GitHub remote URL with any embedded
# credentials removed, or nothing when it is not a github.com URL.
github_url() {
  local u="$1"
  case "${u}" in
    git@github.com:*)          u="https://github.com/${u#git@github.com:}" ;;
    ssh://git@github.com/*)    u="https://github.com/${u#ssh://git@github.com/}" ;;
  esac
  case "${u}" in
    https://*github.com/*|http://*github.com/*) ;;
    *) return 0 ;;
  esac
  u=$(printf '%s' "${u}" | sed -e 's#//[^@/]*@#//#' -e 's#/*$##' -e 's#\.git$##')
  case "${u}" in
    https://github.com/*|http://github.com/*) printf '%s' "${u}" ;;
  esac
}

# ---------------------------------------------------------------- detect

cmd_detect() {
  if ! qooq_resolve; then
    echo "CLOUD=no_cli"
    echo "WHY=${QOOQ_RESOLVE_ERROR}"
    return 0
  fi
  if ! "${QOOQ_CLI}" project --help >/dev/null 2>&1; then
    echo "CLOUD=no_project_command"
    echo "WHY=this qooq CLI has no 'project' command, so it cannot talk to qooq Cloud"
    return 0
  fi
  local out rc=0
  out=$("${QOOQ_CLI}" auth status 2>&1) || rc=$?
  if [ "${rc}" -ne 0 ]; then
    echo "CLOUD=unreachable"
    echo "WHY=$(printf '%s' "${out}" | head -n 1)"
    return 0
  fi
  # auth status exits 0 even when signed out, so the text is the answer. Only
  # a signed-in, still-accepted login prints the "account" line.
  if printf '%s\n' "${out}" | grep -q '^  account  '; then
    echo "CLOUD=ready"
    printf '%s\n' "${out}" | sed -n -e '1s/^/ADDR=/p' -e 's/^  account  /ACCOUNT=/p'
  else
    echo "CLOUD=not_signed_in"
    echo "WHY=$(printf '%s' "${out}" | head -n 1)"
    echo "NEXT=run: qooq auth login   (opens a browser; the user must finish it)"
  fi
}

# ---------------------------------------------------------------- commit

cmd_commit() {
  local dir="${1:?usage: cloud-deploy.sh commit <project-dir> <name>}"
  local name="${2:?usage: cloud-deploy.sh commit <project-dir> <name>}"
  # The large-file guard lives in gitops-setup.sh and only there.
  "${HERE}/gitops-setup.sh" "${dir}" "${name}" --commit-only
}

# ---------------------------------------------------------------- remote

# remote_name prints the name of the GitHub remote of the repo in $PWD
# ("origin" preferred; the local-GitOps remote "qooq" never counts).
remote_name() {
  local r
  for r in origin $(git remote | grep -v -x -e origin -e qooq || true); do
    git config --get "remote.${r}.url" >/dev/null 2>&1 || continue
    [ -n "$(github_url "$(git config --get "remote.${r}.url")")" ] && { printf '%s' "${r}"; return 0; }
  done
  return 1
}

gh_state() {
  if ! command -v gh >/dev/null 2>&1; then echo "not_installed"; return; fi
  if gh auth status >/dev/null 2>&1; then echo "ready"; else echo "not_authenticated"; fi
}

cmd_remote() {
  local dir="${1:?usage: cloud-deploy.sh remote <project-dir>}"
  cd "${dir}" || die "could not enter ${dir}"
  # Not a git repository yet is the same answer as no remote: the user still
  # chooses between uploading it as it is and putting it on GitHub.
  if ! git rev-parse --git-dir >/dev/null 2>&1; then
    echo "REMOTE=none"
    echo "GIT=none"
    echo "GH=$(gh_state)"
    echo "NEXT=ask the user: upload the directory directly (no git needed), or put it on GitHub (then 'commit' and 'create-repo')"
    return 3
  fi
  local branch r url
  branch=$(git symbolic-ref --short HEAD 2>/dev/null || echo main)
  if r=$(remote_name); then
    url=$(github_url "$(git config --get "remote.${r}.url")")
    echo "REMOTE=github"
    echo "REMOTE_NAME=${r}"
    echo "REMOTE_URL=${url}"
    echo "BRANCH=${branch}"
    return 0
  fi
  echo "BRANCH=${branch}"
  if [ -n "$(git remote | grep -v -x qooq || true)" ]; then
    echo "REMOTE=not_github"
    echo "WHY=the remotes here are not on github.com; qooq Cloud builds from a GitHub URL"
  else
    echo "REMOTE=none"
  fi
  echo "GH=$(gh_state)"
  echo "NEXT=ask the user: upload the directory directly (no git needed), or put it on GitHub (creating a repo needs their yes, and private or public)"
  return 3
}

# ---------------------------------------------------------------- create-repo

cmd_create_repo() {
  local dir="${1:?usage: cloud-deploy.sh create-repo <project-dir> <repo-name> private|public}"
  local repo="${2:?usage: cloud-deploy.sh create-repo <project-dir> <repo-name> private|public}"
  local vis="${3:?usage: cloud-deploy.sh create-repo <project-dir> <repo-name> private|public}"
  case "${vis}" in private|public) ;; *) die "visibility must be private or public, not '${vis}'" ;; esac
  cd "${dir}" || die "could not enter ${dir}"
  git rev-parse HEAD >/dev/null 2>&1 || die "there is no commit yet; run 'commit' first"
  if remote_name >/dev/null; then die "this repo already has a GitHub remote; nothing to create"; fi
  case "$(gh_state)" in
    not_installed)
      echo "GH=not_installed"
      echo "NEXT=tell the user: create an empty repo on github.com (${vis}), then run:"
      echo "NEXT=  git remote add origin https://github.com/<owner>/${repo}.git && git push -u origin HEAD"
      return 3 ;;
    not_authenticated)
      echo "GH=not_authenticated"
      echo "NEXT=tell the user to run 'gh auth login' in their terminal (interactive), then retry"
      return 3 ;;
  esac
  # --source . --push creates the remote named origin and pushes the current branch.
  gh repo create "${repo}" "--${vis}" --source . --remote origin --push \
    || die "gh repo create failed (name taken? no permission?)"
  cmd_remote "${dir}"
}

# ---------------------------------------------------------------- ship

# classify reads the combined output of a failed CLI call and prints hints the
# agent can act on. The CLI's own message has already been shown.
classify() {
  local text="$1"
  if printf '%s' "${text}" | grep -qi -e 'not signed in' -e 'not accepted'; then
    echo "HINT=not_signed_in: ask the user to run 'qooq auth login', then retry"
  fi
  if printf '%s' "${text}" | grep -qi \
      -e 'authentication required' -e 'could not read username' -e 'repository not found' \
      -e 'github_' -e 'github app' -e 'terminal prompts disabled' \
      -e 'authorization failed' -e 'invalid credentials'; then
    echo "HINT=github_access: qooq Cloud cannot read this repo (it is probably private)."
    echo "HINT=  tell the user: follow the link in the message above (or the GitHub access page on the qooq Cloud website) to grant access to this repo, then re-run ship."
    echo "HINT=  (a public repo needs nothing.)"
  fi
}

# json_field prints the first string value of a pretty-printed JSON key.
json_field() { sed -n "s/^ *\"$1\": *\"\(.*\)\",\{0,1\}\$/\1/p" | head -n 1; }

# json_endpoints prints the strings inside the "endpoints" array, one per line.
json_endpoints() {
  awk '/"endpoints": *\[/ {on=1; next} on && /\]/ {exit} on {gsub(/^[ \t]*"|",?[ \t]*$/, ""); if (length($0)) print}'
}

cmd_ship() {
  local dir="${1:?usage: cloud-deploy.sh ship <project-dir> <name> [-n <namespace>]}"
  local name="${2:?usage: cloud-deploy.sh ship <project-dir> <name> [-n <namespace>]}"
  shift 2
  local ns="default"
  while [ $# -gt 0 ]; do
    case "$1" in
      -n|--namespace) ns="${2:?-n needs a value}"; shift 2 ;;
      *) die "unknown argument: $1" ;;
    esac
  done
  need_cli
  "${QOOQ_CLI}" project --help >/dev/null 2>&1 || die "this qooq CLI has no 'project' command"

  cd "${dir}" || die "could not enter ${dir}"
  git rev-parse HEAD >/dev/null 2>&1 || die "no commit yet; run 'commit' first"
  if [ -n "$(git status --porcelain)" ]; then
    die "the working tree has uncommitted changes; run 'commit' first (qooq Cloud builds what is pushed, not what is on disk)"
  fi

  local rname url branch
  if ! rname=$(remote_name); then
    echo "REMOTE=none"
    echo "NEXT=no GitHub remote: run 'remote', ask the user, and create one first"
    return 3
  fi
  url=$(github_url "$(git config --get "remote.${rname}.url")")
  branch=$(git symbolic-ref --short HEAD 2>/dev/null || echo main)

  echo "PUSH: ${rname} ${branch}"
  git push -u "${rname}" "${branch}" || {
    echo "NEXT=push failed; the user must fix git access to ${url} (credentials or permission) and retry" >&2
    return 1
  }

  # Does the service exist already? A failed lookup is only "absent" when the
  # CLI says exactly that; a network or auth failure must not be read as "go
  # create it", which would turn an outage into a confusing 409.
  local out err rc=0 verb existing=""
  out=$(mktemp); err=$(mktemp)
  trap 'rm -f "${out:-}" "${err:-}"' EXIT
  if "${QOOQ_CLI}" project get "${name}" -n "${ns}" -o json >"${out}" 2>"${err}"; then
    verb=deploy
    existing=$(json_field repo_url <"${out}")
    if [ -n "${existing}" ] && [ "${existing%.git}" != "${url}" ]; then
      echo "WARN: ${ns}/${name} already exists and builds ${existing}, not ${url}." >&2
      echo "NEXT=ask the user: redeploy that service anyway, or pick another name" >&2
      return 3
    fi
  elif grep -qi 'no service' "${err}"; then
    verb=create
  else
    cat "${err}" >&2
    classify "$(cat "${err}")"
    return 1
  fi

  echo "SERVICE=${ns}/${name}"
  echo "ACTION=${verb}"
  if [ "${verb}" = create ]; then
    "${QOOQ_CLI}" project create "${name}" --repo "${url}" --branch "${branch}" -n "${ns}" --wait -o json \
      >"${out}" 2> >(tee "${err}" >&2) || rc=$?
  else
    "${QOOQ_CLI}" project deploy "${name}" -n "${ns}" --wait -o json \
      >"${out}" 2> >(tee "${err}" >&2) || rc=$?
  fi
  sleep 1 # let the tee finish writing before we read the file back

  report_build "${name}" "${ns}" "${rc}" "${out}" "${err}" ship || return $?
  echo "UPDATES=a git push to ${branch} redeploys on its own; 'qooq project deploy ${name} -n ${ns} --wait' forces one"
}

# report_build turns a finished --wait call into the lines the agent acts on:
# needs_input (exit 2), failed with hints (exit 1), or each endpoint with a
# real probe. <again> is the subcommand to re-run after a fix.
report_build() {
  local name="$1" ns="$2" rc="$3" out="$4" err="$5" again="$6"
  if [ "${rc}" -ne 0 ]; then
    local text
    text="$(cat "${err}")"
    if printf '%s' "${text}" | grep -qi 'needs input'; then
      echo "BUILD: needs_input"
      echo "HINT=the manifest declares values nobody has supplied. Ask the user for them (use the description in the message above), set them with 'qooq project input ${name} -n ${ns} NAME=value', then re-run ${again}."
      return 2
    fi
    echo "BUILD: failed"
    classify "${text}"
    echo "HINT=full build log: qooq project logs ${name} -n ${ns} --build"
    return 1
  fi

  local eps ep code
  eps=$(json_endpoints <"${out}")
  if [ -z "${eps}" ]; then
    echo "ENDPOINT=none"
    echo "NOTE=deployed, but no public URL: the app publishes no port (add run.ports with auto:<port> to qooq.yaml)"
    return 0
  fi
  while IFS= read -r ep; do
    [ -n "${ep}" ] || continue
    echo "ENDPOINT=${ep}"
    code=$(curl -s -o /dev/null -m 10 -w '%{http_code}' "${ep}" 2>/dev/null || true)
    echo "PROBE=${ep} -> HTTP ${code:-000}"
  done <<EOF
${eps}
EOF
}

# ---------------------------------------------------------------- upload

# upload deploys the directory as it is on disk, with no git and no GitHub:
# `qooq deploy --cloud` packs it (honouring .gitignore and .qooqignore, never .env*),
# uploads it, and qooq Cloud builds it. Nothing redeploys on its own:
# every change is another upload.
cmd_upload() {
  local dir="${1:?usage: cloud-deploy.sh upload <project-dir> <name> [-n <namespace>] [--force]}"
  local name="${2:?usage: cloud-deploy.sh upload <project-dir> <name> [-n <namespace>] [--force]}"
  shift 2
  local ns="default" force=()
  while [ $# -gt 0 ]; do
    case "$1" in
      -n|--namespace) ns="${2:?-n needs a value}"; shift 2 ;;
      --force) force=(--force); shift ;;
      *) die "unknown argument: $1" ;;
    esac
  done
  need_cli
  # --cloud, not just `deploy`: since `qooq deploy` became local-first, a CLI
  # without the flag would quietly deploy to this machine instead.
  "${QOOQ_CLI}" deploy --help 2>/dev/null | grep -q -- '--cloud' || die "this qooq CLI cannot upload to the cloud ('qooq deploy --cloud'); update it, or use the GitHub path"
  [ -d "${dir}" ] || die "${dir} is not a directory"

  local out err rc=0
  out=$(mktemp); err=$(mktemp)
  trap 'rm -f "${out:-}" "${err:-}"' EXIT
  echo "SERVICE=${ns}/${name}"
  echo "ACTION=upload"
  "${QOOQ_CLI}" deploy --cloud "${dir}" --name "${name}" -n "${ns}" ${force[@]+"${force[@]}"} -o json \
    >"${out}" 2> >(tee "${err}" >&2) || rc=$?
  sleep 1 # let the tee finish writing before we read the file back

  # The one refusal that is a decision, not a fault: the name is taken by a
  # service that builds from git.
  if [ "${rc}" -ne 0 ] && grep -q 'already exists as a' "${err}"; then
    echo "NEXT=ask the user: ${ns}/${name} is a git-built service; pick another name, or remove that one first (only if they say so)" >&2
    return 3
  fi
  report_build "${name}" "${ns}" "${rc}" "${out}" "${err}" upload || return $?
  echo "UPDATES=nothing redeploys on its own: after a change, run upload again (identical content is a no-op)"
}

# ---------------------------------------------------------------- main

sub="${1:-}"
[ $# -gt 0 ] && shift
case "${sub}" in
  detect)      cmd_detect ;;
  commit)      cmd_commit "$@" ;;
  remote)      cmd_remote "$@" ;;
  create-repo) cmd_create_repo "$@" ;;
  ship)        cmd_ship "$@" ;;
  upload)      cmd_upload "$@" ;;
  *) echo "usage: cloud-deploy.sh detect|commit|remote|create-repo|ship|upload ..." >&2; exit 1 ;;
esac
