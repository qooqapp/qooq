# 偵測語料庫

`pkg/manifest.Detect` 的職責是「看檔案長相猜這個專案是什麼」。它**刻意不讀原始碼**
（讀 code 推行為是 `qooq-deploy` skill 的工作，硬寫一份模仿版只會安靜地猜錯）。

既然輸入只有檔案長相，那**測它就不需要 build、不需要 docker、不需要網路**——只需要一棵
目錄樹。這是這份語料庫存在的理由：涵蓋 18 種專案形狀的成本，跟以前 14 個手寫測試差不多。

## 三層，各自抓不同的東西

| | 在哪 | 跑法 | 抓什麼 |
|---|---|---|---|
| **1. 手寫骨架** | `shapes/` + `golden/` | `make test`（在 CI） | 改 `detect.go` 影響了哪些形狀 |
| **2. 真的部署** | `examples/` | `make smoke-examples` | 偵測→build→run 的接縫 |
| **3. 真實 repo** | `real/` | `make shapes-real` | 第一層的骨架是不是猜錯了 |

第三層是**第一層的校對**。骨架是照著框架鷹架的長相手寫的，難免有出入；真實的樹會說實話。
發現某個真實 repo 長得跟骨架不一樣 → 修骨架，或加一個新的。

## golden 檔不是「答案正確」的斷言

它記錄的是**現在的答案**，好讓改動可見：動了 `detect.go`，diff 會指名每一個答案變了的形狀，
包括你沒想到的那些。

**還缺的：**

| 形狀 | 現在的答案 | 還缺什麼 |
|---|---|---|
| Gradle 專案（`build.gradle`） | `java-gradle`，要自備 Dockerfile | `java` 模板只支援 Maven：它的依賴快取那一步和 `target/*.jar` 都是 Maven 特有的，照猜一份 Gradle 版會產生一個看起來對、建不起來的 Dockerfile |
| `pnpm-monorepo` | `monorepo`，要自備 Dockerfile 或指定子目錄 | 這是**刻意的**——workspace 根不是一個 app，沒有正確答案可以給 |

**已經修掉的（留紀錄，因為它們是同一個根因）：**

| 曾經 | 現在 |
|---|---|
| Laravel → `static-frontend`，服空的 `dist/` | `php-laravel` + `php` 模板（apache docroot 指到 `public/`——指到 repo 根會把 `.env` 和 `vendor/` 公開出去） |
| pnpm workspace 根 → `static-frontend` | `monorepo`，列出 `apps/web, packages/ui` 讓人挑 |
| Django → `uvicorn main:app`（一定開不起來） | `python-django`，requirements 有 gunicorn 就用 `gunicorn <專案>.wsgi:application`，沒有就退回 `manage.py runserver` 並說那是開發伺服器 |
| Rust / Ruby / Java / .NET → `unknown` | 各自有模板了（`rust`/`ruby`/`java`/`dotnet`） |
| bun 專案 → `node` 模板跑 `npm start`（node 映像檔裡沒有 bun） | `bun` 模板，`oven/bun` 基底 |

根因是**偵測看到 `package.json` 就往 Node 走，不回頭看旁邊有沒有更強的訊號**。
現在 `backendMarkers`（`detect.go`）在 Node 之前先找 `composer.json`+`artisan`、
`Gemfile`+`config.ru`、`manage.py`、`pom.xml`、`Cargo.toml`、`*.csproj`——**每個 marker
都刻意不含 `package.json`**，因為用 Vite 打包資源的後端框架也有一份。

**模板本身由 `make smoke-templates` 驗**：每個模板對一個最小的真實 app 跑 `docker build`、
把容器跑起來、打一次 HTTP。單元測試只證明得了「render 得出來、沒有漏掉的變數」——
一個第三層 apt 套件名打錯的模板會通過所有測試，然後在使用者的第一次部署失敗。

## 怎麼更新

```bash
go test ./pkg/manifest/ -run TestShapes -update   # 第一層
bash scripts/shapes-real.sh --update              # 第三層（要網路）
```

更新前先確認**答案的變化是你要的**——golden 的價值全在有人真的讀那份 diff。

## 加一個形狀

`shapes/<名字>/` 放進**偵測會看的檔案**就好，不用是能跑的 app：`package.json` 的依賴區塊、
`next.config.js`、一個 stub 頁面。骨架越小越好，但**長相要真**——不要憑印象寫，
用框架自己的鷹架工具生一次再砍到只剩必要的檔案。然後 `-update` 產 golden。
