package tenancy

import "testing"

func TestJoinAndSplitRoundTrip(t *testing.T) {
	q := Join("alice", "prod")
	if q != "alice--prod" {
		t.Fatalf("Join = %q, want alice--prod", q)
	}
	a, n, ok := Split(q)
	if !ok || a != "alice" || n != "prod" {
		t.Fatalf("Split(%q) = %q, %q, %v", q, a, n, ok)
	}
}

func TestSplitRejectsAnythingButOneSeparator(t *testing.T) {
	for _, s := range []string{
		"default",          // no separator: not a tenant's
		"alice-prod",       // single hyphen is not the separator
		"--prod",           // empty account
		"alice--",          // empty namespace
		"a--b--c",          // two separators: ambiguous
		"alice-corp--prod", // fine on its own, but checked below against prefix matching
	} {
		a, _, ok := Split(s)
		if s == "alice-corp--prod" {
			if !ok || a != "alice-corp" {
				t.Fatalf("Split(%q) account = %q, %v; want alice-corp (the whole left half, not a prefix)", s, a, ok)
			}
			continue
		}
		if ok {
			t.Fatalf("Split(%q) ok, want rejected", s)
		}
	}
}

func TestIsTenantFailsTowardsRestricted(t *testing.T) {
	for s, want := range map[string]bool{
		"default":     false,
		"blog":        false,
		"alice-prod":  false,
		"alice--prod": true,
		"a--b--c":     true, // malformed, but must not escape the tenant restrictions
		"--x":         true,
	} {
		if got := IsTenant(s); got != want {
			t.Fatalf("IsTenant(%q) = %v, want %v", s, got, want)
		}
	}
}
