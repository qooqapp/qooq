# static-web: the smallest deployable qooq project

English | [繁體中文](README.zh-TW.md)

A static web page + `Dockerfile` + `qooq.yaml` (the app manifest). Copy these three files into your own
repo and you have a project skeleton that qooq GitOps can deploy. Among the examples this is the **only one with a hand-written Dockerfile**,
serving as the control; the rest all use `build.template` instead and ship no Dockerfile (see "Built-in Dockerfile templates" in docs/DEPLOY-CONFIG.md).

```
static-web/
├── qooq.yaml     # app manifest: how this app runs and what it needs (field reference in docs/DEPLOY-CONFIG.md)
├── Dockerfile    # how to build the image
└── index.html    # the app itself (just a static page here)
```

## How to run (local demo)

```bash
# 0. Start qooqd + watcher first (see docs/USAGE.md; to run the whole set containerized, use `qooq compose up`)

# 1. Turn this example into a git repo and prepare a bare remote for the watcher to watch
cp -r examples/static-web ~/static-web && cd ~/static-web
git init -q && git add -A && git commit -qm init
git clone --bare . ~/static-web.git && git remote add origin ~/static-web.git

# 2. Register a watch target (the file:// path must be one the watcher process can see;
#    when the watcher runs in a container, mount the repo into it first, see section 6 of docs/USAGE.md)
qooq watch add --name static-web --repo file://$HOME/static-web.git \
  --component static-web --poll 30

# 3. Verify: within 30 seconds
qooq watch get static-web          # last_build_status: success
open $(qooq component get static-web | awk '/endpoints:/{print $2}')   # expected: the "qooq static-web ✅" page
qooq component get static-web      # domain: static-web.example.com (from qooq.yaml)

# 4. Edit index.html → commit → push; within 30 seconds, refreshing the page shows the update
```

## qooq.yaml highlights

- `resource.cpu/memory`: compared against host capacity before deployment; a mismatch marks the target failed instead of deploying anyway.
- `run.ports: ["8081:80"]` + `health.http: /` / `port: 8081`: hits the container's `/` every 10s,
  and automatically restarts the container after consecutive failures.
- `domain`: declares the domain this app serves (in v1 it is only recorded and displayed; a future ingress feature will consume it).

Full field reference: [docs/DEPLOY-CONFIG.md](../../docs/DEPLOY-CONFIG.md).
