package watcher

import (
	"os"
	"testing"
)

// TestReferenceManifestParses keeps docs/qooq.reference.yaml (and its
// zh-TW twin) honest.
//
// That file is presented to users as a complete, annotated manifest — the
// place to look up every field and its default. A reference that no longer
// parses is worse than no reference, and nothing else would catch it: it is
// documentation, so no build breaks and no test fails when a field is renamed
// out from under it. This is the cheapest possible guard against that drift.
func TestReferenceManifestParses(t *testing.T) {
	// The English file keeps the original name; the zh-TW copy must stay in
	// lockstep with it, so both get the same checks.
	for _, path := range []string{
		"../../docs/qooq.reference.yaml",
		"../../docs/qooq.reference.zh-TW.yaml",
	} {
		t.Run(path, func(t *testing.T) { checkReferenceManifest(t, path) })
	}
}

func checkReferenceManifest(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	mf, err := parseManifest(data)
	if err != nil {
		t.Fatalf("%s no longer parses as a manifest: %v", path, err)
	}
	// Spot-check that the blocks documented there actually landed in the
	// parsed struct, rather than being tolerated as unknown keys (yaml.v3 is
	// lenient by design, so "it parsed" alone proves less than it looks).
	if mf.App == "" {
		t.Error("app is empty; the reference should show a real value")
	}
	if len(mf.Inputs) == 0 {
		t.Error("inputs block did not parse into the manifest")
	}
	if len(mf.Services) == 0 {
		t.Error("services block did not parse into the manifest")
	}
	if mf.Ingress == nil || len(mf.Ingress.Hosts) == 0 {
		t.Error("ingress block did not parse into the manifest")
	}
}
