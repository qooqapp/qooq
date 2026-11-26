package watcher

import (
	"bytes"
	"embed"
	"fmt"
	"maps"
	"sort"
	"text/template"

	qm "github.com/qooqapp/qooq/pkg/manifest"
)

// templateFS embeds the built-in Dockerfile templates (M21), one directory
// per template name, so a repo can set build.template instead of committing
// its own Dockerfile — same co-location convention as openapiSpec in
// docs.go.
//
//go:embed templates/*/Dockerfile.tmpl
var templateFS embed.FS

// templateDefaults is pkg/manifest's table of built-in templates and their
// variables. It lives there so `qooq manifest validate` (another module)
// rejects a misspelled template_vars key offline, from the same list the
// build uses.
var templateDefaults = qm.BuiltinTemplateDefaults()

// renderDockerfile renders the built-in Dockerfile template named name,
// merging vars (the manifest's build.template_vars, may be nil) over the
// template's own defaults.
//
// cloneDir is currently unused — it's the extension point for a future
// "template: auto" mode that would inspect the freshly cloned repo (e.g.
// read package.json/go.mod to guess versions) and generate a Dockerfile
// instead of picking one of the fixed named templates. Keeping the
// parameter now avoids changing every call site again when that lands.
func renderDockerfile(name string, vars map[string]string, cloneDir string) (string, error) {
	_ = cloneDir

	if err := qm.CheckTemplate(name, vars); err != nil {
		return "", err
	}
	defaults := templateDefaults[name]

	merged := maps.Clone(defaults)
	for k, v := range vars {
		merged[k] = v
	}

	tmplBytes, err := templateFS.ReadFile("templates/" + name + "/Dockerfile.tmpl")
	if err != nil {
		return "", fmt.Errorf("read template %q: %w", name, err)
	}

	return renderTemplateString(string(tmplBytes), merged)
}

// renderTemplateString executes tmplText (a text/template body) against
// vars.
//
// missingkey=error is deliberate: vars is a plain map, and text/template's
// default behavior for a missing map key is to silently render the literal
// string "<no value>" into the output. For a Dockerfile that's the
// difference between a build failure the operator can act on and a subtly
// broken image (e.g. `FROM golang:<no value>-alpine`) that may still build
// something, just not what was intended — a hard error here is strictly
// better than a bad Dockerfile.
func renderTemplateString(tmplText string, vars map[string]string) (string, error) {
	tmpl, err := template.New("dockerfile").Option("missingkey=error").Parse(tmplText)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// RenderBuiltinDockerfile renders a built-in template by name, with vars
// merged over that template's defaults — the same output a build produces.
//
// It is exported for tooling that needs to see what a template generates
// without standing up a watcher and a git remote to reach the pipeline:
// scripts/smoke-templates.sh builds every template's output for real, which is
// the only check that catches a Dockerfile that renders cleanly and does not
// build.
func RenderBuiltinDockerfile(name string, vars map[string]string) (string, error) {
	return renderDockerfile(name, vars, "")
}

// BuiltinTemplateNames lists the templates that can be selected by name.
func BuiltinTemplateNames() []string {
	names := make([]string, 0, len(templateDefaults))
	for n := range templateDefaults {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
