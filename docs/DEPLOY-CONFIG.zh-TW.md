# qooq.yaml 部署設定參考

[English](DEPLOY-CONFIG.md) | 繁體中文

*app 開發者寫給 qooq-watcher 看的部署宣告檔——描述這個 app 怎麼 build、怎麼跑、需要什麼資源。*

> 操作手冊見 [USAGE.md](USAGE.zh-TW.md)；系統架構見 [ARCHITECTURE.md](ARCHITECTURE.zh-TW.md)。
> 可跑範例見 [examples/](../examples/)（索引在本檔「範例」一節）。

`qooq.yaml` 放在 app repo 的根目錄，由開發者撰寫、跟著程式碼走。watcher 每次 build 前讀取它。

## 目錄

- [基本規則](#基本規則)
- [精簡格式](#精簡格式)
- [欄位參考](#欄位參考)
  - [General 頂層欄位](#general-頂層欄位)
  - [Build](#build)
  - [Run](#run)
  - [Files 與 Volumes](#files-與-volumes)
  - [Services](#services)
  - [Health](#health)
  - [Ingress](#ingress)
  - [Inputs](#inputs)
  - [Policy](#policy)
  - [Requests](#requests)
  - [Limits](#limits)
- [${...} 插值](#-插值)
- [多個專案要互連：設定寫在哪](#多個專案要互連設定寫在哪)
- [內建 Dockerfile 模板](#內建-dockerfile-模板)
  - [使用前要知道的三件事](#使用前要知道的三件事)
- [範例](#範例)
- [欄位流向](#欄位流向)
- [相關文件](#相關文件)

> [!TIP]
> 想一次看完所有欄位與預設值：[qooq.reference.yaml](qooq.reference.zh-TW.yaml) 是一份
> 逐欄標註的完整 manifest（查閱用，不是拿來整份複製）。

## 基本規則

- **位置**：repo 根目錄的 `qooq.yaml`。target 可用 `--manifest PATH` 換路徑、
  `--manifest -` 完全停用；檔案不存在＝沒有 manifest，行為與純 deploy_spec 部署相同。
- **未知欄位容忍**：多寫的 key 會被忽略，不會炸。
- **只有 `version` 是必填**：其餘欄位全部選填，不寫就用預設值（各表的「必填」欄
  標得很清楚）——一份只有五行 general 欄位的 qooq.yaml 就能部署。

> [!IMPORTANT]
> **合併優先序**：manifest 提供**預設值**，target 的 `deploy_spec`（operator 設定）
> 逐欄位、非零值覆蓋——repo 是半信任輸入，operator 永遠有最後決定權。

> [!NOTE]
> 大小上限 1MiB；`build.*` 的路徑欄位必須是 repo 內相對路徑（擋絕對路徑、`..`
> 與 symlink 逃逸）。驗證失敗一律記在 target 的 `last_build_status=failed` +
> `last_build_error`（`qooq watch get <name>` 可看），且不動 `last_commit`——
> 修好 push 上去，下一輪就會重試。

## 精簡格式

多數 app 寫到這樣就能部署——general 欄位（`version` / `app` / `description` /
`requests` / `domain`）加一個 build 方式，其餘全走預設值：

```yaml
version: 1
app: api-server
description: Go + gin 最小 API 範例
requests:          # 這台機器至少要有這麼多資源才部署（省略 = 不檢查）
  cpu: 0.25
  memory: 128Mi

build:
  template: go
```

容器的資源硬上限（`limits`）不寫也行——會自動用 `2× requests`。連 `requests`
都不想寫也可以，那就完全不檢查、也不設限（適合先跑起來再說）。

要對外開 port、做健康檢查、調重啟策略時，才需要往下加 `run` / `health` /
`policy` 區塊——欄位都在下面的參考表裡。完整可跑版本見
[examples/api-server/qooq.yaml](../examples/api-server/qooq.yaml)。

## 欄位參考

每張表都有「必填」欄：**空白＝選填**。整份檔案裡真正必填的只有 `version` 一個。

> [!IMPORTANT]
> `build` / `run` / `health` / `policy` / `requests` / `limits` 都是選填 block。
> **任何一個沒寫，就以它的預設值正常啟動**——不是停用功能，也不會讓部署失敗。
> 各 block 標題下方直接寫出「省略時＝什麼預設值」。

### General 頂層欄位

> 「general」只是本文件的分組稱呼，**yaml 裡沒有 `general:` 這一層**——
> 這幾個欄位直接寫在最外層（如上面精簡格式範例）。

| 欄位 | 型別 | 必填 | 可填的值 / 說明 |
|---|---|---|---|
| `version` | int | ✅ | 只支援 `1` |
| `app` | string | | app 名稱，說明用途（不影響部署） |
| `description` | string | | 一句話說明，純 metadata（不影響部署） |
| `network` | string | | 容器網路名稱。**省略＝專屬網路 `qooq-<component>`**（只有這個 app 與它的 services 在裡面）。填同一個名字的多個 app 會在同一張網路上，彼此可用 **component 名稱**互打（例 `http://api:8080`）。沒有 manifest 的 target 維持引擎預設，行為與 M23 之前相同 |
| `domain` | string | | 對外服務的網域，例 `app.example.com`。qooqd 開 `-ingress` 時**會實際路由**（並排在 `endpoints` 最前面）；沒開就只是 metadata。多網域、path prefix、指定 port 見下方 [Ingress](#ingress) |

### Build

省略時的預設值：用 repo 根目錄的 `Dockerfile` 建置。

> [!NOTE]
> **repo 裡同時有 `Dockerfile` 與 `qooq.yaml` 時兩個都會用，各管一半**：Dockerfile 決定**怎麼建**
> （image 長什麼樣），qooq.yaml 決定**怎麼跑**（ports、env、services、health、limits）。qooq.yaml 不會取代
> Dockerfile。會讓 Dockerfile **不被用**的只有三種寫法：
> - `build.template`：改用內建模板，render 出 `Dockerfile.qooq-generated` 來建（repo 裡的 Dockerfile 被忽略）；
> - `build.dockerfile: <別的路徑>`：改用那一份；
> - `requests.runtime: process`：不建 image，跑 `build.command` 拿 `build.artifact`。
>
> **`build.dockerfile` 是相對於 `build.context` 的**：設了 `build.context: web` 之後，預設的 `Dockerfile`
> 指的是 `web/Dockerfile`，不是 repo 根那一份。
>
> repo **沒有** qooq.yaml 時，qooq 看檔案長相自己推一份（Dockerfile 依序找 `Dockerfile`、`docker/`、
> `build/`、`.docker/`，都沒有才挑內建模板）——**這份只在第一次產生**，之後搬動或新增 Dockerfile
> 不會重新偵測；要改就用 `qooq watch manifest`，或乾脆在 repo 放一份 qooq.yaml（repo 的那份永遠優先）。

| 欄位 | 型別 | 必填 | 可填的值 / 說明 |
|---|---|---|---|
| `build.dockerfile` | 相對路徑 | | Dockerfile 位置，預設 `Dockerfile`。與 `template` 互斥 |
| `build.template` | string | | 內建模板名：`static-nginx` / `go` / `node` / `python` / `go-react` / `rust` / `php` / `ruby` / `java` / `dotnet` / `bun`（見「內建 Dockerfile 模板」一節）。repo 可不放 Dockerfile。與 `dockerfile` 互斥（同時設定報錯） |
| `build.template_vars` | map | | 覆蓋模板的預設變數（見「內建 Dockerfile 模板」各模板表） |
| `build.context` | 相對路徑 | | build context 子目錄，預設 repo 根（monorepo 用） |
| `build.command` | []string | | **process runtime 專用**：在 clone 目錄跑的建置指令，例 `["go","build","-o","bin/app","."]`；不寫＝不建置 |
| `build.artifact` | 相對路徑 | process 時 ✅ | **process runtime 專用**：建置產物（可執行檔）位置 |

### Run

省略時的預設值：用 image 內建的啟動指令，不對外發布 port。

| 欄位 | 型別 | 必填 | 可填的值 / 說明 |
|---|---|---|---|
| `run.command` | []string | | 覆蓋 image 的啟動指令（process 模式為 artifact 的 argv） |
| `run.env` | map | | 環境變數 |
| `run.ports` | []string | | `"host:container"`（例 `"8080:80"`）、`"auto:container"`（**host port 交給 daemon 挑**，見下）或裸 `"container"`（**不發布 host port**，只在容器網路內可見——搭配 ingress 用）。第一筆是 ingress 路由的目標。process runtime 下無作用（只 log 提示） |
| `run.env_file` | []string | | dotenv 檔清單，**依序**載入（後面的蓋前面的）。來源形式同 mount（見下節）：repo 相對路徑、host 絕對路徑、或 `"volume:/檔案路徑"`。檔案**內容不做插值** |
| `run.files` | []mount | | 掛進容器的設定檔／目錄，**預設唯讀**。見下節 |
| `run.volumes` | []mount | | 可持久化的讀寫目錄（資料）。見下節 |

**環境變數優先序**（低 → 高）：`services` 自動注入 → `run.env_file`（依序） →
`run.env` → operator 的 `deploy_spec.env`。

### Files 與 Volumes

`run.files` 是**設定**（預設唯讀），`run.volumes` 是**資料**（讀寫、跨部署保留）。
兩者語法相同，**短字串與長物件都吃**：

```yaml
run:
  files:
    - "./config.toml:/app/config.toml"       # 短字串 src:dst[:ro|rw]
    - from: config/app.toml                  # 等價長物件
      to: /app/config.toml
      mode: ro
  volumes:
    - "appdata:/app/data"                    # 具名 volume（watcher 管理目錄）
    - "/srv/todo/data:/app/data"         # 直接指定 host 目錄
```

來源形式由字串本身判斷：

| `from` 寫法 | 意思 | host 上的實際位置 |
|---|---|---|
| 相對路徑（`./x`、`config/x`） | repo 內容，build 後複製出來 | `<mounts.dir>/<component>/files/<sha7>/<路徑>` |
| 絕對路徑（`/etc/x`、`${env.HOME}/x`） | host 上既有的檔案／目錄，直接掛 | 原路徑 |
| 無斜線的純名稱（`appdata`） | 具名 volume，watcher 建目錄 | `<mounts.dir>/<component>/volumes/<name>` |

> `config.toml` 這種**有點號的名字算路徑不算 volume 名**。要指 repo 根的檔案，
> 寫 `./config.toml` 最不會有歧義。

規則與保證：

- `to` 必須是絕對路徑、同一份 spec 內不得重複、不得為 `/`。
- repo 相對來源**按 commit 版本化**：舊容器不會因為新 build 就地覆寫而突然讀到新設定。
  只保留最新兩份（current + previous，對齊 rollback 的語意）。
- **具名 volume 重新部署時永不清除**——那是資料。**移除專案（刪 target）會連同它與它
  `services:` 的資料一起刪除**（2026-10-30 起）：留下來的資料庫目錄會讓同名重建的資料庫
  不認新產生的密碼，app 永遠登不進去。要保留就用 `qooq watch rm --keep-component`
  （容器與資料都留著）。
- `files` 預設 `ro`：容器內的 root 若能寫，會在 host 留下 root 所有的檔案，
  之後 watcher 清不掉。要寫入就用 `volumes`。
- **rollback 只換 image，不換設定**（維持既有語意）。

> [!WARNING]
> **最常見的坑：app 實際寫檔的路徑，跟你掛 volume 的路徑對不上。**
>
> 症狀很好認也很難查——容器 build 成功、起來、log 只有一兩行，然後**沒有錯誤訊息就退出**。
> 框架的啟動階段（fx OnStart、Django migrate、Rails initializer 之類）建不了檔就整個中止，
> 而那個 error 常常來不及寫進 log。
>
> 兩件事要對：
>
> 1. **app 的資料路徑通常是相對 cwd 的**。sqlite 寫 `./data.db`、cwd 是 `/app`，
>    那個檔就在 `/app/data.db`——掛在 `/app/data` 的 volume **蓋不到它**。
>    要嘛把 volume 掛在能蓋到的位置，要嘛用 `run.env` 把 app 的路徑設定指進 volume 裡。
> 2. **Dockerfile 有 `USER <非 root>` 時，WORKDIR 通常仍屬於 root**，
>    app 沒有權限在那裡建檔。這類 image 一定要把所有會寫入的路徑指進 volume。
>
> 診斷方式：`qooq component logs <名字>`。死前最後一行通常就寫著它在開哪個檔。
>
> ```yaml
> run:
>   volumes:
>     - "appdata:/app/data"
>   env:
>     # 把 app 的資料路徑指進上面掛的 volume，而不是預設的 cwd
>     MYAPP_DATABASE_PATH: /app/data/data.db
> ```

安全（watcher config `mounts.policy`，env `QOOQ_WATCHER_MOUNTS_POLICY`）：

| 值 | 行為 |
|---|---|
| `open`（**預設**） | 全部允許，含 manifest 指定 host 絕對路徑。延續既有立場：建立 target ＝ 信任該 repo |
| `repo-only` | manifest 只能用 repo 相對來源與具名 volume；operator 的 `deploy_spec.mounts` 不受此限 |
| `allowlist:/a,/b` | 絕對路徑必須落在列出的 prefix 底下（operator 的也一樣） |

> [!IMPORTANT]
> bind mount 的來源路徑是 **docker daemon 在 host 上解析**的，不是在 watcher 的檔案系統裡。
> watcher 若跑在容器內，`mounts.dir` 必須是 host bind 路徑、而且在 watcher 容器內掛在**完全相同的路徑**，
> 否則 watcher 寫得成功、docker 卻找不到。

### Services

宣告 app 需要的相依容器（資料庫、快取）。**一行 `type:` 就是完整宣告**——
image、port、資料目錄、bootstrap 環境變數、健康檢查全部有預設值。

```yaml
services:
  db:
    type: postgres        # postgres|mysql|mariadb|redis|mongo|custom
```

展開後就是一個**普通 component**，名稱 `<app-component>-<key>`（上例：`myapp-db`），
在 app 的 group 與 network 上，`qooq component ls/get/logs/stop/rollback` 全部適用。

| 欄位 | 型別 | 必填 | 可填的值 / 說明 |
|---|---|---|---|
| `type` | string | | `postgres`(預設 16) / `mysql`(8) / `mariadb`(11) / `redis`(7) / `mongo`(7) / `custom`。`custom` 必須自己給 `image` 與 `port` |
| `version` | string | | 套進 type 預設 image 的版本，例 `"15"` → `postgres:15-alpine`。設了 `image` 就忽略 |
| `database` / `user` | string | | 預設都是 app 的 component 名（正規化：小寫、非英數轉 `_`） |
| `password` | string | | 省略＝**首次自動生成並記住**（存在 target 上，有 `secret.key` 時加密）。支援 `${...}`，可指向 `${env.DB_PASSWORD}` |
| `storage.volume` | string | | 資料 volume 名，預設 `<component>-<service>-data` |
| `storage.path` | string | | 容器內資料路徑，預設由 type 決定 |
| `storage.size` | string | | 只記錄，v1 不強制 |
| `image` / `port` | string / int | custom 時 ✅ | 覆蓋 type 推導值 |
| `env` | map | | 額外環境變數，蓋在 type 的 bootstrap 變數之上 |
| `ports` | []string | | 對 host 發布（例 `"15432:5432"`）。**預設不發布**，服務只在 network 內可見 |
| `health` | health 區塊 | | 覆蓋 type 內建的健康檢查 |
| `replicas` / `mode` | int / string | | **保留給叢集**。v1 只接受 `1` / `single`，其他值明確報「尚未支援」，不會安靜地只跑一份 |

app 自動拿到這些環境變數（`<NAME>` 是服務名大寫，`db` → `DB`）：

```
DB_HOST=myapp-db  DB_PORT=5432  DB_USER=myapp  DB_PASSWORD=…  DB_DATABASE=myapp
DB_SCHEME=postgres  DB_URL=postgres://…
DATABASE_URL=postgres://…        # 服務名叫 db 時額外給這個慣例名
```

各型別的 URL 都是那個伺服器真的收的形式：

- **redis 沒有 database**，所以不給 `_DATABASE`；使用者是 redis 內建的 `default`，URL 是
  `redis://default:<password>@<host>:6379`。生成的密碼真的會設進 redis（`requirepass`），
  所以只用 `_HOST`／`_PORT` 連而不帶密碼會被拒絕。
- **mongo 的 URL 帶 `?authSource=admin`**：映像檔把使用者建在 `admin`，
  不帶這個參數的話，密碼對了也會 `Authentication failed`。

框架要別的變數名時用插值自己指：

```yaml
run:
  env:
    APP_DATABASE_DRIVER: postgres
    APP_DATABASE_HOST: "${service.db.host}"
    APP_DATABASE_PORT: "${service.db.port}"
    APP_DATABASE_PASSWORD: "${service.db.password}"
```

行為保證：

- **服務先起、確認 ready，app 才部署**（最多等 3 分鐘），所以啟動時就連 DB 的 app 不會撲空。
- **服務規格沒變就不動它**：改 app 的程式碼重新部署，資料庫不會跟著重啟。
- **密碼永久不變**：一旦生成就存起來重複使用——密碼在 DB 初始化時就寫進資料目錄，
  之後換掉會讓 app 打不開自己的資料。
  密碼在**建立服務之前**就存好，所以第一次部署中途失敗也不會弄丟它。
- **從 qooq.yaml 拿掉的服務不會被自動刪除**：下一次部署會**停掉**它，資料留著，
  `qooq watch get` 會標出「已不在 qooq.yaml」。
  加回 qooq.yaml 就會用原本的資料與密碼恢復；確定不要了，
  `qooq watch service-rm <target> <service>`（要打出服務名稱確認）才會刪掉容器與資料。
  移除整個專案時也會一併刪除。理由：改錯一行 YAML（或改了服務名字）不應該讓資料庫永久消失。
- `requests.runtime: process` + `services:` → 明確報錯（process runtime 沒有容器引擎）。

### Health

省略時的預設值：不掛主動 liveness 探測（container crash 仍會被 monitor 偵測、
依 `policy` 處理）。有寫這個區塊就必須給 `http` 或 `exec` 其中一個。

| 欄位 | 型別 | 必填 | 可填的值 / 說明 |
|---|---|---|---|
| `health.http` | string | 二擇一 | HTTP probe 路徑，例 `/healthz` |
| `health.exec` | []string | 二擇一 | exec probe 指令 |
| `health.port` | int | 用 http 時 ✅ | probe 打哪個 port，**必須是 `run.ports` 裡出現過的**（不是的話部署直接失敗——不擋的話 probe 會去打 `127.0.0.1:<那個號碼>`，那遲早是**別人的** component，於是壞掉的 app 檢查到一個健康的陌生人、回報自己健康、永遠不會被重啟）。**建議填 container port**：那個寫法在 `auto:` 下仍然正確，qooq 會自動打到配到的 host port。只有該 container port **完全沒發布**時才會走容器 IP（Docker Desktop 打不到，那個組合要用 exec probe） |
| `health.interval` | duration | | probe 間隔，例 `"10s"`（預設 `10s`） |
| `health.timeout` | duration | | 單次 probe 逾時（預設 `5s`） |
| `health.failures` | int | | 連續失敗幾次觸發重啟（預設 `3`） |
| `health.delay` | duration | | 啟動後的寬限期（預設 `5s`） |

> [!IMPORTANT]
> **probe 從來沒成功過的話，qooq 不會重啟這個 component。** 一次都沒通過代表的是「probe 看不到
> 這個 app」（port 填錯、路徑填錯、或容器 IP 在這台 docker host 上不可達），不是「app 壞了」；
> 照著重啟只會把一個好好的容器重建到天荒地老，而使用者看到的是「一直在重啟」且 app 自己的 log
> 裡什麼都沒有。這種情況會記一筆 `health_unreachable` event 說明**推測的原因與怎麼修**
> （`qooq component events <name>` 看得到），然後繼續 probe——哪天通了就自動恢復重啟能力，
> 不用做任何事。**通過過一次之後**行為完全照舊：連續失敗達 `health.failures` 就重建。

#### 新版本起不來時：自動回到上一版，但會中斷幾秒

新版本換上去之後，watcher 會等它證明站得住（health 通過；沒有 health 就是穩定 Running 10 秒）。
它結束、crash 後被重啟、或 health 一直不過，就把**上一版的 image 換回來**，部署標成 `failed`
並附上新版本最後的輸出，而且**輪詢不會再部署同一個 commit**（推新 commit 或強制重新部署才會）。

**換版那段時間服務會中斷**：從舊容器被換掉、新版本被判定起不來，到舊版重新起來，通常幾秒到
十幾秒（實測：每 8 秒打一次，中間有一次沒回應）。原因是新舊兩個容器**不能同時佔用同一個
host port**，所以沒辦法「新的先在旁邊起來、驗過再切」。真正的零停機要靠前面有一層代理在兩個
容器之間切換（`-ingress` 的 Caddy 是那一層的雛形），目前還沒做。
注意 **回滾只換 image，不換設定**：新 commit 若同時改了 `run.env` 之類的設定，舊 image 會帶著
新設定起來。

### Ingress

只在 qooqd 開了 `-ingress` 時有作用（用法見 [USAGE.md](USAGE.zh-TW.md)「用網址取代 port」）。
省略整塊＝預設值：**衍生的網址、整個 host 全轉、打第一個 port**——絕大多數 app 不用寫。

```yaml
domain: app.example.com     # 單一網域的簡寫，等同 ingress.hosts: [app.example.com]

ingress:
  hosts: [app.example.com, www.example.com]   # 多網域時用這個
  port: 8080                # 開多個 port 時指定哪個對外
  path: /api                # 只轉這個前綴（兩個 app 就能共用同一個網域）
  strip_path: true          # 轉發前拿掉前綴
  tls: true                 # 覆蓋 daemon 的 -ingress-tls
```

| 欄位 | 型別 | 必填 | 可填的值 / 說明 |
|---|---|---|---|
| `ingress.hosts` | []string | | 額外的 hostname。**與 `domain` 相加不是取代**，衍生的 `<名字>.localhost` 等也照給 |
| `ingress.port` | int | | 路由目標的 container port。省略＝`run.ports` 第一筆。**必須是 `run.ports` 裡有的**，不是的話 build 直接失敗並列出有哪些 |
| `ingress.path` | string | | 只轉這個 URL 前綴，要以 `/` 開頭。省略＝整個 host。前綴本身與其下的路徑都會中（`/api` 與 `/api/*`） |
| `ingress.strip_path` | bool | | 轉發前拿掉 `path`。預設 `false`（原樣轉過去）。app 自己服務 `/` 就要設 `true`，app 本來就認得 `/api` 就維持 `false` |
| `ingress.tls` | bool | | 覆蓋 daemon 預設。**只能關不能開**——daemon 沒帶 `-ingress-tls` 就沒有 443 監聽，這時寫 `true` 會被忽略並 log 一行 warn |

**多個 app 共用一個網域**時，有 `path` 的會自動排在沒有的前面（反過來的話全站那條會把
`/api` 整個吃掉，而那個 app 會看起來一切正常卻收不到任何請求）：

```yaml
# 前端的 qooq.yaml
ingress: {hosts: [app.example.com]}
# 後端的 qooq.yaml
ingress: {hosts: [app.example.com], path: /api, strip_path: true}
```

### `${component.<名字>.<欄位>}` — 引用另一個 component 的位址

兩個 app 要互打時，不要在 manifest 裡寫死對方的 port。寫死的代價是：任何一邊換 port，
另外幾邊要跟著改，而且**沒有東西會告訴你改漏了**——搭配 `auto:` 更是連號碼都不存在。

```yaml
run:
  env:
    # 容器內互打（SSR、後端對後端）
    API_INTERNAL: "http://${component.api.address}"      # → http://api:8080
    # 瀏覽器用（前端 JS、CORS 白名單）
    PUBLIC_API: "${component.api.endpoint}"              # → http://192.168.1.50:19000
```

| 欄位 | 值 | 誰在打 |
|---|---|---|
| `.host` | `api` | 容器網路 alias |
| `.port` | `8080` | container port |
| `.address` | `api:8080` | **容器內** |
| `.host_port` | `19000` | host 上的 port |
| `.endpoint` | `http://192.168.1.50:19000` | **容器外**（瀏覽器） |

**qooq 刻意不替你判斷該用哪一種。** 兩個都是同一個 component 的正確位址，但不可互換，
而且**選錯是靜默的**：容器照常 Running、health 照常綠燈，只有瀏覽器看得到錯誤
（典型症狀是 CORS 被擋、或前端打一個沒有東西在聽的位址）。所以由 manifest 講明。

值來自 **qooqd 對它正在跑的 component 的記錄**，不是對方的 repo——qooq 不會為了解析這個
去 clone 別人的原始碼。省略 port 時取 `run.ports` 的第一筆，跟 `ingress.port` 同一條規則。

**限同一個 namespace。** component 的身分是 `(namespace, name)`，兩個 stack 各有一個 `api`
是兩個東西；`${component.api...}` 一定指自己 namespace 裡的那個。

**引用一個還沒部署的 component 不算失敗**，會停在 `needs_input`（`watch get` 會顯示在等誰），
下一輪 sweep 自動重試——那是先後順序問題，會自己好。

**兩個 app 互相引用也部署得起來**（例如 api 的 CORS 清單寫 web 的網址，web 又寫 api 的網址）：
每次部署在展開 env 之前，會先跟 qooqd **預約**自己的網址（`auto:` port 這時就配好並保留），
所以後部署的那個讀得到先部署那個的網址，先部署的那個在下一輪也跟著過。預約的 port 不會分給別人，
真正部署時拿到的就是預約的那個；target 刪掉時預約一併釋放。
還是解不開的循環（例如 qooqd 版本比 watcher 舊）會直接寫 `circular reference: a -> b -> a`，
不會一直顯示「還沒部署」。

> 沒有反向追蹤：被引用的 component 換了 port，引用它的 app **不會**自動重新部署。
> 實務上不常遇到，因為 `auto:` 配到的 port 不會變（見上一節）；真的要換就重新部署引用方。

### `auto:` — 讓 daemon 挑 host port

```yaml
run:
  ports: ["auto:8080"]
```

host port 是這份 manifest **唯一無法知道的東西**：它屬於那台機器，不屬於這份程式碼。
手動指定的代價是每加一個 app 就要跟機器上其他所有 app 對過一次號——三個互連的專案就已經
要人工錯開了。

寫 `auto:` 之後由 qooqd 配置，範圍是它的 `-auto-port-range`（預設 `19000-19999`）。

**配到的 port 不會變。** 重新部署、改 image、rollback，同一個 container port 拿回同一個
host port；比對的是 container port，所以你在 manifest 裡重排或新增 port 也不會打亂既有的位址。
這點是刻意的：**會變的位址比沒有自動配置更糟**——指著舊位址的東西（另一個 app 的設定檔、
CORS 白名單、瀏覽器書籤）會在離改動很遠的地方壞掉。

配到哪個 port，看 `qooq component get <名字>` 的 `ports:` 與 `endpoints:` 兩行。API 回應
一律回實際的 port，不會回 `auto`。範圍用完會明確報錯（不會退回隨機 port）。

CLI 也吃：`qooq component deploy --name web --image nginx --port auto:80`。

### Inputs

宣告「這個 app 需要、但 repo 不可能知道」的值：API 金鑰、對外網址、網域。
省略整塊＝這個 app 不需要任何外部輸入（絕大多數情況）。

有了宣告，缺值時的行為從**部署失敗**變成**告訴你要填什麼**：
target 停在 `last_build_status: needs_input`（**不會跑任何 build**），
`qooq watch get <target>` 印出缺哪些、各是什麼用途，補上就自動重新部署。

```yaml
inputs:
  STRIPE_KEY:
    description: Stripe 密鑰，從 dashboard → Developers → API keys 取得
    required: true
    secret: true
  PUBLIC_HOST:
    description: 瀏覽器連得到的位址
    default: ${host}
    example: 192.168.1.50

run:
  env:
    STRIPE_SECRET_KEY: "${input.STRIPE_KEY}"
    PUBLIC_BASE: "http://${input.PUBLIC_HOST}:8080"
```

| 欄位 | 型別 | 必填 | 可填的值 / 說明 |
|---|---|---|---|
| `inputs.<NAME>.description` | string | | 給填值的人看的說明。**強烈建議寫**——這是宣告 input 而不是用 `${env.*}` 的全部意義 |
| `inputs.<NAME>.default` | string | | 沒給值時用它。**本身也會插值**，所以 `default: ${host}` 成立。有 default 就永遠不算缺 |
| `inputs.<NAME>.required` | bool | | 沒值就擋下部署（`needs_input`）。與 `default` **互斥**（有預設就不可能缺，同時寫會報錯） |
| `inputs.<NAME>.secret` | bool | | 值加密存（同 `secret.key` 機制），API 一律回 `***` |
| `inputs.<NAME>.example` | string | | 範例值，跟 description 一起顯示 |

引用一律是 `${input.<NAME>}`。**沒宣告的名字引用不到**（報 unknown reference）——
宣告區塊是權威來源，不是註解。

怎麼給值：

```bash
qooq watch add ... --input PUBLIC_HOST=192.168.1.50   # 建 target 時就給
qooq watch set-input <target> STRIPE_KEY=@key.txt     # 事後補（@檔案避免進 shell history）
qooq watch set-input <target> STRIPE_KEY=             # 空值＝清掉
```

`set-input` 會立刻觸發重新部署，不用等 poll interval，也不用推一個無關的 commit。

> [!NOTE]
> 值存在 **target** 上，不在 repo 裡。同一份 repo 部署成兩個 target，各有各的值。

### Policy

省略時的預設值：`restart: always`（服務停了就拉起來——crash、docker 重啟、機器重開都一樣）。
不要它被拉起來就明寫 `restart: no`。手動停掉的（`qooq component stop`）
不管 policy 是什麼都不會被重啟。2026-11-12 以前的預設是 `no`，**已經部署的不會變**，下一次部署才套用。

| 欄位 | 型別 | 必填 | 可填的值 / 說明 |
|---|---|---|---|
| `policy.restart` | string | | `always`（預設）/ `on-failure` / `no` |
| `policy.replicas` | int | | 保留欄位，只准 `1` 或不寫（其他值報錯） |

### Requests

「這個 app **至少**需要多少資源」——**部署前拿去跟 host 容量比對**，不足就 failed。
它只是排程/容量門檻，**不會限制容器實際能用多少**（那是 `limits` 的事）。像 k8s 的
`requests`。省略整塊＝不做任何容量檢查。

| 欄位 | 型別 | 必填 | 可填的值 / 說明 |
|---|---|---|---|
| `requests.cpu` | float | | 需要的 CPU cores，例 `0.5`；host 不足＝failed |
| `requests.memory` | string | | 需要的記憶體：`256Mi`/`1Gi`（1024 底）、`256MB`（1000 底）、`64m` 等寫法都認得 |
| `requests.arch` | []string | | 可接受的 CPU 架構：`amd64` / `arm64` / `386` / `arm`（`x86_64`、`aarch64` 等別名會先正規化再比對）。不寫＝不檢查 |
| `requests.runtime` | string | | `container`（預設）或 `process`；跟 qooqd 實際 runtime 兩個方向都驗證，不符＝failed |
| `requests.disk` | string | | 目前只解析不驗證 |
| `requests.gpu` | string | | 目前只解析不驗證 |

### Limits

「容器**最多**能用多少資源」——真正由 cgroup 強制的硬上限（`component get` 的
`cpu_limit`/`mem_limit`）。像 k8s 的 `limits`。

**每個欄位省略時 = `2× 對應的 requests`**（`requests` 有寫才有這個推導；倍數由
watcher config `limits.multiplier` 決定，預設 `2`）。`requests` 也沒寫時＝完全不設限。

| 欄位 | 型別 | 必填 | 可填的值 / 說明 |
|---|---|---|---|
| `limits.cpu` | float | | 容器 CPU 硬上限（cores）。省略＝`2× requests.cpu`；requests 也沒寫＝不設限 |
| `limits.memory` | string | | 容器記憶體硬上限（同 `requests.memory` 的單位寫法）。省略＝`2× requests.memory`；requests 也沒寫＝不設限 |

## ${...} 插值

`run.env` 的值、`run.command` 的每個參數、mount 的路徑、`services.*` 的欄位都支援 `${...}`：

| 形式 | 展開成 |
|---|---|
| `${app}` `${component}` `${network}` `${commit}` | 本次部署的基本資訊（`commit` 是 7 碼短 sha） |
| `${host}` | 這台機器對外的位址（qooqd 的 advertise host，見下方） |
| `${service.<名稱>.host\|port\|user\|password\|database\|scheme\|url}` | 該服務的連線資訊 |
| `${volume.<名稱>.target}` / `${volume.<名稱>.host_path}` | volume 在容器內的掛載點／host 路徑 |
| `${input.NAME}` | 這份 manifest 的 [`inputs:`](#inputs) 宣告的值 |
| `${secret.NAME}` | 該 namespace 的具名 secret（見下方 Secrets） |
| `${env.NAME}` | **watcher 進程**的環境變數（operator 逃生口，見下方警告） |

`${host}` 來自 qooqd 的 `-advertise-host`／`QOOQ_ADVERTISE_HOST`，沒設就偵測主要網卡的
IPv4。要給瀏覽器用的位址寫 `http://${host}:8080` 就好，不必把區網 IP 寫死進 repo。

> [!WARNING]
> **`${env.*}` 是給 operator 的逃生口，不是 app 該用的東西。** 它讀的是 watcher 進程的
> 環境變數，**一台 watcher 上的所有 target 共用同一份**——不同的人共用一台 watcher 時，
> 彼此讀得到對方的 secret。
>
> app 需要的值請優先用 `${input.NAME}`（會告訴使用者缺什麼、那是什麼），
> 部署層級的共用 secret 用 `${secret.NAME}`。

### Secrets（per-namespace，2026-09-29）

`${secret.NAME}` 從**該 namespace 自己的 secret store** 解析，加密存在 watcher 的 sqlite
裡（沿用 M18 的 AES-GCM，跟 git 憑證同一套）。**一個 namespace 讀不到另一個 namespace 的
secret**——多人共用一台 watcher 時這很重要。

```bash
# 設一個（值只在這裡出現一次，之後任何指令都讀不回來）；NAME=@檔案 從檔案讀，不留在 shell history
qooq secret set STRIPE_KEY=sk_live_... -n <namespace>

# 把專案的 .env 整份存進去（每一行 KEY=VALUE 變成一個 secret）
qooq secret import .env -n <namespace>

# 看有哪些（只有名字，永遠沒有值）
qooq secret ls -n <namespace>

# 刪掉
qooq secret rm STRIPE_KEY -n <namespace>
```

manifest 裡就寫 `${secret.STRIPE_KEY}`，跟以前一模一樣——**語法在 store 換掉之前就凍結了，
所以已經寫好的 manifest 一個字都不用改。**

**解析順序**：先看該 namespace 的 store，沒有才退回 watcher 的 `QOOQ_SECRET_<NAME>`
環境變數。順序是刻意的——namespace 自己設過的值一定贏，否則 host 上一個手滑的
`QOOQ_SECRET_DB_PASSWORD` 會無聲蓋掉 namespace 自己設的。

### Build 的上限（qooqd 旗標，不是 manifest）

```bash
qooqd -build-timeout 30m \
      -build-memory 2g \
      -build-cpus 2 \
      -build-cache-max 10g
```

**這些設在 daemon 上、不是每次 build 送過來的**——送 build 的是你不一定完全掌控的 repo，
**一個送出者自己能調高的上限不是上限**。

| 旗標 | 預設 | 說明 |
|---|---|---|
| `-build-timeout` | **30m** | 超時就中止。**這個預設是開的**：一份永遠跑不完的 Dockerfile（`RUN sleep infinity`、卡在互動式提示的套件管理器）會佔住一個 build slot 直到 daemon 關掉，而 watcher 的 slot 是固定少量的——幾個這種 repo 就讓**所有人**都建不起來 |
| `-build-memory` | 無限 | 例 `2g`。同時把 swap 綁成一樣，否則 docker 會再給等量 swap（512m 的限制實際上能用 1g，而且會把機器 swap 到停住） |
| `-build-cpus` | 無限 | 例 `1.5` |
| `-build-cache-max` | 不清 | 例 `10g`。**docker 沒有 per-build 磁碟上限**，所以「build 完把 cache 壓在上限以下」是唯一擋得住磁碟被吃光的做法。在 build **之後**才清——先清會把接下來正要用到的 cache 丟掉 |

**只有 timeout 預設是開的**，其餘預設無限：單人機器上所有 build 都是你自己的，限制它們沒有
意義。**共用機器、或要建不信任的 repo 時，四個都要設。**

> [!NOTE]
> **超時會請 docker 取消建置，但取消只在 step 之間生效。**
>
> | Dockerfile 長相 | 超時之後 |
> |---|---|
> | 多個 `RUN`（常見） | ✅ 取消生效，**下一個 step 完全不會開始** |
> | 單一個很長的 `RUN` | ⚠️ 那個 step 會跑到自己結束（classic builder 在 step 邊界才檢查取消） |
>
> **所以共用機器上 `-build-memory` 與 `-build-cpus` 不是可選的**：它們是上面那個殘留情況的
> 界限（實測有生效）。

**另外，連續失敗 10 次的 target 會停止重試**（`paused_reason` 會說明原因），
修好之後跑 `qooq watch trigger <name>` 恢復。沒有這個的話，一個永遠建不起來的 repo
會每個輪詢週期重試一次、永遠佔著 build 容量。

超時的錯誤會**完整回報給使用者**（`build exceeded this daemon's build timeout after 30s`，
HTTP 504），不是遮成 `internal error`——那是 operator 的政策生效，不是伺服器故障，跟 port
撞號回 409 是同一條原則。

### Registry 模式：build 產物離開本機 docker

```bash
QOOQ_WATCHER_REGISTRY_PREFIX=registry.example.com/qooq   # 空 = 關閉（預設）
QOOQ_WATCHER_REGISTRY_USERNAME=...                        # 選填
QOOQ_WATCHER_REGISTRY_PASSWORD=...                        # 選填
```

**預設是關的，而且這是刻意的不是將就**：單機情況下 build 出來的 image 直接落在等一下要跑
它的那個 docker daemon 裡，push 再 pull 一次不會多任何功能，只多一份磁碟與一次網路來回。

**什麼時候非開不可**：qooqd 不在建置機上的那一刻（多節點，或 build 與 run 分開的
任何形態）。那時 watcher build 完的東西必須有辦法交到另一台機器手上，而 qooq 沒有別的跨機
傳輸 image 的路徑。

開啟後：

| | |
|---|---|
| build 一次打**兩個 tag** | 本機名 + `<prefix>/<本機名>`，同一次 build，不會有兩個名字指向不同 image 的空窗 |
| 部署用的 ref | **remote 那個**（`<prefix>/...`），所以別台機器 pull 得到 |
| 遠端名字帶 namespace | 本機 tag 本來就帶（見 M2），remote 直接繼承——兩個 namespace 在 registry 裡一樣不會撞 |
| push 失敗 | **整個 build 失敗**。不是 warn 後照部署——那會「在建置機上成功、在別台都失敗」，正是 registry 存在要解決的情況 |

> **它不解決的事**：**這不是建置隔離。** build 仍然跑在本機 docker daemon 上，registry
> 只是產物之後的去處。把 build 移出那個 daemon 是另一件事。

### 一台 watcher 服務**不是自己的** repo 時：`untrusted_repos`

```bash
QOOQ_WATCHER_UNTRUSTED_REPOS=true
```

**預設 `false`**（線下版：你建的 target 就是你自己的 code，跑它是重點）。
一旦 watcher 會建**別人的** repo，這個一定要開——它一次關掉三個口子：

| 開啟後 | 原因 |
|---|---|
| **`build.command` 直接被拒絕** | 它把 repo 自己的指令**跑在 host 上**、用 watcher 的權限。對不受控的 repo 這件事沒有安全的做法 |
| `mounts.policy` 自動收緊成 `repo-only` | 預設的 `open` 讓 manifest 掛載任意 host 路徑——包括 watcher 自己的 sqlite |
| `${env.*}` 與 `QOOQ_SECRET_` 退路關閉 | 見上一節（`secrets.allow_process_env` 若沒明寫就跟著這個開關走） |

> **為什麼是「拒絕」而不是「沙箱化」**：`build.command` 存在的理由就是 process runtime
> ——使用者不想裝 docker。要沙箱它就得用 docker，那正好是它的使用者在迴避的東西。
> 而一個不完整的沙箱比明確說不更糟：它讀起來像安全，而 manifest 會繞過去。
> 不受信任的 repo 請用 **Dockerfile 或 `build.template`** 建置，那條路由 docker 沙箱。

**無論開關如何，`build.command` 永遠讀不到 `QOOQ_*` 環境變數**（包含
`QOOQ_WATCHER_SECRET_KEY`——那把鑰匙能解開**每一個** namespace 的憑證與 secret）。
其餘環境變數照常繼承，因為 build 真的可能需要（toolchain 的 cache 路徑、proxy 設定）。

### 多人共用一台 watcher 時：把 `${env.*}` 關掉

```bash
QOOQ_WATCHER_SECRETS_ALLOW_PROCESS_ENV=false
```

**預設是 `true`**，因為單人使用時 watcher 的環境就是你自己的，讀它是功能不是漏洞。
但**多人共用一台 watcher 時這個應該關**，否則 `${env.*}` 與 `QOOQ_SECRET_<NAME>`
退路都能讀到共用的環境。

關掉之後，`${env.FOO}` 會**明確報錯並告訴作者改用 `${secret.FOO}`**，不是安靜地展開成空字串。

> watcher 偵測到自己服務**超過一個 namespace** 而這個開關還開著時，啟動會 warn 一次。
> 只有一個 namespace 時不 warn——那時讀自己的環境是正常行為，而經常無害的警告會被學會忽略。

- **打錯字會明確報錯並列出可用的 key**，不會安靜地展開成空字串。
- `run.env_file` 的**檔案內容不做插值**（檔案是資料，裡面的 `${...}` 原樣傳給 app）。

## 多個專案要互連：設定寫在哪

web 前端與 api 後端分屬兩個 repo、各有各的 `qooq.yaml` 時，「誰連得到誰」的設定分成兩半，
**寫的位置不同**——這是最容易搞混的地方。

### 1. 加入同一張網路：每份 manifest 各寫一次

```yaml
# 兩個專案的 qooq.yaml 都寫這一行（字串必須完全相同）
network: todo
```

沒有「stack 層」的設定檔可以只寫一次——**網路名稱就是那個約定**，
像大家講好用同一個 port 一樣。兩份寫同一個字串是刻意的：
每個 repo 都能獨立部署，不依賴另一個 repo 存在。

同網路的 component 用**名稱**互相定址（不是 IP，IP 每次重建都會變）：

```
http://api:8080     ← 容器內用這個
```

### 2. 連線位址：寫在「發出請求的那一端」

**規則：誰發出請求，就寫在誰的 manifest 裡。** 被連的一方不需要知道誰會來連它。

```yaml
# web/qooq.yaml — server 端在容器內發請求
run:
  env:
    API_INTERNAL: http://api:8080   # 容器名
    API_PUBLIC: http://192.168.1.50:19000   # 瀏覽器用
```

同一個後端，兩個位址，因為**發請求的機器不同**：

| 誰發出請求 | 位址 | 為什麼 |
|---|---|---|
| 另一個容器（server 端渲染、後端呼後端） | `http://<component名>:<容器port>` | 同網路，DNS 解析得到 |
| 使用者的瀏覽器 | `http://<host的區網IP>:<host port>` | 瀏覽器在別台機器上，只看得到 host |

> [!WARNING]
> **容器裡的 `127.0.0.1` 是容器自己**，不是 host。server 端渲染的專案最常在這裡卡住：
> 開發時 `localhost:8080` 會通，進了容器就變成「連自己的 8080」。
>
> 沒有共用 network 時，容器要打 host 上的服務只能用 `host.docker.internal`
> （Docker Desktop 才有；Linux 要自己加 `--add-host`）。

### 3. 給瀏覽器的位址：用 `${host}`，不要寫死區網 IP

上面例子裡的 `192.168.1.50` 是**部署當下那台機器的位址**。寫死它，這份 repo 就只能
部署到那一台機器。改用 `${host}`：

```yaml
run:
  env:
    API_INTERNAL: http://api:8080   # 容器名，不變
    API_PUBLIC: http://${host}:19000   # 部署時才決定
    # 更好：讓 qooq 填入 host 與自動配置的 port
    # API_PUBLIC: "${component.api.endpoint}"
```

`${host}` 是 qooqd 回報的 advertise host。qooqd 沒設 `-advertise-host` 時會偵測主要網卡，
在筆電、單機 VM 上通常就是對的；偵測不準（VPN、多網卡）就在 qooqd 加
`-advertise-host 192.168.1.50`，manifest 一個字都不用改。

需要讓**部署的人**決定（正式網域、不同環境不同位址）時，宣告成 input：

```yaml
inputs:
  PUBLIC_HOST:
    description: 瀏覽器連得到的位址
    default: ${host}          # 給了預設就不會停下來問
run:
  env:
    API_PUBLIC: http://${input.PUBLIC_HOST}:19000
```

operator 側的 `deploy_spec.env` 仍然可以覆蓋任何一項（優先序最高），
適合機器相關、不想進 repo 的設定。

```yaml
# 推薦做法：repo 只寫佔位，實際位址由部署那台機器的 watcher 提供
run:
  env:
    API_PUBLIC: "http://${env.PUBLIC_HOST}:19000"
```

```bash
PUBLIC_HOST=192.168.1.50 qooq-watcher     # 起 watcher 時給
```

更好的做法是開 ingress 並設 `domain`（用網域取代 IP:port），見上方 [Ingress](#ingress)。

### 4. 部署順序沒有保證

兩個 target 各自獨立輪詢，**沒有先後關係**。web 可能在 api 還沒起來時就部署完成，
它的 server 端首次抓資料會失敗（頁面仍然會出來，只是沒資料），等 api 起來後就正常。
要有順序就把兩個放進同一個 stack 並用 `depends_on`（見 `examples/todo-stack`）。

`services:` 宣告的相依服務**有**順序保證，但那只在同一份 manifest 內有效。
跨 target 的依賴順序是已知缺口。

「ready」的定義（2026-09-24 修正）：服務容器起來**而且它的健康探測至少通過一次**，
才會部署 app。對資料庫來說這兩件事差了好幾秒——process 起來不等於已經在接受連線，
而在啟動階段就建連線池的框架（Go fx、NestJS、Spring Boot）正好會在那個空窗期失敗。
內建服務型別各自帶探測指令（postgres 用 `pg_isready`、mysql 用 `mysqladmin ping`、
redis 用 `PING`……），不必自己寫。

唯一的例外是**自訂的 http 探測**：它有可能永遠看不到一個健康的 app
（`health.port` 指到沒人聽的 port，或在 Docker Desktop 上探測容器 IP），
所以等 60 秒仍未通過就會**印一行 warning 然後繼續部署**，不會把整條 pipeline 卡死。
exec 探測沒有這個寬限——`docker exec` 一定進得去，探不到就是服務真的有問題，
會讓這次部署失敗。

## 內建 Dockerfile 模板

指定 `build.template` 時，watcher 會在 clone 後 render 出 `Dockerfile.qooq-generated`
（寫進 build context 根）再 build——repo 完全不用放 Dockerfile。

| 模板 | 用途 | 變數（預設值） |
|---|---|---|
| `static-nginx` | 靜態檔 + nginx；有 `build_cmd` 時先用 node 建置，再把輸出交給 nginx | `nginx_tag`(`alpine`)、`static_dir`(`.`)、`build_cmd`(空＝不建置)、`node_version`(`20`)、`install_cmd`(`npm install`)、`corepack`(空)、`spa`(空；設 `true` 時找不到的路徑一律回 `index.html`，前端路由重新整理不會 404) |
| `go` | multi-stage go build → 單 binary | `go_version`(`1.25`)、`build_path`(`.`)、`base_image`(`alpine:3.20`)、`port`(`8080`)、`cgo`(`0`)、`build_deps`(空)、`runtime_deps`(空)、`extra_copy`(空)、`workdir`(`/app`) |
| `node` | `node:alpine` install（+ 可選 build）+ start | `node_version`(`20`)、`install_cmd`(`npm install --omit=dev`)、`build_cmd`(空)、`start_cmd`(`node index.js`)、`port`(`3000`)、`corepack`(空)、`workdir`(`/app`) |
| `python` | `python:slim` pip + CMD | `python_version`(`3.12`)、`start_cmd`(`uvicorn main:app --host 0.0.0.0 --port 8000`)、`port`(`8000`) |
| `go-react` | node build 前端 → go:embed 進單 binary | `node_version`(`20`)、`web_dir`(`web`)、`install_cmd`(`npm install`)、`frontend_build_cmd`(`npm run build`)、`frontend_dist_dir`(`dist`)、`go_version`(`1.25`)、`embed_dest`(`internal/webui/dist`)、`go_build_path`(`./cmd/server`)、`base_image`(`alpine:3.20`)、`port`(`8080`) |
| `rust` | multi-stage `cargo build --release` → debian slim | `rust_version`(`1.83`)、`debian_tag`(`bookworm`)、`port`(`8080`)、`locked`(`1`)、`bin_name`(空＝自動找唯一的執行檔)、`build_deps`(空)、`runtime_deps`(空) |
| `php` | `php:apache`，**docroot 指到 `public/`** | `php_version`(`8.3`)、`docroot`(`public`)、`php_extensions`(`pdo_mysql zip intl opcache`)、`composer_version`(`2`)、`composer_flags`(`--no-dev --optimize-autoloader …`)。**port 固定 80**（apache） |
| `ruby` | `ruby:slim` bundle install + rackup | `ruby_version`(`3.3`)、`start_cmd`(`bundle exec rackup --host 0.0.0.0 --port 3000`)、`bundle_without`(`development test`)、`port`(`3000`)、`build_cmd`(空)、`runtime_deps`(空) |
| `java` | maven package → temurin JRE 跑 jar | `maven_tag`(`3.9-eclipse-temurin-21`)、`jre_version`(`21`)、`port`(`8080`)。**只支援 Maven**；Gradle 專案偵測得出來但要自備 Dockerfile |
| `dotnet` | SDK publish → aspnet runtime | `dotnet_version`(`8.0`)、`port`(`8080`)、`project`(空＝這個目錄) |
| `bun` | `oven/bun` install（+ 可選 build）+ start | `bun_version`(`1`)、`start_cmd`(`bun run index.ts`)、`build_cmd`(空)、`frozen`(`1`)、`port`(`3000`) |

<details>
<summary>範例：覆寫模板變數</summary>

`template_vars` 只需覆蓋跟預設值不同的部分。**變數名只能用該模板表裡列出的**，
寫錯（例如把 `static_dir` 寫成 `dir`）時 `qooq manifest validate` 就會報錯（不需要 daemon），
建置也會失敗，錯誤訊息會列出這個模板可用的變數
——以前是靜默忽略，結果部署成功、跑的卻是預設值：

```yaml
build:
  template: node
  template_vars:
    node_version: "22"
    start_cmd: "node server.js"
```

**需要 cgo 的 Go 專案**（`gorm.io/driver/sqlite` → `mattn/go-sqlite3`）。
不設 `cgo: "1"` 的話會**編譯得過、跑起來才炸**（`requires cgo to work`）：

```yaml
build:
  template: go
  template_vars:
    go_version: "1.26"        # 要 ≥ go.mod 宣告的版本
    cgo: "1"
    build_deps: gcc musl-dev
    extra_copy: config.toml   # 把設定檔一起放進 image（或改用 run.files，改設定免重 build）
```

**SSR 前端**（Nuxt / Next / SvelteKit）：設 `build_cmd` 就會切成 build → runtime 兩階段，
lockfile 三種 package manager 都會被複製進去：

```yaml
build:
  template: node
  template_vars:
    node_version: "22"
    corepack: "true"                          # 用 pnpm/yarn 時需要
    install_cmd: pnpm install --frozen-lockfile
    build_cmd: pnpm run build
    start_cmd: node .output/server/index.mjs
```

</details>

### 使用前要知道的三件事

> [!NOTE]
> **`port` 變數只影響 `EXPOSE`**，不會自動同步 `start_cmd`／程式碼裡寫死的監聽
> port——改 port 記得把兩邊一起改。

> [!WARNING]
> **`template_vars` 的值逐字插入 Dockerfile**，信任等級與自己寫 Dockerfile 相同，
> 不做沙箱——能寫 `qooq.yaml` 的人本來就能決定 build 要跑什麼。

> [!WARNING]
> **`go-react` 的 `embed_dest` 必須與 Go 原始碼裡 `//go:embed` 的路徑一致**——
> 模板只負責把前端產物複製到 `embed_dest`，原始碼要自己 embed 同一個相對路徑。

## 範例

可跑範例都在 [examples/](../examples/)：

| 範例 | template / 手寫 Dockerfile | 重點 |
|---|---|---|
| [static-web/](../examples/static-web/) | 手寫 Dockerfile（唯一對照組） | 靜態網頁，`run.ports`+`health.http` 最小示範 |
| [api-server/](../examples/api-server/) | `go` | Go + gin，`GET /hello` |
| [fullstack-server/](../examples/fullstack-server/) | `go-react` | React(Vite) 前端 + Go(gin) go:embed 單 binary |
| [worker/](../examples/worker/) | `go` | 背景程序，無 port，`health.exec` 心跳檢查 |
| [python-api/](../examples/python-api/) | `python` | FastAPI，`GET /hello` |
| [node-web/](../examples/node-web/) | `node` | Express，`GET /hello` |
| [config-file-app/](../examples/config-file-app/) | `go` | **`run.files` 掛設定檔 + `run.volumes` 持久化資料** |
| [app-with-postgres/](../examples/app-with-postgres/) | `go` | **`services: {db: {type: postgres}}` 一行拿到資料庫** |

## 欄位流向

`run` / `health` / `policy` / `domain` / `limits` 轉成與 target `deploy_spec`
同形狀的欄位（`command` / `env` / `ports` / `health_check` / `restart_policy` /
`domain` / `cpu_limit` / `mem_limit_bytes`），再套「operator 覆蓋」合併：
manifest → deploy_spec → qooqd。`build.*` 只控制 build 行為本身；`requests.*`
只做部署前的 host 容量驗證，不會傳給 qooqd。

> [!NOTE]
> 舊欄位名 `resource:` 與 `requires:` 都已停用——manifest 裡出現會被明確拒絕
> （`last_build_status=failed` + 提示改名），不會被靜默忽略。把容量需求搬到
> `requests:`、容器上限（需要的話）寫進 `limits:` 即可。

## 相關文件

- [USAGE.md](USAGE.zh-TW.md) — 操作手冊（GitOps 快速開始）
- [ARCHITECTURE.md](ARCHITECTURE.zh-TW.md) — 系統架構總覽
- [examples/](../examples/) — 八個可跑範例
