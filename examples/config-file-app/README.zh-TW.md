# config-file-app — 設定檔注入 + 資料持久化（`run.files` / `run.volumes`）

[English](README.md) | 繁體中文

大部分真實服務都是這個形狀：**啟動時從工作目錄讀一份設定檔，執行時往某個目錄寫資料**。
這個範例把兩件事各用一行 qooq.yaml 解決。

```
config-file-app/
├── qooq.yaml     # run.files 掛設定、run.volumes 給資料
├── config.toml   # app 從 cwd 讀的設定（進版控，跟著 commit 走）
├── main.go       # 讀 config.toml、往 data_dir 追加一行、GET / 顯示累計次數
└── go.mod
```

`GET /` 會回設定檔裡的 greeting，加上「這個容器啟動過幾次」——那個數字存在 volume 裡，
**重新部署不會歸零**。

## 兩個欄位在做什麼

```yaml
run:
  files:
    - "./config.toml:/app/config.toml"   # 設定：唯讀、跟著 commit 版本化
  volumes:
    - "appdata:/app/data"                # 資料：讀寫、永不隨部署清除
```

- **`files` 的來源是 repo 相對路徑**，所以設定跟程式碼一起進版控。watcher 會把它複製到
  `<mounts.dir>/config-file-app/files/<短sha>/config.toml` 再掛進容器——**按 commit 版本化**，
  所以還在跑的舊容器不會因為你 push 了新設定就突然讀到新值。
- **`volumes` 的 `appdata` 是具名 volume**（沒有斜線＝watcher 管理的目錄），
  落在 `<mounts.dir>/config-file-app/volumes/appdata`。這個目錄**永遠不會被清理**，
  刪掉 target 也不會刪它。
- 設定檔預設**唯讀**掛載：容器內若是 root，可寫的掛載會在 host 留下 root 所有的檔案，
  之後 watcher 清不掉。要寫入的東西一律放 `volumes`。

`workdir: /app` 這個模板變數是關鍵：app 讀的是相對路徑 `config.toml`，
所以掛載點 `/app/config.toml` 必須正好落在容器的工作目錄裡。

## 怎麼跑（本機示範）

```bash
# 1. 起 qooqd 與 watcher（在 scratch 目錄跑，別讓 watcher.db 掉進 repo）
qooqd -listen 127.0.0.1:7777 -data /tmp/qooq/qooq.db &
QOOQ_WATCHER_MOUNTS_DIR=/tmp/qooq/mounts qooq-watcher &

# 2. 把這個目錄推進一個本機 bare repo，掛成 target
qooq watch add --name config-file-app \
  --repo file:///tmp/qooq/config-file-app.git --component config-file-app --poll 30

# 3. 驗收
curl $(qooq component get config-file-app | awk '/endpoints:/{print $2}')   # 看到 config.toml 裡的 greeting
cat /tmp/qooq/mounts/config-file-app/volumes/appdata/starts.log
```

改 `config.toml` 的 `greeting` 再 push，30 秒內重新部署、`curl` 會看到新字串，
而 `starts.log` 的累計次數**繼續往上加**——設定換了，資料還在。

完整欄位說明見 [docs/DEPLOY-CONFIG.md](../../docs/DEPLOY-CONFIG.zh-TW.md) 的
「Files 與 Volumes」一節。
