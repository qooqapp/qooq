package manifest

import (
	"strings"
	"testing"
)

func parseOK(t *testing.T, src string) *Manifest {
	t.Helper()
	m, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return m
}

func parseErr(t *testing.T, src, want string) {
	t.Helper()
	_, err := Parse([]byte(src))
	if err == nil {
		t.Fatalf("want an error mentioning %q, got none", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %v, want it to mention %q", err, want)
	}
}

// The whole reason this package is importable: a mistake in a manifest should
// be reportable without a daemon, a clone, or a build.
func TestParseAcceptsAWholeManifest(t *testing.T) {
	m := parseOK(t, `
version: 1
app: demo
description: "a service, with a colon in its description"
run:
  ports: ["8080:80"]
  env:
    MODE: production
build:
  dockerfile: docker/Dockerfile
requests:
  cpu: 0.5
  memory: 256Mi
`)
	if m.App != "demo" {
		t.Errorf("app = %q, want demo", m.App)
	}
	if m.Build.Dockerfile != "docker/Dockerfile" {
		t.Errorf("build.dockerfile = %q", m.Build.Dockerfile)
	}
	if len(m.Run.Ports) != 1 || m.Run.Ports[0] != "8080:80" {
		t.Errorf("run.ports = %v", m.Run.Ports)
	}
}

// The failure that motivated offline validation: an unquoted colon inside a
// value makes the document itself invalid, and the message has to say so
// rather than blaming a field.
func TestParseRejectsMalformedYAML(t *testing.T) {
	parseErr(t, `
version: 1
app: demo
description: Nuxt SPA, ssr: false
`, "invalid yaml")
}

func TestParseRejectsUnsupportedVersion(t *testing.T) {
	parseErr(t, "version: 2\napp: demo\n", "unsupported manifest version")
	parseErr(t, "app: demo\n", "unsupported manifest version")
}

// Both renames must fail loudly. Tolerating the old key would deploy with the
// resource section silently ignored, which looks like qooq losing the limits.
func TestParseRejectsRenamedSections(t *testing.T) {
	// Straight to the current name, not to resource: (itself renamed since).
	parseErr(t, "version: 1\nrequires:\n  cpu: 1\n", "under requests:")
	parseErr(t, "version: 1\nresource:\n  cpu: 1\n", "has been renamed")

	// A bare key with no value has nothing to migrate, so it stays tolerated.
	parseOK(t, "version: 1\nrequires:\n")
	parseOK(t, "version: 1\nresource:\n")
}

func TestParseRejectsContradictoryBuildInstructions(t *testing.T) {
	parseErr(t, `
version: 1
build:
  template: go
  dockerfile: Dockerfile
`, "mutually exclusive")
}

func TestParseRejectsReplicasOtherThanOne(t *testing.T) {
	parseErr(t, "version: 1\npolicy:\n  replicas: 3\n", "only 1 is supported")
	parseOK(t, "version: 1\npolicy:\n  replicas: 1\n")
}

// Moved here with validateInputs itself: required + default can never fire the
// required check, so accepting it would mean silently honouring one and
// ignoring the other.
func TestValidateInputsRejectsContradictions(t *testing.T) {
	err := validateInputs(map[string]Input{"X": {Required: true, Default: "y"}})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("got %v, want a mutual-exclusion error", err)
	}

	// A name with a dot would be ambiguous against the ${input.NAME} syntax.
	if err := validateInputs(map[string]Input{"a.b": {}}); err == nil {
		t.Error("a dotted input name should be rejected")
	}
}

func TestParseSurfacesInputErrors(t *testing.T) {
	parseErr(t, `
version: 1
inputs:
  STRIPE_KEY:
    required: true
    default: sk_test
`, "mutually exclusive")
}

// The mount short form is part of the format, so it is validated here too.
func TestParseMountEntry(t *testing.T) {
	got, err := ParseMountEntry("./config.toml:/app/config.toml:ro")
	if err != nil {
		t.Fatalf("ParseMountEntry: %v", err)
	}
	if got.From != "./config.toml" || got.To != "/app/config.toml" || got.Mode != "ro" {
		t.Errorf("got %+v", got)
	}
	if _, err := ParseMountEntry("just-a-name"); err == nil {
		t.Error("a source with no target should be rejected")
	}
	if _, err := ParseMountEntry("a:b:sideways"); err == nil {
		t.Error("an unknown mode should be rejected")
	}
}

// A mount written as a long object and as a short string must land in the same
// place — they are two spellings of one thing.
func TestMountAcceptsBothSpellings(t *testing.T) {
	m := parseOK(t, `
version: 1
run:
  files:
    - ./config.toml:/app/config.toml
    - from: ./other.toml
      to: /app/other.toml
      mode: ro
`)
	if len(m.Run.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(m.Run.Files))
	}
	if m.Run.Files[0].From != "./config.toml" || m.Run.Files[0].To != "/app/config.toml" {
		t.Errorf("short form = %+v", m.Run.Files[0])
	}
	if m.Run.Files[1].From != "./other.toml" || m.Run.Files[1].Mode != "ro" {
		t.Errorf("long form = %+v", m.Run.Files[1])
	}
}

// The combination that motivated this: switching to an auto port leaves the
// old host port written in health.port, where it names nothing. Accepting it
// gives an app whose probe checks a different component and therefore never
// reports a problem.
func TestParseRejectsAHealthPortThatIsNotDeclared(t *testing.T) {
	parseErr(t, `
version: 1
run:
  ports: ["auto:8080"]
health:
  http: /healthz
  port: 19000
`, "not one of run.ports")

	parseErr(t, `
version: 1
health:
  http: /healthz
  port: 8080
`, "declares no ports")
}

// Both spellings pass. The container port is the one that survives an auto
// host port, which is why the error recommends it.
func TestParseAcceptsAHealthPortOnEitherSide(t *testing.T) {
	parseOK(t, "version: 1\nrun:\n  ports: [\"auto:8080\"]\nhealth:\n  http: /healthz\n  port: 8080\n")
	parseOK(t, "version: 1\nrun:\n  ports: [\"19000:8080\"]\nhealth:\n  http: /healthz\n  port: 19000\n")
	parseOK(t, "version: 1\nrun:\n  ports: [\"19000:8080\"]\nhealth:\n  http: /healthz\n  port: 8080\n")
}

// An exec probe has no port to check, and a manifest with no health block at
// all must stay unaffected.
func TestParseHealthPortCheckLeavesOtherProbesAlone(t *testing.T) {
	parseOK(t, "version: 1\nhealth:\n  exec: [\"true\"]\n")
	parseOK(t, "version: 1\nrun:\n  ports: [\"auto:8080\"]\n")
}

// health.http is appended to http://127.0.0.1:<port>, so a value that is not a
// path would redirect the probe to another host.
func TestParseRejectsAHealthPathThatIsNotAPath(t *testing.T) {
	for _, bad := range []string{`"@evil.example/"`, `healthz`, `"//evil.example/"`} {
		parseErr(t, "version: 1\nrun:\n  ports: [\"8080\"]\nhealth:\n  http: "+bad+"\n  port: 8080\n", "must start with a single")
	}
}

// A misspelled template variable used to be dropped at build time and the
// app deployed with the default; it is now a validation error, offline.
func TestParseRejectsUnknownTemplateVariables(t *testing.T) {
	parseErr(t, "version: 1\nbuild:\n  template: static-nginx\n  template_vars:\n    dir: public\n",
		`template "static-nginx" has no variable "dir"; its variables are: build_cmd, corepack, install_cmd, nginx_tag, node_version, spa, static_dir`)
	parseErr(t, "version: 1\nbuild:\n  template: nginx\n", `unknown build.template "nginx"`)
	parseErr(t, "version: 1\nbuild:\n  template_vars:\n    static_dir: public\n", "build.template is not")
	parseOK(t, "version: 1\nbuild:\n  template: static-nginx\n  template_vars:\n    static_dir: public\n")
}
