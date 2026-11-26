# app-with-postgres — 用一行宣告拿到一台資料庫（`services:`）

[English](README.md) | 繁體中文

這個範例的相依宣告只有四行：

```yaml
services:
  db:
    type: postgres
```

image、port、資料 volume、bootstrap 環境變數、健康檢查全部由 `type` 帶預設值。
部署後會有**兩個** component：

```
$ qooq component ls
NAME                   IMAGE                       PHASE
app-with-postgres-db   postgres:16-alpine          Running
app-with-postgres      app-with-postgres:<sha7>    Running
```

`app-with-postgres-db` 是**普通 component**，不是黑盒：`qooq component logs/get/stop/rollback`
都適用，也可以自己另外 deploy 一台資料庫加入同一個 network 而不用 `services:`。

## app 拿到什麼

什麼都不用寫就有這些環境變數（`db` 這個服務名 → 前綴 `DB`）：

```
DB_HOST=app-with-postgres-db   DB_PORT=5432   DB_USER=app_with_postgres
DB_PASSWORD=<自動生成>          DB_DATABASE=app_with_postgres
DB_SCHEME=postgres             DB_URL=postgres://…
DATABASE_URL=postgres://…      # 服務名叫 db 時額外給這個慣例名
```

框架期待別的變數名時，用插值自己指（本範例的 `MYAPP_DB_DSN` 就是這樣來的）：

```yaml
run:
  env:
    MYAPP_DB_DSN: "postgres://${service.db.user}:${service.db.password}@${service.db.host}:${service.db.port}/${service.db.database}?sslmode=disable"
```

`GET /` 會把 qooq 注入的連線資訊印出來（密碼遮蔽），`GET /healthz` 在沒拿到資料庫時回 503。

## 三個你不用自己處理的細節

- **服務先起、確認 ready，app 才部署**（最多等 3 分鐘），所以啟動時就連 DB 的 app 不會撲空。
- **密碼生成一次就固定**。密碼在資料庫初始化時寫進資料目錄，之後換掉會讓 app 打不開自己的資料
  ——所以它存在 target 上（有 `secret.key` 時加密），每次部署重複使用。
- **改 app 的程式碼重新部署時，資料庫不會被重啟**：服務規格沒變就完全不動它。

## 怎麼跑（本機示範）

```bash
qooqd -listen 127.0.0.1:7777 -data /tmp/qooq/qooq.db &
QOOQ_WATCHER_MOUNTS_DIR=/tmp/qooq/mounts qooq-watcher &

qooq watch add --name app-with-postgres \
  --repo file:///tmp/qooq/app-with-postgres.git --component app-with-postgres --poll 30

curl $(qooq component get app-with-postgres | awk '/endpoints:/{print $2}')   # 看到注入的連線資訊
qooq component logs app-with-postgres-db --tail 20
```

想從 host 用 `psql` 連進去，把服務的 port 發布出來：

```yaml
services:
  db:
    type: postgres
    ports: ["15432:5432"]
```

完整欄位說明見 [docs/DEPLOY-CONFIG.md](../../docs/DEPLOY-CONFIG.zh-TW.md) 的「Services」一節。
