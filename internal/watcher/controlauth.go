package watcher

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/controlclient"
)

// GitHub App auth, from the watcher's side.
//
// The watcher never holds the App's private key. It asks the external manager for a
// credential, naming the namespace it is building for and the repository it
// wants — and the external manager decides whether that namespace's account may
// have it.
//
// This is the one call that goes upward, and it was chosen over the alternative
// of giving the watcher the key. The key would have meant the watcher knowing
// about accounts, which is the wall that keeps the offline product free of the
// external manager: a node that understands accounts is a node that needs Postgres.
// One reverse HTTP call costs less than that.
//
// A watcher with no external manager configured — every offline install — is
// unaffected: the auth type below is simply unusable, and says so.

// appTokenTTLMargin is how early a cached credential is discarded.
//
// The external manager already keeps its own margin against the real expiry; this
// one protects against holding a token across the gap between fetching it and
// the clone that uses it.
const appTokenTTLMargin = 2 * time.Minute

// controlLink is how this watcher reaches its external manager, if it has one.
type controlLink struct {
	client *controlclient.Client

	mu     sync.Mutex
	cached map[string]*cachedCloneToken
}

type cachedCloneToken struct {
	username  string
	token     string
	expiresAt time.Time
}

var errNoControlPlane = errors.New(
	"auth_type=github-app needs a control plane, and none is configured (set control.addr)")

// cloneCredential returns a username and password for cloning repoURL on behalf
// of namespace.
//
// Cached per (namespace, repo) because the poll loop authenticates on every
// check of every target: without this, a hundred targets polling once a minute
// would be a hundred requests a minute to the external manager and, behind it, to
// GitHub's rate limit.
func (l *controlLink) cloneCredential(namespace, repoURL string) (username, password string, err error) {
	if l == nil || l.client == nil {
		return "", "", errNoControlPlane
	}
	key := namespace + "\x00" + repoURL
	now := time.Now()

	l.mu.Lock()
	if c, ok := l.cached[key]; ok && now.Before(c.expiresAt.Add(-appTokenTTLMargin)) {
		u, p := c.username, c.token
		l.mu.Unlock()
		return u, p, nil
	}
	l.mu.Unlock()

	// Its own context rather than the caller's. The git helpers this sits under
	// take no context (they are called from the stack path too, which has
	// none), so a cancelled build can wait out this request — bounded at the
	// timeout below, and only ever before the clone starts. Threading a context
	// through four call sites to save at most this long was not worth the churn;
	// if it ever is, the change is mechanical.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	resp, err := l.client.CloneToken(ctx, controlclient.CloneTokenRequest{
		Namespace: namespace,
		RepoURL:   repoURL,
	})
	if err != nil {
		// The external manager's own message is kept, because it is the one that
		// says something useful — "install the app, or re-grant it if the
		// authorization was removed" is exactly what §3 asks to be legible, and
		// it ends up in LastBuildError where the operator will read it.
		//
		// What is added is who is at fault, for the one consumer that has to
		// decide whether to stop retrying (see errNotTheRepository). The
		// message is unchanged by the mark.
		if isControlUnreachable(err) {
			return "", "", notTheRepository(err)
		}
		return "", "", err
	}

	l.mu.Lock()
	if l.cached == nil {
		l.cached = map[string]*cachedCloneToken{}
	}
	l.cached[key] = &cachedCloneToken{
		username: resp.Username, token: resp.Token, expiresAt: resp.ExpiresAt,
	}
	l.mu.Unlock()

	return resp.Username, resp.Token, nil
}

// forget drops cached credentials for a namespace.
//
// Used when a credential is refused after having worked: the cached one is
// certainly not the problem, but keeping it would make the next attempt reuse a
// token the external manager may have just stopped standing behind.
func (l *controlLink) forget(namespace string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	for k := range l.cached {
		if len(k) > len(namespace) && k[:len(namespace)] == namespace && k[len(namespace)] == 0 {
			delete(l.cached, k)
		}
	}
	l.mu.Unlock()
}

// initControlLink builds the link from configuration, or leaves it nil.
func (m *Watcher) initControlLink(addr, token string) {
	if addr == "" {
		// The overwhelmingly common case: an offline watcher. Nothing is logged
		// at anything louder than debug, because there is nothing wrong.
		return
	}
	m.control = &controlLink{
		client: controlclient.New(addr, controlclient.WithToken(token)),
		cached: map[string]*cachedCloneToken{},
	}
	m.log.Info("control plane configured for GitHub App clone credentials",
		zap.String("addr", addr), zap.Bool("token_set", token != ""))
	if token == "" {
		// Fail-closed on the other side means this only produces 503s, which is
		// confusing unless it is said here.
		m.log.Warn("no control token set; clone-credential requests will be refused",
			zap.String("set", configControlToken))
	}
}

// appCloneAuth is what git.go calls. Separated so the git helpers stay free of
// the Watcher type and remain unit-testable without one.
func (m *Watcher) appCloneAuth() appAuthFunc {
	if m.control == nil {
		return nil
	}
	return m.control.cloneCredential
}

// isControlUnreachable reports whether the external manager failed to answer, as
// opposed to answering "no".
//
// Status 0 is no answer at all: a timeout, a refused connection, a name that
// did not resolve, or a 200 with nothing usable in it. 5xx is an answer saying
// the external manager itself is broken. Everything else — 401, 403, 404, a
// request it would not read — is a decision or a misconfiguration that a person
// has to act on, and those are exactly what the give-up mechanism is for.
//
// Deliberately decided on the status and not on the message: this package has
// twice had to undo matching on error text (pkg/client, pkg/watcherclient), and
// a reword would make the classification silently wrong in the direction that
// pauses people's projects.
func isControlUnreachable(err error) bool {
	code := controlclient.StatusCode(err)
	return code == 0 || code >= 500
}

// isAuthRefused reports whether an error from the external manager means "you may
// not have this" rather than "it is broken", so a caller can tell a
// misconfiguration from an outage.
func isAuthRefused(err error) bool {
	switch controlclient.StatusCode(err) {
	case http.StatusForbidden, http.StatusUnauthorized, http.StatusNotFound:
		return true
	}
	return false
}
