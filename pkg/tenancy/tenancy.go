// Package tenancy is the one definition of how a shared-node namespace is spelled:
// "<account>--<namespace>".
//
// It is the contract between the external manager, which composes these names, and
// qooqd and the watcher, which only read them. Those live in different modules
// (the external manager is not part of the standalone release), so the rule has to
// sit somewhere both can import. Before this package it was written out three
// times; if one copy changed, a user's workload would quietly stop getting the
// isolation the other copies assume, and nothing would fail.
//
// Validation of the two halves (lengths, character set, reserved names) is the
// composer's job and stays in the external manager. This package only knows the
// separator.
package tenancy

import "strings"

// Separator joins the account and namespace halves. Two hyphens, because a
// single one is ambiguous ("alice" + "prod-x" and "alice-prod" + "x" would both
// give "alice-prod-x"); the composer forbids it inside either half.
const Separator = "--"

// Join composes a shared-node namespace. It does not validate the halves.
func Join(account, namespace string) string {
	return account + Separator + namespace
}

// Split recovers the two halves. ok is false unless there is exactly one
// separator with something on both sides — never a prefix match, or account
// "acme" would claim "acme-corp--prod".
func Split(qualified string) (account, namespace string, ok bool) {
	parts := strings.Split(qualified, Separator)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// IsTenant reports whether a namespace belongs to a external-manager account.
//
// Deliberately looser than Split: anything containing the separator is treated
// as a user's. The checks that hang off this are restrictions (own network,
// no self-chosen domain, routing only under the app domain), so a malformed
// name must fall on the restricted side rather than escape it.
func IsTenant(namespace string) bool {
	return strings.Contains(namespace, Separator)
}
