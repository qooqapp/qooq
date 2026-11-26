# api-server: minimal Go + gin API example (build.template: go)

English | [繁體中文](README.zh-TW.md)

A minimal HTTP API written with Go and [gin](https://github.com/gin-gonic/gin): `GET /hello` returns
`{"message":"hello from api-server"}`, listening on `:8080`. **This example deliberately has no Dockerfile.**
`qooq.yaml` uses `build.template: go`; after cloning, the watcher generates
`Dockerfile.qooq-generated` on the spot from the built-in go template and builds it (see section 3 of [docs/DEPLOY-CONFIG.md](../../docs/DEPLOY-CONFIG.md)).

```
api-server/
├── qooq.yaml   # build.template: go (no Dockerfile)
├── main.go     # GET /hello
├── go.mod
└── go.sum
```

(This directory is a standalone go module, `module example.com/qooq-examples/api-server`, so the
`go build ./...` at the repo root does not pick it up.)

## How to run (local demo)

```bash
# 0. Start qooqd + watcher first

# 1. Turn it into a git repo + bare remote
cp -r examples/api-server ~/api-server && cd ~/api-server
git init -q && git add -A && git commit -qm init
git clone --bare . ~/api-server.git && git remote add origin ~/api-server.git

# 2. Register a watch target
qooq watch add --name api-server --repo file://$HOME/api-server.git \
  --component api-server --poll 30

# 3. Verify: within 30 seconds (the watcher builds with the go template; the first run downloads Go modules and takes a bit longer)
qooq watch get api-server                      # last_build_status: success
curl $(qooq component get api-server | awk '/endpoints:/{print $2}')/hello   # expected: {"message":"hello from api-server"}
```

## qooq.yaml highlights

- `build.template: go`: no need to write your own Dockerfile; all template variables use their defaults (go_version 1.25,
  base_image alpine:3.20, port 8080).
- `run.ports` is `auto:8080`, and `health.port` targets the same container port. If you change the port, also change the template variable `port`
  (the template's `port` only affects `EXPOSE`; it does not update a listen port hard-coded in the application).
