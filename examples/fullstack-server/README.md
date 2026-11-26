# fullstack-server: React (Vite) + Go (gin) single-binary example (build.template: go-react)

English | [繁體中文](README.zh-TW.md)

The frontend is a minimal Vite + React project (`web/`, one button that calls `/api/hello`); the backend is Go + gin
(`cmd/server/main.go`), which uses `go:embed` to bundle the frontend build output into the same binary
(`//go:embed all:dist` in `internal/webui/webui.go`). **There is deliberately no Dockerfile.**
`qooq.yaml` uses `build.template: go-react`: after cloning, the watcher runs `npm install`
+ `npm run build` in `web/`, copies `dist/` into `internal/webui/dist/`, and then compiles a single Go binary.

```
fullstack-server/
├── qooq.yaml              # build.template: go-react (all template variables at defaults)
├── web/                   # Vite + React frontend (npm install never run, no lockfile)
│   ├── package.json
│   ├── vite.config.js
│   ├── index.html
│   └── src/{main.jsx,App.jsx}
├── internal/webui/
│   ├── webui.go           # //go:embed all:dist
│   └── dist/index.html    # placeholder; docker build overwrites it with the real Vite output
├── cmd/server/main.go     # gin: GET /api/hello + all other paths serve the embedded dist
├── go.mod
└── go.sum
```

(A standalone go module, `module example.com/qooq-examples/fullstack-server`; `internal/webui/dist/`
holds a one-line placeholder `index.html` only so that `go build`/`go vet` compile even when the
**frontend build has not been run**. During `docker build` the template overwrites it with the real Vite output.)

## How to run (local demo)

```bash
cp -r examples/fullstack-server ~/fullstack-server && cd ~/fullstack-server
git init -q && git add -A && git commit -qm init
git clone --bare . ~/fullstack-server.git && git remote add origin ~/fullstack-server.git

qooq watch add --name fullstack-server --repo file://$HOME/fullstack-server.git \
  --component fullstack-server --poll 30

# Verify: within 30 seconds (the first build includes npm install + go mod download, needs network access, and takes longer)
qooq watch get fullstack-server               # last_build_status: success
open $(qooq component get fullstack-server | awk '/endpoints:/{print $2}')   # expected: the page loads, and clicking the button shows
                                                # "hello from fullstack-server"
curl $(qooq component get fullstack-server | awk '/endpoints:/{print $2}')/api/hello   # expected: {"message":"hello from fullstack-server"}
```

**docker build needs network access**: the go-react template connects to the npm registry (frontend dependencies) and the Go module proxy
(gin). Offline environments need to set up their own registry mirrors to build.

## qooq.yaml highlights

- `build.template: go-react`: this example's directory layout (`web/`, `internal/webui/dist`,
  `cmd/server`) matches the template variable defaults exactly, so `template_vars` is omitted entirely.
- `embed_dest` (a template variable, default `internal/webui/dist`) **must match the
  `//go:embed all:dist` path in `webui.go`**. The two are coupled; change one and you must change the other.
- `run.ports` is `auto:8080`: the host port is allocated by the daemon (a redeploy gets the same one back), `8080` is the port gin listens on inside the container, and `health.port` targets it too.
