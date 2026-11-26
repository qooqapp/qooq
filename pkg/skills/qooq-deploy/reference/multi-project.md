# Several projects, and reaching another component

Read this only when the scope is more than one project, or this project has to call
another one that is already deployed (see "First settle the scope" in SKILL.md).

## Reaching another component

A standalone project can address another deployed component without any of the stack
machinery. Put both on the same network and reference the other by name:

```yaml
network: todo          # the same line in each project's qooq.yaml

run:
  env:
    API_URL:        "http://${component.api.address}"    # this container → api
    PUBLIC_API_URL: "${component.api.endpoint}"          # the browser  → api
```

Never write an IP or a port number here. `${component.<name>.<field>}` is filled in at
deploy time from what qooqd reports, so it survives the other component being rebuilt, and
it is the only way to reference an `auto:` port at all.

**Five fields, split along the one question you have to answer: who is doing the calling?**

| Field | Value | Who uses it |
|---|---|---|
| `host` | `api` | another container |
| `port` | `8080` | another container |
| `address` | `api:8080` | another container |
| `host_port` | `19000` | the browser, or anything off this machine |
| `endpoint` | `http://192.168.1.50:19000` | the browser, or anything off this machine |

**qooq will not guess which one you mean, and neither should you.** Both are correct
addresses for the same component and they are never interchangeable. Decide it by reading
the code: a fetch that runs in the browser (or in SSR code that also runs in the browser)
needs `endpoint`; a server-to-server call needs `address`. Getting it wrong is silent —
the container runs, the health probe passes, and only the browser breaks.

**Inside a container, `127.0.0.1` is that container** — this is where SSR projects get stuck
most often. Without a shared network, a container reaches a service on the host through
`host.docker.internal` (Docker Desktop).

Limits worth knowing before you rely on it:

- **Same namespace only.** The reference grammar has no separator for a namespace, and no
  ambiguous one was invented for it. Standalone components are all in `default`, and a
  stack's projects share the stack's namespace, so the common cases work unremarked.
- **Referencing something not deployed yet is not an error.** The build reports
  `needs_input` and retries on the next sweep, so the order you deploy in does not matter.
  A misspelt name says so instead — the two are distinguished on purpose.
- Omitting the field's port uses the first entry of the other project's `run.ports`.

## Several projects deployed together

When several projects form one system, describe them in a **stack**: one
`qooq.stack.yaml`, deployed in dependency order, sharing a namespace and a network.

The stack manifest lives **outside** every project — the projects' own `qooq.yaml` files are
not edited to join one, which is what lets the same repo be deployed into two different
stacks. It has to live in a git repository of its own, so on a local machine give it the
same treatment as any project: a bare repo the watcher can poll.

```yaml
# qooq.stack.yaml
version: 1
stack: todo            # becomes the namespace, the network, and the container prefix
description: A todo API and the web page that uses it

projects:
  api:
    repo: file:///Users/me/.qooq/repos/todo-api.git
  web:
    repo: file:///Users/me/.qooq/repos/todo-web.git
    depends_on:
      - api                # wait for api to be ready before deploying web
    env:
      API_URL:        "http://${component.api.address}"
      PUBLIC_API_URL: "${component.api.endpoint}"

policy:
  max_failures: 3
```

- `repo:` is an external repository. `path:` instead names a directory **inside the stack's
  own repo** — that is the monorepo shape, and then one bare repo holds everything.
- `depends_on: [api]` waits for `api` to be **ready** (its health check passing). Write
  `{name: api, wait: started}` to only wait for the container to start.
- `env:` here is the composition layer and overrides the project's own `run.env`.
- A failed project's dependents are reported `skipped` and **their containers are left
  alone** — a broken deploy does not take down what is already serving.

Register it the same way as a project:

```bash
~/.claude/skills/qooq-deploy/scripts/gitops-setup.sh <stack-dir> <stack-name>-stack
# output includes BARE=<bare repo path>

$QOOQ_CLI stack add --name <stack-name> --repo file://<BARE> --poll 30 \
  --watcher-addr http://127.0.0.1:7778
```

Then `stack get <name>` shows each project's state, and `stack ls` shows the whole set.
Components appear in `component ls` under the stack's namespace, as `<stack>/<project>`.

**A stack's projects do not get swept individually** — the stack deploys them in order, so
`watch trigger` on one of them pokes the stack instead. This is also why an existing
standalone target has to be removed before its project joins a stack: nothing else prevents
the same app being deployed twice, once by each owner.

