package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestIsLoopbackListen(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:7777":     true,
		"localhost:7777":     true,
		"[::1]:7777":         true,
		"127.0.0.5:7777":     true, // the whole 127/8 block is loopback
		"0.0.0.0:7777":       false,
		":7777":              false, // every interface, the case worth catching
		"[::]:7777":          false,
		"192.168.0.5:77":     false,
		"qooq.internal:7777": false,
		"garbage":            true, // unparseable: the listener reports the real error
	}
	for addr, want := range cases {
		if got := isLoopbackListen(addr); got != want {
			t.Errorf("isLoopbackListen(%q) = %v, want %v", addr, got, want)
		}
	}
}

func warnOutput(t *testing.T, listenAddr, token string, extraHosts []string) string {
	t.Helper()
	var buf bytes.Buffer
	warnExposedWithoutAuth(slog.New(slog.NewTextHandler(&buf, nil)), listenAddr, token, extraHosts)
	return buf.String()
}

func TestWarnExposedWithoutAuthStaysQuietWhenSafe(t *testing.T) {
	if out := warnOutput(t, "127.0.0.1:7777", "", nil); out != "" {
		t.Errorf("loopback with no token needs no warning, got: %s", out)
	}
	if out := warnOutput(t, "0.0.0.0:7777", "s3cret", nil); out != "" {
		t.Errorf("a token is the fix, so warning after it is set is noise, got: %s", out)
	}
}

func TestWarnExposedWithoutAuthWarnsOnExposure(t *testing.T) {
	out := warnOutput(t, "0.0.0.0:7777", "", nil)
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("binding past loopback with no token must warn, got: %s", out)
	}
	// The reader has to learn that requests get refused, not just that
	// something is off.
	if !strings.Contains(out, "refused") {
		t.Errorf("warning should say remote requests are refused, got: %s", out)
	}
}

func TestWarnExposedWithoutAuthWarnsHarderWithAllowlist(t *testing.T) {
	out := warnOutput(t, "0.0.0.0:7777", "", []string{"192.168.0.5:7777"})
	if !strings.Contains(out, "level=WARN") {
		t.Fatalf("expected a warning, got: %s", out)
	}
	// This is the genuinely open configuration: the message must name what is
	// reachable, not repeat the milder "requests will be refused" line.
	if strings.Contains(out, "refused") {
		t.Errorf("with an allowlist, requests are not refused — wrong message: %s", out)
	}
	if !strings.Contains(out, "arbitrary images") {
		t.Errorf("warning should say what an unauthenticated caller can do, got: %s", out)
	}
}
