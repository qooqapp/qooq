package ingress

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
)

const (
	// ComponentName is the proxy's own component name. It is deployed through
	// the ordinary component path, so it appears in `component ls` and is
	// restarted and reconciled like anything else — and it is excluded from
	// routing, since a proxy that proxied to itself would be a loop.
	ComponentName = "qooq-ingress"

	// Network is the shared network the proxy and every routed component join.
	// Components keep their own private network as well; this is an additional
	// attachment, which is why ConnectNetwork exists.
	Network = "qooq-ingress"

	// adminPort is Caddy's config API. It listens on the container's own
	// loopback interface (127.0.0.1) and is never published as a host port —
	// so the only way to reach it at all is from inside the container's own
	// network namespace. A HostIP-restricted port publish is not enough for
	// that: HostIP only limits which host address accepts connections, and
	// says nothing about the docker bridge network, where every routed
	// component's container also lives. qooqd itself reaches the admin API
	// by execing `caddy reload` inside the container (see reload below),
	// which runs in that same namespace and therefore sees the same
	// loopback.
	adminPort = 2019

	// configMountDir is the directory the state dir is bind-mounted at, and
	// configMountPath the config file inside it. qooqd writes a new version on
	// the host side, then execs `caddy reload --config configMountPath`.
	//
	// A directory, never the file itself: a single-file bind mount follows
	// the inode, and writeConfigAtomic's rename replaces the inode — so the
	// container kept reading the first config forever while every reload
	// "succeeded" with it. Seen on Linux docker (E1): no route ever reached
	// Caddy, every user unreachable, qooqd logging "routes updated". A
	// rename inside a mounted directory is visible to the container.
	configMountDir  = "/etc/caddy/qooq"
	configMountPath = configMountDir + "/caddy.json"

	// defaultImage is pinned rather than tracking latest: an ingress that
	// silently changes version underneath a working deployment is not a
	// surprise anyone wants.
	defaultImage = "caddy:2.8-alpine"

	// syncInterval is how often desired routes are recomputed. Routes are
	// derived from component state rather than pushed on change, so a missed
	// event cannot leave routing permanently stale.
	syncInterval = 5 * time.Second

	// IssuerACME obtains certificates from a public CA (Let's Encrypt). It
	// only works for a hostname whose DNS points at this machine and whose
	// port 443 is reachable from the internet.
	IssuerACME = "acme"
	// IssuerInternal signs with Caddy's own CA. It works offline and for any
	// hostname — including .localhost, which no public CA will ever issue for
	// — at the cost of the root having to be trusted on every device that
	// connects. It is what makes local HTTPS, and testing this code path at
	// all, possible.
	IssuerInternal = "internal"
)

// Config is the daemon's ingress configuration.
type Config struct {
	// HTTPPort is the host port the proxy publishes for plain HTTP.
	HTTPPort int
	// TLS is the daemon-wide default for whether components are served over
	// HTTPS. A component's own ingress.tls overrides it.
	TLS bool
	// TLSIssuer selects who signs the certificates: IssuerACME (a public CA,
	// requiring public DNS and reachable ports) or IssuerInternal (Caddy's own
	// CA, which works offline for any hostname but must be trusted by hand).
	TLSIssuer string
	// Image overrides the proxy image.
	Image string
	// StateDir holds the generated bootstrap config, bind-mounted into the
	// proxy container.
	StateDir string
	// AdvertiseHost reports the daemon's advertise host. It is a function
	// rather than a string because the address can change while the daemon
	// runs (a laptop joining another network), and a route built from the
	// address this process started with stops resolving the moment it does.
	// It is used to derive the wildcard-DNS hostname. May be nil.
	AdvertiseHost func() string

	// HTTPSPort is the port the proxy listens on, and publishes, for HTTPS.
	// Zero means 443.
	HTTPSPort int
	// AppDomain, when set, is the only domain a shared-node namespace is routed
	// on: one name, "<label>.<AppDomain>", taken from the component's Domain
	// (see ComponentHostnames). Those names are served on HTTPS with the
	// fixed certificate below and never sent to a certificate authority.
	AppDomain string
	// AppDomainRedirect, when set, answers the bare AppDomain with a 301 to
	// this URL.
	AppDomainRedirect string
	// CertFile and KeyFile are a fixed certificate (on a shared node, a
	// Cloudflare Origin CA certificate for *.AppDomain) on the host. They are
	// copied into the proxy's mounted config directory at deploy.
	CertFile string
	KeyFile  string
	// Cloudflare trusts Cloudflare's address ranges as proxies and takes the
	// client address from CF-Connecting-IP when a request comes from one.
	Cloudflare bool
	// BindIP is the host address the proxy's own ports bind to. Empty means
	// 0.0.0.0, which a server needs (the proxy is the one component meant to
	// be reached from outside); a laptop that wants its apps reachable from
	// this machine only sets 127.0.0.1.
	BindIP string
}

// WithDefaults fills in what a zero Config leaves unset. The API applies it
// too, so the endpoints it reports are worked out from the same values the
// proxy is configured with.
func (c Config) WithDefaults() Config {
	if c.Image == "" {
		c.Image = defaultImage
	}
	if c.HTTPPort == 0 {
		c.HTTPPort = 80
	}
	if c.HTTPSPort == 0 {
		c.HTTPSPort = 443
	}
	if c.TLSIssuer == "" {
		c.TLSIssuer = IssuerACME
	}
	return c
}

// httpsPort is HTTPSPort with zero read as 443.
func (c Config) httpsPort() int {
	if c.HTTPSPort == 0 {
		return 443
	}
	return c.HTTPSPort
}

// servesFixedCert reports whether the proxy loads a certificate from files.
func (c Config) servesFixedCert() bool { return c.CertFile != "" }

// listensHTTPS reports whether the proxy binds the HTTPS port: for automatic
// certificates, or to serve the fixed one.
func (c Config) listensHTTPS() bool { return c.TLS || c.servesFixedCert() }

// Manager keeps the proxy running and its routes in step with the components.
type Manager struct {
	svc *core.Service
	// rt execs `caddy reload` inside the proxy container. It is the same
	// runtime the rest of the daemon uses (passed in separately from svc
	// because core.Service does not expose exec), so ingress needs no
	// dependency the rest of qooqd does not already have.
	rt  runtime.Runtime
	cfg Config
	log *slog.Logger

	// configPath is the host-side path of the file bind-mounted into the
	// proxy container at configMountPath. Set once in deployProxy.
	configPath string

	// lastHash is the digest of the config last accepted by the proxy, and
	// lastProxy which proxy container accepted it: an unchanged desired state
	// costs one comparison instead of a reload — but only for that same
	// container. A recreated proxy starts from whatever file it finds, so the
	// config must be pushed to it again even when nothing else changed.
	lastHash  [32]byte
	lastProxy runtime.ContainerID
	// attached remembers which proxy/component container pairs are
	// connected, and left which components have been taken off the shared
	// network, so each attempt happens once per container rather than every
	// tick. Only ever touched from the single sync goroutine.
	attached map[string]bool
	left     map[string]bool
	// warned holds the hostname problems already logged, so one bad
	// component is a line in the log rather than a line every tick. A
	// problem that goes away is forgotten, and logged again if it returns.
	warned map[Problem]bool
}

func New(svc *core.Service, rt runtime.Runtime, cfg Config, log *slog.Logger) *Manager {
	return &Manager{
		svc: svc,
		rt:  rt,
		cfg: cfg.WithDefaults(),
		log: log,
	}
}

// Run deploys the proxy and then keeps its routes in sync until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	if err := m.deployProxy(ctx); err != nil {
		// A proxy that will not start is worth shouting about, but it must not
		// take the daemon down: every component still works on its host ports.
		m.log.Error("ingress: proxy failed to deploy; components are still reachable on their host ports", "err", err)
		return
	}

	ticker := time.NewTicker(syncInterval)
	defer ticker.Stop()
	for {
		if err := m.sync(ctx); err != nil && ctx.Err() == nil {
			m.log.Warn("ingress: route sync failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// deployProxy writes the bootstrap config and upserts the proxy component.
//
// The bootstrap file only has to be enough for the admin API to come up;
// everything after that is pushed through the API, so this is not where
// routing lives.
func (m *Manager) deployProxy(ctx context.Context) error {
	// The config is bind-mounted, and a bind source has to be absolute — the
	// state dir is derived from -data, which is commonly relative ("qooq.db").
	stateDir, err := filepath.Abs(m.cfg.StateDir)
	if err != nil {
		return fmt.Errorf("resolve ingress state dir: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return fmt.Errorf("create ingress state dir: %w", err)
	}
	m.configPath = filepath.Join(stateDir, "caddy.json")
	if err := installCert(m.cfg, stateDir); err != nil {
		return err
	}
	bootstrap, err := json.MarshalIndent(m.desiredConfig(nil), "", "  ")
	if err != nil {
		return err
	}
	if err := writeConfigAtomic(m.configPath, bootstrap); err != nil {
		return fmt.Errorf("write ingress config: %w", err)
	}

	ports := proxyPorts(m.cfg)

	spec := core.ComponentSpec{
		Name:    ComponentName,
		Image:   m.cfg.Image,
		Network: Network,
		Ports:   ports,
		Mounts: []runtime.Mount{{
			Source:   stateDir,
			Target:   configMountDir,
			ReadOnly: true,
		}},
		Command:       []string{"caddy", "run", "--config", configMountPath},
		RestartPolicy: core.RestartPolicyAlways,
	}

	if _, _, err := m.svc.Upsert(ctx, spec); err != nil {
		return fmt.Errorf("deploy ingress: %w", err)
	}
	m.log.Info("ingress: proxy deployed",
		"image", m.cfg.Image, "http_port", m.cfg.HTTPPort, "tls", m.cfg.TLS, "tls_issuer", m.cfg.TLSIssuer,
		"https_port", m.cfg.httpsPort(), "app_domain", m.cfg.AppDomain, "fixed_cert", m.cfg.servesFixedCert(),
		"cloudflare", m.cfg.Cloudflare)
	return nil
}

// Where the fixed certificate lives inside the mounted config directory.
// Copied into that directory rather than bind-mounted on its own, for the
// same inode reason as the config file (configMountDir's comment).
const (
	certFileName = "app-domain.crt"
	keyFileName  = "app-domain.key"
)

// installCert copies the fixed certificate and key into the proxy's config
// directory, the key readable by its owner only. Nothing to do without one.
func installCert(cfg Config, stateDir string) error {
	if !cfg.servesFixedCert() {
		return nil
	}
	for _, f := range []struct {
		from, name string
		mode       os.FileMode
	}{
		{cfg.CertFile, certFileName, 0o644},
		{cfg.KeyFile, keyFileName, 0o600},
	} {
		body, err := os.ReadFile(f.from)
		if err != nil {
			return fmt.Errorf("read ingress certificate: %w", err)
		}
		dst := filepath.Join(stateDir, f.name)
		tmp := dst + ".tmp"
		if err := os.WriteFile(tmp, body, f.mode); err != nil {
			return fmt.Errorf("install ingress certificate: %w", err)
		}
		// WriteFile only applies the mode when it creates the file.
		if err := os.Chmod(tmp, f.mode); err != nil {
			return fmt.Errorf("install ingress certificate: %w", err)
		}
		if err := os.Rename(tmp, dst); err != nil {
			return fmt.Errorf("install ingress certificate: %w", err)
		}
	}
	return nil
}

// sync recomputes the desired routes and pushes them if they changed.
func (m *Manager) sync(ctx context.Context) error {
	// With the runtime down nothing reads as Running, so the routes would
	// compute as none — and writing that out is how a docker restart took
	// every user offline: the empty file was what the recreated proxy
	// started from (E1, D2). "No routes" closes the whole site and must never
	// be concluded from an error. Skip the tick; the last good config stays.
	if err := m.rt.Ping(ctx); err != nil {
		return fmt.Errorf("ingress: the container runtime is not answering, keeping the current routes: %w", err)
	}
	comps, err := m.svc.List(ctx)
	if err != nil {
		return err
	}

	var proxyContainerID runtime.ContainerID
	for _, c := range comps {
		if c.Spec.Name == ComponentName {
			proxyContainerID = runtime.ContainerID(c.Status.ContainerID)
		}
	}
	for _, c := range comps {
		if c.Spec.Name != ComponentName {
			m.leaveShared(ctx, c)
		}
	}
	routes, routed := m.routes(comps)
	for _, c := range routed {
		m.ensureAttached(ctx, proxyContainerID, c)
	}

	cfg := m.desiredConfig(routes)
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(body)
	if !m.needsPush(hash, proxyContainerID) {
		return nil
	}

	if err := m.reload(ctx, proxyContainerID, body); err != nil {
		return err
	}
	m.lastHash, m.lastProxy = hash, proxyContainerID
	m.log.Info("ingress: routes updated", "routes", len(routes))
	return nil
}

// ensureAttached makes a component reachable from the proxy, once per pair of
// proxy and component containers: the proxy joins the component's network
// (core.ConnectProxy). Either container being recreated is exactly when the
// connection needs making again, and the ids change then.
//
// Done here rather than when a component is created so that turning the
// ingress on, or the proxy being recreated, needs nothing redeployed.
func (m *Manager) ensureAttached(ctx context.Context, proxy runtime.ContainerID, c *core.Component) {
	cid := c.Status.ContainerID
	if proxy == "" || cid == "" {
		return
	}
	key := string(proxy) + "/" + cid
	if m.attached[key] {
		return
	}
	if err := m.svc.ConnectProxy(ctx, proxy, c); err != nil {
		// Worth a line but not worth stopping the sync for: the route is still
		// published, and the next tick tries again.
		m.log.Warn("ingress: could not connect the proxy to a component's network",
			"component", c.Spec.Name, "err", err)
		return
	}
	if m.attached == nil {
		m.attached = make(map[string]bool)
	}
	m.attached[key] = true
}

// leaveShared takes a component off the shared ingress network once per
// container, undoing what an earlier daemon did to every component: on that
// network one user's app could reach another's database by name.
func (m *Manager) leaveShared(ctx context.Context, c *core.Component) {
	cid := c.Status.ContainerID
	if cid == "" || m.left[cid] {
		return
	}
	if err := m.svc.LeaveIngressNetwork(ctx, c); err != nil {
		m.log.Warn("ingress: could not take a component off the shared network",
			"component", c.Spec.Name, "err", err)
		return
	}
	if m.left == nil {
		m.left = make(map[string]bool)
	}
	m.left[cid] = true
}

// route is one component's routing intent, kept in a small struct so sync can
// sort before handing anything to the config builder.
type route struct {
	sortKey   string
	hostnames []string
	upstream  string
	// path, when set, restricts this route to a URL prefix so several
	// components can share a hostname.
	path string
	// stripPath removes path before forwarding.
	stripPath bool
	// tls requests a certificate for this route's public hostnames.
	tls bool
}

// routes derives every component's route, sorted, along with the components
// that got one. Hostnames are resolved for all of them at once (Assign),
// since which component keeps a contested name depends on the others.
func (m *Manager) routes(comps []*core.Component) ([]route, []*core.Component) {
	assigned := Assign(comps, m.cfg.AppDomain, m.advertiseHost())
	m.warnProblems(assigned.Problems)

	routes := make([]route, 0, len(comps))
	var routed []*core.Component
	for _, c := range comps {
		r, ok := m.routeWith(c, assigned.Hosts(c.ID))
		if !ok {
			continue
		}
		routes = append(routes, r)
		routed = append(routed, c)
	}
	// Deterministic order: the config is hashed to decide whether to reload,
	// and map/list ordering wobble would otherwise reload on every tick.
	sort.Slice(routes, func(i, j int) bool { return routes[i].sortKey < routes[j].sortKey })
	return routes, routed
}

// warnProblems logs each hostname problem once while it lasts.
func (m *Manager) warnProblems(problems []Problem) {
	now := make(map[Problem]bool, len(problems))
	for _, p := range problems {
		now[p] = true
		if !m.warned[p] {
			m.log.Warn("ingress: component not routed on a hostname", "component", p.Component, "reason", p.Message)
		}
	}
	m.warned = now
}

// routeFor derives a single component's route as if it were the only one.
func (m *Manager) routeFor(c *core.Component) (route, bool) {
	return m.routeWith(c, Assign([]*core.Component{c}, m.cfg.AppDomain, m.advertiseHost()).Hosts(c.ID))
}

// routeWith derives a component's route on the hostnames it was assigned, or
// reports that it has none.
//
// A component is routable when it is running and exposes at least one port.
// The first port wins when there are several: manifest order is deterministic,
// and choosing between them properly needs the ingress: block that v1 does not
// have. The proxy itself is skipped.
func (m *Manager) routeWith(c *core.Component, hosts []string) (route, bool) {
	if c.Spec.Name == ComponentName {
		return route{}, false
	}
	if c.Status.Phase != runtime.PhaseRunning {
		return route{}, false
	}
	// Turned off for this component: a database qooq starts for an app is
	// reached from that app's network, never through a public hostname.
	if c.Spec.Ingress != nil && c.Spec.Ingress.Off {
		return route{}, false
	}
	if len(c.Spec.Ports) == 0 {
		return route{}, false
	}

	ing := c.Spec.Ingress
	if ing == nil {
		ing = &core.IngressSpec{}
	}

	port := ing.Port
	if port == 0 {
		port = c.Spec.Ports[0].ContainerPort
	}
	if port == 0 {
		return route{}, false
	}

	if len(hosts) == 0 {
		return route{}, false
	}

	tls := m.cfg.TLS
	if ing.TLS != nil {
		// A component can turn HTTPS off for itself, but it cannot turn it on
		// when the daemon never bound 443 — there is no listener to serve it.
		// Saying so beats publishing an https:// endpoint that refuses to
		// connect.
		if *ing.TLS && !m.cfg.TLS {
			m.log.Warn("ingress: component asked for tls but the daemon was started without -ingress-tls; serving it over http",
				"component", c.Spec.Name)
		} else {
			tls = *ing.TLS
		}
	}

	// The upstream is the component's network alias, not its IP: an address
	// changes every time the container is recreated, and the route would then
	// point at nothing until the next sync.
	alias := core.ContainerName(c.Spec)
	return route{
		sortKey:   routeSortKey(ing.Path, c.Spec.Namespace, c.Spec.Name),
		hostnames: hosts,
		upstream:  fmt.Sprintf("%s:%d", alias, port),
		path:      ing.Path,
		stripPath: ing.StripPath,
		tls:       tls,
	}, true
}

// routeSortKey orders routes so that a more specific path is matched first.
//
// Caddy evaluates routes in order and takes the first match, so a route for
// the whole host placed ahead of one for /api on the same host would swallow
// it — the /api component would be deployed, running, listed as routed, and
// never receive a request. Longer paths sort before shorter ones, and the
// hostless case (a route for everything) sorts last. The component name only
// breaks ties, to keep the order stable for the config digest.
func routeSortKey(path, namespace, name string) string {
	return fmt.Sprintf("%03d/%s/%s/%s", 999-min(len(path), 999), path, namespace, name)
}

// desiredConfig renders the full Caddy config for a set of routes.
//
// The whole config is replaced on every change rather than patched: a total
// replacement cannot drift from what qooq believes is deployed, and Caddy
// applies it without dropping connections.
func (m *Manager) desiredConfig(routes []route) map[string]any {
	caddyRoutes := make([]map[string]any, 0, len(routes))
	var certified, skip, appHosts []string
	wantCert := map[string]bool{}
	anyTLS := false
	for _, r := range routes {
		match := map[string]any{"host": r.hostnames}
		if r.path != "" {
			// Caddy's path matcher is exact unless it ends in a wildcard, so
			// "/api" alone would not match "/api/users".
			match["path"] = []string{r.path, r.path + "/*"}
		}
		handlers := []map[string]any{}
		if r.stripPath {
			// strip_path_prefix is what "mount this app under /api" means for
			// an app that serves from its own root: it never sees the prefix.
			handlers = append(handlers, map[string]any{
				"handler":           "rewrite",
				"strip_path_prefix": r.path,
			})
		}
		handlers = append(handlers, map[string]any{
			"handler":   "reverse_proxy",
			"upstreams": []map[string]any{{"dial": r.upstream}},
		})
		caddyRoutes = append(caddyRoutes, map[string]any{
			"match":  []map[string]any{match},
			"handle": handlers,
		})

		if r.tls {
			anyTLS = true
		}
		for _, h := range r.hostnames {
			// A name under the app domain is never sent to a certificate
			// authority: it is served with the fixed certificate, or over
			// plain http behind a proxy that terminates TLS for it.
			if UnderAppDomain(h, m.cfg.AppDomain) {
				appHosts = append(appHosts, h)
				continue
			}
			// With the internal CA there is nothing a certificate authority
			// could refuse, so every hostname is certifiable — which is the
			// whole point of that mode, since .localhost is exactly what a
			// local developer wants HTTPS for. With ACME only names a public
			// CA could issue for are attempted.
			wantCert[h] = wantCert[h] ||
				(r.tls && (m.cfg.TLSIssuer == IssuerInternal || IsPublicDomain(h)))
		}
	}

	// Split by hostname, not by route. Several components share a hostname
	// whenever paths are used to divide it, and Caddy rejects a configuration
	// that names the same host in two automation policies — which made the
	// entire config fail to load, taking every unrelated route with it. A
	// hostname is certified if any route on it wants a certificate.
	for h, want := range wantCert {
		if want {
			certified = append(certified, h)
		} else {
			skip = append(skip, h)
		}
	}

	// The app domain's own catch-alls: unknown names get a page that says so,
	// and the bare domain a redirect when one was given. Last, so every real
	// route matches before them.
	if m.cfg.AppDomain != "" {
		wildcard := "*." + m.cfg.AppDomain
		appHosts = append(appHosts, wildcard)
		caddyRoutes = append(caddyRoutes, map[string]any{
			"match": []map[string]any{{"host": []string{wildcard}}},
			"handle": []map[string]any{{
				"handler":     "static_response",
				"status_code": 404,
				"headers":     map[string][]string{"Content-Type": {"text/html; charset=utf-8"}},
				"body":        notFoundPage,
			}},
		})
		if m.cfg.AppDomainRedirect != "" {
			appHosts = append(appHosts, m.cfg.AppDomain)
			caddyRoutes = append(caddyRoutes, map[string]any{
				"match": []map[string]any{{"host": []string{m.cfg.AppDomain}}},
				"handle": []map[string]any{{
					"handler":     "static_response",
					"status_code": 301,
					"headers":     map[string][]string{"Location": {m.cfg.AppDomainRedirect}},
				}},
			})
		}
	}

	listen := []string{fmt.Sprintf(":%d", m.cfg.HTTPPort)}
	autoHTTPS := map[string]any{"disable": true}
	apps := map[string]any{}
	if anyTLS || m.cfg.servesFixedCert() {
		listen = append(listen, fmt.Sprintf(":%d", m.cfg.httpsPort()))
		appHosts = dedupeSorted(appHosts)
		if !m.cfg.servesFixedCert() {
			// With nothing to serve them on HTTPS here, app-domain names stay
			// plain http — whoever is in front terminates TLS for them.
			skip = append(skip, appHosts...)
		}
		sort.Strings(skip)
		autoHTTPS = map[string]any{"disable": false, "skip": skip}
		if m.cfg.servesFixedCert() {
			// Served on HTTPS with the loaded certificate, and never handed
			// to an issuer: a challenge behind a proxy like Cloudflare need
			// not pass, and failed ones retry forever against a rate limit
			// shared with every name that would have worked.
			autoHTTPS["skip_certificates"] = appHosts
			apps["tls"] = map[string]any{
				"certificates": map[string]any{
					"load_files": []map[string]any{{
						"certificate": configMountDir + "/" + certFileName,
						"key":         configMountDir + "/" + keyFileName,
					}},
				},
			}
		}
		if m.cfg.TLSIssuer == IssuerInternal && anyTLS {
			// http and https both stay usable. Redirecting would silently
			// break whatever was already talking to the http address, and
			// locally that is usually the developer's own running work.
			autoHTTPS["disable_redirects"] = true
			sort.Strings(certified)
			if len(certified) > 0 {
				tlsApp, _ := apps["tls"].(map[string]any)
				if tlsApp == nil {
					tlsApp = map[string]any{}
				}
				tlsApp["automation"] = map[string]any{
					"policies": []map[string]any{{
						"subjects": certified,
						"issuers":  []map[string]any{{"module": "internal"}},
					}},
				}
				apps["tls"] = tlsApp
			}
		}
	}

	server := map[string]any{
		"listen":          listen,
		"routes":          caddyRoutes,
		"automatic_https": autoHTTPS,
	}
	if m.cfg.Cloudflare {
		// Without this every app sees Cloudflare's address as its client's.
		// Only a request arriving from Cloudflare's own ranges may name the
		// client; from anywhere else the header is ignored, or anyone could
		// claim any address.
		server["trusted_proxies"] = map[string]any{"source": "static", "ranges": cloudflareRanges}
		server["client_ip_headers"] = []string{"CF-Connecting-IP"}
	}
	httpApp := map[string]any{"servers": map[string]any{"qooq": server}}
	if p := m.cfg.httpsPort(); p != 443 {
		// Caddy decides which listener gets TLS by this port.
		httpApp["https_port"] = p
	}
	if m.cfg.HTTPPort != 80 {
		// The same decision from the other side: a server that has any
		// certificate serves TLS on every listener except Caddy's http_port,
		// so without this a non-default HTTP port answered plain http with
		// "Client sent an HTTP request to an HTTPS server".
		httpApp["http_port"] = m.cfg.HTTPPort
	}
	apps["http"] = httpApp

	return map[string]any{
		"admin": map[string]any{"listen": fmt.Sprintf("127.0.0.1:%d", adminPort)},
		"apps":  apps,
	}
}

// execOutputter is the subset of a runtime adapter that can return a
// command's output alongside its exit code. It is not part of the
// runtime.Runtime interface (see the docker/process adapters' ExecOutput for
// why), so reload type-asserts for it rather than requiring every Runtime
// implementation — including test fakes elsewhere in the codebase — to grow
// a method only this one caller needs.
type execOutputter interface {
	ExecOutput(ctx context.Context, id runtime.ContainerID, cmd []string) (int, []byte, error)
}

// writeConfigAtomic replaces the bind-mounted config file's content via
// write-then-rename rather than an in-place write.
//
// os.WriteFile truncates the file before writing its new content, which
// leaves a window where the file is empty or partial; found in practice as
// an intermittent "config is not valid JSON: unexpected end of JSON input"
// from the very next reload, on a host where the bind mount is a networked
// file share (Docker Desktop's virtiofs/gRPC-FUSE) rather than a plain local
// bind. Renaming a fully-written temp file over the target is a single
// directory-entry swap: whichever generation the container's next read sees,
// it always sees a complete one.
func writeConfigAtomic(path string, body []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// reload applies a new config by writing it to the file bind-mounted into
// the proxy container and then execing `caddy reload` inside that container.
//
// This — not an HTTP call to the admin API — is how a config change reaches
// Caddy: the admin API listens on the container's own loopback precisely so
// that nothing on the shared ingress network (every routed component's
// container) can reach it, and a caller outside the container is exactly
// such a thing. Running the reload from inside the container's own network
// namespace is what makes that loopback address reachable at all.
func (m *Manager) reload(ctx context.Context, containerID runtime.ContainerID, body []byte) error {
	if containerID == "" {
		return fmt.Errorf("ingress: reload: proxy container not found")
	}
	if err := writeConfigAtomic(m.configPath, body); err != nil {
		return fmt.Errorf("write ingress config: %w", err)
	}

	// No --adapter flag: the config is already Caddy's native JSON (same as
	// the `caddy run` command in deployProxy), and "json" is not itself a
	// registered config adapter name — that flag is only for converting from
	// another format such as the Caddyfile.
	cmd := []string{"caddy", "reload", "--config", configMountPath}
	if eo, ok := m.rt.(execOutputter); ok {
		code, out, err := eo.ExecOutput(ctx, containerID, cmd)
		if err != nil {
			return fmt.Errorf("ingress: caddy reload: %w", err)
		}
		if code != 0 {
			return fmt.Errorf("ingress: caddy reload exited %d: %s", code, bytes.TrimSpace(out))
		}
		return m.verifyLoaded(ctx, eo, containerID, body)
	}

	// Fallback for a runtime that only implements the plain Exec method: no
	// output, but still a correct pass/fail signal.
	code, err := m.rt.Exec(ctx, containerID, cmd)
	if err != nil {
		return fmt.Errorf("ingress: caddy reload: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("ingress: caddy reload exited %d (no output captured)", code)
	}
	return nil
}

// verifyLoaded reads the running config back from Caddy and checks that its
// servers are the ones just written. A reload that exits 0 only says Caddy
// loaded *a* config; when it read a stale file it loaded the old one and
// still said yes (E1). Until this check passes the sync is not recorded as
// done, so the next tick tries again — and says why.
func (m *Manager) verifyLoaded(ctx context.Context, eo execOutputter, id runtime.ContainerID, want []byte) error {
	url := fmt.Sprintf("http://127.0.0.1:%d/config/apps/http/servers", adminPort)
	code, got, err := eo.ExecOutput(ctx, id, []string{"wget", "-qO-", url})
	if err != nil || code != 0 {
		return fmt.Errorf("ingress: read back the running config: exit %d: %v %s", code, err, bytes.TrimSpace(got))
	}
	var desired struct {
		Apps struct {
			HTTP struct {
				Servers any `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(want, &desired); err != nil {
		return fmt.Errorf("ingress: read back: parse what was written: %w", err)
	}
	var running any
	if err := json.Unmarshal(bytes.TrimSpace(got), &running); err != nil {
		return fmt.Errorf("ingress: read back: parse what is running: %w", err)
	}
	// Through any and back: both sides end up with sorted keys and the same
	// number types, so equal configs are equal bytes.
	a, _ := json.Marshal(desired.Apps.HTTP.Servers)
	b, _ := json.Marshal(running)
	if !bytes.Equal(a, b) {
		return fmt.Errorf("ingress: caddy reloaded but is not running the config just written (it may be reading a stale file at %s)", configMountPath)
	}
	return nil
}

// advertiseHost reads the current advertise host, or "" when none was
// supplied. Read per use rather than captured once: that is the entire point
// of the field being a function.
func (m *Manager) advertiseHost() string {
	if m.cfg.AdvertiseHost == nil {
		return ""
	}
	return m.cfg.AdvertiseHost()
}

// needsPush reports whether a config has to go to the proxy: it changed, or
// the proxy is a different container from the one that took the last push.
func (m *Manager) needsPush(hash [32]byte, proxy runtime.ContainerID) bool {
	return hash != m.lastHash || proxy != m.lastProxy
}

// proxyPorts are the host ports the proxy publishes.
func proxyPorts(cfg Config) []runtime.PortMapping {
	// The admin API is not published at all — not even on loopback. It binds
	// to the container's own 127.0.0.1 (see adminPort's comment) and is
	// reached by execing into the container, never by dialing a host port.
	//
	// The public ports name 0.0.0.0 outright. qooqd's -publish-ip binds every
	// port that does not name an address, and on a shared node it is 127.0.0.1
	// so users cannot reach each other's ports — but the proxy is the one
	// component that exists to be reached from outside. Left to the default it
	// was bound to loopback too: every user app unreachable from off the
	// machine, and ACME's HTTP-01 challenge impossible (E1, on Linux).
	bind := cfg.BindIP
	if bind == "" {
		bind = "0.0.0.0"
	}
	ports := []runtime.PortMapping{
		{ContainerPort: cfg.HTTPPort, HostPort: cfg.HTTPPort, HostIP: bind},
	}
	if cfg.listensHTTPS() {
		p := cfg.httpsPort()
		ports = append(ports, runtime.PortMapping{ContainerPort: p, HostPort: p, HostIP: bind})
	}
	return ports
}
