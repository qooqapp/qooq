#!/usr/bin/env bash
# 把 examples/ 底下每個範例真的部署一遍，確認它們還跑得起來。
#
#   scripts/smoke-examples.sh [範例名稱...]
#
# 為什麼要有這支：examples/ 是使用者會照抄的參考材料，但在此之前唯一碰過它們的
# 東西是「manifest 解析得過」——一個語法正確、build 不起來的範例會通過那個檢查，
# 而發現的人是照著抄的使用者。這支跑真的 clone → build → 部署 → HTTP。
#
# 全部範例同時部署（不是一個一個換），因為它們共存本身就是要驗的事：改成
# auto: port 之前有三個都寫死 8080，照抄兩個就撞號。
#
# 做完會清掉自己建立的一切，並把機器還原成執行前的樣子。

set -uo pipefail

QOOQ="${QOOQ:-}"
WORK=""
STARTED_DAEMONS=0
NS=smokeex
FAILED=0
DEPLOYED=()

step() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }
info() { printf '        %s\n' "$*"; }

cleanup() {
  step "清理"
  if [ -n "$QOOQ" ]; then
    for name in "${DEPLOYED[@]:-}"; do
      [ -z "$name" ] && continue
      "$QOOQ" watch rm "${NS}-${name}" >/dev/null 2>&1
      "$QOOQ" component rm "${NS}/${name}" >/dev/null 2>&1
    done
    # services: 宣告的資料庫是它自己的 component，名字不在 DEPLOYED 裡（它叫
    # <app>-db，由 watcher 生出來）。只刪部署清單上的名字會把 postgres 留在機器上。
    for leftover in $("$QOOQ" component ls 2>/dev/null | awk -v ns="$NS" '$1==ns {print $1"/"$2}'); do
      "$QOOQ" component rm "$leftover" >/dev/null 2>&1
    done
    info "移除 target 與 component（含 services 建出來的）"
  fi
  # 這批 image 都帶 qooq.built，但只砍這次自己建的那些 tag。
  for name in "${DEPLOYED[@]:-}"; do
    [ -z "$name" ] && continue
    for ref in $(docker images "$name" --format '{{.Repository}}:{{.Tag}}' 2>/dev/null); do
      docker rmi -f "$ref" >/dev/null 2>&1
    done
  done
  [ -n "$WORK" ] && rm -rf "$WORK" && info "移除暫存目錄"
  if [ "$STARTED_DAEMONS" = 1 ] && [ -n "$QOOQ" ]; then
    "$QOOQ" down >/dev/null 2>&1 && info "關掉本腳本起的 daemon"
  fi
}
trap cleanup EXIT

REPO_ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$REPO_ROOT" || exit 1

# --- 選要跑哪些 -------------------------------------------------------------
if [ $# -gt 0 ]; then
  EXAMPLES=("$@")
else
  EXAMPLES=()
  # A stack example is several projects deployed with `qooq stack`, not one
  # target; it is verified on its own.
  for d in examples/*/; do [ -f "${d}qooq.stack.yaml" ] || EXAMPLES+=("$(basename "$d")"); done
fi

# --- 環境 -------------------------------------------------------------------
step "環境"
if [ -z "$QOOQ" ]; then
  for cand in "$(command -v qooq 2>/dev/null)" ../qooq-cli/bin/qooq ./bin/qooq; do
    [ -n "$cand" ] && [ -x "$cand" ] && QOOQ="$cand" && break
  done
fi
[ -n "$QOOQ" ] || { bad "找不到 qooq CLI"; exit 1; }
case "$QOOQ" in /*) ;; *) QOOQ="$(cd "$(dirname "$QOOQ")" && pwd)/$(basename "$QOOQ")" ;; esac
ok "CLI: ${QOOQ} ($("$QOOQ" --version 2>/dev/null | tail -1))"

docker info >/dev/null 2>&1 || { bad "docker 沒在跑"; exit 1; }
ok "docker 可用"

if [ -z "${QOOQ_BIN_DIR:-}" ] && [ -x ./bin/qooqd ]; then
  export QOOQ_BIN_DIR="${PWD}/bin"
  ok "daemon: ${QOOQ_BIN_DIR}（本 repo build 出來的）"
fi

step "daemon"
if "$QOOQ" status 2>/dev/null | grep -q 'qooq-watcher.*up'; then
  ok "已經在跑，沿用（結束時不會關掉）"
else
  if "$QOOQ" up >/dev/null 2>&1; then
    STARTED_DAEMONS=1
    ok "已啟動（結束時會關掉）"
  else
    bad "qooq up 失敗"; exit 1
  fi
fi

# --- 每個範例一個 bare repo，全部註冊 ----------------------------------------
WORK=$(mktemp -d)
step "註冊 ${#EXAMPLES[@]} 個範例"
for name in "${EXAMPLES[@]}"; do
  src="examples/${name}"
  [ -d "$src" ] || { bad "沒有這個範例：${name}"; continue; }
  bare="${WORK}/${name}.git"
  git init -q --bare "$bare"
  tmp="${WORK}/src/${name}"
  mkdir -p "$tmp"
  cp -R "${src}/." "$tmp/"
  (
    cd "$tmp" || exit 1
    git init -q
    git add -A
    git -c user.email=smoke@qooq -c user.name=smoke commit -qm "example ${name}"
    git remote add qooq "$bare"
    git push -q qooq HEAD:main
  ) || { bad "${name}: 準備 repo 失敗"; continue; }

  if "$QOOQ" watch add --name "${NS}-${name}" --repo "file://${bare}" \
       --component "$name" --namespace "$NS" --poll 15 >/dev/null 2>&1; then
    DEPLOYED+=("$name")
    ok "${name} 已註冊"
  else
    bad "${name}: watch add 失敗"
  fi
done

[ ${#DEPLOYED[@]} -eq 0 ] && { bad "沒有任何範例註冊成功"; exit 1; }

# --- 等 build ---------------------------------------------------------------
step "等待 build（最多 10 分鐘）"
deadline=$(( $(date +%s) + 600 ))
pending=("${DEPLOYED[@]}")
while [ ${#pending[@]} -gt 0 ] && [ "$(date +%s)" -lt "$deadline" ]; do
  still=()
  for name in "${pending[@]}"; do
    status=$("$QOOQ" watch get "${NS}-${name}" 2>/dev/null | awk '/^last_build_status:/{print $2}')
    case "$status" in
      success) ok "${name}: build 成功" ;;
      failed)
        bad "${name}: build 失敗"
        "$QOOQ" watch get "${NS}-${name}" 2>/dev/null | grep -i last_build_error | head -1 | sed 's/^/        /'
        ;;
      *) still+=("$name") ;;
    esac
  done
  pending=("${still[@]:-}")
  # 去掉展開空陣列留下的空字串
  [ "${#pending[@]}" -eq 1 ] && [ -z "${pending[0]}" ] && pending=()
  [ ${#pending[@]} -gt 0 ] && sleep 10
done
for name in "${pending[@]:-}"; do
  [ -n "$name" ] && bad "${name}: 10 分鐘內沒有進入終態"
done

# --- 實際打一次 -------------------------------------------------------------
step "驗證 endpoint"
for name in "${DEPLOYED[@]}"; do
  get=$("$QOOQ" component get "${NS}/${name}" 2>/dev/null)
  [ -n "$get" ] || { bad "${name}: 找不到 component"; continue; }
  phase=$(printf '%s\n' "$get" | awk '/^phase:/{print $2}')

  # worker 沒有 port，跑起來就算過。component get 對「沒有 endpoint」印的是一個
  # 破折號而不是空白，所以這裡要把它當成沒有——只擋空字串會讓 worker 一路走到
  # curl "-"，然後失敗訊息長得像產品壞了。
  endpoint=$(printf '%s\n' "$get" | awk '/^endpoints:/{print $2}' | head -1)
  [ "$endpoint" = "-" ] && endpoint=""
  if [ -z "$endpoint" ]; then
    if [ "$phase" = "Running" ]; then ok "${name}: Running（無對外 port，符合預期）"
    else bad "${name}: phase=${phase}"; fi
    continue
  fi

  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "$endpoint" 2>/dev/null)
  if [ "$phase" = "Running" ] && [ "$code" -ge 200 ] 2>/dev/null && [ "$code" -lt 500 ] 2>/dev/null; then
    ok "${name}: ${endpoint} → HTTP ${code}"
  else
    bad "${name}: phase=${phase} ${endpoint} → HTTP ${code}"
  fi
done

# --- port 唯一性：auto: 存在的理由 -------------------------------------------
step "host port 沒有互相衝突"
ports=$("$QOOQ" component ls 2>/dev/null | awk -v ns="$NS" '$1==ns {print $0}' | grep -o '[0-9]\+->' | sort)
dupes=$(printf '%s\n' "$ports" | uniq -d)
if [ -z "$dupes" ]; then
  ok "$(printf '%s\n' "$ports" | grep -c . ) 個發布的 port 全部互異"
else
  bad "有重複的 host port：${dupes}"
fi

step "結果"
[ "$FAILED" = 0 ] && ok "全部通過" || bad "有失敗項目（往上看）"
exit "$FAILED"
