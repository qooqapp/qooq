#!/usr/bin/env bash
# 三個互相依賴的專案，從 git push 到彼此打得通。
#
#   scripts/smoke-multi.sh
#
# 為什麼要有這支，而不是靠 smoke.sh 就好：有一整類 bug **只在多個專案同時存在時**
# 才會現形，單一專案的測試永遠碰不到。2026-09 那批就是實例，三個都安靜地壞了很久：
#
#   * ${namespace} 一直展開成空字串——沒有 namespace 就看不出來
#   * 宣告的 service 每輪 sweep 都被重新 upsert（＝每次 push 重建資料庫）
#     ——沒有 service 就看不出來
#   * health probe 打到「剛好佔著那個 port 的別人」，於是壞掉的 app 回報健康
#     ——只有一個 component 時，那個 port 不會屬於任何人
#
# 三個都不是靠測得仔細找到的，是靠**跑在一個真的有三個互連專案的環境**找到的。
# 這支就是把那個形狀固定下來，不要再依賴剛好有人在真環境裡踩到。
#
# 跟其他驗證的分工：make test 是單元測試，smoke.sh 問「這個 build 能不能用」，
# 這支問「多個專案放在一起還對不對」。需要 docker，約 2–3 分鐘。
#
# 做完會把自己建立的東西全部清掉，並把機器還原成執行前的樣子。

set -uo pipefail

QOOQ="${QOOQ:-}"
WORK=""
STARTED_DAEMONS=0
NS=smokemulti
NET=smokemulti-net
PROJECTS="api web admin"
FAILED=0

step()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()    { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad()   { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAILED=1; }
info()  { printf '        %s\n' "$*"; }

cleanup() {
  step "清理"
  if [ -n "$QOOQ" ]; then
    for p in $PROJECTS; do
      "$QOOQ" watch rm "$NS-$p" >/dev/null 2>&1
      "$QOOQ" component rm "$NS/$p" >/dev/null 2>&1
    done
    # 宣告出來的 service 是它自己的 component，名字是 <app-component>-<key>。
    "$QOOQ" component rm "$NS/api-cache" >/dev/null 2>&1
    info "移除 target 與 component"
  fi
  for p in $PROJECTS; do
    docker rmi -f "$(docker images "$NS-$p" -q)" >/dev/null 2>&1
  done
  docker network rm "$NET" >/dev/null 2>&1
  [ -n "$WORK" ] && rm -rf "$WORK" && info "移除暫存目錄"
  if [ "$STARTED_DAEMONS" = 1 ] && [ -n "$QOOQ" ]; then
    "$QOOQ" down >/dev/null 2>&1 && info "關掉本腳本起的 daemon"
  fi
}
trap cleanup EXIT

# --- 環境 -------------------------------------------------------------------
step "環境"
if [ -z "$QOOQ" ]; then
  for cand in "$(command -v qooq 2>/dev/null)" ../qooq-cli/bin/qooq ./bin/qooq; do
    [ -n "$cand" ] && [ -x "$cand" ] && QOOQ="$cand" && break
  done
fi
[ -n "$QOOQ" ] || { bad "找不到 qooq CLI（裝了 release 或 make -C ../qooq-cli build）"; exit 1; }
case "$QOOQ" in
  /*) ;;
  *) QOOQ="$(cd "$(dirname "$QOOQ")" && pwd)/$(basename "$QOOQ")" ;;
esac
ok "CLI: $QOOQ"

docker info >/dev/null 2>&1 || { bad "docker 沒在跑"; exit 1; }
ok "docker 可用"

if [ -z "${QOOQ_BIN_DIR:-}" ] && [ -x ./bin/qooqd ]; then
  export QOOQ_BIN_DIR="$PWD/bin"
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
    bad "qooq up 失敗——直接跑 '$QOOQ up' 看原因"
    exit 1
  fi
fi

# --- 三個專案 ---------------------------------------------------------------
# 刻意都是 nginx：這支要驗的是「多個專案之間」的行為，不是任何語言的建置。
# 每個專案帶的結構才是重點，見各自的 qooq.yaml。
step "建立三個 repo"
WORK=$(mktemp -d)

make_repo() {  # make_repo <名字> <qooq.yaml 內容>
  local name=$1 manifest=$2
  local src="$WORK/$name" bare="$WORK/$name.git"
  mkdir -p "$src"
  printf '%s' "$manifest" > "$src/qooq.yaml"
  cat > "$src/Dockerfile" <<'EOF'
FROM nginx:alpine
COPY site/ /usr/share/nginx/html/
EOF
  mkdir -p "$src/site"
  printf '%s is up\n' "$name" > "$src/site/index.html"
  printf 'ok\n' > "$src/site/healthz"
  git -C "$src" init -q -b main
  git -C "$src" -c user.email=t@t -c user.name=t add -A
  git -C "$src" -c user.email=t@t -c user.name=t commit -qm "$name"
  git init -q --bare "$bare"
  git -C "$src" remote add origin "$bare"
  git -C "$src" push -q origin main
}

# api：發布的那個。它同時帶著兩樣只有它會暴露的東西——
#   * 一個宣告的 service（沒有它，「每輪 sweep 重建資料庫」測不出來）
#   * 一個 http health probe，port 寫 **container port**
#     （沒有第二、第三個 component 佔著別的 port，「probe 打到陌生人」測不出來）
make_repo api "$(cat <<EOF
version: 1
app: api
network: $NET
run:
  ports: ["auto:80"]
  env:
    NS: "\${namespace}"
  volumes:
    - apidata:/data
services:
  cache:
    type: redis
health:
  http: /healthz
  port: 80
  interval: 5s
  delay: 2s
EOF
)"

# web 與 admin：兩個引用者。一個 component 被兩邊引用，正是實務上的形狀。
# 同一個 manifest 裡容器內與容器外的位址都用到——那是「不自動判斷該用哪種」
# 這個決定的實證：portal 型的 app 本來就兩種都要。
for name in web admin; do
  make_repo "$name" "$(cat <<EOF
version: 1
app: $name
network: $NET
run:
  ports: ["auto:80"]
  env:
    API_INTERNAL: "http://\${component.api.address}"
    API_PUBLIC: "\${component.api.endpoint}"
    NS: "\${namespace}"
EOF
)"
done
ok "三個 repo 建好（api / web / admin）"

# --- 註冊 target ------------------------------------------------------------
step "註冊 target"
for p in $PROJECTS; do
  if "$QOOQ" watch add --name "$NS-$p" --repo "file://$WORK/$p.git" \
       --component "$p" --namespace "$NS" --poll 10 >/dev/null 2>&1; then
    ok "$NS-$p"
  else
    bad "watch add $NS-$p 失敗"
    exit 1
  fi
done

# --- 等它們自己部署 ---------------------------------------------------------
# api 先起來，web/admin 才引用得到它；引用不到不是失敗，是 needs_input，
# 下一輪 sweep 自己會好——這裡等的就是那個機制真的會收斂。
step "等三個都部署好（最多 5 分鐘）"
deadline=$(( $(date +%s) + 300 ))
while :; do
  done_count=0
  for p in $PROJECTS; do
    "$QOOQ" watch get "$NS-$p" 2>/dev/null | grep -q 'last_build_status: success' && done_count=$((done_count+1))
  done
  [ "$done_count" = 3 ] && break
  if [ "$(date +%s)" -ge "$deadline" ]; then
    bad "逾時：只有 $done_count/3 成功"
    for p in $PROJECTS; do
      info "$NS-$p: $("$QOOQ" watch get "$NS-$p" 2>/dev/null | grep -E 'last_build_(status|error)' | tr '\n' ' ')"
    done
    exit 1
  fi
  sleep 5
done
ok "三個 target 都 success"

# --- 驗證 -------------------------------------------------------------------
# 以下每一項都對應一個「只有多專案才看得見」的失敗。

get() { "$QOOQ" component get "$NS/$1" 2>/dev/null; }

step "1. 三個都在跑，而且對外真的連得上"
for p in $PROJECTS; do
  phase=$(get "$p" | awk '/^phase:/{print $2}')
  [ "$phase" = Running ] || { bad "$p phase=$phase"; continue; }
  # 連通性打 loopback，不打 endpoints 回報的位址：advertise host 是 daemon
  # 開機時偵測一次的顯示值，機器換過網段之後它就指向沒有東西在聽的地方。
  # 這支要驗的是「部署起來了嗎」，不是「這台機器的 IP 有沒有變」。
  port=$(get "$p" | awk '/^ports:/{print $2}' | sed 's/->.*//')
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:$port/" 2>/dev/null)
  if [ "$code" = 200 ]; then ok "$p 在 127.0.0.1:$port 回 200"; else bad "$p 在 127.0.0.1:$port 回 $code"; fi
done

# advertise host 只在 daemon 啟動時偵測一次，所以換過網段之後 endpoints 會指向
# 一個沒有東西在聽的位址。這不是這支腳本要測的東西，但跑的人值得知道——
# 它剛好解釋了「component get 給的網址打不開」這個看起來很像故障的現象。
step "1b. endpoints 的主機位址還有效嗎（僅提示）"
adv=$(get api | awk '/^endpoints:/{print $2}' | sed 's|http://||; s|:.*||')
here=$(ipconfig getifaddr en0 2>/dev/null || true)
if [ -n "$here" ] && [ -n "$adv" ] && [ "$here" != "$adv" ]; then
  info "daemon 報的是 ${adv}，這台機器現在是 ${here}——換過網段了。"
  info "endpoints 會指向舊位址，直到 daemon 重啟或用 -advertise-host 指定。"
else
  ok "endpoints 的主機位址 = $adv"
fi

step "2. auto port 三個互不相同"
ports=$(for p in $PROJECTS; do get "$p" | awk '/^ports:/{print $2}' | sed 's/->.*//'; done)
distinct=$(printf '%s\n' $ports | sort -u | wc -l | tr -d ' ')
if [ "$distinct" = 3 ]; then
  ok "配到 $(printf '%s ' $ports)"
else
  bad "三個 component 只拿到 $distinct 個不同的 port：$(printf '%s ' $ports)"
fi

step "3. \${namespace} 有值（空字串是 2026-09 的回歸）"
for p in $PROJECTS; do
  # component get 的 env 一行一個 KEY=VALUE。
  val=$(get "$p" | grep -oE '\bNS=[^ ]*' | head -1 | cut -d= -f2)
  if [ "$val" = "$NS" ]; then ok "$p NS=$val"; else bad "$p NS='$val'，預期 '$NS'"; fi
done

step "4. 跨 component 引用展開成真的位址"
api_port=$(get api | awk '/^ports:/{print $2}' | sed 's/->.*//')
for p in web admin; do
  internal=$(get "$p" | grep -oE 'API_INTERNAL=[^ ]*' | cut -d= -f2-)
  public=$(get "$p" | grep -oE 'API_PUBLIC=[^ ]*' | cut -d= -f2-)
  # 容器內：靠網路 alias，不含 host port——用 IP 或 host port 都是錯的。
  if [ "$internal" = "http://api:80" ]; then
    ok "$p API_INTERNAL=$internal"
  else
    bad "$p API_INTERNAL=${internal}，預期 http://api:80"
  fi
  # 容器外：必須含 api 實際配到的 host port。
  case "$public" in
    *:"$api_port") ok "$p API_PUBLIC=$public" ;;
    *) bad "$p API_PUBLIC=${public}，預期結尾是 :$api_port" ;;
  esac
  # 引用出來的 port 要真的有東西在聽。位址的主機部分是 advertise host，
  # 它的正確性是 daemon 的事（而且會隨機器換網段而過期），這裡只驗 port。
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://127.0.0.1:$api_port/" 2>/dev/null)
  [ "$code" = 200 ] && ok "$p 引用到的 port 有回應" || bad "$p 引用到的 port 回 $code"
done

step "5. 宣告的 service 起來了，而且是自己的 component"
cache_phase=$(get api-cache | awk '/^phase:/{print $2}')
if [ "$cache_phase" = Running ]; then
  ok "api-cache Running（可以像任何 component 一樣管理）"
else
  bad "api-cache phase=$cache_phase"
fi
cache_container_before=$(get api-cache | awk '/^container:/{print $2}')

step "6. health probe 沒有打到別人（0 次 health_unreachable）"
# api 的 health.port 寫的是 container port 80，而 80 這個號碼不屬於任何人的 host
# port——沒擋住的話 probe 會去打 127.0.0.1:80，那在多專案的機器上遲早是別人。
unreachable=$("$QOOQ" component events "$NS/api" 2>/dev/null | grep -c health_unreachable)
if [ "$unreachable" = 0 ]; then
  ok "api 沒有 health_unreachable 事件"
else
  bad "api 有 $unreachable 次 health_unreachable——probe 沒打到自己"
fi

step "7. 推第二個 commit：重佈保留 port，且不重建 service"
api_port_before=$api_port
printf 'api v2\n' > "$WORK/api/site/index.html"
git -C "$WORK/api" -c user.email=t@t -c user.name=t commit -qam v2
git -C "$WORK/api" push -q origin main
"$QOOQ" watch trigger "$NS-api" >/dev/null 2>&1

deadline=$(( $(date +%s) + 180 ))
while :; do
  body=$(curl -s --max-time 5 "http://127.0.0.1:$api_port_before/" 2>/dev/null)
  case "$body" in *"api v2"*) break ;; esac
  if [ "$(date +%s)" -ge "$deadline" ]; then
    bad "逾時：第二個 commit 沒有部署上去"
    break
  fi
  sleep 5
done

api_port_after=$(get api | awk '/^ports:/{print $2}' | sed 's/->.*//')
if [ "$api_port_before" = "$api_port_after" ]; then
  ok "重佈後仍是 port $api_port_after"
else
  bad "重佈把 port 從 $api_port_before 換成了 $api_port_after"
fi

# 這一項是 2026-09 那個「每次 push 重建資料庫」回歸的守門員：service 的內容
# 一個字都沒改，重佈 app 不該碰它。容器 id 變了就是被重建了。
cache_container_after=$(get api-cache | awk '/^container:/{print $2}')
if [ "$cache_container_before" = "$cache_container_after" ]; then
  ok "service 容器沒有被重建：${cache_container_after}"
else
  bad "service 被重建了：$cache_container_before → $cache_container_after"
fi

step "結果"
if [ "$FAILED" = 0 ]; then
  printf '  \033[32m全部通過\033[0m\n'
else
  printf '  \033[31m有項目失敗（見上）\033[0m\n'
fi
exit $FAILED
