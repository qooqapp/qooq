# node-web: minimal Express API example (build.template: node)

English | [繁體中文](README.zh-TW.md)

A minimal API written with Express: `GET /hello` returns `{"message":"hello from node-web"}`. **There is deliberately no
Dockerfile.** `qooq.yaml` uses `build.template: node`, and the default start_cmd
(`node index.js`) maps directly onto this file.

```
node-web/
├── qooq.yaml     # build.template: node
├── index.js      # GET /hello
└── package.json  # depends only on express
```

(This directory has **never had `npm install` run**, so it contains no `node_modules/` or lockfile; the watcher installs dependencies
only when it builds the image with the node template.)

## How to run (local demo)

```bash
cp -r examples/node-web ~/node-web && cd ~/node-web
git init -q && git add -A && git commit -qm init
git clone --bare . ~/node-web.git && git remote add origin ~/node-web.git

qooq watch add --name node-web --repo file://$HOME/node-web.git \
  --component node-web --poll 30

qooq watch get node-web              # last_build_status: success
curl $(qooq component get node-web | awk '/endpoints:/{print $2}')/hello   # expected: {"message":"hello from node-web"}
```

## qooq.yaml highlights

- `build.template: node`: template variables use their defaults (node_version 20,
  install_cmd `npm install --omit=dev`, start_cmd `node index.js`, port 3000).
- `run.ports` and `health.http` both target `:3000`.
