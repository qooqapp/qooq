# node-web — Express 最小 API 範例（build.template: node）

[English](README.md) | 繁體中文

Express 寫的最小 API：`GET /hello` 回 `{"message":"hello from node-web"}`。**故意沒有
Dockerfile**——`qooq.yaml` 用 `build.template: node`，預設 start_cmd
（`node index.js`）直接對應本檔。

```
node-web/
├── qooq.yaml     # build.template: node
├── index.js      # GET /hello
└── package.json  # 只依賴 express
```

（本目錄**沒有跑過 `npm install`**，不含 `node_modules/`/lockfile；watcher 用 node 模板
build image 時才會安裝依賴。）

## 怎麼跑（本機示範）

```bash
cp -r examples/node-web ~/node-web && cd ~/node-web
git init -q && git add -A && git commit -qm init
git clone --bare . ~/node-web.git && git remote add origin ~/node-web.git

qooq watch add --name node-web --repo file://$HOME/node-web.git \
  --component node-web --poll 30

qooq watch get node-web              # last_build_status: success
curl $(qooq component get node-web | awk '/endpoints:/{print $2}')/hello   # 預期 {"message":"hello from node-web"}
```

## qooq.yaml 重點

- `build.template: node` — 模板變數用預設值（node_version 20、
  install_cmd `npm install --omit=dev`、start_cmd `node index.js`、port 3000）。
- `run.ports` / `health.http` 都對到 `:3000`。
