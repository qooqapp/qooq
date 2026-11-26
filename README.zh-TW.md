# qooq

[English](README.md) | 繁體中文

**為 AI 輔助開發設計的本機部署平台。** 從原始碼到可存取的服務，一個指令完成。

qooq 在本機自動完成專案類型偵測、映像建置、容器部署與健康驗證，並持續維護服務狀態。
與 Claude Code、Codex、Cursor 等 coding agent 原生整合：開發者只需下達「部署」，即可取得可存取的服務網址，
無須自行撰寫 Dockerfile、管理連接埠或設定反向代理。

以 Apache 2.0 授權開源。所有建置與執行皆在本機完成，原始碼不會離開你的電腦。

## 特色

- **零設定部署** — 依專案結構自動偵測技術棧並產生部署設定，支援 Next.js、Vite/React、Express、
  FastAPI、Flask、Django、Go、Rust、Rails、Laravel、Spring Boot、.NET 與靜態網站，亦相容既有 Dockerfile。
- **宣告式設定** — 以 `qooq.yaml` 描述建置、連接埠、健康檢查、資源限制與相依服務，可納入版本控制。
- **託管相依服務** — 一行宣告即可啟用 PostgreSQL、MySQL、MariaDB、Redis、MongoDB；資料跨部署保存，
  就緒後才啟動應用程式，並自動注入 `DATABASE_URL` 等連線資訊。
- **機密管理** — 以 `inputs:` 宣告應用程式所需的金鑰與憑證，於部署時提供；`.env` 檔不會進入建置內容。
- **自我修復與自動回滾** — 程序異常結束時自動重啟，支援健康檢查；新版本無法啟動時自動回滾至上一個可用版本，
  連續失敗達上限即停止重試並回報原因。
- **穩定的服務位址** — 重新部署沿用相同連接埠，相依此位址的服務不受影響。
- **GitOps** — 監看 Git 儲存庫，推送即自動建置與部署（可選）。
- **多服務編排** — 以 `qooq.stack.yaml` 定義多個服務，依相依順序部署並自動串接位址。
- **Ingress** — 內建反向代理，以 `http://<name>.localhost` 取代連接埠存取服務，監聽的連接埠可自訂。
- **AI Agent 整合** — 提供 Claude Code、Codex、Cursor 共用的部署 skill，部署完成前實際驗證服務可存取；另提供 MCP server（`qooq mcp`），將部署、日誌、建置輸出與 secret 管理以工具形式提供給 agent。

## 系統需求

- macOS 或 Linux（amd64／arm64）；Windows 請使用 WSL2，AI agent 也需在 WSL2 內執行（部署 skill 的腳本為 bash）
- Docker（Docker Desktop、OrbStack、Colima 或 Docker Engine），執行 `docker ps` 可正常回應

## 快速開始

### 安裝

```bash
curl -fsSL https://raw.githubusercontent.com/qooqapp/qooq/main/install.sh | sh
```

若 `/usr/local/bin` 無寫入權限，改為安裝至使用者目錄：

```bash
curl -fsSL https://raw.githubusercontent.com/qooqapp/qooq/main/install.sh | PREFIX=~/.local/bin sh
```

### 啟動

```bash
qooq up                    # 於背景啟動，狀態資料儲存於 ~/.qooq
qooq service install       # 選用：設定為登入時自動啟動
```

### 部署

**透過 AI agent**：安裝 skill 後，於專案目錄開啟新的對話並要求部署。

```bash
qooq skill install qooq-deploy                 # Claude Code
qooq skill install qooq-deploy --agent codex   # Codex
qooq skill install qooq-deploy --agent cursor  # Cursor
```

Claude Code 亦可透過 plugin 安裝（未安裝 qooq 時會一併安裝）：
`/plugin marketplace add qooqapp/qooq`，接著 `/plugin install qooq-deploy@qooq`。

**MCP 工具**：讓 AI agent 以工具形式操作 qooq（部署目錄、列出服務、讀取日誌與建置輸出、儲存 secret）。
Claude Code plugin 已內建；其他情況執行一次註冊即可：

```bash
qooq mcp install                  # Claude Code
qooq mcp install --agent cursor   # Cursor
qooq mcp install --agent codex    # Codex
```

**透過 CLI**：不需 Git，直接部署目前目錄。

```bash
qooq deploy .
# deployed: http://my-app.localhost, http://my-app.192.168.1.20.nip.io
```

第一個位址供本機使用，第二個供同網段的其他裝置存取。重複執行即更新部署；內容未變更時不會重新建置。

### 設定

```bash
qooq config                          # 檢視目前設定
qooq config set ingress.port 8080    # <name>.localhost 使用的連接埠（預設 80；0 = 改用 IP:port）
qooq config set access local         # local：僅限本機；lan（預設）：同網段亦可存取
```

設定儲存於 `~/.qooq/config.yaml`，於下次啟動 qooq 時生效（`qooq down && qooq up`）。

### 疑難排解

```bash
qooq watch build-log <name>    # 建置輸出
qooq watch logs <name> -f      # 應用程式執行輸出
```

### 管理服務

| 指令 | 說明 |
|---|---|
| `qooq watch ls` | 列出所有服務與最近一次部署結果 |
| `qooq watch stop <name>` / `start <name>` | 停止／啟動服務 |
| `qooq watch rm <name>` | 移除服務及其資料（含資料庫） |
| `qooq down` | 停止 qooq（已部署的服務不受影響） |

## 架構

```
原始碼 ──► qooq-watcher ──► qooqd ──► 容器／原生程序
           偵測、建置、驗證     生命週期管理、自我修復
```

| 元件 | 職責 |
|---|---|
| **qooqd** | 單機容器編排：生命週期、重啟策略、健康檢查、資源限制、Ingress |
| **qooq-watcher** | 部署管線：專案偵測、映像建置、部署與驗證、Git 監看 |
| **qooq**（[qooq-cli](https://github.com/qooqapp/qooq-cli)） | 命令列工具與 AI agent skill |

皆為純 Go 靜態執行檔，無 CGO 相依。兩個 daemon 預設僅監聽 `127.0.0.1`。
部署即代表在本機執行該專案的程式碼，請僅部署可信任的來源。

## 文件

所有文件皆提供英文與繁體中文版本。

| 文件 | 內容 |
|---|---|
| [WALKTHROUGH](docs/WALKTHROUGH.zh-TW.md) | 入門教學，逐步說明每個階段的預期結果 |
| [USAGE](docs/USAGE.zh-TW.md) | 完整操作手冊：部署、GitOps、Stack、備份、疑難排解 |
| [DEPLOY-CONFIG](docs/DEPLOY-CONFIG.zh-TW.md) | `qooq.yaml` 欄位參考 |
| [qooq.reference.yaml](docs/qooq.reference.zh-TW.yaml) | 含完整註解的設定範例 |
| [ARCHITECTURE](docs/ARCHITECTURE.zh-TW.md) | 架構設計與安全模型 |
| [examples/](examples/) | 十三個可直接執行的範例專案，其中四個完全不需設定（Next.js、Vite + React、Flask、FastAPI + uv），一個示範兩個專案一起部署（`todo-stack`） |

## 從原始碼建置

```bash
make build     # 產生 bin/qooqd、bin/qooq-watcher
make test      # 單元測試（-race）
make check     # 靜態分析與依賴邊界檢查
make smoke     # 端對端測試（需 Docker）
```


## 授權

[Apache License 2.0](LICENSE)
