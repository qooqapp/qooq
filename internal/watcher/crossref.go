package watcher

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/qooqapp/qooq/pkg/client"
)

// Fields ${component.<name>.<field>} understands. They are split along the one
// line a manifest author actually has to decide: whether the caller is another
// container on the same network, or a browser somewhere else.
//
// qooq deliberately does not guess which. Both are correct addresses for the
// same component, they are never interchangeable, and picking the wrong one
// fails silently — the container runs, its health probe passes, and only the
// browser sees the error. So the manifest says which it means.
const (
	// Inside the network.
	crossRefHost    = "host"    // the network alias: "api"
	crossRefPort    = "port"    // the container port: 8080
	crossRefAddress = "address" // both: "api:8080"

	// From outside.
	crossRefHostPort = "host_port" // the published port: 19000
	crossRefEndpoint = "endpoint"  // a URL: "http://192.168.1.50:19000"
)

// componentResolver answers ${component.<name>.<field>} from what qooqd
// reports about the components it is running.
//
// The values come from the daemon rather than from the other app's repo,
// following the rule the stack layer already set: the thing being referenced
// is a deployment, not a source tree, and qooq does not read other people's
// repositories to find out about it.
type componentResolver struct {
	ctx       context.Context
	client    *client.Client
	namespace string

	// comps caches one ListComponents call for the whole expansion: a manifest
	// naming two fields of the same component, or two components, should cost
	// one round trip, not one per reference.
	comps  []client.ComponentResponse
	loaded bool
	failed bool

	// pending records a component that was referenced but is not deployed yet,
	// which is a different condition from a typo: it fixes itself once that
	// component exists, so the deploy waits rather than failing.
	pending []string

	// referenced is every component name the manifest addressed, found or not.
	referenced map[string]bool
}

// references returns the sorted, de-duplicated names that were addressed.
func (r *componentResolver) references() []string {
	out := make([]string, 0, len(r.referenced))
	for n := range r.referenced {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// resolve implements the scope's prefix resolver. rest is "<name>.<field>".
func (r *componentResolver) resolve(rest string) (string, bool) {
	name, field, ok := cutLast(rest, ".")
	if !ok {
		return "", false
	}
	// Cross-namespace references would need a separator the reference grammar
	// does not have, so a name here is always one in this deployment's own
	// namespace — which is what a stack's projects share, and what makes the
	// common case work without saying anything.
	comp := r.lookup(name)
	if comp == nil {
		return "", false
	}

	switch field {
	case crossRefHost:
		return comp.Name, true
	case crossRefPort:
		if p, ok := firstPort(comp); ok {
			return fmt.Sprint(p.ContainerPort), true
		}
	case crossRefAddress:
		if p, ok := firstPort(comp); ok {
			return fmt.Sprintf("%s:%d", comp.Name, p.ContainerPort), true
		}
	case crossRefHostPort:
		if p, ok := firstPort(comp); ok && p.HostPort != 0 {
			return fmt.Sprint(p.HostPort), true
		}
	case crossRefEndpoint:
		if len(comp.Endpoints) > 0 {
			return comp.Endpoints[0], true
		}
	}
	return "", false
}

// lookup finds a component by name in this resolver's namespace, recording a
// miss as pending rather than as absent.
func (r *componentResolver) lookup(name string) *client.ComponentResponse {
	if r.referenced == nil {
		r.referenced = map[string]bool{}
	}
	r.referenced[name] = true
	if !r.loaded {
		r.loaded = true
		comps, err := r.client.ListComponents(r.ctx)
		if err != nil {
			r.failed = true
			return nil
		}
		r.comps = comps
	}
	if r.failed {
		return nil
	}
	for i := range r.comps {
		c := &r.comps[i]
		if c.Namespace == r.namespace && c.Name == name && c.Phase != "Removed" {
			return c
		}
	}
	r.note(name)
	return nil
}

func (r *componentResolver) note(name string) {
	for _, n := range r.pending {
		if n == name {
			return
		}
	}
	r.pending = append(r.pending, name)
}

// waitingFor describes what this deploy is waiting to exist, or "" if nothing.
func (r *componentResolver) waitingFor() string {
	if len(r.pending) == 0 {
		return ""
	}
	names := append([]string(nil), r.pending...)
	sort.Strings(names)
	noun := "component"
	if len(names) > 1 {
		noun = "components"
	}
	return fmt.Sprintf("waiting for %s %s in namespace %q to be deployed", noun, strings.Join(names, ", "), r.namespace)
}

// firstPort is the port a bare reference means. It is the first entry of
// run.ports, the same rule ingress.port already follows when it is omitted, so
// a manifest author does not have to learn a second one.
func firstPort(c *client.ComponentResponse) (client.PortSpec, bool) {
	if len(c.Ports) == 0 {
		return client.PortSpec{}, false
	}
	return c.Ports[0], true
}

// cutLast splits around the final occurrence of sep. Component names are DNS
// labels and so contain no dots, which makes the last one the field separator.
func cutLast(s, sep string) (before, after string, found bool) {
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return s, "", false
	}
	return s[:i], s[i+len(sep):], true
}

// explain says why a ${component....} reference did not resolve, so the error
// distinguishes the two cases that look identical from the outside: a name
// spelled wrong, and a name spelled right for something not deployed yet. The
// generic "unknown reference (available: ...)" is actively misleading for the
// second — it invites someone to hunt for a typo in a line that is correct.
func (r *componentResolver) explain(ref string) (string, bool) {
	rest, ok := strings.CutPrefix(ref, "component.")
	if !ok {
		return "", false
	}
	name, field, ok := cutLast(rest, ".")
	if !ok {
		return fmt.Sprintf("expected component.<name>.<field>, with field one of %s", strings.Join(crossRefFields(), ", ")), true
	}
	if r.failed {
		return "qooqd could not be asked which components are running", true
	}
	if r.lookupCached(name) == nil {
		return fmt.Sprintf("component %q is not deployed in namespace %q yet", name, r.namespace), true
	}
	if !validCrossRefField(field) {
		return fmt.Sprintf("%q is not a field of a component (valid: %s)", field, strings.Join(crossRefFields(), ", ")), true
	}
	// The component and the field are both real, so the value is genuinely
	// absent — an unpublished port has no host_port and no endpoint.
	return fmt.Sprintf("component %q has no %s (it publishes no host port)", name, field), true
}

// lookupCached repeats a lookup without recording a new pending entry: explain
// runs after resolve already did, and must not add to what the deploy claims
// to be waiting for.
func (r *componentResolver) lookupCached(name string) *client.ComponentResponse {
	for i := range r.comps {
		c := &r.comps[i]
		if c.Namespace == r.namespace && c.Name == name && c.Phase != "Removed" {
			return c
		}
	}
	return nil
}

func crossRefFields() []string {
	return []string{crossRefHost, crossRefPort, crossRefAddress, crossRefHostPort, crossRefEndpoint}
}

func validCrossRefField(field string) bool {
	for _, f := range crossRefFields() {
		if f == field {
			return true
		}
	}
	return false
}
