# fullstack-server — React(Vite) + Go(gin) 單 binary 範例（build.template: go-react）

[English](README.md) | 繁體中文

前端是最小的 Vite + React 專案（`web/`，一個按鈕呼叫 `/api/hello`）；後端是 Go + gin
（`cmd/server/main.go`），用 `go:embed` 把前端 build 產物包進同一支 binary
（`internal/webui/webui.go` 的 `//go:embed all:dist`）。**故意沒有 Dockerfile**——
`qooq.yaml` 用 `build.template: go-react`：watcher clone 後在 `web/` 跑 `npm install`
+ `npm run build`，把 `dist/` 複製進 `internal/webui/dist/`，再編出單一 Go binary。

```
fullstack-server/
├── qooq.yaml              # build.template: go-react（全用預設模板變數）
├── web/                   # Vite + React 前端（未跑過 npm install，無 lockfile）
│   ├── package.json
│   ├── vite.config.js
│   ├── index.html
│   └── src/{main.jsx,App.jsx}
├── internal/webui/
│   ├── webui.go           # //go:embed all:dist
│   └── dist/index.html    # 佔位檔——docker build 會覆蓋成真正的 Vite 產物
├── cmd/server/main.go     # gin：GET /api/hello + 其餘路徑服務 embed 的 dist
├── go.mod
└── go.sum
```

（獨立 go module，`module example.com/qooq-examples/fullstack-server`；`internal/webui/dist/`
放了一個一行的佔位 `index.html`，只是為了讓 `go build`/`go vet` 在**沒跑過前端 build** 的
情況下也能通過編譯——`docker build` 時模板會用真正的 Vite 產物覆蓋它。）

## 怎麼跑（本機示範）

```bash
cp -r examples/fullstack-server ~/fullstack-server && cd ~/fullstack-server
git init -q && git add -A && git commit -qm init
git clone --bare . ~/fullstack-server.git && git remote add origin ~/fullstack-server.git

qooq watch add --name fullstack-server --repo file://$HOME/fullstack-server.git \
  --component fullstack-server --poll 30

# 驗收：≤30 秒後（第一次 build 含 npm install + go mod download，需要網路，會比較久）
qooq watch get fullstack-server               # last_build_status: success
open $(qooq component get fullstack-server | awk '/endpoints:/{print $2}')   # 預期看到頁面，按按鈕會顯示
                                                # "hello from fullstack-server"
curl $(qooq component get fullstack-server | awk '/endpoints:/{print $2}')/api/hello   # 預期 {"message":"hello from fullstack-server"}
```

**docker build 需要網路**：go-react 模板會連 npm registry（前端依賴）與 Go module proxy
（gin），離線環境需自建 registry mirror 才能 build。

## qooq.yaml 重點

- `build.template: go-react` — 本範例的目錄結構（`web/`、`internal/webui/dist`、
  `cmd/server`）與模板變數預設值完全對齊，`template_vars` 全部省略。
- `embed_dest`（模板變數，預設 `internal/webui/dist`）**必須與 `webui.go` 的
  `//go:embed all:dist` 路徑一致**——這兩處是耦合的，改一邊要一起改。
- `run.ports` 寫 `auto:8080`——host port 由 daemon 配置（重佈拿回同一個），`8080` 是容器內 gin 監聽的 port，`health.port` 對的也是它。
