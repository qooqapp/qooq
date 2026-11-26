// Package controlclient is the wire contract between a qooq watcher and a qooq
// external manager, plus a small client for it.
//
// It exists in pkg/ for the same reason pkg/watcherclient does: both sides need
// the same DTOs, and the watcher may not import the external manager (which is not in this
// repo) — the dependency wall that keeps the offline product free of it. A shared struct here is one definition; a
// hand-copied one on each side is two that drift.
//
// There is exactly one call, deliberately. The watcher asks for a credential it
// cannot compute; everything else about a build is already on the target.
package controlclient

import "time"

// CloneTokenRequest asks for permission to clone one repository.
//
// The namespace rather than an account: the watcher does not know accounts (it
// is not supposed to), but the qualified namespace it already holds carries the
// account in its first half. The external manager splits it — never by prefix
// matching, see account_manager.SplitQualifiedNamespace.
type CloneTokenRequest struct {
	Namespace string `json:"namespace"`
	RepoURL   string `json:"repo_url"`
}

// CloneTokenResponse is a credential with an expiry.
//
// Username travels with the token because the value GitHub requires
// ("x-access-token") is a GitHub detail, and the watcher is not the place to
// know it. A second git host would answer with its own convention and nothing on
// the watcher side would change.
type CloneTokenResponse struct {
	Username  string    `json:"username"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// CLITokenRequest exchanges a browser sign-in for a token. The
// external manager implements the flow it completes.
type CLITokenRequest struct {
	Code     string `json:"code"`
	Verifier string `json:"verifier"`
}

// CLITokenResponse is the credential, shown once and never again.
type CLITokenResponse struct {
	Token   string `json:"token"`
	Account string `json:"account"`
	User    string `json:"user"`
}

// AccountResponse is what GET /apis/v1/accounts/{slug} returns.
type AccountResponse struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Kind        string `json:"kind"`
	DisplayName string `json:"display_name,omitempty"`
	Plan        string `json:"plan,omitempty"`
}
