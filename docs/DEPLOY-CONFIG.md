# qooq.yaml Deployment Configuration Reference

English | [繁體中文](DEPLOY-CONFIG.zh-TW.md)

*The deployment declaration file that an app developer writes for qooq-watcher: it describes how the app is built, how it runs, and what resources it needs.*

> For operation, see [USAGE.md](USAGE.md); for system architecture, see [ARCHITECTURE.md](ARCHITECTURE.md).
> Runnable examples are in [examples/](../examples/) (an index is in the "Examples" section of this document).

`qooq.yaml` lives at the root of the app repo, is written by the developer, and travels with the code. The watcher reads it before every build.

## Table of Contents

- [Basic Rules](#basic-rules)
- [Compact Format](#compact-format)
- [Field Reference](#field-reference)
  - [General Top-Level Fields](#general-top-level-fields)
  - [Build](#build)
  - [Run](#run)
  - [Files and Volumes](#files-and-volumes)
  - [Services](#services)
  - [Health](#health)
  - [Ingress](#ingress)
  - [Inputs](#inputs)
  - [Policy](#policy)
  - [Requests](#requests)
  - [Limits](#limits)
- [${...} Interpolation](#-interpolation)
- [Connecting Multiple Projects: Where the Settings Go](#connecting-multiple-projects-where-the-settings-go)
- [Built-in Dockerfile Templates](#built-in-dockerfile-templates)
  - [Three Things to Know Before Using Them](#three-things-to-know-before-using-them)
- [Examples](#examples)
- [Field Flow](#field-flow)
- [Related Documents](#related-documents)

> [!TIP]
> To see every field and its default in one place: [qooq.reference.yaml](qooq.reference.yaml) is a
> fully annotated manifest, field by field (for lookup, not for copying wholesale).

## Basic Rules

- **Location**: `qooq.yaml` at the repo root. A target can switch the path with `--manifest PATH`
  or disable it entirely with `--manifest -`; if the file does not exist there is no manifest, and
  behavior is identical to a pure deploy_spec deployment.
- **Unknown fields are tolerated**: extra keys are ignored and do not cause errors.
- **Only `version` is required**: every other field is optional and falls back to its default when
  omitted (the "Required" column in each table is explicit about this) — a qooq.yaml with just five
  general fields can already be deployed.

> [!IMPORTANT]
> **Merge precedence**: the manifest provides **defaults**, and the target's `deploy_spec` (operator
> settings) overrides it field by field with non-zero values — the repo is semi-trusted input, and
> the operator always has the final say.

> [!NOTE]
> Size limit is 1 MiB; path fields under `build.*` must be repo-relative paths (absolute paths, `..`,
> and symlink escapes are rejected). Validation failures are always recorded on the target as
> `last_build_status=failed` + `last_build_error` (visible with `qooq watch get <name>`), and
> `last_commit` is left untouched — push a fix and the next round retries.

## Compact Format

Most apps can be deployed with just this — the general fields (`version` / `app` / `description` /
`requests` / `domain`) plus one build method, with everything else on defaults:

```yaml
version: 1
app: api-server
description: Minimal Go + gin API example
requests:          # the machine must have at least this much to deploy (omitted = no check)
  cpu: 0.25
  memory: 128Mi

build:
  template: go
```

Hard resource caps for the container (`limits`) can be omitted — they automatically default to
`2× requests`. You can even leave out `requests`, in which case nothing is checked and nothing is
capped (good for just getting it running first).

Only when you need to expose a port, add a health check, or tune the restart policy do you need to
add `run` / `health` / `policy` blocks — the fields are all in the reference tables below. For a
complete runnable version, see
[examples/api-server/qooq.yaml](../examples/api-server/qooq.yaml).

## Field Reference

Every table has a "Required" column: **blank = optional**. The only truly required field in the whole file is `version`.

> [!IMPORTANT]
> `build` / `run` / `health` / `policy` / `requests` / `limits` are all optional blocks.
> **Any block you leave out starts normally with its default values** — the feature is not disabled,
> and the deployment does not fail.
> Each block's heading is followed directly by what its omitted-default is.

### General Top-Level Fields

> "General" is only this document's grouping name; **there is no `general:` level in the yaml** —
> these fields are written directly at the outermost level (as in the compact-format example above).

| Field | Type | Required | Allowed values / description |
|---|---|---|---|
| `version` | int | ✅ | Only `1` is supported |
| `app` | string | | App name, descriptive (does not affect deployment) |
| `description` | string | | One-line description, pure metadata (does not affect deployment) |
| `network` | string | | Container network name. **Omitted = a dedicated network `qooq-<component>`** (containing only this app and its services). Multiple apps given the same name end up on the same network and can reach each other by **component name** (e.g. `http://api:8080`). A target without a manifest keeps the engine default, with the same behavior as before M23 |
| `domain` | string | | Public domain of the service, e.g. `app.example.com`. When qooqd runs with `-ingress` it **actually routes** (and is placed first in `endpoints`); without it, it is just metadata. For multiple domains, path prefixes, and a specific port, see [Ingress](#ingress) below |

### Build

Default when omitted: build using the `Dockerfile` at the repo root.

> [!NOTE]
> **When a repo has both a `Dockerfile` and a `qooq.yaml`, both are used, each handling half**: the Dockerfile decides **how to build**
> (what the image looks like), and qooq.yaml decides **how to run** (ports, env, services, health, limits). qooq.yaml does not replace
> the Dockerfile. Only three ways of writing it cause the Dockerfile to **not be used**:
> - `build.template`: switches to a built-in template and renders `Dockerfile.qooq-generated` to build with (the Dockerfile in the repo is ignored);
> - `build.dockerfile: <another path>`: uses that one instead;
> - `requests.runtime: process`: builds no image; runs `build.command` and takes `build.artifact`.
>
> **`build.dockerfile` is relative to `build.context`**: after setting `build.context: web`, the default `Dockerfile`
> means `web/Dockerfile`, not the one at the repo root.
>
> When the repo has **no** qooq.yaml, qooq infers one from the shape of the files (it looks for a Dockerfile in `Dockerfile`, `docker/`,
> `build/`, `.docker/` in that order, and only picks a built-in template if there is none) — **this is generated only the first time**; moving or adding a Dockerfile
> afterwards does not trigger re-detection. To change it, use `qooq watch manifest`, or simply put a qooq.yaml in the repo (the one in the repo always takes priority).

| Field | Type | Required | Allowed values / description |
|---|---|---|---|
| `build.dockerfile` | relative path | | Dockerfile location, default `Dockerfile`. Mutually exclusive with `template` |
| `build.template` | string | | Built-in template name: `static-nginx` / `go` / `node` / `python` / `go-react` / `rust` / `php` / `ruby` / `java` / `dotnet` / `bun` (see the "Built-in Dockerfile Templates" section). The repo does not need a Dockerfile. Mutually exclusive with `dockerfile` (setting both is an error) |
| `build.template_vars` | map | | Override the template's default variables (see each template's table in "Built-in Dockerfile Templates") |
| `build.context` | relative path | | Build context subdirectory, default repo root (for monorepos) |
| `build.command` | []string | | **process runtime only**: the build command run in the clone directory, e.g. `["go","build","-o","bin/app","."]`; omitted = no build |
| `build.artifact` | relative path | ✅ for process | **process runtime only**: location of the build artifact (executable) |

### Run

Default when omitted: use the image's built-in start command and publish no ports.

| Field | Type | Required | Allowed values / description |
|---|---|---|---|
| `run.command` | []string | | Override the image's start command (in process mode, the artifact's argv) |
| `run.env` | map | | Environment variables |
| `run.ports` | []string | | `"host:container"` (e.g. `"8080:80"`), `"auto:container"` (**the daemon picks the host port**, see below), or a bare `"container"` (**no host port published**, visible only inside the container network — use it with ingress). The first entry is the target of ingress routing. Has no effect under the process runtime (only logs a notice) |
| `run.env_file` | []string | | List of dotenv files, loaded **in order** (later ones override earlier ones). Source forms are the same as for mounts (see the next section): a repo-relative path, an absolute host path, or `"volume:/file/path"`. File **contents are not interpolated** |
| `run.files` | []mount | | Config files/directories mounted into the container, **read-only by default**. See the next section |
| `run.volumes` | []mount | | Persistent read-write directories (data). See the next section |

**Environment variable precedence** (low → high): `services` auto-injection → `run.env_file` (in order) →
`run.env` → the operator's `deploy_spec.env`.

### Files and Volumes

`run.files` is **configuration** (read-only by default), and `run.volumes` is **data** (read-write, kept across deployments).
They share the same syntax, and **both the short string and the long object forms are accepted**:

```yaml
run:
  files:
    - "./config.toml:/app/config.toml"       # short string src:dst[:ro|rw]
    - from: config/app.toml                  # equivalent long object
      to: /app/config.toml
      mode: ro
  volumes:
    - "appdata:/app/data"                    # named volume (directory managed by the watcher)
    - "/srv/todo/data:/app/data"         # specify a host directory directly
```

The source kind is determined by the string itself:

| `from` form | Meaning | Actual location on the host |
|---|---|---|
| Relative path (`./x`, `config/x`) | Repo content, copied out after the build | `<mounts.dir>/<component>/files/<sha7>/<path>` |
| Absolute path (`/etc/x`, `${env.HOME}/x`) | An existing file/directory on the host, mounted as-is | The original path |
| Bare name without a slash (`appdata`) | Named volume; the watcher creates the directory | `<mounts.dir>/<component>/volumes/<name>` |

> A name with a dot, such as `config.toml`, **counts as a path, not a volume name**. To refer to a file at the
> repo root, writing `./config.toml` is the least ambiguous.

Rules and guarantees:

- `to` must be an absolute path, must not be duplicated within one spec, and must not be `/`.
- Repo-relative sources are **versioned per commit**: an old container will not suddenly read new config because a new build overwrote files in place.
  Only the latest two copies are kept (current + previous, matching rollback semantics).
- **Named volumes are never cleared on redeploy** — that is data. **Removing a project (deleting the target) deletes it together with the data of its
  `services:`** (since 2026-10-30): a leftover database directory would make a same-name rebuilt database
  reject the newly generated password, and the app could never log in. To keep it, use `qooq watch rm --keep-component`
  (the container and the data are both kept).
- `files` defaults to `ro`: if root inside the container could write, it would leave root-owned files on the host
  that the watcher could not clean up later. To write, use `volumes`.
- **Rollback swaps only the image, not the configuration** (existing semantics preserved).

> [!WARNING]
> **The most common pitfall: the path the app actually writes to does not match the path where you mounted the volume.**
>
> The symptom is easy to recognize but hard to trace — the container builds, starts, logs only a line or two, and then **exits with no error message**.
> The framework's startup phase (fx OnStart, Django migrate, a Rails initializer, and the like) cannot create a file and aborts entirely,
> and that error often does not get written to the log in time.
>
> Two things need to be right:
>
> 1. **An app's data path is usually relative to its cwd.** If sqlite writes `./data.db` and the cwd is `/app`,
>    that file is at `/app/data.db` — a volume mounted at `/app/data` **does not cover it**.
>    Either mount the volume somewhere that covers it, or use `run.env` to point the app's path setting into the volume.
> 2. **When the Dockerfile has `USER <non-root>`, WORKDIR usually still belongs to root**,
>    and the app has no permission to create files there. For this kind of image, every path that gets written to must point into a volume.
>
> To diagnose: `qooq component logs <name>`. The last line before it died usually names the file it was trying to open.
>
> ```yaml
> run:
>   volumes:
>     - "appdata:/app/data"
>   env:
>     # point the app's data path into the volume mounted above, instead of the default cwd
>     MYAPP_DATABASE_PATH: /app/data/data.db
> ```

Security (watcher config `mounts.policy`, env `QOOQ_WATCHER_MOUNTS_POLICY`):

| Value | Behavior |
|---|---|
| `open` (**default**) | Everything is allowed, including absolute host paths specified in the manifest. Continues the existing stance: creating a target = trusting that repo |
| `repo-only` | The manifest may use only repo-relative sources and named volumes; the operator's `deploy_spec.mounts` is not subject to this |
| `allowlist:/a,/b` | Absolute paths must fall under one of the listed prefixes (the operator's too) |

> [!IMPORTANT]
> The source path of a bind mount is **resolved by the docker daemon on the host**, not inside the watcher's filesystem.
> If the watcher runs inside a container, `mounts.dir` must be a host bind path, and it must be mounted at the **exact same path** inside the watcher container,
> otherwise the watcher writes successfully but docker cannot find it.

### Services

Declare the dependency containers the app needs (databases, caches). **A single `type:` line is a complete declaration** —
image, port, data directory, bootstrap environment variables, and health check all have defaults.

```yaml
services:
  db:
    type: postgres        # postgres|mysql|mariadb|redis|mongo|custom
```

It expands into a **regular component** named `<app-component>-<key>` (in the example above: `myapp-db`),
on the app's group and network, and `qooq component ls/get/logs/stop/rollback` all apply.

| Field | Type | Required | Allowed values / description |
|---|---|---|---|
| `type` | string | | `postgres` (default 16) / `mysql` (8) / `mariadb` (11) / `redis` (7) / `mongo` (7) / `custom`. `custom` requires you to provide `image` and `port` |
| `version` | string | | Version substituted into the type's default image, e.g. `"15"` → `postgres:15-alpine`. Ignored when `image` is set |
| `database` / `user` | string | | Both default to the app's component name (normalized: lowercased, non-alphanumerics become `_`) |
| `password` | string | | Omitted = **generated on first use and remembered** (stored on the target, encrypted when `secret.key` is set). Supports `${...}`, e.g. `${env.DB_PASSWORD}` |
| `storage.volume` | string | | Data volume name, default `<component>-<service>-data` |
| `storage.path` | string | | Data path inside the container, default determined by type |
| `storage.size` | string | | Recorded only; not enforced in v1 |
| `image` / `port` | string / int | ✅ for custom | Override the values derived from type |
| `env` | map | | Extra environment variables, layered over the type's bootstrap variables |
| `ports` | []string | | Publish to the host (e.g. `"15432:5432"`). **Not published by default**; the service is visible only inside the network |
| `health` | health block | | Override the type's built-in health check |
| `replicas` / `mode` | int / string | | **Reserved for clustering.** v1 accepts only `1` / `single`; any other value is rejected with an explicit "not supported yet" rather than quietly running a single copy |

The app automatically receives these environment variables (`<NAME>` is the service name uppercased, `db` → `DB`):

```
DB_HOST=myapp-db  DB_PORT=5432  DB_USER=myapp  DB_PASSWORD=…  DB_DATABASE=myapp
DB_SCHEME=postgres  DB_URL=postgres://…
DATABASE_URL=postgres://…        # additionally provided, as a convention name, when the service is named db
```

Each type's URL is in the form that server actually accepts:

- **redis has no database**, so `_DATABASE` is not provided; the user is redis's built-in `default`, and the URL is
  `redis://default:<password>@<host>:6379`. The generated password is really set in redis (`requirepass`),
  so connecting with only `_HOST`/`_PORT` and no password will be rejected.
- **mongo's URL carries `?authSource=admin`**: the image creates the user in `admin`,
  and without this parameter you get `Authentication failed` even when the password is correct.

When a framework wants different variable names, point to them yourself with interpolation:

```yaml
run:
  env:
    APP_DATABASE_DRIVER: postgres
    APP_DATABASE_HOST: "${service.db.host}"
    APP_DATABASE_PORT: "${service.db.port}"
    APP_DATABASE_PASSWORD: "${service.db.password}"
```

Behavior guarantees:

- **Services start first and are confirmed ready before the app is deployed** (waiting up to 3 minutes), so an app that connects to the DB at startup does not hit an empty socket.
- **A service whose spec has not changed is left alone**: redeploying after changing the app's code does not restart the database.
- **The password never changes**: once generated it is stored and reused — the password is written into the data directory when the DB is initialized,
  and changing it later would make the app unable to open its own data.
  The password is saved **before the service is created**, so it is not lost even if the first deployment fails midway.
- **A service removed from qooq.yaml is not deleted automatically**: the next deployment **stops** it and leaves the data in place,
  and `qooq watch get` flags it as "no longer in qooq.yaml".
  Add it back to qooq.yaml and it resumes with its original data and password; when you are sure you no longer want it,
  `qooq watch service-rm <target> <service>` (you must type the service name to confirm) deletes the container and the data.
  Removing the whole project deletes it too. The reason: getting one line of YAML wrong (or renaming a service) should not make a database disappear permanently.
- `requests.runtime: process` + `services:` → an explicit error (the process runtime has no container engine).

### Health

Default when omitted: no active liveness probe (a container crash is still detected by the monitor
and handled per `policy`). If you write this block, you must give either `http` or `exec`.

| Field | Type | Required | Allowed values / description |
|---|---|---|---|
| `health.http` | string | one of two | HTTP probe path, e.g. `/healthz` |
| `health.exec` | []string | one of two | exec probe command |
| `health.port` | int | ✅ with http | Which port the probe hits; it **must be one that appears in `run.ports`** (otherwise the deployment fails outright — if it were not enforced, the probe would hit `127.0.0.1:<that number>`, which sooner or later belongs to **someone else's** component, so a broken app would check a healthy stranger, report itself healthy, and never be restarted). **Writing the container port is recommended**: that form stays correct under `auto:`, and qooq automatically hits the allocated host port. Only when that container port is **not published at all** does it fall back to the container IP (unreachable from Docker Desktop; use an exec probe for that combination) |
| `health.interval` | duration | | Probe interval, e.g. `"10s"` (default `10s`) |
| `health.timeout` | duration | | Timeout of a single probe (default `5s`) |
| `health.failures` | int | | Consecutive failures that trigger a restart (default `3`) |
| `health.delay` | duration | | Grace period after start (default `5s`) |

> [!IMPORTANT]
> **If a probe has never succeeded, qooq will not restart the component.** Never passing even once means "the probe cannot see
> this app" (wrong port, wrong path, or the container IP is unreachable on this docker host), not "the app is broken";
> restarting in response would just rebuild a perfectly good container forever, and the user would see "it keeps restarting" with nothing
> in the app's own log. In that case a `health_unreachable` event is recorded explaining the **suspected cause and how to fix it**
> (visible with `qooq component events <name>`), and probing continues — if it starts passing someday, the ability to restart
> is restored automatically, with nothing for you to do. **Once it has passed once**, behavior is exactly as before: after consecutive failures reach `health.failures`, the component is rebuilt.

#### When the New Version Fails to Start: Automatic Return to the Previous Version, With a Few Seconds of Interruption

After a new version is swapped in, the watcher waits for it to prove it can stand (health passes; with no health, stable Running for 10 seconds).
If it exits, is restarted after a crash, or health never passes, it **swaps the previous version's image back**, marks the deployment `failed`
with the new version's last output attached, and **polling will not deploy the same commit again** (only pushing a new commit or forcing a redeploy will).

**The service is interrupted during the swap**: from the moment the old container is replaced, through the new version being judged unable to start, until the old version is back up,
usually a few seconds to a dozen or so (measured: probing every 8 seconds, one probe went unanswered). The reason is that the old and new containers **cannot both hold the same
host port**, so there is no way to "start the new one alongside, verify it, then cut over". True zero downtime needs a proxy layer in front switching between the two
containers (`-ingress`'s Caddy is the embryo of that layer), which has not been built yet.
Note that **rollback swaps only the image, not the configuration**: if the new commit also changed settings such as `run.env`, the old image will start up with
the new settings.

### Ingress

Takes effect only when qooqd runs with `-ingress` (for usage, see [USAGE.md](USAGE.md), "Using URLs Instead of Ports").
Omitting the whole block = defaults: **a derived URL, forward the entire host, hit the first port** — most apps do not need to write it.

```yaml
domain: app.example.com     # shorthand for a single domain, equivalent to ingress.hosts: [app.example.com]

ingress:
  hosts: [app.example.com, www.example.com]   # use this for multiple domains
  port: 8080                # specify which one to expose when several ports are open
  path: /api                # forward only this prefix (so two apps can share one domain)
  strip_path: true          # strip the prefix before forwarding
  tls: true                 # override the daemon's -ingress-tls
```

| Field | Type | Required | Allowed values / description |
|---|---|---|---|
| `ingress.hosts` | []string | | Extra hostnames. **Added to `domain`, not a replacement**; derived ones such as `<name>.localhost` are still provided |
| `ingress.port` | int | | The container port that is the routing target. Omitted = the first entry of `run.ports`. **Must be one in `run.ports`**; otherwise the build fails outright and lists which ones exist |
| `ingress.path` | string | | Forward only this URL prefix; must start with `/`. Omitted = the whole host. Both the prefix itself and the paths under it match (`/api` and `/api/*`) |
| `ingress.strip_path` | bool | | Strip `path` before forwarding. Default `false` (forwarded as-is). Set `true` when the app itself serves `/`; keep `false` when the app already recognizes `/api` |
| `ingress.tls` | bool | | Override the daemon default. **Can only turn off, not on** — if the daemon was not given `-ingress-tls` there is no 443 listener, and writing `true` is ignored with one warn line logged |

**When several apps share one domain**, entries with a `path` are automatically ordered before those without (the other way around, the site-wide entry would
swallow all of `/api`, and that app would look perfectly fine while receiving no requests at all):

```yaml
# the frontend's qooq.yaml
ingress: {hosts: [app.example.com]}
# the backend's qooq.yaml
ingress: {hosts: [app.example.com], path: /api, strip_path: true}
```

### `${component.<name>.<field>}` — Referencing Another Component's Address

When two apps need to call each other, do not hard-code the other side's port in the manifest. The cost of hard-coding is: when either side changes its port,
the others must change with it, and **nothing will tell you that you missed one** — with `auto:` the number does not even exist.

```yaml
run:
  env:
    # container-to-container (SSR, backend-to-backend)
    API_INTERNAL: "http://${component.api.address}"      # → http://api:8080
    # for the browser (frontend JS, CORS allowlist)
    PUBLIC_API: "${component.api.endpoint}"              # → http://192.168.1.50:19000
```

| Field | Value | Who is calling |
|---|---|---|
| `.host` | `api` | Container network alias |
| `.port` | `8080` | Container port |
| `.address` | `api:8080` | **Inside the container** |
| `.host_port` | `19000` | Port on the host |
| `.endpoint` | `http://192.168.1.50:19000` | **Outside the container** (browser) |

**qooq deliberately does not decide for you which kind to use.** Both are correct addresses of the same component, but they are not interchangeable,
and **choosing wrong is silent**: the container is still Running, health is still green, and only the browser sees the error
(the typical symptom is CORS being blocked, or the frontend calling an address nobody is listening on). So the manifest states it explicitly.

Values come from **qooqd's record of the component it is running**, not from the other side's repo — qooq will not
clone someone else's source code to resolve this. When the port is omitted, the first entry of `run.ports` is taken, the same rule as `ingress.port`.

**Limited to the same namespace.** A component's identity is `(namespace, name)`; an `api` in each of two stacks
is two different things; `${component.api...}` always refers to the one in its own namespace.

**Referencing a component that has not been deployed yet is not a failure**: it stops at `needs_input` (`watch get` shows who it is waiting for),
and the next sweep retries automatically — that is an ordering issue and resolves itself.

**Two apps that reference each other deploy too** (say the api's CORS list names the web app's URL and the web app names the api's):
before a deploy resolves its env, it **reserves** its own address with qooqd (its `auto:` ports are assigned and kept at that point),
so whichever is tried second can read the first one's address, and the first follows on the next sweep. A reserved port is never handed
to anyone else, the deploy gets exactly that port, and deleting the target releases it.
A loop that still cannot resolve (for example a qooqd older than the watcher) says `circular reference: a -> b -> a`
instead of "not deployed yet" forever.

> There is no reverse tracking: if the referenced component changes its port, the app that references it does **not** redeploy automatically.
> In practice this rarely comes up, because a port allocated by `auto:` does not change (see the previous section); if you really do change it, redeploy the referencing side.

### `auto:` — Let the Daemon Pick the Host Port

```yaml
run:
  ports: ["auto:8080"]
```

The host port is the **one thing this manifest cannot know**: it belongs to the machine, not to this code.
The cost of specifying it by hand is that every new app has to be reconciled against every other app on the machine — three interconnected projects
already need manual staggering.

With `auto:`, qooqd allocates it, in the range of its `-auto-port-range` (default `19000-19999`).

**An allocated port does not change.** Redeploying, changing the image, or rolling back gives the same container port the same
host port; the match is on the container port, so reordering or adding ports in the manifest will not disturb existing addresses.
This is deliberate: **an address that changes is worse than no auto-allocation** — whatever points at the old address (another app's config file,
a CORS allowlist, a browser bookmark) will break somewhere far from the change.

To see which port was allocated, look at the `ports:` and `endpoints:` lines of `qooq component get <name>`. API responses
always return the actual port, never `auto`. If the range is exhausted it fails explicitly (it does not fall back to a random port).

The CLI accepts it too: `qooq component deploy --name web --image nginx --port auto:80`.

### Inputs

Declare values that "this app needs but the repo cannot possibly know": API keys, public URLs, domains.
Omitting the whole block = this app needs no external input (the vast majority of cases).

With a declaration, the behavior on a missing value changes from **deployment failure** to **telling you what to fill in**:
the target stops at `last_build_status: needs_input` (**no build is run**),
`qooq watch get <target>` prints what is missing and what each is for, and filling it in redeploys automatically.

```yaml
inputs:
  STRIPE_KEY:
    description: Stripe secret key, from dashboard → Developers → API keys
    required: true
    secret: true
  PUBLIC_HOST:
    description: Address reachable from the browser
    default: ${host}
    example: 192.168.1.50

run:
  env:
    STRIPE_SECRET_KEY: "${input.STRIPE_KEY}"
    PUBLIC_BASE: "http://${input.PUBLIC_HOST}:8080"
```

| Field | Type | Required | Allowed values / description |
|---|---|---|---|
| `inputs.<NAME>.description` | string | | Explanation for the person filling in the value. **Strongly recommended** — this is the whole point of declaring an input instead of using `${env.*}` |
| `inputs.<NAME>.default` | string | | Used when no value is given. **It is itself interpolated**, so `default: ${host}` works. With a default it is never considered missing |
| `inputs.<NAME>.required` | bool | | Block the deployment when there is no value (`needs_input`). **Mutually exclusive** with `default` (with a default it can never be missing, so writing both is an error) |
| `inputs.<NAME>.secret` | bool | | The value is stored encrypted (same mechanism as `secret.key`), and the API always returns `***` |
| `inputs.<NAME>.example` | string | | Example value, shown together with description |

References are always `${input.<NAME>}`. **Undeclared names cannot be referenced** (unknown reference error) —
the declaration block is the authoritative source, not a comment.

How to supply values:

```bash
qooq watch add ... --input PUBLIC_HOST=192.168.1.50   # supply when creating the target
qooq watch set-input <target> STRIPE_KEY=@key.txt     # supply later (@file keeps it out of shell history)
qooq watch set-input <target> STRIPE_KEY=             # empty value = clear it
```

`set-input` triggers a redeploy immediately, without waiting for the poll interval or pushing an unrelated commit.

> [!NOTE]
> Values are stored on the **target**, not in the repo. The same repo deployed as two targets can have different values for each.

### Policy

Default when omitted: `restart: always` (a stopped service is brought back — the same for a crash, a docker restart, or a machine reboot).
If you do not want it brought back, write `restart: no` explicitly. A component stopped manually (`qooq component stop`)
is not restarted regardless of policy. Before 2026-11-12 the default was `no`; **already-deployed ones do not change** and pick it up at the next deployment.

| Field | Type | Required | Allowed values / description |
|---|---|---|---|
| `policy.restart` | string | | `always` (default) / `on-failure` / `no` |
| `policy.replicas` | int | | Reserved field; only `1` or omitted is allowed (other values are an error) |

### Requests

"The **minimum** resources this app needs" — **compared against host capacity before deployment**; if insufficient, it fails.
It is only a scheduling/capacity threshold and **does not limit how much the container can actually use** (that is `limits`' job). Like k8s
`requests`. Omitting the whole block = no capacity check at all.

| Field | Type | Required | Allowed values / description |
|---|---|---|---|
| `requests.cpu` | float | | CPU cores needed, e.g. `0.5`; insufficient host = failed |
| `requests.memory` | string | | Memory needed: `256Mi`/`1Gi` (base 1024), `256MB` (base 1000), `64m` and similar forms are all recognized |
| `requests.arch` | []string | | Acceptable CPU architectures: `amd64` / `arm64` / `386` / `arm` (aliases such as `x86_64`, `aarch64` are normalized before comparison). Omitted = no check |
| `requests.runtime` | string | | `container` (default) or `process`; verified in both directions against qooqd's actual runtime, mismatch = failed |
| `requests.disk` | string | | Currently parsed but not validated |
| `requests.gpu` | string | | Currently parsed but not validated |

### Limits

"The **maximum** resources the container can use" — a hard cap actually enforced by cgroups (`cpu_limit`/`mem_limit` in
`component get`). Like k8s `limits`.

**Each field, when omitted, = `2× the corresponding requests`** (this derivation exists only when `requests` is written; the multiplier is set by
the watcher config `limits.multiplier`, default `2`). When `requests` is also omitted = no limit at all.

| Field | Type | Required | Allowed values / description |
|---|---|---|---|
| `limits.cpu` | float | | Hard CPU cap for the container (cores). Omitted = `2× requests.cpu`; if requests is also omitted = no limit |
| `limits.memory` | string | | Hard memory cap for the container (same unit forms as `requests.memory`). Omitted = `2× requests.memory`; if requests is also omitted = no limit |

## ${...} Interpolation

`${...}` is supported in values of `run.env`, every argument of `run.command`, mount paths, and fields of `services.*`:

| Form | Expands to |
|---|---|
| `${app}` `${component}` `${network}` `${commit}` | Basic info of this deployment (`commit` is the 7-character short sha) |
| `${host}` | This machine's public-facing address (qooqd's advertise host, see below) |
| `${service.<name>.host\|port\|user\|password\|database\|scheme\|url}` | That service's connection info |
| `${volume.<name>.target}` / `${volume.<name>.host_path}` | The volume's mount point inside the container / its host path |
| `${input.NAME}` | The value declared in this manifest's [`inputs:`](#inputs) |
| `${secret.NAME}` | A named secret of that namespace (see Secrets below) |
| `${env.NAME}` | An environment variable of the **watcher process** (an operator escape hatch, see the warning below) |

`${host}` comes from qooqd's `-advertise-host` / `QOOQ_ADVERTISE_HOST`; if unset, the primary network interface's
IPv4 is detected. For an address meant for the browser, just write `http://${host}:8080`; there is no need to hard-code a LAN IP into the repo.

> [!WARNING]
> **`${env.*}` is an escape hatch for operators, not something an app should use.** It reads the watcher process's
> environment variables, and **all targets on one watcher share the same set** — when different people share one watcher,
> they can read each other's secrets.
>
> For values an app needs, prefer `${input.NAME}` (it tells the user what is missing and what it is),
> and use `${secret.NAME}` for deployment-level shared secrets.

### Secrets (per-namespace, 2026-09-29)

`${secret.NAME}` is resolved from **that namespace's own secret store**, stored encrypted in the watcher's sqlite
(reusing M18's AES-GCM, the same scheme as git credentials). **One namespace cannot read another namespace's
secrets**, which matters when several people share one watcher.

```bash
# set one (the value appears only here, once; no later command can read it back); NAME=@file reads from a file, keeping it out of shell history
qooq secret set STRIPE_KEY=sk_live_... -n <namespace>

# store a project's whole .env (each KEY=VALUE line becomes one secret)
qooq secret import .env -n <namespace>

# list what exists (names only, never values)
qooq secret ls -n <namespace>

# delete one
qooq secret rm STRIPE_KEY -n <namespace>
```

In the manifest you still write `${secret.STRIPE_KEY}`, exactly as before — **the syntax was frozen before the store was swapped,
so manifests already written need not change by a single character.**

**Resolution order**: that namespace's store is checked first, and only if absent does it fall back to the watcher's `QOOQ_SECRET_<NAME>`
environment variable. The order is deliberate — a value the namespace set itself always wins; otherwise a careless
`QOOQ_SECRET_DB_PASSWORD` on the host would silently override the one a namespace set.

### Build Limits (qooqd Flags, Not the Manifest)

```bash
qooqd -build-timeout 30m \
      -build-memory 2g \
      -build-cpus 2 \
      -build-cache-max 10g
```

**These are set on the daemon, not sent with each build** — what submits a build is a repo you may not fully control,
and **a limit that the submitter can raise itself is not a limit**.

| Flag | Default | Description |
|---|---|---|
| `-build-timeout` | **30m** | Abort on timeout. **This default is on**: a Dockerfile that never finishes (`RUN sleep infinity`, a package manager stuck at an interactive prompt) holds a build slot until the daemon shuts down, and the watcher has a small fixed number of slots — a few such repos make it impossible for **everyone** to build |
| `-build-memory` | unlimited | e.g. `2g`. Swap is bound to the same value, otherwise docker grants an equal amount of swap on top (a 512m limit can actually use 1g, and will swap the machine to a halt) |
| `-build-cpus` | unlimited | e.g. `1.5` |
| `-build-cache-max` | not pruned | e.g. `10g`. **docker has no per-build disk limit**, so "prune the cache to under the cap after a build" is the only way to keep the disk from being eaten up. Pruning happens **after** the build — pruning first would throw away the cache that is about to be used |

**Only the timeout default is on**; the rest default to unlimited: on a single-user machine every build is your own, so limiting them
is pointless. **On a shared machine, or when building repos you do not trust, set all four.**

> [!NOTE]
> **On timeout, docker is asked to cancel the build, but cancellation only takes effect between steps.**
>
> | Shape of the Dockerfile | After the timeout |
> |---|---|
> | Several `RUN`s (common) | ✅ Cancellation takes effect, and **the next step never starts** |
> | One very long `RUN` | ⚠️ That step runs until it finishes on its own (the classic builder checks for cancellation only at step boundaries) |
>
> **So on a shared machine `-build-memory` and `-build-cpus` are not optional**: they bound the
> leftover case above (measured to take effect).

**In addition, a target with 10 consecutive failures stops being retried** (`paused_reason` explains why);
after fixing it, run `qooq watch trigger <name>` to resume. Without this, a repo that can never build
would retry every polling cycle and occupy build capacity forever.

A timeout error is **reported to the user in full** (`build exceeded this daemon's build timeout after 30s`,
HTTP 504), not masked as `internal error` — it is the operator's policy taking effect, not a server fault, the same principle as returning 409 for a
port collision.

### Registry Mode: Build Output Leaves the Local Docker

```bash
QOOQ_WATCHER_REGISTRY_PREFIX=registry.example.com/qooq   # empty = disabled (default)
QOOQ_WATCHER_REGISTRY_USERNAME=...                        # optional
QOOQ_WATCHER_REGISTRY_PASSWORD=...                        # optional
```

**It is off by default, and this is deliberate rather than a compromise**: on a single machine the built image lands directly in the very docker daemon that
will run it next, so pushing and pulling again adds no capability, only an extra copy on disk and a network round trip.

**When it is mandatory**: the moment qooqd is not on the build machine (multiple nodes, or any setup where build and run are separate).
At that point what the watcher built must be handed to another machine, and qooq has no other path for moving an image across machines.

When enabled:

| | |
|---|---|
| A build produces **two tags** | The local name + `<prefix>/<local name>`, from the same build, so there is no window in which two names point at different images |
| The ref used for deployment | **The remote one** (`<prefix>/...`), so other machines can pull it |
| The remote name carries the namespace | The local tag already does (see M2) and the remote inherits it directly — two namespaces will not collide in the registry either |
| Push failure | **The whole build fails**. It does not warn and deploy anyway — that would be "succeeds on the build machine, fails everywhere else", which is exactly the situation the registry exists to solve |

> **What it does not solve**: **this is not build isolation.** The build still runs on the local docker daemon, and the registry
> is just where the output goes afterwards. Moving the build out of that daemon is a separate matter.

### When One Watcher Serves Repos That Are **Not Your Own**: `untrusted_repos`

```bash
QOOQ_WATCHER_UNTRUSTED_REPOS=true
```

**Default `false`** (the offline edition: the targets you create are your own code, and running it is the point).
Once a watcher will build **other people's** repos, this must be turned on — it closes three holes at once:

| When enabled | Reason |
|---|---|
| **`build.command` is rejected outright** | It **runs the repo's own commands on the host** with the watcher's privileges. For an uncontrolled repo there is no safe way to do this |
| `mounts.policy` is automatically tightened to `repo-only` | The default `open` lets a manifest mount any host path — including the watcher's own sqlite |
| `${env.*}` and the `QOOQ_SECRET_` fallback are closed | See the previous section (if `secrets.allow_process_env` is not written explicitly it follows this switch) |

> **Why "reject" instead of "sandbox"**: the reason `build.command` exists is the process runtime
> — users who do not want to install docker. Sandboxing it would require docker, which is exactly what its users are avoiding.
> And an incomplete sandbox is worse than a plain no: it reads as secure, while a manifest can get around it.
> For untrusted repos, build with a **Dockerfile or `build.template`**; that path goes through the docker sandbox.

**Regardless of the switch, `build.command` can never read `QOOQ_*` environment variables** (including
`QOOQ_WATCHER_SECRET_KEY` — that key can unlock **every** namespace's credentials and secrets).
Other environment variables are inherited as usual, because a build may really need them (toolchain cache paths, proxy settings).

### When One Watcher Is Shared: Turn Off `${env.*}`

```bash
QOOQ_WATCHER_SECRETS_ALLOW_PROCESS_ENV=false
```

**The default is `true`**, because for a single user the watcher's environment is your own, and reading it is a feature, not a hole.
But **when one watcher is shared by several people this should be turned off**, otherwise both `${env.*}` and the `QOOQ_SECRET_<NAME>`
fallback can read the shared environment.

Once it is off, `${env.FOO}` **fails explicitly and tells the author to use `${secret.FOO}` instead**, rather than quietly expanding to an empty string.

> When the watcher detects that it is serving **more than one namespace** while this switch is still on, it warns once at startup.
> With only one namespace it does not warn — reading its own environment is normal behavior then, and a warning that is often harmless gets learned and ignored.

- **A typo fails explicitly and lists the available keys**; it is never quietly expanded to an empty string.
- The **file contents of `run.env_file` are not interpolated** (a file is data; any `${...}` inside is passed to the app verbatim).

## Connecting Multiple Projects: Where the Settings Go

When the web frontend and the api backend live in two repos, each with its own `qooq.yaml`, the settings for "who can reach whom" split into two halves
**written in different places** — this is the easiest thing to get confused about.

### 1. Joining the Same Network: Each Manifest Writes It Once

```yaml
# both projects' qooq.yaml write this line (the string must be exactly identical)
network: todo
```

There is no "stack-level" config file where it could be written just once — **the network name is the agreement**,
like everyone agreeing to use the same port. Writing the same string in both is deliberate:
every repo can be deployed independently, without depending on another repo existing.

Components on the same network address each other by **name** (not IP; IPs change on every rebuild):

```
http://api:8080     ← use this inside containers
```

### 2. Connection Addresses: Written on "the Side That Makes the Request"

**Rule: whoever makes the request writes it in their own manifest.** The side being connected to does not need to know who will connect.

```yaml
# web/qooq.yaml — the server side makes requests from inside the container
run:
  env:
    API_INTERNAL: http://api:8080   # container name
    API_PUBLIC: http://192.168.1.50:19000   # for the browser
```

The same backend, two addresses, because **the machine making the request differs**:

| Who makes the request | Address | Why |
|---|---|---|
| Another container (server-side rendering, backend-to-backend) | `http://<component name>:<container port>` | Same network, resolvable by DNS |
| The user's browser | `http://<host's LAN IP>:<host port>` | The browser is on another machine and can only see the host |

> [!WARNING]
> **`127.0.0.1` inside a container is the container itself**, not the host. Server-rendered projects most often get stuck here:
> in development `localhost:8080` works, but inside the container it becomes "connect to my own 8080".
>
> Without a shared network, the only way for a container to reach a service on the host is `host.docker.internal`
> (available only on Docker Desktop; on Linux you have to add `--add-host` yourself).

### 3. Addresses for the Browser: Use `${host}`, Do Not Hard-Code the LAN IP

The `192.168.1.50` in the example above is **the address of the machine at the time of deployment**. Hard-code it, and this repo can only
be deployed to that one machine. Use `${host}` instead:

```yaml
run:
  env:
    API_INTERNAL: http://api:8080   # container name, unchanged
    API_PUBLIC: http://${host}:19000   # decided at deployment time
    # even better: let qooq fill in the host and the auto-assigned port
    # API_PUBLIC: "${component.api.endpoint}"
```

`${host}` is the advertise host reported by qooqd. When qooqd has no `-advertise-host` set it detects the primary network interface,
which is usually correct on a laptop or a single-machine VM; when detection is wrong (VPN, multiple NICs), add
`-advertise-host 192.168.1.50` to qooqd, and the manifest does not need to change by a single character.

When you need **the person deploying** to decide (a production domain, different addresses per environment), declare it as an input:

```yaml
inputs:
  PUBLIC_HOST:
    description: Address reachable from the browser
    default: ${host}          # with a default it will not stop to ask
run:
  env:
    API_PUBLIC: http://${input.PUBLIC_HOST}:19000
```

The operator-side `deploy_spec.env` can still override any item (highest precedence),
which suits machine-specific settings you do not want in the repo.

```yaml
# recommended approach: the repo writes only a placeholder, and the actual address is supplied by the watcher on the deploying machine
run:
  env:
    API_PUBLIC: "http://${env.PUBLIC_HOST}:19000"
```

```bash
PUBLIC_HOST=192.168.1.50 qooq-watcher     # supply when starting the watcher
```

A better approach is to enable the ingress and set `domain` (a domain name instead of IP:port); see [Ingress](#ingress) above.

### 4. Deployment Order Is Not Guaranteed

The two targets poll independently, with **no ordering between them**. web may finish deploying before api is up,
and its first server-side data fetch will fail (the page still renders, just without data); once api is up it works normally.
To get an order, put both in a stack and use `depends_on` (see `examples/todo-stack`).

Dependency services declared in `services:` **do** have an ordering guarantee, but only within a single manifest.
Dependency ordering across targets is a known gap.

The definition of "ready" (corrected 2026-09-24): the app is deployed only after the service container is up **and its health probe has passed at least once**.
For a database the two events are several seconds apart — the process being up does not mean it is already accepting connections,
and frameworks that build a connection pool at startup (Go fx, NestJS, Spring Boot) fail in exactly that gap.
The built-in service types each carry their own probe command (postgres uses `pg_isready`, mysql uses `mysqladmin ping`,
redis uses `PING`, ...), so you need not write one.

The only exception is a **custom http probe**: it may never see a healthy app
(`health.port` pointing at a port nobody listens on, or probing the container IP on Docker Desktop),
so after waiting 60 seconds without passing it **prints one warning line and continues the deployment**, rather than hanging the whole pipeline.
An exec probe has no such grace — `docker exec` can always get in, so failing to probe means the service really has a problem,
and the deployment fails.

## Built-in Dockerfile Templates

When `build.template` is specified, the watcher renders `Dockerfile.qooq-generated` after cloning
(written at the root of the build context) and then builds — the repo does not need a Dockerfile at all.

| Template | Purpose | Variables (defaults) |
|---|---|---|
| `static-nginx` | Static files + nginx; with `build_cmd`, builds with node first, then hands the output to nginx | `nginx_tag` (`alpine`), `static_dir` (`.`), `build_cmd` (empty = no build), `node_version` (`20`), `install_cmd` (`npm install`), `corepack` (empty), `spa` (empty; when set to `true`, any path not found returns `index.html`, so refreshing a frontend route does not 404) |
| `go` | Multi-stage go build → single binary | `go_version` (`1.25`), `build_path` (`.`), `base_image` (`alpine:3.20`), `port` (`8080`), `cgo` (`0`), `build_deps` (empty), `runtime_deps` (empty), `extra_copy` (empty), `workdir` (`/app`) |
| `node` | `node:alpine` install (+ optional build) + start | `node_version` (`20`), `install_cmd` (`npm install --omit=dev`), `build_cmd` (empty), `start_cmd` (`node index.js`), `port` (`3000`), `corepack` (empty), `workdir` (`/app`) |
| `python` | `python:slim` pip + CMD | `python_version` (`3.12`), `start_cmd` (`uvicorn main:app --host 0.0.0.0 --port 8000`), `port` (`8000`) |
| `go-react` | node builds the frontend → go:embed into a single binary | `node_version` (`20`), `web_dir` (`web`), `install_cmd` (`npm install`), `frontend_build_cmd` (`npm run build`), `frontend_dist_dir` (`dist`), `go_version` (`1.25`), `embed_dest` (`internal/webui/dist`), `go_build_path` (`./cmd/server`), `base_image` (`alpine:3.20`), `port` (`8080`) |
| `rust` | Multi-stage `cargo build --release` → debian slim | `rust_version` (`1.83`), `debian_tag` (`bookworm`), `port` (`8080`), `locked` (`1`), `bin_name` (empty = automatically find the only executable), `build_deps` (empty), `runtime_deps` (empty) |
| `php` | `php:apache`, **docroot pointed at `public/`** | `php_version` (`8.3`), `docroot` (`public`), `php_extensions` (`pdo_mysql zip intl opcache`), `composer_version` (`2`), `composer_flags` (`--no-dev --optimize-autoloader …`). **Port is fixed at 80** (apache) |
| `ruby` | `ruby:slim` bundle install + rackup | `ruby_version` (`3.3`), `start_cmd` (`bundle exec rackup --host 0.0.0.0 --port 3000`), `bundle_without` (`development test`), `port` (`3000`), `build_cmd` (empty), `runtime_deps` (empty) |
| `java` | maven package → temurin JRE runs the jar | `maven_tag` (`3.9-eclipse-temurin-21`), `jre_version` (`21`), `port` (`8080`). **Maven only**; Gradle projects can be detected but need their own Dockerfile |
| `dotnet` | SDK publish → aspnet runtime | `dotnet_version` (`8.0`), `port` (`8080`), `project` (empty = this directory) |
| `bun` | `oven/bun` install (+ optional build) + start | `bun_version` (`1`), `start_cmd` (`bun run index.ts`), `build_cmd` (empty), `frozen` (`1`), `port` (`3000`) |

<details>
<summary>Examples: overriding template variables</summary>

`template_vars` only needs to override what differs from the defaults. **Variable names must be ones listed in that template's table**;
a wrong one (for example writing `static_dir` as `dir`) is reported by `qooq manifest validate` (no daemon needed),
and the build also fails, with an error message listing the variables available for that template
— it used to be silently ignored, so the deployment succeeded but ran with the defaults:

```yaml
build:
  template: node
  template_vars:
    node_version: "22"
    start_cmd: "node server.js"
```

**A Go project that needs cgo** (`gorm.io/driver/sqlite` → `mattn/go-sqlite3`).
Without `cgo: "1"` it **compiles but blows up at runtime** (`requires cgo to work`):

```yaml
build:
  template: go
  template_vars:
    go_version: "1.26"        # must be >= the version declared in go.mod
    cgo: "1"
    build_deps: gcc musl-dev
    extra_copy: config.toml   # put the config file into the image too (or use run.files instead, so changing config needs no rebuild)
```

**SSR frontends** (Nuxt / Next / SvelteKit): setting `build_cmd` switches to two stages, build → runtime,
and the lockfiles of all three package managers are copied in:

```yaml
build:
  template: node
  template_vars:
    node_version: "22"
    corepack: "true"                          # needed when using pnpm/yarn
    install_cmd: pnpm install --frozen-lockfile
    build_cmd: pnpm run build
    start_cmd: node .output/server/index.mjs
```

</details>

### Three Things to Know Before Using Them

> [!NOTE]
> **The `port` variable affects only `EXPOSE`**; it does not automatically sync the listening port hard-coded in `start_cmd` or in
> the code — when you change the port, remember to change both sides together.

> [!WARNING]
> **`template_vars` values are inserted into the Dockerfile verbatim**, at the same trust level as writing the Dockerfile yourself,
> with no sandboxing — whoever can write `qooq.yaml` could already decide what the build runs.

> [!WARNING]
> **`go-react`'s `embed_dest` must match the path in the Go source's `//go:embed`** —
> the template only copies the frontend output to `embed_dest`; the source has to embed the same relative path itself.

## Examples

All runnable examples are in [examples/](../examples/):

| Example | template / hand-written Dockerfile | Highlights |
|---|---|---|
| [static-web/](../examples/static-web/) | Hand-written Dockerfile (the only control case) | Static web page; minimal demo of `run.ports` + `health.http` |
| [api-server/](../examples/api-server/) | `go` | Go + gin, `GET /hello` |
| [fullstack-server/](../examples/fullstack-server/) | `go-react` | React (Vite) frontend + Go (gin) go:embed single binary |
| [worker/](../examples/worker/) | `go` | Background process, no port, `health.exec` heartbeat check |
| [python-api/](../examples/python-api/) | `python` | FastAPI, `GET /hello` |
| [node-web/](../examples/node-web/) | `node` | Express, `GET /hello` |
| [config-file-app/](../examples/config-file-app/) | `go` | **`run.files` mounts a config file + `run.volumes` persists data** |
| [app-with-postgres/](../examples/app-with-postgres/) | `go` | **`services: {db: {type: postgres}}` gets you a database in one line** |

## Field Flow

`run` / `health` / `policy` / `domain` / `limits` are converted into fields with the same shape as the target's `deploy_spec`
(`command` / `env` / `ports` / `health_check` / `restart_policy` /
`domain` / `cpu_limit` / `mem_limit_bytes`), then go through the "operator override" merge:
manifest → deploy_spec → qooqd. `build.*` only controls the build behavior itself; `requests.*`
only does the pre-deployment host capacity check and is not passed to qooqd.

> [!NOTE]
> The old field names `resource:` and `requires:` have both been retired — appearing in a manifest they are explicitly rejected
> (`last_build_status=failed` + a hint to rename), not silently ignored. Move capacity needs to
> `requests:`, and write the container caps (if needed) into `limits:`.

## Related Documents

- [USAGE.md](USAGE.md) — Operation manual (GitOps quick start)
- [ARCHITECTURE.md](ARCHITECTURE.md) — System architecture overview
- [examples/](../examples/) — Eight runnable examples
