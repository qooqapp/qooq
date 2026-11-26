package manifest

import (
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
)

// templateDefaults holds each built-in template's default variable values.
// build.template_vars in the manifest overrides individual keys; anything
// left unset here falls back to these defaults.
var templateDefaults = map[string]map[string]string{
	"static-nginx": {
		"nginx_tag":  "alpine",
		"static_dir": ".",
		// build_cmd empty serves static_dir as it is in the repo, which is what
		// this template always did. Set it (e.g. "npm run build") and a node
		// stage builds first, and static_dir is read from that stage's output —
		// a front end whose dist/ is gitignored, i.e. nearly every one.
		"build_cmd":    "",
		"node_version": "20",
		// The build needs devDependencies (vite, typescript), so unlike the
		// node template this does not omit them.
		"install_cmd": "npm install",
		"corepack":    "",
		// spa sends any path without a file to index.html, so a client-side
		// router's /settings survives a reload instead of answering 404.
		"spa": "",
	},
	"go": {
		"go_version": "1.25",
		"build_path": ".",
		"base_image": "alpine:3.20",
		"port":       "8080",
		// cgo defaults off (the pure-Go build that produces a static binary
		// runnable on a bare base image). Set it to "1" — with build_deps
		// "gcc musl-dev" — for a project pulling in a cgo dependency such as
		// mattn/go-sqlite3, which otherwise builds fine and then fails at
		// runtime with "requires cgo to work".
		"cgo":        "0",
		"build_deps": "", // apk packages for the build stage
		// extra_copy names additional repo paths (space-separated) to place
		// beside the binary in the final image — a config file the app reads
		// from its working directory, say.
		"extra_copy":   "",
		"runtime_deps": "", // apk packages for the final image
		"workdir":      "/app",
	},
	"node": {
		"node_version": "20",
		"install_cmd":  "npm install --omit=dev",
		"port":         "3000",
		"start_cmd":    "node index.js",
		// build_cmd empty keeps the single-stage image this template has
		// always produced. Setting it (e.g. "npm run build") switches to
		// build-then-run, which is what a framework with a compile step —
		// Nuxt, Next, Vite — needs.
		"build_cmd": "",
		// corepack enables the pnpm/yarn shims bundled with node. The
		// lockfile COPY already globs all three package managers'.
		"corepack": "",
		"workdir":  "/app",
	},
	"python": {
		"python_version": "3.12",
		"port":           "8000",
		"start_cmd":      "uvicorn main:app --host 0.0.0.0 --port 8000",
	},
	"rust": {
		"rust_version": "1.83",
		"debian_tag":   "bookworm",
		"port":         "8080",
		// bin_name is only needed for a workspace or a crate producing several
		// binaries; the template finds the single one otherwise.
		"bin_name":     "",
		"build_deps":   "",
		"runtime_deps": "",
	},
	"php": {
		"php_version":      "8.3",
		"composer_version": "2",
		// docroot is where the front controller lives. Laravel, Symfony and
		// Slim all use public/; serving the repo root instead would publish
		// .env and vendor/.
		"docroot":        "public",
		"php_extensions": "pdo_mysql zip intl opcache",
		"composer_flags": "--no-dev --optimize-autoloader --no-interaction --no-progress",
	},
	"ruby": {
		"ruby_version": "3.3",
		"port":         "3000",
		// rackup serves any config.ru, which Rails has too — one command that
		// covers Rails, Sinatra and bare Rack rather than three templates.
		"start_cmd":      "bundle exec rackup --host 0.0.0.0 --port 3000",
		"bundle_without": "development test",
		"build_cmd":      "",
		"runtime_deps":   "",
	},
	"java": {
		// The maven image carries its own JDK; jre_version is only the runtime
		// stage, which is why they are separate and can differ.
		"maven_tag":   "3.9-eclipse-temurin-21",
		"jre_version": "21",
		"port":        "8080",
	},
	"dotnet": {
		"dotnet_version": "8.0",
		"port":           "8080",
		// project empty means "whatever is in this directory", which is right
		// for a single-project repo; a solution with several needs the path.
		"project": "",
	},
	"bun": {
		"bun_version": "1",
		"port":        "3000",
		"start_cmd":   "bun run index.ts",
		"build_cmd":   "",
	},
	"go-react": {
		"node_version":       "20",
		"web_dir":            "web",
		"install_cmd":        "npm install",
		"frontend_build_cmd": "npm run build",
		"frontend_dist_dir":  "dist",
		"go_version":         "1.25",
		"embed_dest":         "internal/webui/dist",
		"go_build_path":      "./cmd/server",
		"base_image":         "alpine:3.20",
		"port":               "8080",
	},
}

// BuiltinTemplateDefaults returns a copy of every built-in template's default
// variables, keyed by template name. The watcher renders from it; the
// Dockerfile templates themselves stay embedded in the watcher.
func BuiltinTemplateDefaults() map[string]map[string]string {
	out := make(map[string]map[string]string, len(templateDefaults))
	for name, vars := range templateDefaults {
		out[name] = maps.Clone(vars)
	}
	return out
}

// CheckTemplate reports an unknown build.template, or a template_vars key
// the template does not have. A misspelled variable must fail, not vanish:
// the template falls back to its default and builds something that looks
// deployed and is not what was asked for (static_dir written as dir serves
// nginx's welcome page). Every variable a template reads has a default, so
// the defaults are the complete list of valid names.
func CheckTemplate(name string, vars map[string]string) error {
	defaults, ok := templateDefaults[name]
	if !ok {
		return fmt.Errorf("unknown build.template %q, available templates: %s", name, strings.Join(sortedKeys(templateDefaults), ", "))
	}
	var unknown []string
	for k := range vars {
		if _, ok := defaults[k]; !ok {
			unknown = append(unknown, strconv.Quote(k))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("build.template_vars: template %q has no variable %s; its variables are: %s",
			name, strings.Join(unknown, ", "), strings.Join(sortedKeys(defaults), ", "))
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
