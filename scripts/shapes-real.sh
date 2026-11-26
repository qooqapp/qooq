#!/usr/bin/env bash
# 拿真實 repo 檢查偵測，只跑偵測不 build。
#
#   scripts/shapes-real.sh [--update] [名稱...]
#
# 為什麼不進 CI：
#   1. 紅燈可能不是你的錯——上游推一個 commit 就變色，而這種紅燈會被靜音。
#      所以每一筆都釘死 commit，而且這支只在你手動跑的時候跑。
#   2. build 第三方 repo ＝ 用真權限跑別人的 Dockerfile 和 build.command。
#      這支只跑偵測（讀檔案長相），永遠不執行 repo 裡的任何東西。
#   3. clone 要網路。網路不通是「跳過」不是「失敗」。
#
# 跟 testdata/shapes 的分工：那邊是手寫骨架，快、密封、進 CI；這邊是真的樹，
# 用來抓「我以為 Next.js 長這樣，但它其實不是」。手寫骨架猜錯時，**是這支會發現**。
#
# 動 pkg/manifest/detect.go 的時候跑一次。

set -uo pipefail

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$REPO_ROOT" || exit 1

CACHE="${QOOQ_SHAPES_CACHE:-${REPO_ROOT}/.shapes-cache}"
OUT="${REPO_ROOT}/pkg/manifest/testdata/real"
UPDATE=0
FAILED=0
SKIPPED=0

ok()   { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }
skip() { printf '  \033[33mSKIP\033[0m  %s\n' "$*"; SKIPPED=$((SKIPPED+1)); }
info() { printf '        %s\n' "$*"; }

# name|repo|commit|為什麼收它（對應 testdata/shapes 的哪個手寫骨架）
PINS=$(cat <<'EOF'
laravel|https://github.com/laravel/laravel|aa0cf127fc365a56ee016867144ddffabc2290ae|php-laravel
spring-petclinic|https://github.com/spring-projects/spring-petclinic|818c4136ea971c21674525f9053de0d9c7ad8cfe|java-spring-boot
axum|https://github.com/tokio-rs/axum|8b30afbfeee457146d29c9c9db47f0ae621a22a8|rust-axum
flask|https://github.com/pallets/flask|d318b683471101618febed18996405ad26462110|flask-app
fastapi|https://github.com/fastapi/fastapi|50113da16fec53b66b80d75e80a89296de4fa5a5|fastapi-app
express|https://github.com/expressjs/express|023767fe9872e029271df1418f73401bff20ff40|express-api
gin|https://github.com/gin-gonic/gin|dcaa4296d111981ffb31ac3eba90bb63e1eb5ab9|go-monorepo
nuxt|https://github.com/nuxt/nuxt|4436de29812fa56dbd823b686f9701a35141572a|pnpm-monorepo
EOF
)

QOOQ="${QOOQ:-}"
if [ -z "$QOOQ" ]; then
  for cand in "$(command -v qooq 2>/dev/null)" ../qooq-cli/bin/qooq ./bin/qooq; do
    [ -n "$cand" ] && [ -x "$cand" ] && QOOQ="$cand" && break
  done
fi
[ -n "$QOOQ" ] || { bad "找不到 qooq CLI（make -C ../qooq-cli build）"; exit 1; }
case "$QOOQ" in /*) ;; *) QOOQ="$(cd "$(dirname "$QOOQ")" && pwd)/$(basename "$QOOQ")" ;; esac

WANT=()
for arg in "$@"; do
  case "$arg" in
    --update) UPDATE=1 ;;
    *) WANT+=("$arg") ;;
  esac
done

wanted() {
  [ ${#WANT[@]} -eq 0 ] && return 0
  for w in "${WANT[@]}"; do [ "$w" = "$1" ] && return 0; done
  return 1
}

mkdir -p "$CACHE" "$OUT"
printf '\033[1m真實 repo 偵測（釘 commit，只讀不執行）\033[0m\n'
printf '快取：%s\n\n' "$CACHE"

while IFS='|' read -r name url commit shape; do
  [ -z "$name" ] && continue
  wanted "$name" || continue

  dir="${CACHE}/${name}"
  if [ ! -d "${dir}/.git" ]; then
    info "clone ${name} …"
    rm -rf "$dir"; mkdir -p "$dir"
    if ! ( cd "$dir" && git init -q && git remote add origin "$url" \
           && git fetch -q --depth 1 origin "$commit" && git checkout -q FETCH_HEAD ) 2>/dev/null; then
      skip "${name}: clone 失敗（沒網路，或這個 commit 不在了）"
      rm -rf "$dir"
      continue
    fi
  fi
  have=$(cd "$dir" && git rev-parse HEAD 2>/dev/null)
  if [ "$have" != "$commit" ]; then
    skip "${name}: 快取是別的 commit（${have:0:7}），刪掉 ${dir} 重跑"
    continue
  fi

  # --dry-run 不寫檔、不碰 daemon；輸出就是偵測結果。
  got=$("$QOOQ" init "$dir" --app "$name" --dry-run 2>&1)
  golden="${OUT}/${name}.txt"
  if [ "$UPDATE" = 1 ]; then
    printf '%s\n' "$got" > "$golden"
    ok "${name}: 已更新（對應手寫骨架 ${shape}）"
    continue
  fi
  if [ ! -f "$golden" ]; then
    skip "${name}: 還沒有 golden，用 --update 建立"
    continue
  fi
  if [ "$got" = "$(cat "$golden")" ]; then
    ok "${name}: 與 golden 相符（對應 ${shape}）"
  else
    bad "${name}: 偵測結果變了"
    diff <(cat "$golden") <(printf '%s\n' "$got") | head -20 | sed 's/^/        /'
  fi
done <<< "$PINS"

printf '\n'
[ "$SKIPPED" -gt 0 ] && info "略過 ${SKIPPED} 筆"
if [ "$FAILED" = 0 ]; then ok "沒有偵測結果改變"; else bad "有偵測結果改變——確認是你要的改動，然後 --update"; fi
exit "$FAILED"
