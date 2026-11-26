package watcher

import (
	"encoding/json"
	"testing"

	"github.com/qooqapp/qooq/pkg/client"
)

// A deploy spec may spell ports either way: the object form it has always
// taken, or the "host:container" string the manifest and the CLI use.
func TestPortSpec_UnmarshalAcceptsBothForms(t *testing.T) {
	cases := map[string]struct {
		in   string
		want portSpec
	}{
		"string with host port": {`"8080:80"`, portSpec{HostPort: 8080, ContainerPort: 80}},
		"bare container port":   {`"80"`, portSpec{ContainerPort: 80}},
		"object":                {`{"host_port":8080,"container_port":80}`, portSpec{HostPort: 8080, ContainerPort: 80}},
		"object with protocol":  {`{"container_port":53,"protocol":"udp"}`, portSpec{ContainerPort: 53, Protocol: "udp"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var got portSpec
			if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
				t.Fatalf("unmarshal %s: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPortSpec_UnmarshalRejectsGarbage(t *testing.T) {
	for _, in := range []string{`"not-a-port"`, `"8080:"`, `42`} {
		var got portSpec
		if err := json.Unmarshal([]byte(in), &got); err == nil {
			t.Errorf("unmarshal %s: got %+v, want an error", in, got)
		}
	}
}

// A deploy spec that names no namespace must reach the daemon saying "default",
// which is what the daemon stores. If the watcher left it empty, findComponent
// would compare "" against the daemon's "default", conclude the component does
// not exist, and upsert it on every single sweep.
func TestParseDeploySpec_FillsDefaultNamespace(t *testing.T) {
	for _, raw := range []string{`{"namespace":"default"}`, `{}`, ``} {
		spec, err := parseDeploySpec(raw)
		if err != nil {
			t.Fatalf("parseDeploySpec(%q): %v", raw, err)
		}
		if spec.Namespace != client.DefaultNamespace {
			t.Errorf("parseDeploySpec(%q) namespace = %q, want %q", raw, spec.Namespace, client.DefaultNamespace)
		}
	}
	spec, err := parseDeploySpec(`{"namespace":"acme"}`)
	if err != nil {
		t.Fatalf("parseDeploySpec: %v", err)
	}
	if spec.Namespace != "acme" {
		t.Errorf("namespace = %q, want acme", spec.Namespace)
	}
}
