package watcher

import (
	"strings"
	"testing"
)

// The go template must be able to build a project that needs cgo and a config
// file next to the binary — the shape of a real weedbox/gorm-sqlite service.
func TestGoTemplateCgoAndExtraCopy(t *testing.T) {
	out, err := renderDockerfile("go", map[string]string{
		"go_version": "1.26",
		"cgo":        "1",
		"build_deps": "gcc musl-dev",
		"extra_copy": "config.toml",
	}, "")
	if err != nil {
		t.Fatalf("renderDockerfile: %v", err)
	}
	for _, want := range []string{
		"FROM golang:1.26-alpine",
		"RUN apk add --no-cache gcc musl-dev",
		"CGO_ENABLED=1",
		"COPY config.toml /app/",
		`ENTRYPOINT ["/app/app"]`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered Dockerfile missing %q:\n%s", want, out)
		}
	}
}

// Defaults must keep producing what they always did.
func TestGoTemplateDefaultsStayPureGo(t *testing.T) {
	out, err := renderDockerfile("go", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "CGO_ENABLED=0") {
		t.Errorf("default build is no longer pure Go:\n%s", out)
	}
	if strings.Contains(out, "apk add") {
		t.Errorf("default build installs packages it did not before:\n%s", out)
	}
}

// A Nuxt-shaped project: pnpm, a build step, and a start command pointing at
// the build output.
func TestNodeTemplateBuildStage(t *testing.T) {
	out, err := renderDockerfile("node", map[string]string{
		"node_version": "22",
		"corepack":     "true",
		"install_cmd":  "pnpm install --frozen-lockfile",
		"build_cmd":    "pnpm run build",
		"start_cmd":    "node .output/server/index.mjs",
	}, "")
	if err != nil {
		t.Fatalf("renderDockerfile: %v", err)
	}
	for _, want := range []string{
		"AS build",
		"RUN corepack enable",
		"COPY package*.json pnpm-lock.yaml* yarn.lock* ./",
		"RUN pnpm install --frozen-lockfile",
		"RUN pnpm run build",
		"CMD node .output/server/index.mjs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered Dockerfile missing %q:\n%s", want, out)
		}
	}
}

// Without build_cmd the node template stays the single-stage image it was.
func TestNodeTemplateDefaultsStaySingleStage(t *testing.T) {
	out, err := renderDockerfile("node", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "AS build") {
		t.Errorf("default node template gained a build stage:\n%s", out)
	}
	if strings.Contains(out, "corepack") {
		t.Errorf("default node template enables corepack:\n%s", out)
	}
	if !strings.Contains(out, "CMD node index.js") {
		t.Errorf("default start command changed:\n%s", out)
	}
}

// Every registered template has to render with nothing but its own defaults.
// A template whose defaults leave a {{.var}} unfilled, or that is registered
// without a file, only fails when someone's build reaches it — by which point
// the error surfaces as a Docker syntax error in a file they never wrote.
func TestEveryTemplateRendersFromDefaults(t *testing.T) {
	if len(templateDefaults) == 0 {
		t.Fatal("no templates registered")
	}
	for name := range templateDefaults {
		t.Run(name, func(t *testing.T) {
			out, err := renderDockerfile(name, nil, "")
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Contains(out, "<no value>") {
				t.Errorf("a variable had no default:\n%s", out)
			}
			if !strings.HasPrefix(strings.TrimSpace(out), "FROM ") {
				t.Errorf("does not start with FROM:\n%s", out)
			}
			// A template that never states a port produces a container qooq
			// cannot route to, and the omission is invisible until deploy.
			if name != "static-nginx" && !strings.Contains(out, "EXPOSE ") {
				t.Errorf("no EXPOSE:\n%s", out)
			}
		})
	}
}

// The templates that exist as files and the templates that have defaults must
// be the same set. Registering one without the other is the failure this
// catches: defaults with no file render nothing, a file with no defaults is
// unreachable by name.
func TestTemplateFilesAndDefaultsAgree(t *testing.T) {
	entries, err := templateFS.ReadDir("templates")
	if err != nil {
		t.Fatalf("read templates: %v", err)
	}
	onDisk := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			onDisk[e.Name()] = true
		}
	}
	for name := range templateDefaults {
		if !onDisk[name] {
			t.Errorf("template %q has defaults but no templates/%s/Dockerfile.tmpl", name, name)
		}
	}
	for name := range onDisk {
		if _, ok := templateDefaults[name]; !ok {
			t.Errorf("templates/%s exists but is not in templateDefaults, so nothing can select it", name)
		}
	}
}
