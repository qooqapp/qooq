package watcher

import (
	"strings"
	"testing"
)

func TestASeparateBuildNodeNeedsARegistry(t *testing.T) {
	const deploy = "http://127.0.0.1:7777"
	for _, c := range []struct {
		name, buildAddr, prefix string
		refused                 bool
	}{
		{"one node", "", "", false},
		{"build_addr is the deploy node", deploy, "", false},
		{"registry alone (single machine)", "", "localhost:5000/qooq", false},
		{"separate build node with a registry", "http://10.0.0.2:7777", "localhost:5000/qooq", false},
		{"separate build node without a registry", "http://10.0.0.2:7777", "", true},
	} {
		err := checkBuildNodeRegistry(c.buildAddr, deploy, registryConfig{Prefix: c.prefix})
		if (err != nil) != c.refused {
			t.Errorf("%s: err = %v, refused want %v", c.name, err, c.refused)
			continue
		}
		if err != nil {
			for _, want := range []string{configQooqdBuildAddr, configRegistryPrefix, "could not pull"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%s: message does not say %q: %s", c.name, want, err)
				}
			}
		}
	}
}
