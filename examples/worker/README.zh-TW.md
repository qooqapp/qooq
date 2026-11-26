# worker — stdlib-only 背景長駐程序範例（build.template: go）

[English](README.md) | 繁體中文

一支不對外開 port 的背景程序：每 5 秒 `log.Println("tick")` 並 touch `/tmp/heartbeat`。
用來示範沒有 HTTP 介面的服務怎麼做 liveness health check——用 **exec probe** 檢查心跳檔案
是否在 1 分鐘內更新過。**故意沒有 Dockerfile**，`qooq.yaml` 用 `build.template: go`。

```
worker/
├── qooq.yaml   # build.template: go + health.exec + policy.restart: always
├── main.go     # 每 5s tick + touch /tmp/heartbeat
└── go.mod
```

（獨立 go module，`module example.com/qooq-examples/worker`，stdlib only，無 go.sum 也沒關係。）

## 怎麼跑（本機示範）

```bash
cp -r examples/worker ~/worker && cd ~/worker
git init -q && git add -A && git commit -qm init
git clone --bare . ~/worker.git && git remote add origin ~/worker.git

qooq watch add --name worker --repo file://$HOME/worker.git \
  --component worker --poll 30

# 驗收：≤30 秒後
qooq watch get worker              # last_build_status: success
qooq component logs worker --tail 20   # 預期每 5 秒一行 "tick"
```

## qooq.yaml 重點

- `health.exec` — 用 `find /tmp/heartbeat -mmin -1 | grep -q .` 判斷心跳；**不用 `pgrep`**，
  因為容器內程序名是完整路徑 `/app`，`pgrep` 匹配不到。
- `policy.restart: always` — 這是背景程序，任何退出都該重啟（不像 web app 可能是刻意
  被 operator stop）。
- 沒有 `run.ports`：這個範例不對外提供服務。
