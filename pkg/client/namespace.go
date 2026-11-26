package client

// DefaultNamespace is the namespace a component lands in when none is given.
// Unlike the empty string it replaced, it is a real stored value: every
// component has a namespace, and `namespace: default` and an omitted namespace
// are the same namespace rather than two spellings of "none".
//
// This lives in the wire package because the daemon, the watcher and the CLI
// all have to agree on it exactly. Any of them treating "" and "default" as
// different would split one component into two that share a displayed name.
const DefaultNamespace = "default"

// NormalizeNamespace fills in the default. Every path that accepts a namespace
// from outside calls it, and so does every path that reads one that may have
// been stored before the default existed — which is why it is written to be
// safe to apply twice.
func NormalizeNamespace(ns string) string {
	if ns == "" {
		return DefaultNamespace
	}
	return ns
}
