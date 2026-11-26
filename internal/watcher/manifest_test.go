package watcher

import (
	"os"
	"strings"
	"testing"
)

func TestParseManifestVersion(t *testing.T) {
	if _, err := parseManifest([]byte("version: 1\napp: foo\n")); err != nil {
		t.Fatalf("version 1 should parse: %v", err)
	}
	if _, err := parseManifest([]byte("version: 2\napp: foo\n")); err == nil {
		t.Fatal("version 2 should be rejected")
	}
	if _, err := parseManifest([]byte("version: 1\n  bad: [indent\n")); err == nil {
		t.Fatal("malformed yaml should error")
	}
}

func TestParseManifestReplicas(t *testing.T) {
	if _, err := parseManifest([]byte("version: 1\npolicy:\n  replicas: 0\n")); err != nil {
		t.Fatalf("replicas=0 should be allowed: %v", err)
	}
	if _, err := parseManifest([]byte("version: 1\npolicy:\n  replicas: 1\n")); err != nil {
		t.Fatalf("replicas=1 should be allowed: %v", err)
	}
	if _, err := parseManifest([]byte("version: 1\npolicy:\n  replicas: 2\n")); err == nil {
		t.Fatal("replicas=2 should be rejected")
	}
}

func TestParseManifestMemBytes(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"256Mi", 256 * 1024 * 1024, false},
		{"1Gi", 1024 * 1024 * 1024, false},
		{"512Ki", 512 * 1024, false},
		{"256MB", 256 * 1000 * 1000, false},
		{"1GB", 1000 * 1000 * 1000, false},
		{"512KB", 512 * 1000, false},
		{"64m", 64 * 1024 * 1024, false},
		{"1g", 1024 * 1024 * 1024, false},
		{"128k", 128 * 1024, false},
		{"1024", 1024, false},
		{"1KiB", 1024, false},
		{"-5", 0, true},
		{"-1Mi", 0, true},
		{"abc", 0, true},
		{"5Xi", 0, true},
	}
	for _, c := range cases {
		got, err := parseManifestMemBytes(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseManifestMemBytes(%q): expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseManifestMemBytes(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseManifestMemBytes(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestNormalizeArch(t *testing.T) {
	cases := map[string]string{
		"x86_64":  "amd64",
		"amd64":   "amd64",
		"aarch64": "arm64",
		"arm64":   "arm64",
		"x86":     "386",
		"i386":    "386",
		"386":     "386",
		"armv7l":  "arm",
		"arm":     "arm",
		"AMD64":   "amd64",
		"riscv64": "riscv64", // unknown passthrough, lowercased
	}
	for in, want := range cases {
		if got := normalizeArch(in); got != want {
			t.Errorf("normalizeArch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMergeDeploySpecOverridesNonZeroFields(t *testing.T) {
	base := deploySpecFields{
		Command:       []string{"base-cmd"},
		Env:           map[string]string{"A": "1"},
		RestartPolicy: "always",
		CPULimit:      1.0,
		MemLimitBytes: 100,
		Domain:        "base.example.com",
	}
	override := deploySpecFields{
		Command:       []string{"override-cmd"},
		RestartPolicy: "never",
		CPULimit:      2.0,
		Domain:        "override.example.com",
	}
	out := mergeDeploySpec(base, override)
	if out.Command[0] != "override-cmd" {
		t.Errorf("Command not overridden: %v", out.Command)
	}
	if out.RestartPolicy != "never" {
		t.Errorf("RestartPolicy not overridden: %v", out.RestartPolicy)
	}
	if out.CPULimit != 2.0 {
		t.Errorf("CPULimit not overridden: %v", out.CPULimit)
	}
	// override left Env/MemLimitBytes zero -> base preserved
	if out.Env["A"] != "1" {
		t.Errorf("Env should be preserved from base: %v", out.Env)
	}
	if out.MemLimitBytes != 100 {
		t.Errorf("MemLimitBytes should be preserved from base: %v", out.MemLimitBytes)
	}
	if out.Domain != "override.example.com" {
		t.Errorf("Domain not overridden: %v", out.Domain)
	}
}

func TestMergeDeploySpecEnvMergesPerKey(t *testing.T) {
	base := deploySpecFields{Env: map[string]string{"API_PUBLIC": "from-manifest", "MODE": "repo"}}
	override := deploySpecFields{Env: map[string]string{"API_INTERNAL": "from-stack", "MODE": "operator"}}
	out := mergeDeploySpec(base, override)
	want := map[string]string{"API_PUBLIC": "from-manifest", "API_INTERNAL": "from-stack", "MODE": "operator"}
	if len(out.Env) != len(want) {
		t.Fatalf("Env = %v, want %v", out.Env, want)
	}
	for k, v := range want {
		if out.Env[k] != v {
			t.Errorf("Env[%s] = %q, want %q", k, out.Env[k], v)
		}
	}
	if base.Env["API_INTERNAL"] != "" {
		t.Error("merge wrote into the manifest's own map")
	}
}

func TestManifestDomainFlowsToDeploySpec(t *testing.T) {
	m, err := parseManifest([]byte("version: 1\napp: demo\ndomain: app.example.com\n"))
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}
	spec, err := manifestToDeploySpec(m, defaultLimitsMultiplier)
	if err != nil {
		t.Fatalf("manifestToDeploySpec: %v", err)
	}
	if spec.Domain != "app.example.com" {
		t.Errorf("Domain = %q, want app.example.com", spec.Domain)
	}
	// merge with an operator override that leaves domain unset -> manifest wins
	out := mergeDeploySpec(spec, deploySpecFields{CPULimit: 0.5})
	if out.Domain != "app.example.com" {
		t.Errorf("Domain lost in merge: %q", out.Domain)
	}
}

func TestMergeDeploySpecIdentityWhenBaseZero(t *testing.T) {
	override := deploySpecFields{
		Command:       []string{"only-cmd"},
		RestartPolicy: "always",
	}
	out := mergeDeploySpec(deploySpecFields{}, override)
	if out.Command[0] != "only-cmd" || out.RestartPolicy != "always" {
		t.Errorf("merge with zero base should equal override: %+v", out)
	}
}

func TestManifestHealthToSpec(t *testing.T) {
	// empty -> nil, nil
	hc, err := manifestHealthToSpec(manifestHealth{})
	if err != nil || hc != nil {
		t.Fatalf("empty health should be (nil, nil), got (%+v, %v)", hc, err)
	}

	// http + port -> ok
	hc, err = manifestHealthToSpec(manifestHealth{HTTP: "/healthz", Port: 8080})
	if err != nil {
		t.Fatalf("http+port should be valid: %v", err)
	}
	if hc.Type != "http" || hc.HTTPPath != "/healthz" || hc.HTTPPort != 8080 {
		t.Errorf("unexpected http spec: %+v", hc)
	}

	// exec -> ok
	hc, err = manifestHealthToSpec(manifestHealth{Exec: []string{"sh", "-c", "true"}})
	if err != nil {
		t.Fatalf("exec should be valid: %v", err)
	}
	if hc.Type != "exec" {
		t.Errorf("unexpected exec spec: %+v", hc)
	}

	// http and exec both set -> error
	if _, err := manifestHealthToSpec(manifestHealth{HTTP: "/x", Exec: []string{"true"}}); err == nil {
		t.Fatal("http+exec together should error")
	}

	// health block present (Failures set) but neither http nor exec -> error
	if _, err := manifestHealthToSpec(manifestHealth{Failures: 3}); err == nil {
		t.Fatal("health block with no probe should error, not silently drop")
	}

	// http without port -> error
	if _, err := manifestHealthToSpec(manifestHealth{HTTP: "/healthz"}); err == nil {
		t.Fatal("http probe without port should error")
	}
}

func TestManifestDurationSeconds(t *testing.T) {
	secs, err := manifestDurationSeconds("f", "")
	if err != nil || secs != 0 {
		t.Fatalf("empty string should be 0, nil, got %d, %v", secs, err)
	}
	secs, err = manifestDurationSeconds("f", "10s")
	if err != nil || secs != 10 {
		t.Fatalf("10s should be 10, nil, got %d, %v", secs, err)
	}
	secs, err = manifestDurationSeconds("f", "1m")
	if err != nil || secs != 60 {
		t.Fatalf("1m should be 60, nil, got %d, %v", secs, err)
	}
	if _, err := manifestDurationSeconds("f", "500ms"); err == nil {
		t.Fatal("sub-second duration should error")
	}
	if _, err := manifestDurationSeconds("f", "not-a-duration"); err == nil {
		t.Fatal("garbage duration should error")
	}
}

func TestParseManifestPort(t *testing.T) {
	ps, err := parseManifestPort("8080:80")
	if err != nil || ps.HostPort != 8080 || ps.ContainerPort != 80 {
		t.Fatalf("8080:80 parse wrong: %+v, %v", ps, err)
	}
	ps, err = parseManifestPort("80")
	if err != nil || ps.HostPort != 0 || ps.ContainerPort != 80 {
		t.Fatalf("bare 80 parse wrong: %+v, %v", ps, err)
	}
	if _, err := parseManifestPort("abc:80"); err == nil {
		t.Fatal("invalid host port should error")
	}
	if _, err := parseManifestPort("80:xyz"); err == nil {
		t.Fatal("invalid container port should error")
	}
	if _, err := parseManifestPort("xyz"); err == nil {
		t.Fatal("invalid bare port should error")
	}
}

func TestMaskRepoCredentials(t *testing.T) {
	repoURL := "https://user:token123@github.com/org/repo.git"
	msg := "clone failed: https://user:token123@github.com/org/repo.git: auth error"
	masked := maskRepoCredentials(msg, repoURL)
	if strings.Contains(masked, "token123") {
		t.Errorf("token should be masked: %q", masked)
	}
	if !strings.Contains(masked, "***@") {
		t.Errorf("expected ***@ mask, got: %q", masked)
	}

	// decoded-password path (F7): message contains the raw (non-percent-encoded)
	// user:pass combo, distinct from what url.Parse's userinfo string would be.
	repoURL2 := "https://user:my%2Fsecretpw@github.com/org/repo.git"
	msg2 := "build failed using user:my/secretpw somewhere in stderr"
	masked2 := maskRepoCredentials(msg2, repoURL2)
	if strings.Contains(masked2, "my/secretpw") {
		t.Errorf("decoded password should be masked: %q", masked2)
	}

	// no userinfo -> message unchanged
	plainURL := "https://github.com/org/repo.git"
	plainMsg := "clone failed: https://github.com/org/repo.git: not found"
	if got := maskRepoCredentials(plainMsg, plainURL); got != plainMsg {
		t.Errorf("message without credentials should be unchanged, got: %q", got)
	}
}

func TestResolveWithinNormalRelativePath(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(root+"/sub/dir", 0o755); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveWithin(root, "sub/dir")
	if err != nil {
		t.Fatalf("normal relative path should resolve: %v", err)
	}
	if resolved == "" {
		t.Fatal("expected non-empty resolved path")
	}
}

func TestResolveWithinRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := outside + "/secret"
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := root + "/escape"
	if err := os.Symlink(outsideFile, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveWithin(root, "escape"); err == nil {
		t.Fatal("symlink pointing outside root should be rejected")
	}
}

func TestResolveWithinNonexistentLeafUsesParent(t *testing.T) {
	root := t.TempDir()
	// leaf "Dockerfile" does not exist yet, but its parent (root) does.
	resolved, err := resolveWithin(root, "Dockerfile")
	if err != nil {
		t.Fatalf("nonexistent leaf with existing parent should resolve via parent: %v", err)
	}
	if resolved == "" {
		t.Fatal("expected non-empty resolved path")
	}
}

func TestResolveWithinNonexistentLeafInSymlinkedParentEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := root + "/escapedir"
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// "escapedir/newfile" - leaf doesn't exist, but its parent (escapedir) is
	// a symlink pointing outside root.
	if _, err := resolveWithin(root, "escapedir/newfile"); err == nil {
		t.Fatal("nonexistent leaf under a symlinked-outside parent should be rejected")
	}
}

// --- M21: requires -> resource rename, description, build.template ---
// --- M22: resource -> requests/limits split ---

func TestParseManifestRequestsParsesNormally(t *testing.T) {
	m, err := parseManifest([]byte("version: 1\napp: foo\nrequests:\n  cpu: 0.5\n  memory: 256Mi\n  arch: [amd64, arm64]\n"))
	if err != nil {
		t.Fatalf("requests: should parse: %v", err)
	}
	if m.Requests.CPU != 0.5 {
		t.Errorf("Requests.CPU = %v, want 0.5", m.Requests.CPU)
	}
	if m.Requests.Memory != "256Mi" {
		t.Errorf("Requests.Memory = %q, want 256Mi", m.Requests.Memory)
	}
	if len(m.Requests.Arch) != 2 || m.Requests.Arch[0] != "amd64" || m.Requests.Arch[1] != "arm64" {
		t.Errorf("Requests.Arch = %v, want [amd64 arm64]", m.Requests.Arch)
	}
}

func TestParseManifestLegacyRequiresWithContentRejected(t *testing.T) {
	_, err := parseManifest([]byte("version: 1\napp: foo\nrequires:\n  cpu: 0.5\n"))
	if err == nil {
		t.Fatal("requires: with content should be rejected")
	}
	if !strings.Contains(err.Error(), "requests:") || strings.Contains(err.Error(), "resource:") {
		t.Errorf("error should point straight at requests:, not at the also-retired resource:, got: %v", err)
	}
}

func TestParseManifestLegacyResourceWithContentRejected(t *testing.T) {
	_, err := parseManifest([]byte("version: 1\napp: foo\nresource:\n  cpu: 0.5\n"))
	if err == nil {
		t.Fatal("resource: with content should be rejected")
	}
	if !strings.Contains(err.Error(), "requests") {
		t.Errorf("error should mention requests: as the replacement, got: %v", err)
	}
}

func TestParseManifestLegacyResourceBareNullAllowed(t *testing.T) {
	if _, err := parseManifest([]byte("version: 1\napp: foo\nresource:\n")); err != nil {
		t.Fatalf("bare resource: (null) should not error: %v", err)
	}
}

func TestParseManifestLegacyResourceEmptyMapRejected(t *testing.T) {
	_, err := parseManifest([]byte("version: 1\napp: foo\nresource: {}\n"))
	if err == nil {
		t.Fatal("resource: {} should be rejected")
	}
	if !strings.Contains(err.Error(), "requests") {
		t.Errorf("error should mention requests: as the replacement, got: %v", err)
	}
}

func TestParseManifestLegacyRequiresBareNullAllowed(t *testing.T) {
	// A bare `requires:` key with no value (null scalar) is tolerated —
	// nothing to migrate.
	if _, err := parseManifest([]byte("version: 1\napp: foo\nrequires:\n")); err != nil {
		t.Fatalf("bare requires: (null) should not error: %v", err)
	}
}

func TestParseManifestLegacyRequiresEmptyMapRejected(t *testing.T) {
	// An explicit empty map is content, not "unset" — strict rejection.
	_, err := parseManifest([]byte("version: 1\napp: foo\nrequires: {}\n"))
	if err == nil {
		t.Fatal("requires: {} should be rejected")
	}
	if !strings.Contains(err.Error(), "requests:") || strings.Contains(err.Error(), "resource:") {
		t.Errorf("error should point straight at requests:, not at the also-retired resource:, got: %v", err)
	}
}

func TestParseManifestDescriptionPassesThrough(t *testing.T) {
	m, err := parseManifest([]byte("version: 1\napp: foo\ndescription: a demo app\n"))
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}
	if m.Description != "a demo app" {
		t.Errorf("Description = %q, want %q", m.Description, "a demo app")
	}
}

func TestParseManifestTemplateAndDockerfileMutuallyExclusive(t *testing.T) {
	_, err := parseManifest([]byte("version: 1\napp: foo\nbuild:\n  template: go\n  dockerfile: Dockerfile\n"))
	if err == nil {
		t.Fatal("build.template + build.dockerfile together should be rejected")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error should say mutually exclusive, got: %v", err)
	}
}

func TestParseManifestTemplateAlone(t *testing.T) {
	m, err := parseManifest([]byte("version: 1\napp: foo\nbuild:\n  template: go\n  template_vars:\n    go_version: \"1.24\"\n"))
	if err != nil {
		t.Fatalf("build.template alone should parse: %v", err)
	}
	if m.Build.Template != "go" {
		t.Errorf("Build.Template = %q, want go", m.Build.Template)
	}
	if m.Build.TemplateVars["go_version"] != "1.24" {
		t.Errorf("Build.TemplateVars[go_version] = %q, want 1.24", m.Build.TemplateVars["go_version"])
	}
}

func TestEffectiveArch(t *testing.T) {
	cases := []struct {
		reqArch     []string
		defaultArch string
		want        []string
	}{
		{nil, "", nil},
		{nil, "amd64", []string{"amd64"}},
		{[]string{"arm64"}, "amd64", []string{"arm64"}},
	}
	for _, c := range cases {
		got := effectiveArch(c.reqArch, c.defaultArch)
		if len(got) != len(c.want) {
			t.Errorf("effectiveArch(%v, %q) = %v, want %v", c.reqArch, c.defaultArch, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("effectiveArch(%v, %q) = %v, want %v", c.reqArch, c.defaultArch, got, c.want)
				break
			}
		}
	}
}

func TestDeriveContainerLimits(t *testing.T) {
	cases := []struct {
		name       string
		req        manifestRequests
		lim        manifestLimits
		multiplier float64
		wantCPU    float64
		wantMem    int64
		wantErr    bool
	}{
		{
			name:       "limits set wins outright",
			req:        manifestRequests{CPU: 0.25, Memory: "128Mi"},
			lim:        manifestLimits{CPU: 0.5, Memory: "256Mi"},
			multiplier: 2,
			wantCPU:    0.5,
			wantMem:    256 * 1024 * 1024,
		},
		{
			name:       "no limits, requests present, multiplier 2x",
			req:        manifestRequests{CPU: 0.25, Memory: "128Mi"},
			multiplier: 2,
			wantCPU:    0.5,
			wantMem:    256 * 1024 * 1024,
		},
		{
			name:       "neither requests nor limits -> unbounded",
			multiplier: 2,
			wantCPU:    0,
			wantMem:    0,
		},
		{
			name:       "only requests.cpu set, no requests.memory, no limits -> mem stays 0",
			req:        manifestRequests{CPU: 0.25},
			multiplier: 2,
			wantCPU:    0.5,
			wantMem:    0,
		},
		{
			name:       "multiplier 1.5x",
			req:        manifestRequests{CPU: 1, Memory: "100Mi"},
			multiplier: 1.5,
			wantCPU:    1.5,
			wantMem:    int64(float64(100*1024*1024) * 1.5),
		},
		{
			name:       "invalid limits.memory errors",
			lim:        manifestLimits{Memory: "not-a-quantity"},
			multiplier: 2,
			wantErr:    true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotCPU, gotMem, err := deriveContainerLimits(c.req, c.lim, c.multiplier)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error, got cpu=%v mem=%v", gotCPU, gotMem)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotCPU != c.wantCPU {
				t.Errorf("cpu = %v, want %v", gotCPU, c.wantCPU)
			}
			if gotMem != c.wantMem {
				t.Errorf("mem = %v, want %v", gotMem, c.wantMem)
			}
		})
	}
}
