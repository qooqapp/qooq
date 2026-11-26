#!/usr/bin/env sh
# qooq installer: downloads the release archives and puts the three binaries
# on your PATH.
#
#   sh install.sh                       # latest release
#   VERSION=v0.2.0 sh install.sh        # a specific one
#   PREFIX=~/.local/bin sh install.sh   # somewhere other than /usr/local/bin
#
# qooq's repositories are currently private, so downloading needs a GitHub
# token with read access:
#
#   GITHUB_TOKEN=ghp_... sh install.sh
#
# When the repositories become public the token stops being necessary and
# nothing else here changes. To serve the archives from somewhere else
# entirely (an object store, your own site), point QOOQ_DOWNLOAD_BASE at a
# directory holding the same filenames and no token is involved at all:
#
#   QOOQ_DOWNLOAD_BASE=https://dl.example.com/qooq/v0.2.0 sh install.sh

set -eu

PREFIX="${PREFIX:-/usr/local/bin}"
VERSION="${VERSION:-latest}"
QOOQ_REPO="${QOOQ_REPO:-qooqapp/qooq}"
QOOQ_CLI_REPO="${QOOQ_CLI_REPO:-qooqapp/qooq-cli}"
QOOQ_DOWNLOAD_BASE="${QOOQ_DOWNLOAD_BASE:-}"
# The GitHub API root. Overridable for GitHub Enterprise — and so that this
# script's release path can be exercised against a stand-in API, which is the
# only way to test it without publishing a release first.
QOOQ_API_BASE="${QOOQ_API_BASE:-https://api.github.com}"

say()  { printf '%s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

need() {
  command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"
}
need curl
need tar
need uname

# --- platform ---------------------------------------------------------------
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  linux|darwin) ;;
  # Git Bash, MSYS2 and Cygwin report these. They are Windows, not Linux — WSL2
  # reports "linux" and is handled by the branch above, so anyone landing here
  # is on a shell that cannot run the Linux build. Saying only "unsupported"
  # leaves them with nowhere to go, and the way out is short.
  mingw*|msys*|cygwin*|windows*)
    say "qooq has no Windows build (this shell reports itself as \"${os}\")."
    say ""
    say "Run it inside WSL2 instead. Docker Desktop already runs its engine"
    say "there, so the Linux build works unmodified:"
    say ""
    say "    wsl --install     # once, in PowerShell, if you have no distro yet"
    say "    wsl               # then run this installer again inside it"
    say ""
    # curl reports error 23 when this script exits while it is still writing to
    # the pipe. It looks like a failed download and is not one; saying so here
    # is cheaper than the reader chasing it.
    die "not installing. (A \"curl: (23)\" after this line is this script exiting, not a download failure.)" ;;
  *) die "unsupported operating system: ${os} (qooq ships linux and darwin builds)" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  aarch64|arm64) arch=arm64 ;;
  *) die "unsupported architecture: $arch (qooq ships amd64 and arm64 builds)" ;;
esac
say "platform: ${os}/${arch}"

# --- where to download from -------------------------------------------------
# Two shapes: a plain directory of files (QOOQ_DOWNLOAD_BASE), or the GitHub
# releases API. The API path is separated out because a private repository
# needs the asset's numeric id, not its name — a browser-style URL returns the
# HTML login page instead of the file.
auth_header() {
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    printf 'Authorization: Bearer %s' "$GITHUB_TOKEN"
  fi
}

api_release_json() {
  repo="$1"
  if [ "$VERSION" = latest ]; then
    url="${QOOQ_API_BASE}/repos/${repo}/releases/latest"
  else
    url="${QOOQ_API_BASE}/repos/${repo}/releases/tags/${VERSION}"
  fi
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    curl -fsSL -H "$(auth_header)" -H 'Accept: application/vnd.github+json' "$url"
  else
    curl -fsSL -H 'Accept: application/vnd.github+json' "$url"
  fi
}

# release_json <repo> caches one API response per repository, so asking for the
# archive and then for its checksums does not cost two round trips — and cannot
# read two different releases if one is published between the calls.
release_json() {
  repo="$1"
  cache="$tmp/$(printf '%s' "$repo" | tr / _).json"
  if [ ! -f "$cache" ]; then
    api_release_json "$repo" > "$cache" \
      || die "cannot read releases for ${repo}$([ -n "${GITHUB_TOKEN:-}" ] && printf ' (is the token valid?)' || printf ' (if it is private, set GITHUB_TOKEN)')"
  fi
  printf '%s' "$cache"
}

# asset_id <json-file> <name>  — the numeric id of an asset, by exact name.
#
# A private asset can only be fetched by id: requesting it by name returns the
# HTML login page, which downloads "successfully" and is not an archive.
asset_id() {
  awk -v want="$2" '
    /"id":/    { gsub(/[^0-9]/, "", $2); id = $2 }
    /"name":/  { name = $0
                 sub(/.*"name": *"/, "", name); sub(/".*/, "", name)
                 if (name == want) { print id; exit } }
  ' "$1"
}

# asset_name <json-file> <prefix> — the full name of this platform's archive.
#
# Matched by prefix and platform suffix so the version does not have to be
# known here, which is what makes VERSION=latest work.
asset_name() {
  awk -v pre="$2" -v suf="${os}_${arch}.tar.gz" '
    /"name":/ { name = $0
                sub(/.*"name": *"/, "", name); sub(/".*/, "", name)
                if (index(name, pre) == 1 && name ~ suf"$") { print name; exit } }
  ' "$1"
}

fetch_by_id() {
  repo="$1"; id="$2"; dest="$3"
  if [ -n "${GITHUB_TOKEN:-}" ]; then
    curl -fsSL -H "$(auth_header)" -H 'Accept: application/octet-stream' \
      -o "$dest" "${QOOQ_API_BASE}/repos/${repo}/releases/assets/${id}"
  else
    curl -fsSL -H 'Accept: application/octet-stream' \
      -o "$dest" "${QOOQ_API_BASE}/repos/${repo}/releases/assets/${id}"
  fi
}

# sha256_of prints a file's digest, using whichever tool this system has.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    printf ''
  fi
}

# verify checks a downloaded archive against the release's checksums.txt.
#
# An unverified download is the whole attack: whoever can change the archive in
# transit or at rest chooses what these three binaries do, and they are about to
# be put on a PATH and run. So a mismatch stops the install, and a checksums
# file that cannot be read is reported rather than skipped quietly — the one
# case that is allowed to pass with a warning is having no sha256 tool at all,
# which is a property of the machine and not of the download.
verify() {
  archive="$1"; name="$2"; sums="$3"
  [ -f "$sums" ] || die "no checksums file for ${name}; refusing to install an unverified download"

  want=$(awk -v n="$name" '$2 == n || $2 == "*"n { print $1; exit }' "$sums")
  [ -n "$want" ] || die "${name} is not listed in checksums.txt; refusing to install an unverified download"

  got=$(sha256_of "$archive")
  if [ -z "$got" ]; then
    say "warning: neither sha256sum nor shasum is installed, so ${name} could not be verified"
    return
  fi
  [ "$got" = "$want" ] || die "checksum mismatch for ${name}
  expected ${want}
  got      ${got}
The download does not match what the release publishes. Do not use it."
  say "verified  ${name}"
}

# get_archive <repo> <prefix> — downloads this platform's archive and its
# checksums, verifies it, and sets ARCHIVE_PATH.
#
# The result comes back in a variable rather than on stdout: this function also
# reports progress, and a caller using command substitution would capture those
# lines as part of the path.
get_archive() {
  ARCHIVE_PATH=""
  repo="$1"; prefix="$2"

  if [ -n "$QOOQ_DOWNLOAD_BASE" ]; then
    [ "$VERSION" = latest ] && die "QOOQ_DOWNLOAD_BASE needs an explicit VERSION (there is no index to ask)"
    name="${prefix}_${VERSION#v}_${os}_${arch}.tar.gz"
    say "downloading ${name}"
    curl -fsSL -o "$tmp/$name" "${QOOQ_DOWNLOAD_BASE}/${name}" \
      || die "download failed: ${QOOQ_DOWNLOAD_BASE}/${name}"
    # Served from a plain directory, checksums.txt sits beside the archives.
    # Both repositories publish a file by that name, so it is stored per
    # prefix to keep qooq's from overwriting qooq-cli's.
    curl -fsSL -o "$tmp/${prefix}.sums" "${QOOQ_DOWNLOAD_BASE}/checksums.txt" \
      || die "cannot fetch ${QOOQ_DOWNLOAD_BASE}/checksums.txt; refusing to install an unverified download"
    verify "$tmp/$name" "$name" "$tmp/${prefix}.sums"
    ARCHIVE_PATH="$tmp/$name"
    return
  fi

  json=$(release_json "$repo")
  name=$(asset_name "$json" "$prefix")
  [ -n "$name" ] || die "no ${prefix} archive for ${os}/${arch} in that release"

  id=$(asset_id "$json" "$name")
  [ -n "$id" ] || die "cannot resolve the download id for ${name}"
  say "downloading ${name}"
  # A token that can read a private repository's metadata but not its contents
  # gets this far and then 404s on the asset itself, so the token is named here
  # too rather than only at the metadata step.
  fetch_by_id "$repo" "$id" "$tmp/$name" \
    || die "download failed for ${name}$([ -n "${GITHUB_TOKEN:-}" ] && printf ' (does the token have read access to %s contents?)' "$repo" || printf ' (if %s is private, set GITHUB_TOKEN)' "$repo")"

  sums_id=$(asset_id "$json" checksums.txt)
  [ -n "$sums_id" ] || die "the ${repo} release publishes no checksums.txt; refusing to install an unverified download"
  fetch_by_id "$repo" "$sums_id" "$tmp/${prefix}.sums" || die "cannot fetch checksums for ${repo}"

  verify "$tmp/$name" "$name" "$tmp/${prefix}.sums"
  ARCHIVE_PATH="$tmp/$name"
}

# --- fetch and unpack -------------------------------------------------------
tmp=$(mktemp -d)
# Removed on every exit path: a failed install should not leave archives in
# /tmp for someone to wonder about later.
trap 'rm -rf "$tmp"' EXIT INT TERM

get_archive "$QOOQ_REPO" qooq;         qooq_archive="$ARCHIVE_PATH"
get_archive "$QOOQ_CLI_REPO" qooq-cli; cli_archive="$ARCHIVE_PATH"

tar xzf "$qooq_archive" -C "$tmp" || die "cannot unpack the qooq archive"
tar xzf "$cli_archive"  -C "$tmp" || die "cannot unpack the qooq-cli archive"

for b in qooqd qooq-watcher qooq; do
  [ -f "$tmp/$b" ] || die "the archives did not contain $b"
done

# --- install ----------------------------------------------------------------
# sudo only if the destination is not writable, so an install into a user
# directory never asks for a password it does not need.
install_cmd=""
if [ ! -d "$PREFIX" ]; then
  mkdir -p "$PREFIX" 2>/dev/null || install_cmd="sudo"
fi
if [ -z "$install_cmd" ] && [ ! -w "$PREFIX" ]; then
  install_cmd="sudo"
fi
if [ -n "$install_cmd" ]; then
  say "installing to ${PREFIX} (needs sudo — it is not writable by you)"
  sudo mkdir -p "$PREFIX"
fi

for b in qooqd qooq-watcher qooq; do
  chmod +x "$tmp/$b"
  $install_cmd cp "$tmp/$b" "$PREFIX/$b" || die "cannot write to ${PREFIX}; try PREFIX=~/.local/bin"
done

say ""
say "installed to ${PREFIX}:"
for b in qooqd qooq-watcher qooq; do
  say "  $b"
done

# Being on PATH is the difference between installed and usable, and the two are
# easy to confuse when the command works in this shell but not the next one.
case ":${PATH}:" in
  *":${PREFIX}:"*) ;;
  *) say ""
     say "note: ${PREFIX} is not on your PATH. Add this to your shell profile:"
     say "  export PATH=\"${PREFIX}:\$PATH\"" ;;
esac

say ""
say "next:  qooq up        # start the daemons (needs docker running)"
say "       qooq status    # check on them"
