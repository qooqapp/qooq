package watcher

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/qooqapp/qooq/pkg/controlclient"
)

// The watcher's half of GitHub App auth: it holds no key and no token, and asks
// its external manager per use. These tests are about what happens when there is no
// external manager (every offline install), and about what is sent when there is.

func TestGitHubAppAuthNeedsAControlPlane(t *testing.T) {
	// The auth type is only settable in a managed deployment, but a row could
	// arrive from a restored database or a hand-written request, so the answer
	// has to be an error and not a nil credential that silently clones nothing.
	target := &Target{Name: "app", AuthType: AuthTypeGitHubApp, RepoURL: "https://github.com/o/r.git"}
	_, err := authMethodFor(target, gitAuth{})
	if !errors.Is(err, errNoControlPlane) {
		t.Fatalf("err = %v, want errNoControlPlane", err)
	}
	if !strings.Contains(err.Error(), "control.addr") {
		t.Errorf("the error does not name the setting that fixes it: %v", err)
	}
}

// Without a namespace there is no account to authorise against, and guessing one
// would be guessing whose repositories to open.
func TestGitHubAppAuthRefusesATargetWithNoNamespace(t *testing.T) {
	target := &Target{Name: "app", AuthType: AuthTypeGitHubApp, RepoURL: "https://github.com/o/r.git"}
	ga := gitAuth{appToken: func(ns, repo string) (string, string, error) {
		t.Fatal("a credential was requested for a target with no namespace")
		return "", "", nil
	}}
	if _, err := authMethodFor(target, ga); err == nil {
		t.Fatal("a namespace-less target was authorised")
	}
}

func TestGitHubAppAuthSendsTheNamespaceAndRepoAndUsesWhatComesBack(t *testing.T) {
	target := &Target{
		Name:       "app",
		AuthType:   AuthTypeGitHubApp,
		RepoURL:    "https://github.com/acme-org/app.git",
		DeploySpec: `{"namespace":"acme--prod"}`,
	}

	var gotNS, gotRepo string
	ga := gitAuth{appToken: func(ns, repo string) (string, string, error) {
		gotNS, gotRepo = ns, repo
		return "x-access-token", "ghs_token", nil
	}}

	auth, err := authMethodFor(target, ga)
	if err != nil {
		t.Fatalf("auth: %v", err)
	}
	basic, ok := auth.(*githttp.BasicAuth)
	if !ok {
		t.Fatalf("auth is %T, want BasicAuth", auth)
	}
	// The username comes from the answer rather than being assumed here: which
	// value GitHub wants is GitHub's business, and a second git host would send
	// its own.
	if basic.Username != "x-access-token" || basic.Password != "ghs_token" {
		t.Errorf("credential = %s/%s", basic.Username, basic.Password)
	}
	// The qualified namespace, not the bare one: the account half is what the
	// external manager authorises against.
	if gotNS != "acme--prod" {
		t.Errorf("namespace sent = %q, want acme--prod", gotNS)
	}
	if gotRepo != target.RepoURL {
		t.Errorf("repo sent = %q", gotRepo)
	}
}

// A refusal has to reach the operator intact: the external manager's message is the
// one that says what to do about it, and it ends up in LastBuildError.
func TestGitHubAppAuthKeepsTheControlPlanesMessage(t *testing.T) {
	target := &Target{
		AuthType: AuthTypeGitHubApp, RepoURL: "https://github.com/o/r.git",
		DeploySpec: `{"namespace":"acme--prod"}`,
	}
	ga := gitAuth{appToken: func(ns, repo string) (string, string, error) {
		return "", "", &controlclient.APIError{
			StatusCode: http.StatusForbidden,
			Message:    "github_app_not_installed: install it, or re-grant it if the authorization was removed",
		}
	}}
	_, err := authMethodFor(target, ga)
	if err == nil {
		t.Fatal("a refusal was swallowed")
	}
	if !strings.Contains(err.Error(), "re-grant") {
		t.Fatalf("the actionable part of the message was lost: %v", err)
	}
	if !isAuthRefused(err) {
		t.Error("a 403 was not classified as a refusal, so a caller cannot tell it from an outage")
	}
}

// Caching is required rather than nice: the poll loop authenticates on every
// check of every target, so one request per check would be a request per target
// per poll interval to the external manager and, behind it, to GitHub's rate limit.
func TestCloneCredentialsAreCachedUntilNearExpiry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer wtoken" {
			t.Errorf("Authorization = %q", got)
		}
		var req controlclient.CloneTokenRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Namespace == "" || req.RepoURL == "" {
			t.Errorf("incomplete request: %+v", req)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(controlclient.CloneTokenResponse{
			Username:  "x-access-token",
			Token:     "ghs_token",
			ExpiresAt: time.Now().Add(time.Hour),
		})
		_ = n
	}))
	defer srv.Close()

	l := &controlLink{
		client: controlclient.New(srv.URL, controlclient.WithToken("wtoken")),
		cached: map[string]*cachedCloneToken{},
	}

	for i := 0; i < 3; i++ {
		u, p, err := l.cloneCredential("acme--prod", "https://github.com/acme-org/app.git")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if u != "x-access-token" || p != "ghs_token" {
			t.Fatalf("call %d: %s/%s", i, u, p)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("%d requests for three clones, want 1", got)
	}

	// A different repository is a different credential — the installation
	// covering it may belong to a different grant entirely.
	if _, _, err := l.cloneCredential("acme--prod", "https://github.com/acme-org/other.git"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("%d requests, want 2 after asking about another repository", got)
	}

	// Inside the margin the cached one is dropped, so a credential is never
	// handed out with less life left than the clone might need.
	l.mu.Lock()
	for _, c := range l.cached {
		c.expiresAt = time.Now().Add(appTokenTTLMargin / 2)
	}
	l.mu.Unlock()
	if _, _, err := l.cloneCredential("acme--prod", "https://github.com/acme-org/app.git"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("%d requests, want 3 after the cached credential neared expiry", got)
	}
}

// forget must not take another namespace's credentials with it, and must not be
// fooled by a namespace that is a prefix of another.
func TestForgetDropsOnlyThatNamespace(t *testing.T) {
	l := &controlLink{cached: map[string]*cachedCloneToken{}}
	keys := []string{"acme--prod", "acme--staging", "acme-corp--prod", "other--prod"}
	for _, k := range keys {
		l.cached[k+"\x00repo"] = &cachedCloneToken{token: "t", expiresAt: time.Now().Add(time.Hour)}
	}

	l.forget("acme--prod")

	if _, ok := l.cached["acme--prod\x00repo"]; ok {
		t.Error("the named namespace was kept")
	}
	for _, k := range []string{"acme--staging", "acme-corp--prod", "other--prod"} {
		if _, ok := l.cached[k+"\x00repo"]; !ok {
			t.Errorf("%s was dropped too", k)
		}
	}
}

// An unreachable external manager is an error, not an empty credential: cloning
// anonymously would fail later with "repository not found", which reads as the
// repository having been deleted.
func TestAnUnreachableControlPlaneIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":"watcher_auth_not_configured","message":"not configured"}}`))
	}))
	defer srv.Close()

	l := &controlLink{client: controlclient.New(srv.URL), cached: map[string]*cachedCloneToken{}}
	_, _, err := l.cloneCredential("acme--prod", "https://github.com/o/r.git")
	if err == nil {
		t.Fatal("a 503 produced a credential")
	}
	if controlclient.StatusCode(err) != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", controlclient.StatusCode(err))
	}
	if isAuthRefused(err) {
		t.Error("a 503 was classified as a refusal; it is an outage and should be retried")
	}
}

// Every github.com project on a shared node is created with App auth, public ones too.
// A refusal therefore falls back to anonymous access, and is reported only
// when that fails — in which case it is the external manager's reason that
// reaches LastBuildError, not git's "Repository not found".
func TestAppRefusalFallsBackToAnonymousAndOtherwiseReportsTheReason(t *testing.T) {
	refuse := gitAuth{appToken: func(ns, repo string) (string, string, error) {
		return "", "", &controlclient.APIError{
			StatusCode: http.StatusForbidden,
			Message:    "github_repo_not_selected: add it under Repository access",
		}
	}}
	public, mainHead, _ := newTwoBranchRepo(t, "feature")
	branch := "master" // go-git's default branch name varies by version
	if _, err := ResolveRemoteHead(&Target{RepoURL: public, Branch: "main"}, gitAuth{}); err == nil {
		branch = "main"
	}
	target := &Target{
		AuthType: AuthTypeGitHubApp, RepoURL: public, Branch: branch,
		DeploySpec: `{"namespace":"acme--prod"}`,
	}
	head, err := ResolveRemoteHead(target, refuse)
	if err != nil || head != mainHead {
		t.Fatalf("a readable repository was not read without credentials: head=%q err=%v", head, err)
	}
	if _, err := cloneBranch(target, t.TempDir(), refuse); err != nil {
		t.Fatalf("clone fell over on the refusal: %v", err)
	}

	target.RepoURL = "file://" + t.TempDir() + "/missing"
	_, err = ResolveRemoteHead(target, refuse)
	if err == nil || !strings.Contains(err.Error(), "github_repo_not_selected") {
		t.Fatalf("the control plane's reason did not reach the error: %v", err)
	}
	if !isAuthRefused(err) {
		t.Error("the reported error lost its status, so the give-up logic cannot classify it")
	}
}
