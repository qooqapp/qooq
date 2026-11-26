package watcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRepo lays out a fake repo: keys are paths, values are contents.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for path, content := range files {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func detect(t *testing.T, files map[string]string) *detection {
	t.Helper()
	d, err := detectManifest(writeRepo(t, files), "demo", "")
	if err != nil {
		t.Fatalf("detectManifest: %v", err)
	}
	return d
}

func notesContain(d *detection, substr string) bool {
	for _, n := range d.Notes {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}

// A Dockerfile the repo already has is the authors' own statement of how to
// build it, and beats anything qooq would generate.
func TestDetectUsesRepoDockerfile(t *testing.T) {
	d := detect(t, map[string]string{
		"Dockerfile": "FROM nginx:alpine\nEXPOSE 8080\n",
		"go.mod":     "module x\n\ngo 1.25\n",
	})
	if d.Kind != "dockerfile" {
		t.Fatalf("kind = %q, want dockerfile (it must win over go.mod)", d.Kind)
	}
	if d.Manifest.Build.Dockerfile != "Dockerfile" || d.Manifest.Build.Template != "" {
		t.Errorf("build = %+v", d.Manifest.Build)
	}
	if len(d.Manifest.Run.Ports) != 1 || d.Manifest.Run.Ports[0] != "8080" {
		t.Errorf("ports = %v, want the EXPOSEd port", d.Manifest.Run.Ports)
	}
}

// The three projects that motivated this all keep their Dockerfile in docker/.
func TestDetectFindsDockerfileOutsideRoot(t *testing.T) {
	for _, loc := range []string{"docker/Dockerfile", "build/Dockerfile", ".docker/Dockerfile"} {
		t.Run(loc, func(t *testing.T) {
			d := detect(t, map[string]string{loc: "FROM alpine\nEXPOSE 3000\n"})
			if d.Manifest.Build.Dockerfile != loc {
				t.Errorf("dockerfile = %q, want %q", d.Manifest.Build.Dockerfile, loc)
			}
		})
	}
}

// A cgo dependency builds fine with CGO_ENABLED=0 and then dies at runtime,
// which is a genuinely confusing way to find out.
func TestDetectGoWithCgoDependency(t *testing.T) {
	d := detect(t, map[string]string{
		"go.mod": "module x\n\ngo 1.26\n\nrequire gorm.io/driver/sqlite v1.6.0\n",
	})
	if d.Kind != "go" {
		t.Fatalf("kind = %q", d.Kind)
	}
	vars := d.Manifest.Build.TemplateVars
	if vars["cgo"] != "1" || vars["build_deps"] != "gcc musl-dev" {
		t.Errorf("cgo not enabled: %v", vars)
	}
	if vars["go_version"] != "1.26" {
		t.Errorf("go_version = %q, want the version go.mod requires", vars["go_version"])
	}
	if !notesContain(d, "cgo") {
		t.Errorf("the cgo decision was not explained: %v", d.Notes)
	}
}

// go-react has no cgo switch, so detection must not hand it the go
// template's variables: those now fail the build (they used to be dropped
// while the note claimed cgo was enabled). It says so instead.
func TestDetectGoReactWithCgoDependency(t *testing.T) {
	d := detect(t, map[string]string{
		"go.mod":           "module x\n\ngo 1.25\n\nrequire gorm.io/driver/sqlite v1.6.0\n",
		"web/package.json": `{"scripts":{"build":"vite build"}}`,
	})
	if d.Manifest.Build.Template != "go-react" {
		t.Fatalf("template = %q", d.Manifest.Build.Template)
	}
	if _, err := renderDockerfile(d.Manifest.Build.Template, d.Manifest.Build.TemplateVars, ""); err != nil {
		t.Fatalf("the detected manifest does not build: %v", err)
	}
	if !notesContain(d, "cannot enable") {
		t.Errorf("the missing cgo was not explained: %v", d.Notes)
	}
}

// An indirect cgo dependency (gorm pulls mattn) only shows in go.sum.
func TestDetectGoCgoFromGoSum(t *testing.T) {
	d := detect(t, map[string]string{
		"go.mod": "module x\n\ngo 1.25\n",
		"go.sum": "github.com/mattn/go-sqlite3 v1.14.22 h1:xxx=\n",
	})
	if d.Manifest.Build.TemplateVars["cgo"] != "1" {
		t.Errorf("cgo not detected from go.sum: %v", d.Manifest.Build.TemplateVars)
	}
}

func TestDetectGoWithFrontend(t *testing.T) {
	d := detect(t, map[string]string{
		"go.mod":           "module x\n\ngo 1.25\n",
		"web/package.json": `{"scripts":{"build":"vite build"}}`,
	})
	if d.Kind != "go-react" || d.Manifest.Build.Template != "go-react" {
		t.Fatalf("kind = %q template = %q", d.Kind, d.Manifest.Build.Template)
	}
	if d.Manifest.Build.TemplateVars["web_dir"] != "web" {
		t.Errorf("web_dir = %q", d.Manifest.Build.TemplateVars["web_dir"])
	}
}

// Nuxt's output is not the source tree: the single-stage default would deploy
// unbuilt sources.
func TestDetectNuxt(t *testing.T) {
	d := detect(t, map[string]string{
		"package.json":   `{"packageManager":"pnpm@10.0.0","scripts":{"build":"nuxt build"}}`,
		"nuxt.config.ts": "export default {}",
		"pnpm-lock.yaml": "lockfileVersion: 9\n",
	})
	if d.Kind != "node-nuxt" {
		t.Fatalf("kind = %q, want node-nuxt", d.Kind)
	}
	vars := d.Manifest.Build.TemplateVars
	if vars["build_cmd"] != "pnpm run build" {
		t.Errorf("build_cmd = %q, want pnpm's", vars["build_cmd"])
	}
	if vars["start_cmd"] != "node .output/server/index.mjs" {
		t.Errorf("start_cmd = %q", vars["start_cmd"])
	}
	if vars["corepack"] != "true" {
		t.Errorf("corepack not enabled for pnpm: %v", vars)
	}
}

// packageManager is an explicit statement; lockfiles are circumstantial and
// repos often carry more than one.
func TestDetectNodePackageManagerPrefersDeclaration(t *testing.T) {
	d := detect(t, map[string]string{
		"package.json":      `{"packageManager":"pnpm@10.0.0","scripts":{"start":"node index.js"}}`,
		"package-lock.json": "{}",
		"pnpm-lock.yaml":    "lockfileVersion: 9\n",
	})
	if got := d.Manifest.Build.TemplateVars["install_cmd"]; !strings.HasPrefix(got, "pnpm") {
		t.Errorf("install_cmd = %q, want pnpm's despite the npm lockfile", got)
	}
}

func TestDetectNodeServer(t *testing.T) {
	d := detect(t, map[string]string{
		"package.json": `{"scripts":{"start":"node server.js"}}`,
	})
	if d.Kind != "node" || d.Manifest.Build.Template != "node" {
		t.Fatalf("kind = %q", d.Kind)
	}
	if d.Manifest.Build.TemplateVars["start_cmd"] != "npm start" {
		t.Errorf("start_cmd = %q", d.Manifest.Build.TemplateVars["start_cmd"])
	}
}

func TestDetectNodeEntryPointWithoutStartScript(t *testing.T) {
	d := detect(t, map[string]string{
		"package.json": `{"scripts":{}}`,
		"index.js":     "console.log('hi')",
	})
	if d.Manifest.Build.TemplateVars["start_cmd"] != "node index.js" {
		t.Errorf("start_cmd = %q", d.Manifest.Build.TemplateVars["start_cmd"])
	}
}

// The static case is the guess most likely to be wrong, so it must say so.
func TestDetectStaticFrontendWarnsAboutItsGuess(t *testing.T) {
	d := detect(t, map[string]string{
		"package.json": `{"scripts":{"build":"vite build"}}`,
	})
	if d.Kind != "static-frontend" || d.Manifest.Build.Template != "static-nginx" {
		t.Fatalf("kind = %q template = %q", d.Kind, d.Manifest.Build.Template)
	}
	if !notesContain(d, "guess") {
		t.Errorf("the static_dir guess was not flagged: %v", d.Notes)
	}
}

func TestDetectPython(t *testing.T) {
	d := detect(t, map[string]string{"requirements.txt": "fastapi\n"})
	if d.Kind != "python" || d.Manifest.Build.Template != "python" {
		t.Fatalf("kind = %q", d.Kind)
	}
}

func TestDetectStaticSite(t *testing.T) {
	d := detect(t, map[string]string{"index.html": "<h1>hi</h1>"})
	if d.Kind != "static" || d.Manifest.Build.Template != "static-nginx" {
		t.Fatalf("kind = %q", d.Kind)
	}
}

// An unrecognizable repo still gets a manifest — the pipeline's existing
// behavior — with a note saying what it will try.
func TestDetectUnknownStillProducesAManifest(t *testing.T) {
	d := detect(t, map[string]string{"README.md": "hello"})
	if d.Manifest == nil {
		t.Fatal("no manifest generated")
	}
	if d.Kind != "unknown" {
		t.Errorf("kind = %q", d.Kind)
	}
	if len(d.Notes) == 0 {
		t.Error("an unrecognized repo must explain what will be attempted")
	}
}

// Whatever detection produces has to survive the real parser, or it would
// generate manifests qooq itself rejects.
func TestDetectedManifestsAreValid(t *testing.T) {
	repos := map[string]map[string]string{
		"dockerfile": {"Dockerfile": "FROM alpine\nEXPOSE 8080\n"},
		"go":         {"go.mod": "module x\n\ngo 1.26\n"},
		"nuxt":       {"package.json": `{"scripts":{"build":"nuxt build"}}`, "nuxt.config.ts": "{}"},
		"python":     {"requirements.txt": "flask\n"},
		"static":     {"index.html": "<h1>hi</h1>"},
	}
	for name, files := range repos {
		t.Run(name, func(t *testing.T) {
			d := detect(t, files)
			data := renderGeneratedManifest(d, false)
			if _, err := parseManifest(data); err != nil {
				t.Fatalf("generated manifest does not parse: %v\n%s", err, data)
			}
		})
	}
}

// testWatcherWithDB is a Watcher wired to an in-memory store, for the paths
// that persist what they detect.
func testWatcherWithDB(t *testing.T) *Watcher {
	t.Helper()
	w := testWatcher(t, "open")
	w.db = newTestDB(t)
	return w
}

// The correction loop only works if a stored manifest is reused: a fix that
// gets regenerated over on the next sweep is not a fix.
func TestResolveManifestGeneratesOnceThenReuses(t *testing.T) {
	w := testWatcherWithDB(t)
	target := &Target{Name: "demo", ComponentName: "demo", Dockerfile: "Dockerfile"}
	if err := w.db.Create(target).Error; err != nil {
		t.Fatal(err)
	}
	repo := writeRepo(t, map[string]string{"docker/Dockerfile": "FROM alpine\nEXPOSE 8080\n"})

	mf, err := w.resolveManifest(target, repo)
	if err != nil {
		t.Fatalf("resolveManifest: %v", err)
	}
	if mf.Build.Dockerfile != "docker/Dockerfile" {
		t.Errorf("dockerfile = %q, want the one found outside the root", mf.Build.Dockerfile)
	}
	if target.GeneratedManifest == "" {
		t.Fatal("the generated manifest was not stored, so it cannot be corrected")
	}

	// Stand in for an operator correcting a wrong guess.
	corrected := "version: 1\napp: demo\nbuild:\n  dockerfile: docker/Dockerfile\nrun:\n  ports:\n    - \"19200:8080\"\n"
	if err := w.db.Model(target).Update("generated_manifest", corrected).Error; err != nil {
		t.Fatal(err)
	}
	var reloaded Target
	if err := w.db.First(&reloaded, target.ID).Error; err != nil {
		t.Fatal(err)
	}

	mf, err = w.resolveManifest(&reloaded, repo)
	if err != nil {
		t.Fatalf("resolveManifest after correction: %v", err)
	}
	if len(mf.Run.Ports) != 1 || mf.Run.Ports[0] != "19200:8080" {
		t.Fatalf("the correction was ignored: %+v", mf.Run.Ports)
	}
	if reloaded.GeneratedManifest != corrected {
		t.Error("the correction was overwritten by regeneration")
	}
}

// A repo with its own qooq.yaml must be left alone entirely.
func TestResolveManifestPrefersTheRepoOwn(t *testing.T) {
	w := testWatcherWithDB(t)
	target := &Target{Name: "demo", ComponentName: "demo", ManifestPath: "qooq.yaml"}
	if err := w.db.Create(target).Error; err != nil {
		t.Fatal(err)
	}
	repo := writeRepo(t, map[string]string{
		"qooq.yaml":  "version: 1\napp: mine\nbuild:\n  template: go\n",
		"Dockerfile": "FROM alpine\n",
	})

	mf, err := w.resolveManifest(target, repo)
	if err != nil {
		t.Fatal(err)
	}
	if mf.App != "mine" {
		t.Errorf("app = %q, want the repo's own manifest", mf.App)
	}
	if target.GeneratedManifest != "" {
		t.Error("nothing should be generated for a repo that has its own manifest")
	}
}

// "-" is the documented escape hatch for repos that want nothing to do with
// manifests; generating one would defeat it.
func TestResolveManifestRespectsDisabled(t *testing.T) {
	w := testWatcherWithDB(t)
	target := &Target{Name: "demo", ComponentName: "demo", ManifestPath: "-"}
	if err := w.db.Create(target).Error; err != nil {
		t.Fatal(err)
	}
	repo := writeRepo(t, map[string]string{"go.mod": "module x\n\ngo 1.25\n"})

	mf, err := w.resolveManifest(target, repo)
	if err != nil {
		t.Fatal(err)
	}
	if mf != nil {
		t.Errorf("manifest = %+v, want none", mf)
	}
	if target.GeneratedManifest != "" {
		t.Error("a disabled manifest must not be generated")
	}
}

// A stored manifest that no longer parses (someone edited it badly) must say
// where to fix it.
func TestResolveManifestReportsBadStoredManifest(t *testing.T) {
	w := testWatcherWithDB(t)
	target := &Target{Name: "demo", ComponentName: "demo", GeneratedManifest: "version: 99\n"}
	if err := w.db.Create(target).Error; err != nil {
		t.Fatal(err)
	}

	_, err := w.resolveManifest(target, writeRepo(t, nil))
	if err == nil {
		t.Fatal("an unparseable stored manifest was accepted")
	}
	if !strings.Contains(err.Error(), "qooq watch manifest") {
		t.Errorf("error %q does not say how to fix it", err)
	}
}

// A target the external manager created publishes the ports of a manifest qooq
// generated; one an operator added does not, as before.
func TestGeneratedManifestPublishesPortsOnlyWhenAsked(t *testing.T) {
	d := detect(t, map[string]string{"index.html": "<h1>hi</h1>"})
	for _, tc := range []struct {
		publish bool
		want    string
	}{{false, `- "80"`}, {true, `- "auto:80"`}} {
		data := string(renderGeneratedManifest(d, tc.publish))
		if !strings.Contains(data, tc.want) {
			t.Errorf("publish=%v: want %s in\n%s", tc.publish, tc.want, data)
		}
	}
}
