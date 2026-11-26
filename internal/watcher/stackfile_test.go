package watcher

import (
	"strings"
	"testing"
)

const shrimpStack = `
version: 1
stack: acme-prod
description: 糖醋蝦球

projects:
  shell-shrimp:
    path: ./shrimp-sheller

  whip-mayo:
    repo: https://github.com/acme/mayo-whipper.git
    branch: main
    depends_on:
      - shell-shrimp

  plate-it:
    repo: https://github.com/acme/plating.git
    depends_on:
      - whip-mayo
      - name: pineapple
        wait: started
    env:
      MAYO_URL: "http://whip-mayo:8080"

  pineapple:
    repo: https://github.com/acme/pineapple-opener.git
`

func TestParseStackManifest(t *testing.T) {
	r, err := parseStackManifest([]byte(shrimpStack))
	if err != nil {
		t.Fatalf("parseStackManifest: %v", err)
	}
	if r.Stack != "acme-prod" || len(r.Projects) != 4 {
		t.Fatalf("stack=%q projects=%d", r.Stack, len(r.Projects))
	}

	if got := r.Projects["shell-shrimp"].source(); got != projectSourcePath {
		t.Errorf("shell-shrimp source = %v, want path", got)
	}
	if got := r.Projects["whip-mayo"].source(); got != projectSourceRepo {
		t.Errorf("whip-mayo source = %v, want repo", got)
	}
	if got := r.Projects["plate-it"].Env["MAYO_URL"]; got != "http://whip-mayo:8080" {
		t.Errorf("project env = %q", got)
	}
}

// The short form means ready; that is the whole point of having a short form
// for the common case.
func TestProjectDepBothForms(t *testing.T) {
	r, err := parseStackManifest([]byte(shrimpStack))
	if err != nil {
		t.Fatal(err)
	}
	deps := r.Projects["plate-it"].DependsOn
	if len(deps) != 2 {
		t.Fatalf("got %d deps", len(deps))
	}
	if deps[0].Name != "whip-mayo" || deps[0].Wait != waitReady {
		t.Errorf("bare name should default to ready, got %+v", deps[0])
	}
	if deps[1].Name != "pineapple" || deps[1].Wait != waitStarted {
		t.Errorf("explicit wait not honored: %+v", deps[1])
	}
}

// Order comes from depends_on: alone — writing the projects in a different order in
// the file must not change anything, since a YAML map has no order.
func TestStackDeployOrder(t *testing.T) {
	r, err := parseStackManifest([]byte(shrimpStack))
	if err != nil {
		t.Fatal(err)
	}
	order, err := r.deployOrder()
	if err != nil {
		t.Fatalf("deployOrder: %v", err)
	}

	pos := make(map[string]int, len(order))
	for i, name := range order {
		pos[name] = i
	}
	if pos["shell-shrimp"] > pos["whip-mayo"] {
		t.Error("shell-shrimp must come before whip-mayo")
	}
	if pos["whip-mayo"] > pos["plate-it"] || pos["pineapple"] > pos["plate-it"] {
		t.Errorf("plate-it must come last, order = %v", order)
	}
}

// A deterministic order matters for reading deploy logs: the same stack manifest must
// not shuffle between runs just because Go randomizes map iteration.
func TestStackDeployOrderIsStable(t *testing.T) {
	r, err := parseStackManifest([]byte(shrimpStack))
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.deployOrder()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := r.deployOrder()
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("order changed between runs: %v vs %v", first, got)
		}
	}
}

func TestParseStackManifestRejects(t *testing.T) {
	cases := map[string]struct {
		yaml string
		want string
	}{
		"wrong version": {
			yaml: "version: 2\nstack: d\nprojects:\n  a: {path: ./a}\n",
			want: "version",
		},
		"missing stack": {
			yaml: "version: 1\nprojects:\n  a: {path: ./a}\n",
			want: "stack",
		},
		"stack not a dns label": {
			yaml: "version: 1\nstack: Acme_Prod\nprojects:\n  a: {path: ./a}\n",
			want: "DNS label",
		},
		"no projects": {
			yaml: "version: 1\nstack: d\nprojects: {}\n",
			want: "at least one project",
		},
		"project name not a dns label": {
			yaml: "version: 1\nstack: d\nprojects:\n  My_Step: {path: ./a}\n",
			want: "DNS label",
		},
		"project with no source": {
			yaml: "version: 1\nstack: d\nprojects:\n  a: {}\n",
			want: "needs either path",
		},
		"project with two sources": {
			yaml: "version: 1\nstack: d\nprojects:\n  a: {path: ./a, repo: https://x/y.git}\n",
			want: "mutually exclusive",
		},
		"path escapes the repo": {
			yaml: "version: 1\nstack: d\nprojects:\n  a: {path: ../outside}\n",
			want: "must stay inside",
		},
		"dangling dependency": {
			yaml: "version: 1\nstack: d\nprojects:\n  a: {path: ./a, depends_on: [ghost]}\n",
			want: "not a project in this stack",
		},
		"self dependency": {
			yaml: "version: 1\nstack: d\nprojects:\n  a: {path: ./a, depends_on: [a]}\n",
			want: "cannot wait for itself",
		},
		"bad wait mode": {
			yaml: "version: 1\nstack: d\nprojects:\n  a: {path: ./a}\n  b:\n    path: ./b\n    depends_on:\n      - name: a\n        wait: eventually\n",
			want: "wait must be",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseStackManifest([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected an error, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// A cycle has to be a parse error: discovering it halfway through a deploy
// would leave the stack in a state nobody asked for.
func TestParseStackManifestRejectsCycles(t *testing.T) {
	cycles := map[string]string{
		"two projects": `
version: 1
stack: d
projects:
  a: {path: ./a, depends_on: [b]}
  b: {path: ./b, depends_on: [a]}
`,
		"three projects": `
version: 1
stack: d
projects:
  a: {path: ./a, depends_on: [c]}
  b: {path: ./b, depends_on: [a]}
  c: {path: ./c, depends_on: [b]}
`,
	}
	for name, y := range cycles {
		t.Run(name, func(t *testing.T) {
			_, err := parseStackManifest([]byte(y))
			if err == nil {
				t.Fatal("a cycle was accepted")
			}
			if !strings.Contains(err.Error(), "cycle") {
				t.Fatalf("error %q does not say cycle", err)
			}
		})
	}
}

func TestStackMaxFailures(t *testing.T) {
	tests := map[int]int{
		0:  defaultMaxFailures, // omitted
		5:  5,
		-1: 0, // explicitly disabled
	}
	for configured, want := range tests {
		if got := (stackPolicy{MaxFailures: configured}).maxFailures(); got != want {
			t.Errorf("maxFailures(%d) = %d, want %d", configured, got, want)
		}
	}
}

// A project with no depends_on: at all is legal and starts immediately — a stack whose
// parts are independent should not have to say so.
func TestParseStackManifestAllIndependent(t *testing.T) {
	r, err := parseStackManifest([]byte("version: 1\nstack: d\nprojects:\n  a: {path: ./a}\n  b: {path: ./b}\n  c: {path: ./c}\n"))
	if err != nil {
		t.Fatal(err)
	}
	order, err := r.deployOrder()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 {
		t.Fatalf("order = %v", order)
	}
}
