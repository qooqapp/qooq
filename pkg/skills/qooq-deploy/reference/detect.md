# Project type detection → template and defaults

Evaluate top to bottom and **stop at the first match**. After detecting, always read the
entry point source to confirm the port; do not trust this table's defaults alone.

| # | Condition | Template | Default container port | Notes |
|---|---|---|---|---|
| 1 | **The project has a Dockerfile**: in the root, or under `docker/`, `build/`, `.docker/` | **no template** | read `EXPOSE` | What the user wrote wins. Put `build.dockerfile: <relative path>` in qooq.yaml, e.g. `docker/Dockerfile`. **Check all of these locations before moving on** — plenty of projects keep the Dockerfile out of the root |
| 2 | `go.mod` **and** a frontend directory (`web/`, `frontend/`, `client/` containing `package.json`) | `go-react` | 8080 | frontend and backend become one binary |
| 3 | `go.mod` | `go` | 8080 | needs cgo (e.g. `mattn/go-sqlite3`), or a config file has to ship in the image → see "Two things that bite with the go template" below |
| 4 | `requirements.txt` / `pyproject.toml` / a `*.py` entry point | `python` | 8000 | |
| 5 | `nuxt.config.*` / `next.config.*` / `svelte.config.*` (SSR frameworks) | `node` + build stage | 3000 | **not** static-nginx. See "SSR frontends" below |
| 6 | `package.json` with a `start` script, or `server.js`/`index.js`/`app.js` | `node` | 3000 | |
| 7 | `package.json`, frontend only (react/vue/vite, `build` but no `start`) | `static-nginx` | 80 | the static files have to be built first, see below |
| 8 | Only `index.html` / static files | `static-nginx` | 80 | |

## Template variables (override only when a default does not fit)

The templates themselves are at `$QOOQ_SRC/internal/watcher/templates/<name>/Dockerfile.tmpl`
— read them when you need to check a detail.

| Template | Common variables (default) |
|---|---|
| `go` | `go_version`(1.25), `build_path`(.), `base_image`(alpine:3.20), `port`(8080), `cgo`(0), `build_deps`(empty), `runtime_deps`(empty), `extra_copy`(empty), `workdir`(/app) |
| `node` | `node_version`(20), `install_cmd`(npm install --omit=dev), `build_cmd`(empty), `start_cmd`(node index.js), `port`(3000), `corepack`(empty), `workdir`(/app) |
| `python` | `python_version`(3.12), `start_cmd`(uvicorn main:app --host 0.0.0.0 --port 8000), `port`(8000) |
| `static-nginx` | `nginx_tag`(alpine), `static_dir`(.) |
| `go-react` | `web_dir`(web), `frontend_dist_dir`(dist), `embed_dest`(internal/webui/dist), `go_build_path`(./cmd/server), `port`(8080) |

> [!IMPORTANT]
> The `port` variable only affects the Dockerfile's `EXPOSE`. It does **not** rewrite the
> port inside `start_cmd`. If the two disagree, override `start_cmd` as well, or the app
> listens somewhere else and the health check fails.

## Getting the container port right

The defaults above are a starting point. Always read the source to see what the app really
listens on:

- **Go**: `.Run(":8080")`, `http.ListenAndServe(":8080"`, `os.Getenv("PORT")`
- **Node**: `app.listen(3000)`, `process.env.PORT`
- **Python**: `uvicorn.run(..., port=8000)`, `app.run(port=5000)`; FastAPI usually takes it from `start_cmd`
- **Static**: always 80

**If the app reads a `PORT` environment variable** (very common), the least fiddly approach
is to set `PORT` in qooq.yaml's `run.env` so it listens on the same port the template
`EXPOSE`s and the health check probes — no template variables needed.

## Health endpoints

Only declare one when you are **sure it exists**. Reason: repeated probe failures trigger a
restart, so a wrong path produces a crash loop — worse than having no health check at all.

How to tell: look for registered routes in the source; `/health`, `/healthz`, `/ping`,
`/api/health` are the usual ones. Static sites use `/`. **If there is no clear health
endpoint, omit the whole `health:` block** — omitting it means no health checking, and the
component runs normally.

> [!NOTE]
> `health.port` takes a port that appears in `run.ports`. With `ports: ["auto:8080"]` that is
> the **container** port (`8080`) — qooq probes whichever host port it assigned. This holds in
> both modes: one-shot mode reads the same `qooq.yaml`.

## Two things that bite with the go template

**1. Projects that need cgo** (most often `gorm.io/driver/sqlite` → `mattn/go-sqlite3`).
The template defaults to `CGO_ENABLED=0`, so these **compile fine and blow up at runtime**
(`requires cgo to work`). Check with
`grep -r "mattn/go-sqlite3\|gorm.io/driver/sqlite" go.mod go.sum`. On a hit:

```yaml
build:
  template: go
  template_vars:
    cgo: "1"
    build_deps: gcc musl-dev
    go_version: "1.26"     # match the first line of go.mod; lower than that fails the build
```

**2. A config file that has to sit next to the binary** (viper reading `config.toml` or
`config.yaml` from the cwd). Either ship it with `extra_copy`, or mount it with `run.files`
— the latter is better, because changing the config then does not need a rebuild:

```yaml
run:
  files:
    - "./config.toml:/app/config.toml"
```

## SSR frontends (Nuxt / Next / SvelteKit)

These **have a build step and their output is not static files**, so `static-nginx` is
guaranteed to fail. Use the `node` template with a build stage:

```yaml
build:
  template: node
  template_vars:
    node_version: "22"
    corepack: "true"                         # only needed for pnpm/yarn projects
    install_cmd: pnpm install --frozen-lockfile
    build_cmd: pnpm run build
    start_cmd: node .output/server/index.mjs # Nuxt; Next is node_modules/.bin/next start
```

To tell which package manager: the `packageManager` field in `package.json` and the lockfile
(`pnpm-lock.yaml` / `yarn.lock` / `package-lock.json`). **If the project already ships a
Dockerfile, use it** (rule 1) rather than rebuilding the idea.

Nuxt/Nitro takes its port from the `PORT` environment variable (default 3000); set
`run.env.PORT` to change it.

## Config files and data directories

When you spot these, put them in qooq.yaml rather than letting the user discover the problem:

| Seen in the project | Write this |
|---|---|
| `config.toml` / `config.yaml` / `config/*.toml` (read from the cwd) | `run.files: - "./config.toml:/app/config.toml"` |
| `.env.example` / `.env.production.example` | tell the user to create the real `.env` and point `run.env_file` at it (**do not** commit secrets) |
| a directory the app writes to (sqlite `data.db`, `uploads/`, `storage/`) | `run.volumes: - "appdata:/app/data"`, otherwise every redeploy resets the data. **Then check the volume actually covers where the app writes** — see below |
| needs a DB (`postgres`/`mysql` connection settings, gorm/prisma/sqlalchemy) | `services: {db: {type: postgres}}`, then point the app's connection settings at `${service.db.*}` |
| the code reads an environment variable that `.env.example` gives no value for (keys, third-party credentials) | `inputs: {NAME: {description: ..., required: true, secret: true}}` plus `run.env` using `${input.NAME}` |
| a public address meant for the browser (`NUXT_PUBLIC_API_BASE`, `VITE_API_URL`) | an `inputs` entry with `default: ${host}` — a default means the user is not asked, and it follows the machine automatically |

The test: **would this value differ on another machine?** If yes it is an `inputs:` entry;
if no, hard-code it. An `inputs:` entry must always have a `description` — that sentence is
how the user knows where to get the value.

### Two ways a mounted volume still dies (**check every time**)

The symptom is identical in both: the build succeeds, the container starts, the log has one
or two lines, and it **exits with no error message**. The framework aborts during startup
when it cannot create a file, often before the error reaches the log.

1. **The app's data path is relative to the cwd.** `./data.db` or `sqlite:///data.db` really
   means `<WORKDIR>/data.db`, which a volume mounted at `<WORKDIR>/data` **does not cover**.
   Point the app's path setting into the volume with `run.env`
   (e.g. `MYAPP_DATABASE_PATH=/app/data/data.db`); mounting the volume is not by itself the fix.
2. **`grep -n '^USER' Dockerfile` shows a non-root user.** The WORKDIR usually still belongs
   to root, so the app cannot create anything there. For these images **every writable path
   must point into a volume**.

Do both checks before writing qooq.yaml: read the app's settings for where data goes, and
`grep USER` the Dockerfile. If it does die after deploying, `qooq component logs <name>` —
the last line before it died usually names the file it was trying to open.

## Several projects that need to reach each other

For a front and back end deployed separately on the same machine, give every qooq.yaml the
same `network:` and they can address each other by **component name**:

```yaml
network: todo          # the same line in both projects
```

```
inside a container (SSR backend calls, server-side fetch) → http://api:8080
in the browser (the user's device)                        → http://<LAN IP>:<host port>
```

**Inside a container, `127.0.0.1` is that container**, not the host — SSR projects trip on
this the most. Without a shared network, a container reaches a service on the host through
`host.docker.internal` (Docker Desktop).

## Frontend-only projects (case 7)

The `static-nginx` template only copies static files into nginx; it does **not** run
`npm run build` for you. Two options:

1. Have the user run `npm run build` first, then point `build.template_vars.static_dir` at
   `dist` / `build`
2. Use the `node` template's build stage to build inside the container, and serve it by
   something other than nginx (see the SSR section above)

Option 1 is cleaner, but say plainly in the report that changing the frontend means
rebuilding before redeploying.
