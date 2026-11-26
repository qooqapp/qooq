# qooq

English | [繁體中文](README.zh-TW.md)

**A local deployment platform built for AI-assisted development.** From source code to a
running, reachable service in one command.

qooq detects a project's stack, builds an image, deploys it as a container, verifies that it
answers, and keeps it running. It integrates natively with coding agents such as Claude Code,
Codex and Cursor: ask the agent to deploy and you get a working URL back — no Dockerfile to
write, no ports to manage, no reverse proxy to configure.

Open source under Apache 2.0. Everything is built and run on your machine; your source code
never leaves it.

## Features

- **Zero-config deployment** — detects the stack from the project's structure and generates the
  deployment configuration. Supports Next.js, Vite/React, Express, FastAPI, Flask, Django, Go,
  Rust, Rails, Laravel, Spring Boot, .NET and static sites, and works with existing Dockerfiles.
- **Declarative configuration** — `qooq.yaml` describes the build, ports, health checks, resource
  limits and dependent services, and lives in version control.
- **Managed backing services** — PostgreSQL, MySQL, MariaDB, Redis and MongoDB in a single line.
  Data persists across deploys, the application starts only once the service is ready, and
  connection details such as `DATABASE_URL` are injected automatically.
- **Secrets management** — declare the keys and credentials an application needs under `inputs:`
  and supply them at deploy time. `.env` files never enter the build.
- **Self-healing and automatic rollback** — restarts processes that exit, supports health checks,
  and rolls back to the last working version when a new one fails to start. After repeated
  failures it stops retrying and reports why.
- **Stable service addresses** — redeploys keep the same port, so anything that depends on the
  address keeps working.
- **GitOps** — watch a Git repository and build and deploy on every push (optional).
- **Multi-service orchestration** — `qooq.stack.yaml` defines several services, deploys them in
  dependency order and wires their addresses together.
- **Ingress** — a built-in reverse proxy serves each service at `http://<name>.localhost`
  instead of a port number; the port it listens on is configurable.
- **AI agent integration** — one deployment skill shared by Claude Code, Codex and Cursor, which
  verifies the service is reachable before reporting success, and an MCP server (`qooq mcp`) that
  exposes deploying, logs, build output and secrets as agent tools.

## Requirements

- macOS or Linux (amd64/arm64); on Windows, use WSL2 — and run your AI agent inside WSL2 too, since the deployment skill's scripts are bash
- Docker (Docker Desktop, OrbStack, Colima or Docker Engine), with `docker ps` responding

## Quick start

### Install

```bash
curl -fsSL https://raw.githubusercontent.com/qooqapp/qooq/main/install.sh | sh
```

Without write access to `/usr/local/bin`, install into your home directory instead:

```bash
curl -fsSL https://raw.githubusercontent.com/qooqapp/qooq/main/install.sh | PREFIX=~/.local/bin sh
```

### Start

```bash
qooq up                    # runs in the background; state is kept in ~/.qooq
qooq service install       # optional: start automatically at login
```

### Deploy

**With an AI agent** — install the skill, open a new session in the project directory and ask
it to deploy.

```bash
qooq skill install qooq-deploy                 # Claude Code
qooq skill install qooq-deploy --agent codex   # Codex
qooq skill install qooq-deploy --agent cursor  # Cursor
```

Claude Code can also install it as a plugin, which installs qooq as well if it is missing:
`/plugin marketplace add qooqapp/qooq`, then `/plugin install qooq-deploy@qooq`.

**MCP tools** — give the agent qooq as tools (deploy a directory, list services, read logs and
build output, store secrets). The Claude Code plugin includes them; otherwise register once:

```bash
qooq mcp install                  # Claude Code
qooq mcp install --agent cursor   # Cursor
qooq mcp install --agent codex    # Codex
```

**With the CLI** — deploy the current directory, no Git required.

```bash
qooq deploy .
# deployed: http://my-app.localhost, http://my-app.192.168.1.20.nip.io
```

The first address works on this machine; the second reaches it from other devices on the same
network. Running the command again updates the deployment; unchanged content is not rebuilt.

### Configuration

```bash
qooq config                          # show the current settings
qooq config set ingress.port 8080    # port for <name>.localhost (default 80; 0 = use IP:port instead)
qooq config set access local         # local: this machine only; lan (default): also the local network
```

Settings are kept in `~/.qooq/config.yaml` and apply the next time qooq starts
(`qooq down && qooq up`).

### Troubleshooting

```bash
qooq watch build-log <name>    # build output
qooq watch logs <name> -f      # application output
```

### Managing services

| Command | Description |
|---|---|
| `qooq watch ls` | List services and the result of their last deploy |
| `qooq watch stop <name>` / `start <name>` | Stop / start a service |
| `qooq watch rm <name>` | Remove a service and its data, including databases |
| `qooq down` | Stop qooq (deployed services keep running) |

## Architecture

```
source ──► qooq-watcher ──► qooqd ──► containers / native processes
           detect, build,    lifecycle, self-healing
           verify
```

| Component | Responsibility |
|---|---|
| **qooqd** | Single-host orchestration: lifecycle, restart policies, health checks, resource limits, ingress |
| **qooq-watcher** | Deployment pipeline: project detection, image builds, deploy and verification, Git watching |
| **qooq** ([qooq-cli](https://github.com/qooqapp/qooq-cli)) | Command-line interface and AI agent skill |

All components are static Go binaries with no CGO dependencies. Both daemons listen on
`127.0.0.1` only by default. Deploying a project runs its code on your machine, so deploy only
sources you trust.

## Documentation

Every document is available in English and Traditional Chinese.

| Document | Contents |
|---|---|
| [WALKTHROUGH](docs/WALKTHROUGH.md) | Getting started, with the expected result of each step |
| [USAGE](docs/USAGE.md) | Operations guide: deploying, GitOps, stacks, backups, troubleshooting |
| [DEPLOY-CONFIG](docs/DEPLOY-CONFIG.md) | `qooq.yaml` field reference |
| [qooq.reference.yaml](docs/qooq.reference.yaml) | Fully annotated configuration example |
| [ARCHITECTURE](docs/ARCHITECTURE.md) | Architecture and security model |
| [examples/](examples/) | Thirteen ready-to-run example projects, including four with no configuration at all (Next.js, Vite + React, Flask, FastAPI with uv) and one that deploys two projects together (`todo-stack`) |

## Building from source

```bash
make build     # bin/qooqd, bin/qooq-watcher
make test      # unit tests (-race)
make check     # static analysis and dependency-boundary checks
make smoke     # end-to-end tests (requires Docker)
```

## License

[Apache License 2.0](LICENSE)
