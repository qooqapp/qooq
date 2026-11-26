#!/usr/bin/env bash
# 一鍵驗證整條鏈是不是活的：起 daemon → git push → build → 部署 → 實際連得上。
#
#   scripts/smoke.sh
#
# 做完會把自己建立的東西全部清掉，並把機器還原成執行前的樣子——daemon 本來沒在跑
# 就關掉、本來在跑就留著。
#
# 用途是「這個 build 到底能不能用」，不是取代 make test（那是單元測試）。
# 兩者測的東西不一樣：這支跑的是真的 docker、真的 git、真的 HTTP。

set -uo pipefail

QOOQ="${QOOQ:-}"
WORK=""
STARTED_DAEMONS=0
NAME=qooq-smoke
FAILED=0

step()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()    { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad()   { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }
info()  { printf '        %s\n' "$*"; }

cleanup() {
  step "清理"
  if [ -n "$QOOQ" ]; then
    "$QOOQ" watch rm "$NAME"      >/dev/null 2>&1 && info "移除 watch target"
    "$QOOQ" component rm "$NAME"  >/dev/null 2>&1 && info "移除 component"
  fi
  docker rmi -f "$(docker images "$NAME" -q) " >/dev/null 2>&1
  [ -n "$WORK" ] && rm -rf "$WORK" && info "移除暫存目錄"
  # 只有這支腳本起的 daemon 才由它關掉：本來就在跑的是使用者的，不要動。
  if [ "$STARTED_DAEMONS" = 1 ] && [ -n "$QOOQ" ]; then
    "$QOOQ" down >/dev/null 2>&1 && info "關掉本腳本起的 daemon"
  fi
}
trap cleanup EXIT

# --- 找 qooq CLI ------------------------------------------------------------
step "環境"
if [ -z "$QOOQ" ]; then
  for cand in "$(command -v qooq 2>/dev/null)" ../qooq-cli/bin/qooq ./bin/qooq; do
    [ -n "$cand" ] && [ -x "$cand" ] && QOOQ="$cand" && break
  done
fi
[ -n "$QOOQ" ] || { bad "找不到 qooq CLI（裝了 release 或 make -C ../qooq-cli build）"; exit 1; }
# 轉成絕對路徑：底下會 cd 進暫存目錄，相對路徑到那時就失效了。
case "$QOOQ" in
  /*) ;;
  *) QOOQ="$(cd "$(dirname "$QOOQ")" && pwd)/$(basename "$QOOQ")" ;;
esac
ok "CLI: $QOOQ ($("$QOOQ" --version 2>/dev/null | tail -1))"

docker info >/dev/null 2>&1 || { bad "docker 沒在跑"; exit 1; }
ok "docker 可用"

# 從原始碼跑的時候，daemon 在這個 repo 的 bin/ 裡而不是 CLI 旁邊——`qooq up`
# 只能靠這個環境變數知道去哪找（見它的 findDaemonBinary）。
if [ -z "${QOOQ_BIN_DIR:-}" ] && [ -x ./bin/qooqd ]; then
  export QOOQ_BIN_DIR="$PWD/bin"
  # 大括號是必要的：全形括號緊接在變數名後面，bash 會把它算進變數名裡。
  ok "daemon: ${QOOQ_BIN_DIR}（本 repo build 出來的）"
fi

# --- daemon -----------------------------------------------------------------
step "daemon"
if "$QOOQ" status 2>/dev/null | grep -q 'qooq-watcher.*up'; then
  ok "已經在跑，沿用（結束時不會關掉）"
else
  info "沒在跑，啟動中…"
  if "$QOOQ" up >/dev/null 2>&1; then
    STARTED_DAEMONS=1
    ok "已啟動（結束時會關掉）"
  else
    bad "qooq up 失敗——直接跑 '$QOOQ up' 看原因"
    exit 1
  fi
fi

# --- 準備一個真的 repo -------------------------------------------------------
step "建立測試專案"
WORK=$(mktemp -d)
mkdir -p "$WORK/src"
cd "$WORK/src" || exit 1

cat > qooq.yaml <<'YAML'
version: 1
app: qooq-smoke
description: smoke test：驗證 push → build → 部署 → 連得上整條鏈

build:
  dockerfile: Dockerfile

run:
  ports: ["19999:8080"]

health:
  http: /
  port: 19999
YAML

cat > Dockerfile <<'DOCKER'
FROM busybox
RUN mkdir -p /www
COPY index.html /www/index.html
CMD ["httpd", "-f", "-p", "8080", "-h", "/www"]
DOCKER

echo 'qooq smoke ok' > index.html

git init -q -b main . 2>/dev/null
git add -A
git -c user.email=smoke@qooq -c user.name=smoke commit -q -m "smoke test app"
cd "$WORK" || exit 1
git clone -q --bare src repo.git
# clone 只在「複製出來的那份」設 origin，來源 repo 沒有任何 remote——
# 下面要 push 第二個 commit，所以這裡明確接上。
git -C src remote add qooq "$WORK/repo.git"
ok "repo 建好（含 qooq.yaml + Dockerfile）"

# --- GitOps：建 target，等它自己 build + 部署 --------------------------------
step "GitOps 部署"
"$QOOQ" watch rm "$NAME" >/dev/null 2>&1   # 上一次沒清乾淨的話
if ! "$QOOQ" watch add --name "$NAME" --repo "file://$WORK/repo.git" \
       --component "$NAME" --poll 10 >/dev/null 2>&1; then
  bad "watch add 失敗"
  exit 1
fi
ok "watch target 已建立"

info "等 watcher clone → build → 部署（最多 3 分鐘）…"
status=""
for _ in $(seq 1 90); do
  status=$("$QOOQ" watch get "$NAME" 2>/dev/null | awk '/^last_build_status:/ {print $2}')
  case "$status" in
    success) break ;;
    failed|needs_input)
      bad "build $status"
      "$QOOQ" watch get "$NAME" 2>/dev/null | sed -n '/^last_build_error:/,$p' | head -8
      exit 1 ;;
  esac
  sleep 2
done
[ "$status" = success ] || { bad "build 逾時（最後狀態=${status:-未知}）"; exit 1; }
ok "build 成功"

# --- 驗證：用 qooq 自己回報的網址去連 ---------------------------------------
step "實際連線"
phase=""
for _ in $(seq 1 30); do
  phase=$("$QOOQ" component get "$NAME" 2>/dev/null | awk '/^phase:/ {print $2}')
  [ "$phase" = Running ] && break
  sleep 2
done
[ "$phase" = Running ] || { bad "component 沒進入 Running（目前=${phase:-找不到}）"; exit 1; }
ok "component Running"

# 打的是 qooq 自己回報的位址，不是我們猜的——要驗的正是「它給的網址能不能用」。
endpoint=$("$QOOQ" component get "$NAME" 2>/dev/null | awk '/^endpoints:/ {print $2}' | tr -d ',')
[ -n "$endpoint" ] || { bad "component get 沒有回報 endpoint"; exit 1; }
info "qooq 回報的位址：$endpoint"

# https 的位址一律不驗憑證：daemon 開 -ingress-tls-issuer internal 時，憑證本來就是
# Caddy 內建 CA 簽的，系統信任庫裡沒有它——那是設計如此，不是故障。少了這個，
# 一台完全正常的機器會在這裡紅，而這支腳本的用途正好是建立信心。
# 這裡要驗的是「這個網址回不回得出我們的內容」，不是憑證鏈。
fetch() {
  case "$1" in
    https://*) curl -sk -m "$2" "$1" 2>/dev/null ;;
    *)         curl -s  -m "$2" "$1" 2>/dev/null ;;
  esac
}
case "$endpoint" in
  https://*) info "https 位址：不驗憑證（內建 CA 簽的憑證系統本來就不信任）" ;;
esac

# Running 不等於「網址已經通」：開了 -ingress 的話，路由要等下一次 sync tick（5s）
# 把 component 接上 ingress 網路並重載 Caddy 設定。所以這裡重試而不是一擊定生死——
# 第一次就判失敗會讓一台正常的機器紅在這裡（實測就是這樣紅的）。
for _ in $(seq 1 20); do
  body=$(fetch "$endpoint" 10)
  [ "$body" = "qooq smoke ok" ] && break
  sleep 2
done
if [ "$body" = "qooq smoke ok" ]; then
  ok "連得上，內容正確：$body"
else
  bad "連不上或內容不符（拿到：${body:-空})"
fi

# --- 改一行 code，驗證會自動重新部署 -----------------------------------------
step "自動重新部署"
cd "$WORK/src" || exit 1
echo 'qooq smoke v2' > index.html
git add -A && git -c user.email=smoke@qooq -c user.name=smoke commit -q -m "v2"
if ! git push -q qooq main 2>&1; then
  bad "push 失敗"
  exit 1
fi
info "已 push 第二個 commit，等自動重建…"

for _ in $(seq 1 60); do
  body=$(fetch "$endpoint" 5)
  [ "$body" = "qooq smoke v2" ] && break
  sleep 3
done
if [ "$body" = "qooq smoke v2" ]; then
  ok "push 後自動重新部署，新內容已生效"
else
  bad "180 秒內沒看到新版本（拿到：${body:-空}）"
fi

# --- 停止 / 啟動 / 暫停：行程與輪詢是兩回事 ---------------------------------
#
# 這段跑真的容器，因為這三個動詞唯一會壞的地方就是「它到底有沒有真的動到東西」：
# 單元測試看得到呼叫送出去了，看不到位址是不是真的打不開了。
step "停止與啟動"

"$QOOQ" watch stop "$NAME" >/dev/null 2>&1 || { bad "watch stop 指令失敗"; exit 1; }
down=""
for _ in $(seq 1 20); do
  body=$(fetch "${endpoint}" 2)
  [ -z "$body" ] && { down=yes; break; }
  sleep 1
done
if [ "$down" = yes ]; then
  ok "stop 之後位址真的打不開了"
else
  bad "stop 之後位址還在回應（拿到：${body}）"
fi

# 停掉的是行程，不是這個專案：target 還在，而且 ls 要講得出它停了。
if "$QOOQ" watch ls 2>/dev/null | grep -q "^${NAME}[[:space:]]"; then
  ok "target 還在（停的是服務，不是專案）"
else
  bad "stop 把 target 一起弄不見了"
fi

"$QOOQ" watch start "$NAME" >/dev/null 2>&1 || { bad "watch start 指令失敗"; exit 1; }
back=""
for _ in $(seq 1 30); do
  body=$(fetch "${endpoint}" 2)
  [ -n "$body" ] && { back=yes; break; }
  sleep 1
done
if [ "$back" = yes ]; then
  ok "start 之後位址又活了（內容：${body}）"
else
  bad "start 之後 30 秒內位址還是不通"
fi

step "暫停自動部署"
"$QOOQ" watch pause "$NAME" >/dev/null 2>&1 || { bad "watch pause 指令失敗"; exit 1; }

# 看得見，這是重點：一個看不出來的暫停就是「為什麼它不再部署了」永遠查不到。
if "$QOOQ" watch ls 2>/dev/null | grep "^${NAME}[[:space:]]" | grep -q "(paused)"; then
  ok "ls 的 STATUS 欄看得出它暫停了"
else
  bad "ls 沒有講出暫停：$("$QOOQ" watch ls 2>/dev/null | grep "^${NAME}[[:space:]]")"
fi

# 暫停動的是輪詢，不是行程——東西要照常在服務。
body=$(fetch "$endpoint" 5)
if [ -n "$body" ]; then
  ok "暫停不影響正在跑的服務（${body}）"
else
  bad "暫停之後服務不見了"
fi

# push 一個 commit，它不該被自動部署。
cd "$WORK/src" || exit 1
echo 'qooq smoke v3' > index.html
git add -A && git -c user.email=smoke@qooq -c user.name=smoke commit -q -m "v3"
git push -q qooq main 2>&1 || { bad "push 失敗"; exit 1; }
info "已 push 第三個 commit，等 40 秒確認它不會被自動部署…"
sleep 40
body=$(fetch "$endpoint" 5)
if [ "$body" = "qooq smoke v3" ]; then
  bad "暫停中仍然自動部署了"
else
  ok "暫停中沒有自動部署（還是 ${body:-空}）"
fi

# 但明確要求部署仍然要動——暫停的是自動，不是部署本身。
"$QOOQ" watch trigger "$NAME" --force >/dev/null 2>&1 || { bad "trigger 失敗"; exit 1; }
for _ in $(seq 1 60); do
  body=$(fetch "$endpoint" 5)
  [ "$body" = "qooq smoke v3" ] && break
  sleep 3
done
if [ "$body" = "qooq smoke v3" ]; then
  ok "暫停中明確按部署仍然會建（這是刻意的）"
else
  bad "暫停把明確的部署也吞掉了（拿到：${body:-空}）"
fi

"$QOOQ" watch resume "$NAME" >/dev/null 2>&1 || bad "watch resume 指令失敗"

# --- 移除：連服務一起帶走 -----------------------------------------------------
step "移除"
"$QOOQ" watch rm "$NAME" >/dev/null 2>&1 || { bad "watch rm 失敗"; exit 1; }
gone=""
for _ in $(seq 1 20); do
  body=$(fetch "$endpoint" 2)
  [ -z "$body" ] && { gone=yes; break; }
  sleep 1
done
if [ "$gone" = yes ]; then
  ok "移除之後位址失效了（容器真的被拆掉）"
else
  bad "移除之後位址還在回應——又生出一個沒人管的容器了（拿到：${body}）"
fi

# --- 結論 -------------------------------------------------------------------
if [ "$FAILED" = 0 ]; then
  printf '\n\033[32m全部通過\033[0m — 部署鏈是活的\n'
else
  printf '\n\033[31m有項目失敗\033[0m — 見上方 FAIL\n'
fi
exit $FAILED
