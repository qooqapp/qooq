# config-file-app: config file injection + data persistence (`run.files` / `run.volumes`)

English | [繁體中文](README.zh-TW.md)

Most real services have this shape: **read a config file from the working directory at startup, and write data to some directory at runtime**.
This example solves each with one line of qooq.yaml.

```
config-file-app/
├── qooq.yaml     # run.files mounts the config, run.volumes provides data storage
├── config.toml   # config the app reads from cwd (under version control, travels with the commit)
├── main.go       # reads config.toml, appends a line to data_dir, GET / shows the running count
└── go.mod
```

`GET /` returns the greeting from the config file, plus "how many times this container has started". That number is stored in the volume
and **does not reset on redeploy**.

## What the two fields do

```yaml
run:
  files:
    - "./config.toml:/app/config.toml"   # config: read-only, versioned with the commit
  volumes:
    - "appdata:/app/data"                # data: read-write, never cleared by deployments
```

- **The source of `files` is a repo-relative path**, so the config goes into version control together with the code. The watcher copies it to
  `<mounts.dir>/config-file-app/files/<short sha>/config.toml` and then mounts it into the container, **versioned per commit**,
  so an old container that is still running does not suddenly read new values just because you pushed new config.
- **`appdata` in `volumes` is a named volume** (no slash means a watcher-managed directory),
  located at `<mounts.dir>/config-file-app/volumes/appdata`. This directory is **never cleaned up**;
  deleting the target does not delete it either.
- Config files are mounted **read-only** by default: if the container runs as root, a writable mount leaves root-owned files on the host
  that the watcher cannot clean up later. Anything that needs to be written goes in `volumes`.

The template variable `workdir: /app` is the key: the app reads the relative path `config.toml`,
so the mount point `/app/config.toml` must land exactly in the container's working directory.

## How to run (local demo)

```bash
# 1. Start qooqd and the watcher (run in a scratch directory so watcher.db does not land in the repo)
qooqd -listen 127.0.0.1:7777 -data /tmp/qooq/qooq.db &
QOOQ_WATCHER_MOUNTS_DIR=/tmp/qooq/mounts qooq-watcher &

# 2. Push this directory into a local bare repo and register it as a target
qooq watch add --name config-file-app \
  --repo file:///tmp/qooq/config-file-app.git --component config-file-app --poll 30

# 3. Verify
curl $(qooq component get config-file-app | awk '/endpoints:/{print $2}')   # shows the greeting from config.toml
cat /tmp/qooq/mounts/config-file-app/volumes/appdata/starts.log
```

Change `greeting` in `config.toml` and push. Within 30 seconds it redeploys and `curl` shows the new string,
while the running count in `starts.log` **keeps increasing**: the config changed, the data is still there.

For the full field reference, see the
"Files and Volumes" section of [docs/DEPLOY-CONFIG.md](../../docs/DEPLOY-CONFIG.md).
