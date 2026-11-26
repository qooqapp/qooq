// Package api exposes the qooq REST API. Handlers depend only on core.Service
// (which itself depends only on the runtime and store interfaces) — this
// package never imports a concrete engine or database.
package api

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/ingress"
	"github.com/qooqapp/qooq/internal/version"
)

// Version is the qooq build version reported by /v1/version. It comes from
// internal/version, which is injected at build time — the literal that used to
// live here meant every build ever made called itself 0.1.0.
var Version = version.Version

// NewRouter builds the HTTP handler for the daemon. When token is non-empty,
// every /v1 route except /healthz requires `Authorization: Bearer <token>`;
// an empty token (the default) leaves the API without bearer auth.
//
// listenAddr is qooqd's -listen address; its port seeds the Host-header
// allowlist. extraAllowedHosts (from -allowed-hosts/QOOQ_ALLOWED_HOSTS)
// widens that allowlist. The Host guard is only mounted when token == ""
// (see hostGuard's doc comment). Content-Type enforcement is always mounted.
//
// advertiseHost is the address this host is reachable at from elsewhere; it is
// what component endpoints are composed from. Empty means the daemon could not
// determine one, and endpoints fall back to 127.0.0.1.
//
// ing is the ingress configuration the proxy runs with; nil means no ingress
// is running, so components are reported at their host ports.
func NewRouter(svc *core.Service, log *slog.Logger, token, listenAddr string, extraAllowedHosts []string, advertiseHost *AdvertiseHost, ing *ingress.Config) http.Handler {
	h := &handlers{svc: svc, log: log, advertiseHost: advertiseHost}
	if ing != nil {
		cfg := ing.WithDefaults()
		h.ingress = &cfg
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	// Docs are unauthenticated (like /v1/healthz): they describe the API
	// shape, not its data, and a bearer-gated docs page is just friction for
	// the one person allowed to hold that token anyway.
	r.Get("/openapi.yaml", h.openapiYAML)
	r.Get("/docs", h.docsUI)

	allowedHosts := append(defaultAllowedHosts(listenAddr), extraAllowedHosts...)

	r.Route("/v1", func(r chi.Router) {
		// Host guard: only when there's no bearer auth. With no auth, a
		// malicious web page could drive-by POST to 127.0.0.1 from a victim's
		// browser (no-cors); pinning the Host header to the known localhost
		// names blocks that, since a browser can't forge Host on a
		// cross-origin request. With a token set, bearer auth already blocks
		// the browser and Host pinning would only break legitimate
		// reverse-proxy setups, so it's skipped there.
		if token == "" {
			r.Use(hostGuard(allowedHosts))
		}

		r.Get("/healthz", h.health)

		r.Group(func(r chi.Router) {
			if token != "" {
				r.Use(bearerAuth(token))
			}

			r.Get("/version", h.version)

			r.Route("/system", func(r chi.Router) {
				r.Get("/capacity", h.capacity)
				r.Get("/backup", h.backup)
			})

			r.Route("/components", func(r chi.Router) {
				r.With(requireJSONContentType).Post("/", h.deploy)
				r.With(requireJSONContentType).Put("/", h.upsertComponent)
				r.Get("/", h.listComponents)
				r.Get("/{id}", h.getComponent)
				r.Delete("/{id}", h.removeComponent)
				r.Post("/{id}/start", h.startComponent)
				r.Post("/{id}/stop", h.stopComponent)
				r.Post("/{id}/rollback", h.rollbackComponent)
				r.Get("/{id}/stats", h.componentStats)
				r.Get("/{id}/events", h.componentEvents)
				r.Get("/{id}/logs", h.componentLogs)
			})

			r.Route("/groups", func(r chi.Router) {
				r.With(requireJSONContentType).Post("/", h.createGroup)
				r.Get("/", h.listGroups)
				r.Get("/{id}", h.getGroup)
				r.Delete("/{id}", h.deleteGroup)
				r.Post("/{id}/start", h.startGroup)
				r.Post("/{id}/stop", h.stopGroup)
				r.With(requireJSONContentType).Post("/{id}/members", h.addMember)
				r.Delete("/{id}/members/{componentID}", h.removeMember)
			})

			r.Route("/images", func(r chi.Router) {
				r.With(requireJSONContentType).Post("/pull", h.pullImage)
				r.With(requireJSONContentType).Post("/push", h.pushImage)
				r.With(requireJSONContentType).Post("/digest", h.digestImage)
				// load/build are tar streams, not JSON — no Content-Type guard.
				r.Post("/load", h.loadImage)
				r.Post("/build", h.buildImage)
				r.Get("/", h.listImages)
				r.Delete("/", h.removeImage)
			})
		})
	})

	return r
}

// defaultAllowedHosts derives the Host-header allowlist from the listen
// address' port: the three canonical localhost forms qooqd is reachable at.
// Returns nil when the port can't be parsed (guard then rejects everything,
// which is the safe default — operators can widen it with -allowed-hosts).
func defaultAllowedHosts(listenAddr string) []string {
	_, port, err := net.SplitHostPort(listenAddr)
	if err != nil || port == "" {
		return nil
	}
	return []string{
		"127.0.0.1:" + port,
		"localhost:" + port,
		"[::1]:" + port,
	}
}

// hostGuard rejects any request whose Host header is not an exact match for
// the allowlist. It is only mounted when auth is disabled (see NewRouter),
// as a defense against browser-based drive-by requests to localhost.
func hostGuard(allowed []string) func(http.Handler) http.Handler {
	set := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		if a != "" {
			set[a] = struct{}{}
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := set[r.Host]; !ok {
				writeErr(w, http.StatusForbidden, errors.New("Host header not allowed; if you are accessing qooqd through a proxy or non-localhost name, set -allowed-hosts or enable -token auth"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// requireJSONContentType rejects requests whose Content-Type is not
// application/json (a trailing "; charset=..." is allowed). It guards the
// routes that decode a JSON body, blocking browser no-cors POSTs (which can
// only send text/plain, multipart/form-data, or application/x-www-form-urlencoded).
func requireJSONContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.Header.Get("Content-Type")
		if i := strings.IndexByte(ct, ';'); i >= 0 {
			ct = ct[:i]
		}
		if !strings.EqualFold(strings.TrimSpace(ct), "application/json") {
			writeErr(w, http.StatusUnsupportedMediaType, errors.New("Content-Type must be application/json"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerAuth rejects any request whose Authorization header is not exactly
// "Bearer <token>", using a constant-time comparison on the token itself.
func bearerAuth(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			const prefix = "Bearer "
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, prefix) {
				writeErr(w, http.StatusUnauthorized, errors.New("missing bearer token"))
				return
			}
			got := strings.TrimPrefix(h, prefix)
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				writeErr(w, http.StatusUnauthorized, errors.New("invalid bearer token"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
