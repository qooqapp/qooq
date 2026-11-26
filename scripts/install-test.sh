#!/usr/bin/env bash
# Exercises install.sh against stand-in servers.
#
#   scripts/install-test.sh
#
# install.sh is the first thing a new user runs and the one part of the release
# that no test touched: it is not imported by anything, so a broken change to it
# is invisible until someone tries to install qooq and cannot. This runs the
# real script — both of the ways it can fetch, and every case where it is
# supposed to refuse.
#
# The archives here hold stub binaries rather than real builds. What is being
# tested is the installer: filenames, checksum verification, unpacking, where
# things land. Whether the daemons work is what make test and scripts/smoke.sh
# are for.

set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
INSTALL="$ROOT/install.sh"
WORK=$(mktemp -d)
FAILED=0
FILE_PORT=18090
API_PORT=18091
SERVERS=()

VERSION=1.2.3
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m); case "$arch" in x86_64|amd64) arch=amd64 ;; aarch64|arm64) arch=arm64 ;; esac

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }

cleanup() {
  for pid in "${SERVERS[@]:-}"; do [ -n "$pid" ] && kill "$pid" 2>/dev/null; done
  rm -rf "$WORK"
}
trap cleanup EXIT

command -v python3 >/dev/null 2>&1 || { echo "python3 is required (it stands in for the servers)"; exit 1; }

# --- archives ---------------------------------------------------------------
step "準備 release 檔案"
mkdir -p "$WORK/dl" "$WORK/build"
for b in qooqd qooq-watcher qooq; do
  printf '#!/bin/sh\necho "%s stub"\n' "$b" > "$WORK/build/$b"
  chmod +x "$WORK/build/$b"
done
tar czf "$WORK/dl/qooq_${VERSION}_${os}_${arch}.tar.gz"     -C "$WORK/build" qooqd qooq-watcher
tar czf "$WORK/dl/qooq-cli_${VERSION}_${os}_${arch}.tar.gz" -C "$WORK/build" qooq
( cd "$WORK/dl" && (command -v sha256sum >/dev/null 2>&1 && sha256sum ./*.tar.gz || shasum -a 256 ./*.tar.gz) \
  | sed 's|\./||' > checksums.txt )
ok "兩個 archive + checksums.txt"

# --- servers ----------------------------------------------------------------
# --directory rather than a subshell that cds: $! must be the server's own pid,
# or cleanup kills a wrapper and leaves the port held for the next run.
python3 -m http.server "$FILE_PORT" --bind 127.0.0.1 --directory "$WORK/dl" >/dev/null 2>&1 &
SERVERS+=("$!")

# A stand-in for the GitHub releases API, in the shape the real one answers:
# pretty-printed, a release-level id and name before the assets, node_id fields
# and an uploader object carrying its own id. install.sh reads that with awk, so
# the layout is the thing under test — a compact or reordered response is
# exactly what would break it.
cat > "$WORK/api.py" <<PY
import json, os
from http.server import BaseHTTPRequestHandler, HTTPServer
DL = "$WORK/dl"
FILES = {3001: "qooq_${VERSION}_${os}_${arch}.tar.gz",
         3002: "qooq-cli_${VERSION}_${os}_${arch}.tar.gz",
         3003: "checksums.txt"}

def release(repo):
    assets = []
    for aid, name in FILES.items():
        assets.append({
            "url": f"http://127.0.0.1:$API_PORT/repos/{repo}/releases/assets/{aid}",
            "id": aid, "node_id": "RA_kwDOAbCdEf", "name": name, "label": None,
            "uploader": {"login": "someone", "id": 42, "node_id": "MDQ6VXNlcjQy"},
            "content_type": "application/gzip", "state": "uploaded",
        })
    return json.dumps({
        "url": f"http://127.0.0.1:$API_PORT/repos/{repo}/releases/900",
        "id": 900, "node_id": "RE_kwDOAbCdEf", "tag_name": "v$VERSION",
        "name": "v$VERSION", "draft": False, "prerelease": False,
        "assets": assets,
    }, indent=2).encode()

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def do_GET(self):
        p = self.path.strip("/").split("/")
        if len(p) >= 5 and p[0] == "repos" and p[3] == "releases":
            repo = f"{p[1]}/{p[2]}"
            if p[4] in ("latest", "tags"):
                body = release(repo)
                self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers()
                self.wfile.write(body); return
            if p[4] == "assets" and len(p) == 6:
                # Private repository: without a token the asset is simply not there.
                if not self.headers.get("Authorization"): self.send_error(404); return
                name = FILES.get(int(p[5]))
                if not name: self.send_error(404); return
                data = open(os.path.join(DL, name), "rb").read()
                self.send_response(200); self.send_header("Content-Length", str(len(data))); self.end_headers()
                self.wfile.write(data); return
        self.send_error(404)

HTTPServer(("127.0.0.1", $API_PORT), H).serve_forever()
PY
python3 "$WORK/api.py" &
SERVERS+=("$!")

# Both servers must actually answer before anything is tested: a port still
# held by a previous run would otherwise turn into a string of failures that
# look like install.sh bugs.
up=0
for _ in $(seq 1 30); do
  if curl -sf "http://127.0.0.1:$FILE_PORT/checksums.txt" >/dev/null 2>&1 \
     && curl -sf "http://127.0.0.1:$API_PORT/repos/qooqapp/qooq/releases/latest" >/dev/null 2>&1; then
    up=1; break
  fi
  sleep 0.3
done
[ "$up" = 1 ] || { echo "the stand-in servers did not come up on :$FILE_PORT and :$API_PORT (is something else holding them?)"; exit 1; }

run_install() { # run_install <prefix-name> [env...]
  local name="$1"; shift
  env PREFIX="$WORK/$name" "$@" sh "$INSTALL" >"$WORK/$name.out" 2>&1
  echo $?
}

# --- the mirror path --------------------------------------------------------
step "QOOQ_DOWNLOAD_BASE（自架鏡像）"
code=$(run_install mirror VERSION="$VERSION" QOOQ_DOWNLOAD_BASE="http://127.0.0.1:$FILE_PORT")
if [ "$code" = 0 ] && [ -x "$WORK/mirror/qooqd" ] && [ -x "$WORK/mirror/qooq-watcher" ] && [ -x "$WORK/mirror/qooq" ]; then
  ok "三個執行檔都裝好且可執行"
else
  bad "安裝失敗（exit=${code}）"; sed -n '1,15p' "$WORK/mirror.out"
fi
grep -q 'verified' "$WORK/mirror.out" && ok "有驗 checksum" || bad "沒有印出 checksum 驗證"

step "VERSION 缺席時要講清楚"
code=$(run_install noversion QOOQ_DOWNLOAD_BASE="http://127.0.0.1:$FILE_PORT")
if [ "$code" != 0 ] && grep -q 'needs an explicit VERSION' "$WORK/noversion.out"; then
  ok "拒絕並說明原因"
else
  bad "應該拒絕（exit=${code}）"
fi

step "Windows 要指向 WSL2，不能只說不支援"
# Git Bash / MSYS2 是 Windows 使用者最可能的入口，而他們拿到的第一個訊息如果是
# 「unsupported」就到此為止了。用一個假的 uname 走那條分支，確認它有講出路。
mkdir -p "$WORK/fakebin"
printf '#!/bin/sh\n[ "$1" = "-s" ] && echo "MINGW64_NT-10.0-26200" && exit 0\nexec /usr/bin/uname "$@"\n' > "$WORK/fakebin/uname"
chmod +x "$WORK/fakebin/uname"
PATH="$WORK/fakebin:$PATH" sh "$INSTALL" > "$WORK/windows.out" 2>&1
code=$?
if [ "$code" != 0 ] \
   && grep -q 'no Windows build' "$WORK/windows.out" \
   && grep -q 'wsl' "$WORK/windows.out" \
   && grep -q 'curl: (23)' "$WORK/windows.out"; then
  ok "擋下來、指向 WSL2、並先講明 curl 的 23 不是下載失敗"
else
  bad "Windows 分支的訊息不完整（exit=${code}）"
  sed 's/^/        /' "$WORK/windows.out"
fi

# --- refusals ---------------------------------------------------------------
# The installer's one security property: what it puts on your PATH is what the
# release published. These two cases are that property.
step "檔案被竄改要拒絕安裝"
cp "$WORK/dl/qooq_${VERSION}_${os}_${arch}.tar.gz" "$WORK/clean.tar.gz"
printf 'tampered' >> "$WORK/dl/qooq_${VERSION}_${os}_${arch}.tar.gz"
code=$(run_install tampered VERSION="$VERSION" QOOQ_DOWNLOAD_BASE="http://127.0.0.1:$FILE_PORT")
if [ "$code" != 0 ] && grep -q 'checksum mismatch' "$WORK/tampered.out" && [ ! -e "$WORK/tampered/qooqd" ]; then
  ok "拒絕，且什麼都沒裝"
else
  bad "竄改的檔案沒有被擋下來（exit=${code}）"
fi
cp "$WORK/clean.tar.gz" "$WORK/dl/qooq_${VERSION}_${os}_${arch}.tar.gz"

step "沒有 checksums.txt 要拒絕安裝"
mv "$WORK/dl/checksums.txt" "$WORK/dl/checksums.hidden"
code=$(run_install nosums VERSION="$VERSION" QOOQ_DOWNLOAD_BASE="http://127.0.0.1:$FILE_PORT")
if [ "$code" != 0 ] && grep -q 'unverified' "$WORK/nosums.out"; then
  ok "拒絕安裝未經驗證的下載"
else
  bad "少了 checksums.txt 卻照裝（exit=${code}）"
fi
mv "$WORK/dl/checksums.hidden" "$WORK/dl/checksums.txt"

# --- the release path -------------------------------------------------------
step "GitHub releases API（私有 repo）"
code=$(run_install api GITHUB_TOKEN=stub-token QOOQ_API_BASE="http://127.0.0.1:$API_PORT")
if [ "$code" = 0 ] && [ -x "$WORK/api/qooq" ]; then
  ok "帶 token：找到 asset id、下載、驗證、安裝"
else
  bad "帶 token 仍失敗（exit=${code}）"; sed -n '1,15p' "$WORK/api.out"
fi

code=$(run_install notoken QOOQ_API_BASE="http://127.0.0.1:$API_PORT")
if [ "$code" != 0 ] && grep -q 'GITHUB_TOKEN' "$WORK/notoken.out"; then
  ok "沒 token：失敗並指出要設 GITHUB_TOKEN"
else
  bad "沒 token 時的錯誤訊息沒有提到 GITHUB_TOKEN（exit=${code}）"; sed -n '1,10p' "$WORK/notoken.out"
fi

step "讀不到 release 時的訊息"
code=$(run_install badrepo QOOQ_API_BASE="http://127.0.0.1:$API_PORT/nope")
grep -q 'cannot read releases' "$WORK/badrepo.out" && ok "說得出是讀不到 release" || bad "訊息不明確"

if [ "$FAILED" = 0 ]; then
  printf '\n\033[32m全部通過\033[0m — install.sh 兩條下載路徑與所有拒絕路徑都正確\n'
else
  printf '\n\033[31m有項目失敗\033[0m — 見上方 FAIL\n'
fi
exit $FAILED
