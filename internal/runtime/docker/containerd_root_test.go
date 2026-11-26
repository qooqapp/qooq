package docker

import (
	"os"
	"path/filepath"
	"testing"
)

// containerd's root comes from its own config; guessing the default warned
// against a correct -build-disk-path and missed a wrong one on a machine whose
// root had been moved (E4).
func TestContainerdRootComesFromItsConfig(t *testing.T) {
	dir := t.TempDir()
	write := func(body string) string {
		p := filepath.Join(dir, "config.toml")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name, body, want string
		known            bool
	}{
		{"moved", "version = 2\nroot = \"/mnt/dockerdata/containerd\"\nstate = \"/run/containerd\"\n", "/mnt/dockerdata/containerd", true},
		{"no root key: containerd's default", "version = 2\n", containerdDefaultRoot, true},
		{"a section's root is not containerd's", "version = 2\n[plugins.\"io.containerd.grpc.v1.cri\"]\n  root = \"/elsewhere\"\n", containerdDefaultRoot, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, known, err := containerdRoot(write(tc.body))
			if err != nil || got != tc.want || known != tc.known {
				t.Errorf("= %q, %v, %v; want %q, %v", got, known, err, tc.want, tc.known)
			}
		})
	}
	// No readable config: not known, so nothing is compared or warned about.
	if _, known, _ := containerdRoot(filepath.Join(dir, "absent.toml")); known {
		t.Error("a missing config was treated as knowing where containerd keeps its layers")
	}
}
