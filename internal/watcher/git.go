package watcher

import (
	"errors"
	"fmt"
	"os"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/plumbing/transport/ssh"
	cryptossh "golang.org/x/crypto/ssh"

	"github.com/qooqapp/qooq/pkg/client"
)

// sshHostKeyPolicy decides how SSH clones verify the remote host key. A
// headless watcher has no interactive known_hosts, so without an explicit
// policy the first clone of any real host fails with "knownhosts: key is
// unknown". The three states are mutually ordered by callback():
//   - Insecure: skip verification entirely (dev/test only; logs a warning).
//   - KnownHostsPath set: verify against that known_hosts file.
//   - both zero: return a nil callback, leaving go-git's strict default
//     (its own known_hosts DB) in force.
type sshHostKeyPolicy struct {
	KnownHostsPath string
	Insecure       bool
}

// callback resolves the policy into a crypto/ssh HostKeyCallback. A nil
// callback (with nil error) means "let go-git apply its default".
func (p sshHostKeyPolicy) callback() (cryptossh.HostKeyCallback, error) {
	if p.Insecure {
		return cryptossh.InsecureIgnoreHostKey(), nil
	}
	if p.KnownHostsPath != "" {
		return ssh.NewKnownHostsCallback(p.KnownHostsPath)
	}
	return nil, nil
}

// sshUserFromURL extracts the SSH login user from a git remote URL, defaulting
// to "git" (GitHub/GitLab convention) when the URL carries no user or cannot be
// parsed. Self-hosted servers that use a different account (e.g. "gitea") are
// honored via scp-style URLs like "gitea@host:org/repo.git".
func sshUserFromURL(raw string) string {
	ep, err := transport.NewEndpoint(raw)
	if err != nil || ep.User == "" {
		return "git"
	}
	return ep.User
}

// appAuthFunc fetches a clone credential for a namespace from somewhere this
// package does not know about — the external manager, in practice. nil means this
// watcher has none, which is every offline install.
type appAuthFunc func(namespace, repoURL string) (username, password string, err error)

// gitAuth is everything the git helpers need to authenticate.
//
// A struct rather than a growing parameter list: the four call sites pass
// w.gitAuth() and adding a third kind of credential does not touch them.
type gitAuth struct {
	hostKey sshHostKeyPolicy
	// appToken is nil unless a external manager is configured.
	appToken appAuthFunc
}

// authMethodFor builds the go-git transport.AuthMethod for a target based on
// its AuthType. "none" returns (nil, nil): unauthenticated access.
func authMethodFor(t *Target, ga gitAuth) (transport.AuthMethod, error) {
	hk := ga.hostKey
	switch t.AuthType {
	case "", AuthTypeNone:
		return nil, nil
	case AuthTypeGitHubApp:
		// Nothing is stored on the target for this: no token, not even the
		// installation id. It is fetched per use
		// and expires by itself, so a target row is never a credential.
		if ga.appToken == nil {
			return nil, errNoControlPlane
		}
		// A namespace that names no account can never be authorised, and the
		// watcher can say so without a round trip. Note namespaceOf normalises
		// an absent namespace to "default" — the offline default, which belongs
		// to nobody — so checking for the empty string alone would send a
		// request that is certain to be refused, and the operator would get the
		// external manager's "bad request" instead of being told what is actually
		// wrong with the target.
		//
		// Whether any other namespace really belongs to the asking account is
		// not knowable here, and deliberately so: that is the external manager's
		// decision, and duplicating it would put a second opinion about
		// ownership on the node.
		ns := namespaceOf(t)
		if ns == "" || ns == client.DefaultNamespace {
			return nil, fmt.Errorf(
				"auth_type=%s needs a namespace that names an account, and this target's is %q",
				AuthTypeGitHubApp, ns)
		}
		user, pass, err := ga.appToken(ns, t.RepoURL)
		if err != nil {
			if isAuthRefused(err) {
				// The external manager answered "no" (not installed, not
				// selected, another account's...). The repository may be
				// public, so the caller tries without credentials and only
				// reports this if that fails too — see withAppFallback.
				return nil, &appRefusal{err: err}
			}
			return nil, err
		}
		return &githttp.BasicAuth{Username: user, Password: pass}, nil
	case AuthTypeToken:
		if t.AuthCredential == "" {
			return nil, errors.New("auth_type=token but auth_credential is empty")
		}
		// "git" is the conventional username GitHub expects for token auth
		// over HTTPS; GitLab (and most other hosts) accept any non-empty
		// username alongside the token as the password.
		return &githttp.BasicAuth{Username: "git", Password: t.AuthCredential}, nil
	case AuthTypeSSHKey:
		if t.AuthCredential == "" {
			return nil, errors.New("auth_type=ssh-key but auth_credential (PEM) is empty")
		}
		cb, err := hk.callback()
		if err != nil {
			return nil, fmt.Errorf("ssh host key policy: %w", err)
		}
		pk, err := ssh.NewPublicKeys(sshUserFromURL(t.RepoURL), []byte(t.AuthCredential), "")
		if err != nil {
			return nil, err
		}
		if cb != nil {
			pk.HostKeyCallback = cb
		}
		return pk, nil
	default:
		return nil, fmt.Errorf("unknown auth_type %q", t.AuthType)
	}
}

// appRefusal is the external manager declining to give a GitHub App credential.
//
// Every github.com project on a shared node is created with App auth, including public
// ones nobody installed the App for, so a refusal is not yet a failure: the
// repository is tried anonymously. If that fails as well, the refusal is the
// error reported — it says which of "not installed", "this repository is not
// selected", "connected to another account" or "suspended" it is and what to
// do, where git would only say "Repository not found".
type appRefusal struct{ err error }

func (e *appRefusal) Error() string { return e.err.Error() }
func (e *appRefusal) Unwrap() error { return e.err }

// withAppFallback runs op with the target's credentials, or without any when
// the external manager refused App access, and decides which error to report.
func withAppFallback(t *Target, ga gitAuth, op func(transport.AuthMethod) error) error {
	auth, err := authMethodFor(t, ga)
	var refused *appRefusal
	if errors.As(err, &refused) {
		auth = nil
	} else if err != nil {
		return err
	}
	opErr := op(auth)
	if opErr != nil && refused != nil {
		return fmt.Errorf("GitHub App access: %w (without it: %v)", refused.err, opErr)
	}
	return opErr
}

// ResolveRemoteHead returns the commit hash that branch currently points to
// on the remote, without cloning (equivalent to `git ls-remote`).
func ResolveRemoteHead(t *Target, ga gitAuth) (string, error) {
	remote := git.NewRemote(nil, &config.RemoteConfig{
		Name: "origin",
		URLs: []string{t.RepoURL},
	})

	var refs []*plumbing.Reference
	err := withAppFallback(t, ga, func(auth transport.AuthMethod) error {
		var err error
		refs, err = remote.List(&git.ListOptions{Auth: auth})
		if err != nil {
			return fmt.Errorf("list remote refs: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	branchRef := plumbing.NewBranchReferenceName(t.Branch)
	for _, ref := range refs {
		if ref.Name() == branchRef {
			return ref.Hash().String(), nil
		}
	}
	return "", fmt.Errorf("branch %q not found on remote %s", t.Branch, t.RepoURL)
}

// cloneBranch does a shallow (depth 1), single-branch clone of t's repo/branch
// into destDir (which must already exist and be empty), returning the resolved
// HEAD commit hash.
func cloneBranch(t *Target, destDir string, ga gitAuth) (string, error) {
	var repo *git.Repository
	err := withAppFallback(t, ga, func(auth transport.AuthMethod) error {
		var err error
		repo, err = git.PlainClone(destDir, false, &git.CloneOptions{
			URL:           t.RepoURL,
			Auth:          auth,
			ReferenceName: plumbing.NewBranchReferenceName(t.Branch),
			SingleBranch:  true,
			Depth:         1,
		})
		if err != nil {
			return fmt.Errorf("clone: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("resolve HEAD after clone: %w", err)
	}
	return head.Hash().String(), nil
}

// mkTempCloneDir creates a fresh temp directory for a pipeline run's clone.
func mkTempCloneDir(name string) (string, error) {
	return os.MkdirTemp("", "qooq-watcher-"+name+"-")
}
