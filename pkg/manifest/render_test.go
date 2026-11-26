package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func renderFor(t *testing.T, files map[string]string, opts RenderOptions) string {
	t.Helper()
	d, err := Detect(writeRepo(t, files), "demo", "")
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	return string(Render(d, opts))
}

// Whether a port is published depends on who asked. `qooq init` is the
// operator deciding about their own app; the watcher is guessing about a repo
// it merely found and has no business publishing anything from.
func TestRenderPublishesPortsOnlyWhenAsked(t *testing.T) {
	files := map[string]string{"Dockerfile": "FROM nginx\nEXPOSE 8080\n"}

	draft := renderFor(t, files, RenderOptions{PublishPorts: true})
	if !strings.Contains(draft, `"auto:8080"`) {
		t.Errorf("init draft should publish the port:\n%s", draft)
	}

	generated := renderFor(t, files, RenderOptions{})
	if strings.Contains(generated, "auto:") {
		t.Errorf("a generated manifest should not publish on its own:\n%s", generated)
	}
	if !strings.Contains(generated, `"8080"`) {
		t.Errorf("the container port is still declared:\n%s", generated)
	}
}

// The detection notes must not claim a consequence the render decides. A note
// saying "inside the network only" beside a published port is a file that
// contradicts itself.
func TestRenderNotesDoNotContradictThePorts(t *testing.T) {
	draft := renderFor(t, map[string]string{"Dockerfile": "FROM nginx\nEXPOSE 8080\n"},
		RenderOptions{PublishPorts: true})
	if strings.Contains(draft, "inside the network only") {
		t.Errorf("note contradicts the published port:\n%s", draft)
	}
}

// The health block is suggested, never filled in: which path an app serves is
// a fact about its code, and a wrong guess produces a probe that fails forever
// against an app that is fine.
func TestRenderSuggestsHealthWithoutGuessing(t *testing.T) {
	draft := renderFor(t, map[string]string{"Dockerfile": "FROM nginx\nEXPOSE 8080\n"},
		RenderOptions{PublishPorts: true})

	for _, want := range []string{"# health:", "curl -i http://localhost:8080/healthz", "/ping"} {
		if !strings.Contains(draft, want) {
			t.Errorf("draft should mention %q:\n%s", want, draft)
		}
	}
	// Commented out — an uncommented health block would be a guess deployed.
	m, err := Parse([]byte(draft))
	if err != nil {
		t.Fatalf("the draft must parse: %v", err)
	}
	if m.Health.HTTP != "" {
		t.Errorf("health was filled in rather than suggested: %q", m.Health.HTTP)
	}
}

// Whatever is generated has to survive the validator, or someone goes looking
// for a mistake in their project that is really a mistake in here.
func TestRenderedDraftsParse(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"dockerfile": {"Dockerfile": "FROM nginx\nEXPOSE 8080\n"},
		"go":         {"go.mod": "module demo\n\ngo 1.25\n", "main.go": "package main\n"},
		"node":       {"package.json": `{"name":"demo","scripts":{"start":"node index.js"}}`},
		"unknown":    {"README.md": "hi\n"},
	} {
		t.Run(name, func(t *testing.T) {
			draft := renderFor(t, files, RenderOptions{PublishPorts: true})
			if _, err := Parse([]byte(draft)); err != nil {
				t.Errorf("draft does not parse: %v\n%s", err, draft)
			}
		})
	}
}

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
