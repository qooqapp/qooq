# todo-stack: two projects deployed together

English | [繁體中文](README.zh-TW.md)

A todo API and the web page that uses it, kept in one repo and deployed as a **stack**:
`web` waits until `api` is up, and the two find each other by name.

```
todo-stack/
├── qooq.stack.yaml   # which projects form the stack and who waits for whom
├── api/              # Go, standard library only — GET/POST /api/todos on :8080
│   └── qooq.yaml
└── web/              # Node, no dependencies — server-rendered page on :3000
    └── qooq.yaml
```

## The two addresses

`web` talks to `api` from two places, and each needs a different address:

| Who calls | Variable | Value | Set in |
|---|---|---|---|
| web's server, rendering the list | `API_INTERNAL` | `http://api:8080` — container name, container port | `qooq.stack.yaml` (`${project.api.host}`) |
| the browser, adding an item | `API_PUBLIC` | `http://192.168.1.50:19000` — this machine's IP, published port | `web/qooq.yaml` (`${component.api.endpoint}`) |

The browser runs outside every container, so it cannot use the container name. Neither address
is written by hand: qooq fills both in when it deploys.

## How to run

```bash
# 0. qooq running (qooq up)

# 1. Turn the example into a git repo with a bare remote for the watcher
cp -R examples/todo-stack ~/todo-stack && cd ~/todo-stack
git init -q -b main && git add -A && git commit -qm init
git clone -q --bare . ~/todo-stack.git

# 2. Register the stack
qooq stack add --name todo --repo file://$HOME/todo-stack.git

# 3. Watch it: api deploys first, then web (about a minute the first time)
qooq stack get todo        # both projects: success
qooq component ls             # todo/api and todo/web, Running

# 4. Open web's address from `qooq component get todo/web`: the list shows
#    "Deploy the todo stack"; add an item and it appears after the reload.
```

Clean up:

```bash
qooq stack rm todo --prune     # also removes the api and web components
rm -rf ~/todo-stack ~/todo-stack.git
```

The todos live in memory, so they reset whenever `api` restarts.
