package client

import "testing"

// An omitted namespace and an explicit "default" have to be the same namespace,
// or a component deployed one way becomes invisible to a lookup written the
// other. Applying the fill-in twice must also be a no-op: it runs both on the
// way in from a client and on the way out of a store written before the default
// existed, and those paths overlap.
func TestNormalizeNamespace(t *testing.T) {
	cases := []struct{ in, want string }{
		{in: "", want: DefaultNamespace},
		{in: DefaultNamespace, want: DefaultNamespace},
		{in: "acme", want: "acme"},
	}
	for _, c := range cases {
		got := NormalizeNamespace(c.in)
		if got != c.want {
			t.Errorf("NormalizeNamespace(%q) = %q, want %q", c.in, got, c.want)
		}
		if again := NormalizeNamespace(got); again != c.want {
			t.Errorf("NormalizeNamespace applied twice to %q = %q, want %q", c.in, again, c.want)
		}
	}
}
