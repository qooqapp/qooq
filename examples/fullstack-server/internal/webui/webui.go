// Package webui embeds the built Vite frontend (web/) so the Go binary
// serves it directly, with no separate static file host. `docker build`
// (via the qooq go-react build.template) runs `npm run build` in web/ and
// copies the resulting dist/ here before compiling the Go binary.
package webui

import "embed"

//go:embed all:dist
var FS embed.FS
