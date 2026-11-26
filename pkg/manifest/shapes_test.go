package manifest_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	qm "github.com/qooqapp/qooq/pkg/manifest"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files from the current detection")

// The corpus in testdata/shapes holds the *file appearance* of real project
// layouts — a Next.js app is its package.json, next.config and an app/ page,
// not a working Next.js app. Detection reads appearance and nothing else (it
// deliberately never reads source to infer behaviour), so appearance is the
// whole input, and a skeleton tests it exactly as well as the real thing while
// costing nothing to build.
//
// The golden files are not assertions that the current answer is *right*. They
// make the answer *visible*: change detect.go and the diff names every shape
// whose answer moved, including the ones you were not thinking about. Several
// goldens below record answers that are plainly wrong (Rust, Ruby, Java, PHP
// and .NET have no template, so they fall through to the root-Dockerfile
// fallback) — that is the point. A corpus where everything already passes
// documents nothing.
//
// Refresh with:  go test ./pkg/manifest/ -run TestShapes -update
func TestShapes(t *testing.T) {
	shapesDir := filepath.Join("testdata", "shapes")
	entries, err := os.ReadDir(shapesDir)
	if err != nil {
		t.Fatalf("read shapes: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no shapes in testdata/shapes")
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			d, err := qm.Detect(filepath.Join(shapesDir, name), name, "")
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			got := describe(d)

			goldenPath := filepath.Join("testdata", "golden", name+".txt")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden (run with -update to create it): %v", err)
			}
			if got != string(want) {
				t.Errorf("detection changed for %s.\n--- want ---\n%s\n--- got ---\n%s", name, want, got)
			}
		})
	}
}

// describe records everything a caller can observe: the kind, the notes that
// justify it, and the file `qooq init` would write. Rendering is included
// because a detection that is right and a manifest that is wrong look the same
// from outside, and the manifest is what the user actually gets.
func describe(d *qm.Detection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kind: %s\n\nnotes:\n", d.Kind)
	if len(d.Notes) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, n := range d.Notes {
		fmt.Fprintf(&b, "  - %s\n", n)
	}
	b.WriteString("\nrendered qooq.yaml:\n")
	rendered := qm.Render(d, qm.RenderOptions{PublishPorts: true, Heading: "# generated\n"})
	for _, line := range strings.Split(strings.TrimRight(string(rendered), "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// Whatever detection decides, the manifest it produces has to be one the
// pipeline can actually parse — a shape that detects "correctly" into an
// unparseable manifest fails later, where the cause is no longer visible.
func TestShapesProduceParseableManifests(t *testing.T) {
	shapesDir := filepath.Join("testdata", "shapes")
	entries, err := os.ReadDir(shapesDir)
	if err != nil {
		t.Fatalf("read shapes: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		t.Run(e.Name(), func(t *testing.T) {
			d, err := qm.Detect(filepath.Join(shapesDir, e.Name()), e.Name(), "")
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			for _, opts := range []qm.RenderOptions{
				{PublishPorts: true, Heading: "# init\n"},
				{},
			} {
				rendered := qm.Render(d, opts)
				if _, err := qm.Parse(rendered); err != nil {
					t.Errorf("rendered manifest does not parse (PublishPorts=%v): %v\n%s",
						opts.PublishPorts, err, rendered)
				}
			}
		})
	}
}
