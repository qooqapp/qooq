# static-web — 最小可部署的 qooq 專案範例

[English](README.md) | 繁體中文

一個靜態網頁 + `Dockerfile` + `qooq.yaml`（app manifest）。把這三個檔案複製到你自己的
repo 就是一個可被 qooq GitOps 部署的專案骨架。這是範例裡**唯一手寫 Dockerfile**
的對照組——其餘範例都改用 `build.template`（見 docs/DEPLOY-CONFIG.md「內建 Dockerfile 模板」一節）不放 Dockerfile。

```
static-web/
├── qooq.yaml     # app manifest：這個 app 怎麼跑、需要什麼（欄位參考 docs/DEPLOY-CONFIG.md）
├── Dockerfile    # 怎麼 build image
└── index.html    # app 本體（這裡只是一頁靜態網頁）
```

## 怎麼跑（本機示範）

```bash
# 0. 先起 qooqd + watcher（見 docs/USAGE.md；容器化整組跑用 `qooq compose up`）

# 1. 把這個範例變成一個 git repo，並準備一個 bare remote 給 watcher 盯
cp -r examples/static-web ~/static-web && cd ~/static-web
git init -q && git add -A && git commit -qm init
git clone --bare . ~/static-web.git && git remote add origin ~/static-web.git

# 2. 註冊 watch target（file:// 路徑要是 watcher 進程看得到的路徑；
#    watcher 跑在容器裡時要先把 repo 掛載進去，見 docs/USAGE.md 第六節）
qooq watch add --name static-web --repo file://$HOME/static-web.git \
  --component static-web --poll 30

# 3. 驗收：≤30 秒後
qooq watch get static-web          # last_build_status: success
open $(qooq component get static-web | awk '/endpoints:/{print $2}')   # 預期看到「qooq static-web ✅」頁面
qooq component get static-web      # domain: static-web.example.com（來自 qooq.yaml）

# 4. 改 index.html → commit → push，≤30 秒後重新整理頁面就會更新
```

## qooq.yaml 重點

- `resource.cpu/memory` — 部署前跟 host 容量比對，不符就標 failed，不會硬部署。
- `run.ports: ["8081:80"]` + `health.http: /` / `port: 8081` — 每 10s 打一次
  容器的 `/`，連續失敗就自動重啟容器。
- `domain` — 宣告這個 app 服務的網域（v1 只記錄與顯示，未來 ingress 功能消費）。

完整欄位參考：[docs/DEPLOY-CONFIG.md](../../docs/DEPLOY-CONFIG.zh-TW.md)。
