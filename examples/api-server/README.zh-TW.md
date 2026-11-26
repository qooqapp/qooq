# api-server — Go + gin 最小 API 範例（build.template: go）

[English](README.md) | 繁體中文

一個 Go + [gin](https://github.com/gin-gonic/gin) 寫的最小 HTTP API：`GET /hello` 回
`{"message":"hello from api-server"}`，監聽 `:8080`。**本範例故意沒有 Dockerfile**——
`qooq.yaml` 用 `build.template: go`，watcher clone 後會用內建 go 模板現場生成
`Dockerfile.qooq-generated` 再 build（見 [docs/DEPLOY-CONFIG.md](../../docs/DEPLOY-CONFIG.zh-TW.md) 第三節）。

```
api-server/
├── qooq.yaml   # build.template: go（不放 Dockerfile）
├── main.go     # GET /hello
├── go.mod
└── go.sum
```

（本目錄是獨立 go module，`module example.com/qooq-examples/api-server`——repo 根的
`go build ./...` 不會踩到它。）

## 怎麼跑（本機示範）

```bash
# 0. 先起 qooqd + watcher

# 1. 變成 git repo + bare remote
cp -r examples/api-server ~/api-server && cd ~/api-server
git init -q && git add -A && git commit -qm init
git clone --bare . ~/api-server.git && git remote add origin ~/api-server.git

# 2. 註冊 watch target
qooq watch add --name api-server --repo file://$HOME/api-server.git \
  --component api-server --poll 30

# 3. 驗收：≤30 秒後（watcher 用 go 模板 build，第一次含下載 Go module，稍久）
qooq watch get api-server                      # last_build_status: success
curl $(qooq component get api-server | awk '/endpoints:/{print $2}')/hello   # 預期 {"message":"hello from api-server"}
```

## qooq.yaml 重點

- `build.template: go` — 不用自己寫 Dockerfile；模板變數皆用預設值（go_version 1.25、
  base_image alpine:3.20、port 8080）。
- `run.ports` 寫 `auto:8080`，`health.port` 對的是同一個容器 port——改 port 記得也一起改模板變數 `port`（模板的
  `port` 只影響 `EXPOSE`，不會同步應用程式裡寫死的監聽 port）。
