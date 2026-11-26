# worker: stdlib-only long-running background process example (build.template: go)

English | [繁體中文](README.zh-TW.md)

A background process that exposes no port: every 5 seconds it calls `log.Println("tick")` and touches `/tmp/heartbeat`.
It demonstrates how to do a liveness health check for a service with no HTTP interface, using an **exec probe** to check
whether the heartbeat file has been updated within the last minute. **There is deliberately no Dockerfile**; `qooq.yaml` uses `build.template: go`.

```
worker/
├── qooq.yaml   # build.template: go + health.exec + policy.restart: always
├── main.go     # tick every 5s + touch /tmp/heartbeat
└── go.mod
```

(A standalone go module, `module example.com/qooq-examples/worker`, stdlib only; a missing go.sum is fine.)

## How to run (local demo)

```bash
cp -r examples/worker ~/worker && cd ~/worker
git init -q && git add -A && git commit -qm init
git clone --bare . ~/worker.git && git remote add origin ~/worker.git

qooq watch add --name worker --repo file://$HOME/worker.git \
  --component worker --poll 30

# Verify: within 30 seconds
qooq watch get worker              # last_build_status: success
qooq component logs worker --tail 20   # expected: one "tick" line every 5 seconds
```

## qooq.yaml highlights

- `health.exec`: uses `find /tmp/heartbeat -mmin -1 | grep -q .` to judge the heartbeat; **it does not use `pgrep`**,
  because inside the container the process name is the full path `/app`, which `pgrep` cannot match.
- `policy.restart: always`: this is a background process, so any exit should trigger a restart (unlike a web app, which may have been stopped
  deliberately by an operator).
- There is no `run.ports`: this example offers no service to the outside.
