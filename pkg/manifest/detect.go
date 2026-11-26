package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Detection exists for repos that have no qooq.yaml — most often a repo you do
// not own and cannot add one to. Rather than refusing to deploy it, the
// watcher reads what is on disk and writes the Manifest it would have wanted.
//
// The rules here are deliberately file-shaped: which files exist, what a
// package.json declares, what a Dockerfile EXPOSEs. They never read source
// code to infer behavior — that is what the qooq-deploy skill does with an LLM
// reading the entry point, and a hard-coded imitation of it would guess wrong
// silently. What makes guessing acceptable at all is that the result is
// persisted and editable: a wrong guess is a draft to fix, not a mystery.
//
// Every decision is recorded in Detection.Notes so the person reading the
// generated Manifest can see why it says what it says.

// Detection is a generated Manifest plus the reasoning behind it.
type Detection struct {
	Manifest *Manifest
	// Kind is the shape that was recognized ("dockerfile", "go", "node-ssr",
	// …), for logging and for the API to show.
	Kind string
	// Notes explain each decision in the order they were made.
	Notes []string
}

func (d *Detection) note(format string, args ...any) {
	d.Notes = append(d.Notes, fmt.Sprintf(format, args...))
}

// dockerfileLocations are searched in order. A repo that keeps its Dockerfile
// out of the root is common enough that only looking in the root is how three
// real projects got misdetected.
var dockerfileLocations = []string{
	"Dockerfile",
	"docker/Dockerfile",
	"build/Dockerfile",
	".docker/Dockerfile",
}

// Detect generates a Manifest for a repo that has none. app is the
// component name, used where the Manifest wants a name.
//
// It never returns nil: a repo it cannot recognize still gets a Manifest that
// builds the root Dockerfile, because that is what the pipeline would have
// done anyway — with a note saying so.
// preferredDockerfile, when set and present, wins over everything: the
// operator already said which Dockerfile to use, and Detection second-guessing
// an explicit instruction would be worse than useless.
func Detect(dir, app, preferredDockerfile string) (*Detection, error) {
	d := &Detection{
		Manifest: &Manifest{Version: 1, App: app},
		Kind:     "unknown",
	}

	if preferredDockerfile != "" && exists(filepath.Join(dir, preferredDockerfile)) {
		d.Kind = "dockerfile"
		d.Manifest.Build.Dockerfile = preferredDockerfile
		d.note("the target names %s, using it", preferredDockerfile)
		if port, ok := readExposedPort(filepath.Join(dir, preferredDockerfile)); ok {
			d.Manifest.Run.Ports = []string{strconv.Itoa(port)}
			d.note("EXPOSE %d, declaring it as a container port", port)
		}
		return d, nil
	}

	// 1. A Dockerfile the repo already has beats anything qooq would generate:
	// it is the authors' own statement of how to build this.
	if path, ok := findDockerfile(dir); ok {
		d.Kind = "dockerfile"
		d.Manifest.Build.Dockerfile = path
		d.note("using the repo's own %s", path)
		if port, ok := readExposedPort(filepath.Join(dir, path)); ok {
			d.Manifest.Run.Ports = []string{strconv.Itoa(port)}
			// The note says what was read, not what will be done with it:
			// whether the port is published depends on who asked for the
			// render, and a note that decided for them goes stale the moment
			// the other caller uses it.
			d.note("EXPOSE %d in %s", port, path)
		} else {
			d.note("no EXPOSE found, declaring no ports (siblings can still reach it if it listens)")
		}
		return d, nil
	}

	// 2. Go, with or without a bundled frontend.
	if exists(filepath.Join(dir, "go.mod")) {
		return detectGo(dir, d)
	}

	// 3. A server written in something other than Node, which may still carry
	// a package.json for its assets. This has to come before the package.json
	// branch: a backend framework that bundles its CSS with Vite has a build
	// script and no start script, and reading that as "a front end" is how a
	// Laravel app gets deployed as an nginx serving an empty dist/ while
	// composer.json and artisan sit unread beside it. Same shape for Rails
	// with jsbundling, Django with a frontend pipeline, and Phoenix.
	if marker, ok := backendLanguage(dir); ok {
		return detectOtherBackend(dir, d, marker)
	}

	// 4. Node, split by whether it has a build step (a framework) or not.
	if exists(filepath.Join(dir, "package.json")) {
		return detectNode(dir, d)
	}

	// 5. Python without any of the strong markers above — a bare
	// requirements.txt or pyproject.toml.
	if exists(filepath.Join(dir, "requirements.txt")) || exists(filepath.Join(dir, "pyproject.toml")) {
		return detectPython(dir, d)
	}

	// 6. Static files.
	if exists(filepath.Join(dir, "index.html")) {
		d.Kind = "static"
		d.Manifest.Build.Template = "static-nginx"
		d.Manifest.Run.Ports = []string{"80"}
		d.note("index.html at the repo root, serving it with nginx")
		return d, nil
	}

	d.note("nothing recognizable found; expecting a Dockerfile at the repo root")
	return d, nil
}

// backendMarker names a server language by files only that language's projects
// have.
//
// package.json is deliberately absent from every marker: a backend framework
// that bundles its assets with Vite ships one, and treating it as the server is
// the misdetection this whole table exists to prevent. Each marker is a pair or
// a distinctive filename rather than a single common one, because the cost of a
// false positive here is high — it takes a repo away from a template that would
// have worked.
type backendMarker struct {
	// kind is what the detection reports.
	kind string
	// all of these must be present.
	all []string
	// template is the qooq template that builds it, or "" when there is none
	// yet and the repo needs its own Dockerfile.
	template string
	// port is the container port that template exposes. It has to be stated
	// here rather than read from the template's defaults, because those live in
	// internal/watcher and pkg/ cannot import internal/.
	port string
	// note explains what was recognized. It says what was found, never what
	// the caller should do with it — Render decides that.
	note string
}

// backendMarkers is ordered: the first match wins. Python is last because its
// markers are the weakest, and a Django project carrying a Gemfile is far less
// likely than a Ruby project carrying a stray .py file.
var backendMarkers = []backendMarker{
	{kind: "php-laravel", all: []string{"composer.json", "artisan"}, template: "php", port: "80",
		note: "composer.json and artisan: a Laravel application"},
	{kind: "php", all: []string{"composer.json"}, template: "php", port: "80",
		note: "composer.json: a PHP project"},
	{kind: "ruby-rack", all: []string{"Gemfile", "config.ru"}, template: "ruby", port: "3000",
		note: "Gemfile and config.ru: a Rack application (Rails, Sinatra)"},
	{kind: "ruby", all: []string{"Gemfile"}, template: "ruby", port: "3000",
		note: "Gemfile: a Ruby project"},
	{kind: "java-maven", all: []string{"pom.xml"}, template: "java", port: "8080",
		note: "pom.xml: a Maven project"},
	// Gradle has no template: the maven template's dependency-cache step and
	// its target/*.jar are both Maven-specific, and guessing at the Gradle
	// equivalents would produce a Dockerfile that looks right and does not
	// build. Naming the project type is still worth more than "unknown".
	{kind: "java-gradle", all: []string{"build.gradle"},
		note: "build.gradle: a Gradle project"},
	{kind: "java-gradle", all: []string{"build.gradle.kts"},
		note: "build.gradle.kts: a Gradle project"},
	{kind: "rust", all: []string{"Cargo.toml"}, template: "rust", port: "8080",
		note: "Cargo.toml: a Rust project"},
	{kind: "python-django", all: []string{"manage.py"}, template: "python",
		note: "manage.py: a Django project"},
}

// backendLanguage finds a non-Node server language. It also covers .NET, whose
// marker is an extension rather than a filename.
func backendLanguage(dir string) (backendMarker, bool) {
	for _, m := range backendMarkers {
		found := true
		for _, f := range m.all {
			if !exists(filepath.Join(dir, f)) {
				found = false
				break
			}
		}
		if found {
			return m, true
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "*.csproj")); len(matches) > 0 {
		return backendMarker{kind: "dotnet", template: "dotnet", port: "8080",
			note: filepath.Base(matches[0]) + ": a .NET project"}, true
	}
	return backendMarker{}, false
}

func detectOtherBackend(dir string, d *Detection, m backendMarker) (*Detection, error) {
	d.Kind = m.kind
	d.note("%s", m.note)

	if m.kind == "python-django" {
		return detectDjango(dir, d)
	}

	if m.template != "" {
		d.Manifest.Build.Template = m.template
		d.Manifest.Run.Ports = []string{m.port}
		d.note("using the %s template", m.template)
	} else {
		// Saying "nothing recognizable found" about a repo that was in fact
		// recognized is worse than saying nothing: it invites the reader to go
		// looking for what they did wrong. Name the language and the gap.
		d.note("qooq has no build template for this yet — add a Dockerfile at the repo root and it will be used")
	}
	if exists(filepath.Join(dir, "package.json")) {
		// Worth stating outright, because the previous behaviour was to act on
		// exactly this file and deploy the wrong thing.
		d.note("its package.json is being read as an asset pipeline, not as the server")
	}
	return d, nil
}

// detectDjango picks a start command instead of leaving the python template's
// uvicorn default, which names a module Django projects do not have and so
// exits immediately with a traceback that never mentions qooq.
func detectDjango(dir string, d *Detection) (*Detection, error) {
	d.Manifest.Build.Template = "python"
	d.Manifest.Run.Ports = []string{"8000"}
	vars := map[string]string{}

	wsgi, ok := djangoWSGIModule(dir)
	switch {
	case ok && requirementsMention(dir, "gunicorn"):
		vars["start_cmd"] = fmt.Sprintf("gunicorn %s:application --bind 0.0.0.0:8000", wsgi)
		d.note("gunicorn is in requirements and %s exists, serving with it", strings.ReplaceAll(wsgi, ".", "/")+".py")
	case ok:
		// runserver is Django's development server and it says so on startup,
		// but it needs no dependency the repo does not already have. A command
		// that runs and warns beats one that cannot start.
		vars["start_cmd"] = "python manage.py runserver 0.0.0.0:8000"
		d.note("no gunicorn in requirements, falling back to manage.py runserver — add gunicorn for production")
	default:
		vars["start_cmd"] = "python manage.py runserver 0.0.0.0:8000"
		d.note("no wsgi.py found, starting with manage.py runserver")
	}
	d.Manifest.Build.TemplateVars = vars
	return d, nil
}

// djangoWSGIModule finds the settings package by the wsgi.py inside it, which
// is where startproject puts it.
func djangoWSGIModule(dir string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if exists(filepath.Join(dir, e.Name(), "wsgi.py")) {
			return e.Name() + ".wsgi", true
		}
	}
	return "", false
}

func requirementsMention(dir, pkg string) bool {
	for _, f := range []string{"requirements.txt", "pyproject.toml"} {
		data, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		if strings.Contains(strings.ToLower(string(data)), pkg) {
			return true
		}
	}
	return false
}

func detectPython(dir string, d *Detection) (*Detection, error) {
	d.Kind = "python"
	d.Manifest.Build.Template = "python"
	d.Manifest.Run.Ports = []string{"8000"}
	if exists(filepath.Join(dir, "requirements.txt")) {
		d.note("requirements.txt found, using the python template")
	} else {
		d.note("pyproject.toml found (no requirements.txt), using the python template — dependencies are installed from [project].dependencies with uv")
	}

	// The start command comes from which framework the dependency files name
	// and which entry file exists — both are file shapes, not source. The
	// template's own default (uvicorn main:app) fits one layout of one
	// framework; a Flask app started that way exits at once, with a traceback
	// that never mentions qooq.
	module, file := pythonEntryModule(dir)
	vars := map[string]string{}
	switch {
	case requirementsMention(dir, "fastapi"):
		if module == "" {
			module = "main"
			d.note("FastAPI, but no main.py/app.py/app/main.py/src/main.py — guessing main:app; check it if the container exits immediately")
		} else {
			d.note("FastAPI, serving %s with uvicorn (the app object is assumed to be named app)", file)
		}
		vars["start_cmd"] = fmt.Sprintf("uvicorn %s:app --host 0.0.0.0 --port 8000", module)
	case requirementsMention(dir, "flask"):
		if module == "" {
			module = "app"
			d.note("Flask, but no app.py/main.py/app/main.py/src/main.py — guessing app:app; check it if the container exits immediately")
		}
		if requirementsMention(dir, "gunicorn") {
			vars["start_cmd"] = fmt.Sprintf("gunicorn %s:app --bind 0.0.0.0:8000", module)
			d.note("Flask with gunicorn, serving %s:app", module)
		} else {
			// Flask's development server, like Django's runserver fallback: it
			// warns on startup, but needs nothing the repo does not have.
			vars["start_cmd"] = fmt.Sprintf("flask --app %s run --host 0.0.0.0 --port 8000", module)
			d.note("Flask without gunicorn, starting %s with flask run — add gunicorn for production", module)
		}
	default:
		d.note("the start command is a guess (uvicorn main:app) — check it if the container exits immediately")
	}
	if len(vars) > 0 {
		d.Manifest.Build.TemplateVars = vars
	}
	return d, nil
}

// pythonEntryModule finds the file a Python web app is usually started from,
// as an import path ("app.main") and as the file it came from.
func pythonEntryModule(dir string) (module, file string) {
	for _, c := range []struct{ file, module string }{
		{"main.py", "main"},
		{"app.py", "app"},
		{"app/main.py", "app.main"},
		{"src/main.py", "src.main"},
		{"server.py", "server"},
	} {
		if exists(filepath.Join(dir, filepath.FromSlash(c.file))) {
			return c.module, c.file
		}
	}
	return "", ""
}

// workspaceRoot reports whether this directory is the root of a monorepo, and
// describes what it holds. Both of the common declarations count: pnpm keeps
// its list in its own file, npm and yarn keep it in package.json.
func workspaceRoot(dir string, pkg packageJSON) (string, bool) {
	declared := pkg.hasWorkspaces() || exists(filepath.Join(dir, "pnpm-workspace.yaml"))
	if !declared {
		return "", false
	}
	// Report the directories that actually hold a package, not the globs: a
	// reader needs to pick one, and "apps/*" is not a choice.
	var members []string
	for _, parent := range []string{"apps", "packages", "services"} {
		entries, err := os.ReadDir(filepath.Join(dir, parent))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() && exists(filepath.Join(dir, parent, e.Name(), "package.json")) {
				members = append(members, parent+"/"+e.Name())
			}
		}
	}
	if len(members) == 0 {
		return "no sub-package found under apps/, packages/ or services/", true
	}
	sort.Strings(members)
	// A note long enough to wrap is a note nobody finishes. Naming a few is
	// enough to show where to look; the count says how much else is there.
	const shown = 5
	if len(members) > shown {
		return fmt.Sprintf("%s and %d more", strings.Join(members[:shown], ", "), len(members)-shown), true
	}
	return strings.Join(members, ", "), true
}

// detectBun builds on the bun image and keeps the repo's own start script,
// which is the one thing a bun project reliably has.
func detectBun(dir string, d *Detection, pkg packageJSON) (*Detection, error) {
	d.Kind = "bun"
	d.Manifest.Build.Template = "bun"
	d.Manifest.Run.Ports = []string{"3000"}
	vars := map[string]string{}

	switch {
	case pkg.Scripts["start"] != "":
		vars["start_cmd"] = "bun run start"
		d.note("bun lockfile found, using the bun image and its start script")
	default:
		entry := "index.ts"
		for _, candidate := range []string{"index.ts", "index.js", "server.ts", "server.js", "src/index.ts"} {
			if exists(filepath.Join(dir, candidate)) {
				entry = candidate
				break
			}
		}
		vars["start_cmd"] = "bun run " + entry
		d.note("bun lockfile found, using the bun image and running %s", entry)
	}
	if _, ok := pkg.Scripts["build"]; ok {
		vars["build_cmd"] = "bun run build"
		d.note("package.json has a build script, running it in the image")
	}
	d.Manifest.Build.TemplateVars = vars
	return d, nil
}

func detectGo(dir string, d *Detection) (*Detection, error) {
	d.Manifest.Build.Template = "go"
	d.Manifest.Run.Ports = []string{"8080"}
	vars := map[string]string{}

	if webDir, ok := findFrontendDir(dir); ok {
		d.Kind = "go-react"
		d.Manifest.Build.Template = "go-react"
		vars["web_dir"] = webDir
		d.note("go.mod plus a frontend in %s/, using the go-react template", webDir)
	} else {
		d.Kind = "go"
		d.note("go.mod at the repo root, using the go template")
	}

	if version, ok := readGoVersion(filepath.Join(dir, "go.mod")); ok {
		vars["go_version"] = version
		d.note("go.mod requires go %s, pinning the build image to it", version)
	}

	// A cgo dependency compiles fine with CGO_ENABLED=0 and then fails at
	// runtime ("requires cgo to work"), which is a genuinely confusing way to
	// find out. Cheap to detect, expensive to debug.
	if dep, ok := findCgoDependency(dir); ok {
		if d.Kind == "go-react" {
			// That template has no cgo switch; setting the variables anyway
			// is now a build error, and before was a silent no-op.
			d.note("%s needs cgo, which the go-react template cannot enable: the build will succeed and the app will fail at runtime; write a Dockerfile with CGO_ENABLED=1 instead", dep)
		} else {
			vars["cgo"] = "1"
			vars["build_deps"] = "gcc musl-dev"
			d.note("%s needs cgo, enabling it and installing gcc/musl-dev", dep)
		}
	}

	if len(vars) > 0 {
		d.Manifest.Build.TemplateVars = vars
	}
	return d, nil
}

// packageJSON is the subset of package.json Detection reads.
type packageJSON struct {
	Scripts        map[string]string `json:"scripts"`
	PackageManager string            `json:"packageManager"`
	// Workspaces is raw because npm and yarn disagree on its shape: an array
	// of globs in one, an object with a "packages" key in the other. Nothing
	// here needs to read the globs — only whether the field is there at all.
	Workspaces json.RawMessage `json:"workspaces"`
	// Only read to tell which tool builds a front end (Create React App writes
	// build/, the rest dist/).
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// has reports a package in dependencies or devDependencies.
func (p packageJSON) has(name string) bool {
	_, a := p.Dependencies[name]
	_, b := p.DevDependencies[name]
	return a || b
}

// hasWorkspaces reports a declared workspace list, ignoring the empty forms
// that some generators leave behind.
func (p packageJSON) hasWorkspaces() bool {
	switch strings.TrimSpace(string(p.Workspaces)) {
	case "", "null", "[]", "{}":
		return false
	}
	return true
}

func detectNode(dir string, d *Detection) (*Detection, error) {
	d.Manifest.Build.Template = "node"
	vars := map[string]string{}

	var pkg packageJSON
	if data, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		_ = json.Unmarshal(data, &pkg) // a malformed package.json just means fewer hints
	}

	// bun is not a package manager choice inside the node image, it is a
	// different runtime: `npm start` on a bun project runs a start script that
	// says "bun run …", and the node image has no bun to run it. The whole
	// image changes, so this decision comes before the package-manager one.
	if exists(filepath.Join(dir, "bun.lockb")) || exists(filepath.Join(dir, "bun.lock")) {
		return detectBun(dir, d, pkg)
	}

	// The package manager decides both the install command and whether
	// corepack has to be turned on.
	switch manager := nodePackageManager(dir, pkg); manager {
	case "pnpm":
		vars["corepack"] = "true"
		vars["install_cmd"] = "pnpm install --frozen-lockfile"
		d.note("pnpm detected, enabling corepack")
	case "yarn":
		vars["corepack"] = "true"
		vars["install_cmd"] = "yarn install --frozen-lockfile"
		d.note("yarn detected, enabling corepack")
	default:
		if exists(filepath.Join(dir, "package-lock.json")) {
			vars["install_cmd"] = "npm ci"
			d.note("package-lock.json found, installing with npm ci")
		}
	}

	if framework, start, ok := detectNodeFramework(dir); ok {
		// An SSR framework has a build step and its output is not the source
		// tree — the single-stage default would deploy unbuilt sources.
		d.Kind = "node-" + framework
		vars["build_cmd"] = buildCmdFor(pkg, vars["install_cmd"])
		vars["start_cmd"] = start
		d.Manifest.Run.Ports = []string{"3000"}
		d.note("%s detected: building in the image and starting %q", framework, start)
		// A workspace root can still be the app — someone factoring one shared
		// package out does not stop their Next.js app being at the root — so
		// this adds a note rather than overriding an answer that works. The
		// reverse (a framework repo whose apps live in apps/) is where the note
		// earns its place.
		if members, ok := workspaceRoot(dir, pkg); ok {
			d.note("this is also a workspace root (%s) — if the deployable app is one of those, point context_subdir at it instead", members)
		}
		d.Manifest.Build.TemplateVars = vars
		return d, nil
	}

	if _, hasStart := pkg.Scripts["start"]; hasStart {
		d.Kind = "node"
		vars["start_cmd"] = "npm start"
		d.Manifest.Run.Ports = []string{"3000"}
		d.note("package.json has a start script, running it")
		d.Manifest.Build.TemplateVars = vars
		return d, nil
	}

	for _, entry := range []string{"server.js", "index.js", "app.js"} {
		if exists(filepath.Join(dir, entry)) {
			d.Kind = "node"
			vars["start_cmd"] = "node " + entry
			d.Manifest.Run.Ports = []string{"3000"}
			d.note("no start script, running %s directly", entry)
			d.Manifest.Build.TemplateVars = vars
			return d, nil
		}
	}

	// A workspace root is not an app. It has no start script because nothing
	// starts *here* — the apps are one level down — and calling it a front end
	// produces an nginx serving a dist/ that will never exist. Naming the
	// subdirectories is the only useful thing to say.
	if members, ok := workspaceRoot(dir, pkg); ok {
		d.Kind = "monorepo"
		d.Manifest.Build.Template = ""
		d.note("this is a workspace root, not an app: %s", members)
		d.note("point the target at the subdirectory holding the app (context_subdir), or give each one its own project in a stack")
		return d, nil
	}

	// A package.json with only a build script is a front end: build it in a
	// node stage and serve the output with nginx. The output directory is
	// the guess most likely to be wrong, so it is called out.
	d.Kind = "static-frontend"
	d.Manifest.Build.Template = "static-nginx"
	d.Manifest.Run.Ports = []string{"80"}
	d.note("no start script and no server entry point: treating this as a static front end")
	outDir := "dist"
	if pkg.has("react-scripts") {
		outDir = "build" // Create React App
	}
	vars["static_dir"] = outDir
	if _, hasBuild := pkg.Scripts["build"]; hasBuild {
		vars["build_cmd"] = buildCmdFor(pkg, vars["install_cmd"])
		// The node template's install default omits devDependencies; the
		// build needs them (vite, typescript), so npm gets a plain install
		// unless a lockfile already chose npm ci.
		vars["spa"] = "true"
		d.note("building with %q in the image and serving %s/ with nginx; unknown paths fall back to index.html for client-side routing", vars["build_cmd"], outDir)
		d.note("%s/ is a guess — change static_dir if the build writes elsewhere", outDir)
	} else {
		d.note("no build script: serving %s/ as it is in the repo — change static_dir if that is wrong", outDir)
	}
	d.Manifest.Build.TemplateVars = vars
	return d, nil
}

// buildCmdFor picks the build command matching the install command's package
// manager, so a pnpm project does not end up running npm.
func buildCmdFor(pkg packageJSON, installCmd string) string {
	switch {
	case strings.HasPrefix(installCmd, "pnpm"):
		return "pnpm run build"
	case strings.HasPrefix(installCmd, "yarn"):
		return "yarn build"
	default:
		return "npm run build"
	}
}

// detectNodeFramework recognizes the SSR frameworks whose build output is not
// the source tree, returning the command that starts the built app.
func detectNodeFramework(dir string) (name, startCmd string, ok bool) {
	frameworks := []struct {
		configPrefix string
		name         string
		start        string
	}{
		{"nuxt.config", "nuxt", "node .output/server/index.mjs"},
		{"next.config", "next", "npx next start"},
		{"svelte.config", "sveltekit", "node build"},
		{"astro.config", "astro", "node ./dist/server/entry.mjs"},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", false
	}
	for _, e := range entries {
		for _, f := range frameworks {
			if strings.HasPrefix(e.Name(), f.configPrefix) {
				return f.name, f.start, true
			}
		}
	}
	return "", "", false
}

// nodePackageManager prefers package.json's packageManager field (the explicit
// statement) over lockfiles (circumstantial, and repos often carry several).
func nodePackageManager(dir string, pkg packageJSON) string {
	if name, _, _ := strings.Cut(pkg.PackageManager, "@"); name != "" {
		return name
	}
	switch {
	case exists(filepath.Join(dir, "pnpm-lock.yaml")):
		return "pnpm"
	case exists(filepath.Join(dir, "yarn.lock")):
		return "yarn"
	default:
		return "npm"
	}
}

// findDockerfile returns the first known Dockerfile location present.
func findDockerfile(dir string) (string, bool) {
	for _, candidate := range dockerfileLocations {
		if exists(filepath.Join(dir, candidate)) {
			return candidate, true
		}
	}
	return "", false
}

// findFrontendDir spots a bundled frontend beside a Go module.
func findFrontendDir(dir string) (string, bool) {
	for _, candidate := range []string{"web", "frontend", "client", "ui"} {
		if exists(filepath.Join(dir, candidate, "package.json")) {
			return candidate, true
		}
	}
	return "", false
}

var exposeRe = regexp.MustCompile(`(?mi)^\s*EXPOSE\s+(\d+)`)

// readExposedPort returns the first port a Dockerfile EXPOSEs.
func readExposedPort(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	m := exposeRe.FindSubmatch(data)
	if m == nil {
		return 0, false
	}
	port, err := strconv.Atoi(string(m[1]))
	if err != nil || port <= 0 || port > 65535 {
		return 0, false
	}
	return port, true
}

var goDirectiveRe = regexp.MustCompile(`(?m)^go\s+(\d+\.\d+)`)

// readGoVersion reads go.mod's language version. Only major.minor is kept:
// that is what the golang image tags use.
func readGoVersion(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	m := goDirectiveRe.FindSubmatch(data)
	if m == nil {
		return "", false
	}
	return string(m[1]), true
}

// cgoDependencies are modules that compile under CGO_ENABLED=0 and then fail
// at runtime. The list is short on purpose — these are the ones that actually
// come up.
var cgoDependencies = []string{
	"github.com/mattn/go-sqlite3",
	"gorm.io/driver/sqlite",
}

// findCgoDependency reports whether the module graph pulls in something that
// needs cgo. go.sum is checked as well as go.mod because the dependency is
// usually indirect (gorm's sqlite driver pulls mattn's).
func findCgoDependency(dir string) (string, bool) {
	for _, file := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			continue
		}
		for _, dep := range cgoDependencies {
			if strings.Contains(string(data), dep) {
				return dep, true
			}
		}
	}
	return "", false
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Render writes the detected Manifest as YAML a person can
// read and edit, with the reasoning as leading comments.
//
// It is written by hand rather than yaml.Marshal for two reasons: the comments
// are the point (a generated file with no explanation is a file nobody trusts
// enough to edit), and the Manifest struct carries fields that exist only to
// reject legacy keys, which have no business appearing in generated output.
// RenderOptions vary the draft by who is asking for it.
type RenderOptions struct {
	// PublishPorts writes ports as "auto:<port>" instead of a bare container
	// port. It is set by `qooq init`, where the person running it is the
	// operator deciding how their own app should run, and left unset by the
	// watcher, which is generating a manifest for a repo it merely found and
	// has no business publishing anything from.
	PublishPorts bool
	// Heading replaces the opening comment. The watcher's default explains an
	// unasked-for file; `qooq init` was asked for, and telling someone their
	// repo "has no qooq.yaml" as they create one reads as a fault report.
	Heading string
}

func Render(d *Detection, opts RenderOptions) []byte {
	var b strings.Builder

	if opts.Heading != "" {
		b.WriteString(opts.Heading)
	} else {
		b.WriteString("# Generated by qooq: this repo has no qooq.yaml of its own.\n")
	}
	b.WriteString("#\n")
	b.WriteString("# Edit this freely — qooq keeps your changes and will not regenerate it.\n")
	b.WriteString("# Field reference: docs/DEPLOY-CONFIG.md\n")
	if len(d.Notes) > 0 {
		b.WriteString("#\n# What was detected (" + d.Kind + "):\n")
		for _, note := range d.Notes {
			b.WriteString("#   - " + note + "\n")
		}
	}
	b.WriteString("\nversion: 1\n")
	fmt.Fprintf(&b, "app: %s\n", d.Manifest.App)

	b.WriteString("\nbuild:\n")
	switch {
	case d.Manifest.Build.Dockerfile != "":
		fmt.Fprintf(&b, "  dockerfile: %s\n", d.Manifest.Build.Dockerfile)
	case d.Manifest.Build.Template != "":
		fmt.Fprintf(&b, "  template: %s\n", d.Manifest.Build.Template)
	default:
		b.WriteString("  dockerfile: Dockerfile\n")
	}
	if vars := d.Manifest.Build.TemplateVars; len(vars) > 0 {
		b.WriteString("  template_vars:\n")
		keys := make([]string, 0, len(vars))
		for k := range vars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "    %s: %q\n", k, vars[k])
		}
	}

	if ports := d.Manifest.Run.Ports; len(ports) > 0 {
		b.WriteString("\nrun:\n  ports:\n")
		for _, p := range ports {
			if opts.PublishPorts {
				// "auto:" leaves the number to the daemon, which is the only
				// party that knows what the machine already uses.
				fmt.Fprintf(&b, "    - %q\n", "auto:"+p)
				continue
			}
			// A bare container port: reachable by siblings on the stack's
			// network. Publishing to the host is the operator's call, not a
			// guess Detection should make on a repo it merely found.
			fmt.Fprintf(&b, "    - %q\n", p)
		}
	}
	writeHealthSuggestion(&b, d)
	return []byte(b.String())
}

// healthPathCandidates are the paths worth trying first, commonest first.
var healthPathCandidates = []string{"/healthz", "/health", "/ping", "/"}

// writeHealthSuggestion leaves the health block commented out, with the paths
// worth trying.
//
// Commented, not filled in, because which path an app serves is the one thing
// this cannot be read off the files: it is a fact about the code, and guessing
// produces a probe that fails forever against an app that is perfectly fine —
// which then either restarts it in a loop or, since probes that never succeed
// no longer restart anything, sits there reporting a problem that isn't one.
//
// It is worth writing at all because the alternative is what actually happens:
// someone reads their framework's source to find out that it serves /healthz
// and not /health. Naming the candidates turns that into one guess and one
// curl.
func writeHealthSuggestion(b *strings.Builder, d *Detection) {
	port := ""
	if ports := d.Manifest.Run.Ports; len(ports) > 0 {
		port = ports[0]
	}
	if port == "" {
		return
	}
	b.WriteString("\n# A health check lets qooq notice when this stops answering and restart it.\n")
	b.WriteString("# qooq cannot tell which path your app serves — that is in the code, not the\n")
	b.WriteString("# files — so try these against a running instance and uncomment the one that\n")
	b.WriteString("# answers 200:\n#\n")
	for _, p := range healthPathCandidates {
		fmt.Fprintf(b, "#   curl -i http://localhost:%s%s\n", port, p)
	}
	b.WriteString("#\n# health:\n")
	fmt.Fprintf(b, "#   http: %s\n", healthPathCandidates[0])
	// The container port, not a host port: that spelling keeps working when
	// the host side is left to the daemon.
	fmt.Fprintf(b, "#   port: %s\n", port)
}
