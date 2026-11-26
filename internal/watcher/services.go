package watcher

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/qooqapp/qooq/pkg/client"
	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// serviceDefinition is what one built-in service type provides when the
// manifest says nothing beyond `type:`. Every field here is a default an
// entry in the manifest can override; the point of the table is that
// `type: postgres` alone is a complete, working declaration.
type serviceDefinition struct {
	image       string   // %s is replaced by the version
	version     string   // default image tag/version
	port        int      // the port the service listens on
	dataPath    string   // container path holding its data, "" if stateless
	scheme      string   // URL scheme for ${service.<n>.url}
	healthExec  []string // exec probe; %USER%/%DB% are substituted
	envTemplate map[string]string
	command     []string // container command, when the image's default does not use the credentials

	// user, when set, is the server's own fixed user instead of one named
	// after the app; noDatabase is a server with no named databases. Both
	// are redis: its password belongs to the built-in "default" user, and a
	// URL with an empty user is refused by some clients (redis-cli 7 sends
	// AUTH "" <password> and gets WRONGPASS).
	user       string
	noDatabase bool
	// urlQuery is appended to the URL, for a server that needs to be told
	// where the credentials live (mongo keeps its root user in admin).
	urlQuery string
	// healthFailures is how many failed probes count as down. A database
	// that initializes its data directory on first start (mysql, mongo:
	// 20-30 seconds) needs more than the default three, or every healthy
	// first deploy logs "health check has never succeeded". The deploy gate
	// waits for the first success, so this does not slow anything down.
	healthFailures int
}

// serviceDefaults is the single source of truth for built-in service types,
// laid out like templateDefaults: a repo names a type, and every operational
// detail (image, port, where its data lives, how to tell it is up, what
// environment it needs to bootstrap itself) comes from here unless overridden.
//
// %USER%, %PASSWORD% and %DATABASE% are substituted with the service's
// resolved credentials.
var serviceDefaults = map[string]serviceDefinition{
	"postgres": {
		image:      "postgres:%s-alpine",
		version:    "16",
		port:       5432,
		dataPath:   "/var/lib/postgresql/data",
		scheme:     "postgres",
		healthExec: []string{"pg_isready", "-U", "%USER%"},
		envTemplate: map[string]string{
			"POSTGRES_USER":     "%USER%",
			"POSTGRES_PASSWORD": "%PASSWORD%",
			"POSTGRES_DB":       "%DATABASE%",
		},
	},
	"mysql": {
		image:    "mysql:%s",
		version:  "8",
		port:     3306,
		dataPath: "/var/lib/mysql",
		scheme:   "mysql",
		// No credentials on purpose: `mysqladmin ping` exits 0 whenever the
		// server answers, "Access denied" included, and a probe command is
		// shown in logs and in `component get`, so a password in it leaks.
		// -h 127.0.0.1 matters: during first-start initialization the server
		// runs without networking, and that must not count as ready.
		healthExec:     []string{"mysqladmin", "ping", "-h", "127.0.0.1"},
		healthFailures: 20,
		envTemplate: map[string]string{
			"MYSQL_USER":          "%USER%",
			"MYSQL_PASSWORD":      "%PASSWORD%",
			"MYSQL_DATABASE":      "%DATABASE%",
			"MYSQL_ROOT_PASSWORD": "%PASSWORD%",
		},
	},
	"mariadb": {
		image:          "mariadb:%s",
		version:        "11",
		port:           3306,
		dataPath:       "/var/lib/mysql",
		scheme:         "mysql",
		healthExec:     []string{"mariadb-admin", "ping", "-h", "127.0.0.1"}, // see mysql
		healthFailures: 20,
		envTemplate: map[string]string{
			"MARIADB_USER":          "%USER%",
			"MARIADB_PASSWORD":      "%PASSWORD%",
			"MARIADB_DATABASE":      "%DATABASE%",
			"MARIADB_ROOT_PASSWORD": "%PASSWORD%",
		},
	},
	"redis": {
		image:    "redis:%s-alpine",
		version:  "7",
		port:     6379,
		dataPath: "/data",
		scheme:   "redis",
		// The image sets no password by itself, so the generated one used
		// to reach the app but not the server, and a URL carrying it failed
		// with WRONGPASS. The password goes in as REDISCLI_AUTH, which
		// redis-cli (the probe) also reads, and the command hands it to the
		// server — through the image's own entrypoint, which drops root.
		envTemplate: map[string]string{"REDISCLI_AUTH": "%PASSWORD%"},
		command: []string{"sh", "-c",
			`exec docker-entrypoint.sh redis-server --requirepass "$REDISCLI_AUTH"`},
		healthExec: []string{"redis-cli", "ping"},
		user:       "default",
		noDatabase: true,
	},
	"mongo": {
		image:          "mongo:%s",
		version:        "7",
		port:           27017,
		dataPath:       "/data/db",
		scheme:         "mongodb",
		healthExec:     []string{"mongosh", "--quiet", "--eval", "db.runCommand({ping:1})"},
		healthFailures: 20,
		// MONGO_INITDB_ROOT_* creates the user in admin, not in the app's
		// database; without this the client authenticates against the wrong
		// one and gets "Authentication failed" with the right password.
		urlQuery: "authSource=admin",
		envTemplate: map[string]string{
			"MONGO_INITDB_ROOT_USERNAME": "%USER%",
			"MONGO_INITDB_ROOT_PASSWORD": "%PASSWORD%",
			"MONGO_INITDB_DATABASE":      "%DATABASE%",
		},
	},
	// "custom" carries no defaults on purpose: image and port are required,
	// and qooq makes no assumption about what the thing is.
	"custom": {},
}

// serviceReadyTimeout bounds how long the pipeline waits for a service to
// come up before giving up on this deploy. A database's first start
// initializes its data directory, which is the slow case; a redeploy against
// an existing volume is near-instant.
//
// A var, not a const, only so tests can shorten it: the behaviour under test
// is which condition ends the wait, not how many minutes it is willing to
// spend. Product code must not assign to it.
var serviceReadyTimeout = 3 * time.Minute

// servicePollInterval is how often readiness is re-checked while waiting.
// A var for the same reason as serviceReadyTimeout.
var servicePollInterval = 2 * time.Second

// healthGraceTimeout bounds the extra wait for an *http* probe to pass once
// the container is already running.
//
// An http probe can be permanently unable to see a healthy app — health.port
// naming a port nothing listens on, or a container-only port on a host whose
// container network the daemon cannot route to, which is the normal case on
// Docker Desktop. The monitor already refuses to restart on that verdict for
// exactly this reason. Blocking a deploy on it forever would turn that same
// misconfiguration into "nothing deploys any more", so the wait gives up,
// says so, and continues.
//
// Exec probes get no such grace: `docker exec` works regardless of network
// topology, so an exec probe that never passes is evidence about the service,
// not about the probe, and the deploy should fail on it.
// A var for the same reason as serviceReadyTimeout.
var healthGraceTimeout = 60 * time.Second

// serviceState is the per-service data that must survive redeploys, stored on
// the Target. Passwords are the reason it exists: a generated password is
// baked into the database's data directory on first start, so regenerating it
// on the next deploy would leave the app unable to authenticate against its
// own data — with "password authentication failed" as the only clue.
type serviceState struct {
	Password string `json:"password,omitempty"`
}

// loadServiceState decodes t.ServiceState, decrypting it when a secret key is
// configured. A target written before M23 has an empty field, which decodes to
// an empty map.
func (w *Watcher) loadServiceState(t *Target) (map[string]serviceState, error) {
	if strings.TrimSpace(t.ServiceState) == "" {
		return map[string]serviceState{}, nil
	}
	decrypted, err := decryptCredential(t.ServiceState, w.secretKey)
	if err != nil {
		return nil, fmt.Errorf("service_state: %w", err)
	}
	var out map[string]serviceState
	if err := json.Unmarshal([]byte(decrypted), &out); err != nil {
		return nil, fmt.Errorf("service_state: %w", err)
	}
	if out == nil {
		out = map[string]serviceState{}
	}
	return out, nil
}

// saveServiceState persists state onto t, encrypting it when a secret key is
// configured (encryptCredential is a passthrough when it is not, matching the
// stance the other credential fields take).
func (w *Watcher) saveServiceState(t *Target, state map[string]serviceState) error {
	encoded, err := marshalServiceState(state, w.secretKey)
	if err != nil {
		return err
	}
	t.ServiceState = encoded
	return w.db.Model(t).Update("service_state", encoded).Error
}

// marshalServiceState encodes state for storage, encrypting when a secret key
// is set (encryptCredential passes through when it is not).
func marshalServiceState(state map[string]serviceState, secretKey string) (string, error) {
	data, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	encrypted, err := encryptCredential(string(data), secretKey)
	if err != nil {
		return "", fmt.Errorf("service_state: %w", err)
	}
	return encrypted, nil
}

// generatePassword returns a URL-safe random secret. It is only ever
// generated once per service (see serviceState).
func generatePassword() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// resolvedService is one service after defaults, overrides and stored state
// have all been applied.
type resolvedService struct {
	name      string
	typ       string
	version   string
	component string
	image     string
	port      int
	user      string
	password  string
	database  string
	scheme    string
	env       map[string]string
	command   []string
	query     string
	mounts    []mountSpec
	health    *client.HealthCheckSpec
	ports     []client.PortSpec
}

// url renders the connection string apps most often want.
func (s resolvedService) url() string {
	if s.scheme == "" {
		return ""
	}
	u := url.URL{
		Scheme:   s.scheme,
		Host:     fmt.Sprintf("%s:%d", s.component, s.port),
		RawQuery: s.query,
	}
	if s.database != "" {
		u.Path = "/" + s.database
	}
	if s.user != "" {
		if s.password != "" {
			u.User = url.UserPassword(s.user, s.password)
		} else {
			u.User = url.User(s.user)
		}
	}
	return u.String()
}

// applyServices deploys every service the manifest declares and returns the
// environment variables the app should inherit from them.
//
// Services are ordinary components: they are upserted through the same API a
// hand-run `qooq component deploy` uses, land in the same group as the app,
// and show up in `qooq component ls`. The services: block is sugar for
// declaring them, not a separate kind of thing qooq hides.
//
// Each service is deployed and waited on before the app itself is upserted by
// the caller, so an app that connects on startup finds its database already
// accepting connections.
func (w *Watcher) applyServices(ctx context.Context, t *Target, mf *manifest, namespace, network string, scope *interpolationScope) (map[string]string, error) {
	injected, _, err := w.applyServicesTopology(ctx, t, mf, namespace, network, scope)
	return injected, err
}

// applyServicesTopology is applyServices plus a record of what was deployed.
func (w *Watcher) applyServicesTopology(ctx context.Context, t *Target, mf *manifest, namespace, network string, scope *interpolationScope) (map[string]string, []watcherclient.TopologyService, error) {
	// Services an earlier deploy recorded that the manifest no longer
	// declares. They stay on the list, marked, so the warning can be shown
	// and removal still finds them: a list rebuilt from the manifest alone
	// forgot them, leaving a database running, with its data on disk, that
	// nothing pointed at and removing the project did not take.
	prior := w.storedTopology(t)
	dropped := droppedServices(prior, mf)
	if len(mf.Services) == 0 && len(dropped) == 0 {
		return nil, nil, nil
	}
	var deployed []watcherclient.TopologyService
	// What gets written to the row at any moment: what this run has resolved
	// so far, then whatever the previous deploy recorded that this run has not
	// reached yet, then the dropped ones. The list used to be "deployed so far
	// + dropped", so a failure at the first of three services rewrote the row
	// with one: the other two kept running, with their data on disk, but
	// removal and the next dropped calculation could no longer see them.
	// A write may only ever add to what is recorded, never lose a service.
	listing := func() []watcherclient.TopologyService {
		out := append([]watcherclient.TopologyService(nil), deployed...)
		have := make(map[string]bool, len(deployed))
		for _, d := range deployed {
			have[d.Name] = true
		}
		if prior != nil {
			for _, p := range prior.Services {
				if _, declared := mf.Services[p.Name]; declared && !have[p.Name] {
					out = append(out, p)
				}
			}
		}
		return append(out, dropped...)
	}
	if len(dropped) > 0 {
		w.persistServices(t, listing())
	}

	state, err := w.loadServiceState(t)
	if err != nil {
		return nil, nil, err
	}

	// One group per app, holding the app and its services, so `qooq group
	// stop <app>` takes the whole thing down in one move. A group failure is
	// not fatal — it is an organizational convenience, and refusing to deploy
	// a database because a group could not be created would be absurd.
	groupID, err := w.ensureGroup(ctx, groupName(namespaceOf(t), t.ComponentName))
	if err != nil {
		w.log.Warn("could not resolve group for services (continuing ungrouped)",
			zap.String("target", t.Name), zap.Error(err))
	}

	// Deterministic order: the deploy log (and any failure) should read the
	// same way every run.
	names := make([]string, 0, len(mf.Services))
	for name := range mf.Services {
		names = append(names, name)
	}
	sort.Strings(names)

	injected := make(map[string]string)
	for _, name := range names {
		decl := mf.Services[name]
		svc, changed, err := w.resolveService(t, namespace, name, decl, state, scope)
		if svc.component != "" {
			// Recorded before it is deployed, and written to the row now
			// rather than with the build's result: removal reads this list to
			// know what to take down, and a first build that fails (or a
			// watcher restarted mid-build) would otherwise leave a database
			// running — and its data on disk — that nothing points at.
			deployed = append(deployed, watcherclient.TopologyService{
				Name: name, Component: svc.component, Type: svc.typ, Version: svc.version,
			})
			w.persistServices(t, listing())
		}
		if err != nil {
			return nil, nil, fmt.Errorf("services.%s: %w", name, err)
		}
		// Saved before the service is created, not after every service is
		// up: the database writes this password into its data on first
		// start, and a deploy that fails after that (a timeout, a name
		// conflict on the daemon) used to lose it — leaving data nobody can
		// log in to and every later build refused with "the password that
		// data was created with is no longer known".
		if changed {
			if err := w.saveServiceState(t, state); err != nil {
				return nil, nil, err
			}
		}

		if err := w.deployService(ctx, t, svc, namespace, network, groupID); err != nil {
			return nil, nil, fmt.Errorf("services.%s: %w", name, err)
		}

		// Register both the interpolation keys and the conventional
		// environment variables. The env vars mean a manifest that declares
		// only `type: postgres` still gets a usable app; the interpolation
		// keys are what a framework expecting different variable names uses.
		prefix := envPrefix(name)
		for key, value := range map[string]string{
			"host":     svc.component,
			"port":     strconv.Itoa(svc.port),
			"user":     svc.user,
			"password": svc.password,
			"database": svc.database,
			"scheme":   svc.scheme,
			"url":      svc.url(),
		} {
			scope.set("service."+name+"."+key, value)
			if envName := prefix + "_" + strings.ToUpper(key); value != "" {
				injected[envName] = value
			}
		}
		// A service named "db" also gets the unprefixed DATABASE_URL that
		// most frameworks look for first.
		if name == "db" && svc.url() != "" {
			injected["DATABASE_URL"] = svc.url()
		}
	}

	for _, s := range dropped {
		w.stopDroppedService(ctx, t, namespace, s)
	}
	return injected, append(deployed, dropped...), nil
}

// storedTopology is the topology on t's row as stored — not t.Topology, which
// the pipeline clears before a deploy so a rebuild does not inherit old
// references. What the last deploy recorded is exactly what this needs.
func (w *Watcher) storedTopology(t *Target) *watcherclient.Topology {
	if w.db == nil || t.ID == 0 {
		return t.topologyForResponse()
	}
	var row Target
	if err := w.db.Select("topology").First(&row, t.ID).Error; err != nil {
		return nil
	}
	return row.topologyForResponse()
}

// droppedServices lists the services topo records that mf no longer
// declares, marked Dropped (already-dropped ones carry over unchanged).
func droppedServices(topo *watcherclient.Topology, mf *manifest) []watcherclient.TopologyService {
	if topo == nil {
		return nil
	}
	var out []watcherclient.TopologyService
	for _, s := range topo.Services {
		if _, declared := mf.Services[s.Name]; declared {
			continue
		}
		s.Dropped = true
		s.Phase = ""
		out = append(out, s)
	}
	return out
}

// stopDroppedService stops the container of a service the manifest no longer
// declares. It is not deleted, and neither is its data (see
// TopologyService.Dropped). A failure only warns: the app's deploy does not
// depend on it, and the next deploy tries again.
func (w *Watcher) stopDroppedService(ctx context.Context, t *Target, namespace string, s watcherclient.TopologyService) {
	comp, err := w.findComponent(ctx, componentRef{namespace: namespace, name: s.Component})
	if err != nil || comp == nil {
		if err != nil {
			w.log.Warn("could not check a service no longer in the manifest", zap.String("component", s.Component), zap.Error(err))
		}
		return
	}
	if comp.DesiredState == "Stopped" {
		return
	}
	if _, err := w.client.StopComponent(ctx, comp.ID); err != nil {
		w.log.Warn("could not stop a service no longer in the manifest", zap.String("component", s.Component), zap.Error(err))
		return
	}
	w.log.Info("stopped a service no longer in the manifest; its data is kept until it is deleted",
		zap.String("target", t.Name), zap.String("service", s.Name), zap.String("component", s.Component))
}

// envPrefix turns a service name into an environment variable prefix
// ("db" -> "DB", "main-cache" -> "MAIN_CACHE").
func envPrefix(name string) string {
	return strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name))
}

// resolveService merges the type's defaults with the manifest's overrides and
// the stored state, generating a password on first sight. The bool reports
// whether the stored state changed and needs persisting.
func (w *Watcher) resolveService(t *Target, namespace, name string, decl manifestService, state map[string]serviceState, scope *interpolationScope) (resolvedService, bool, error) {
	typ := decl.Type
	if typ == "" {
		typ = "custom"
	}
	def, ok := serviceDefaults[typ]
	if !ok {
		return resolvedService{}, false, fmt.Errorf("unknown type %q (known: %s)", typ, strings.Join(serviceTypeNames(), ", "))
	}

	// Reserved cluster knobs: accepted in the schema so a manifest can be
	// written against the eventual shape, but refused loudly rather than
	// silently deploying a single instance where several were asked for.
	if decl.Replicas != 0 && decl.Replicas != 1 {
		return resolvedService{}, false, fmt.Errorf("replicas: only 1 is supported today (got %d)", decl.Replicas)
	}
	if decl.Mode != "" && decl.Mode != "single" {
		return resolvedService{}, false, fmt.Errorf("mode %q: only \"single\" is supported today", decl.Mode)
	}

	svc := resolvedService{
		name:      name,
		typ:       typ,
		component: t.ComponentName + "-" + name,
		port:      def.port,
		scheme:    def.scheme,
		command:   def.command,
		query:     def.urlQuery,
	}

	version := decl.Version
	if version == "" {
		version = def.version
	}
	// A custom image has no version qooq knows of unless the manifest says.
	svc.version = decl.Version
	if decl.Image == "" {
		svc.version = version
	}
	switch {
	case decl.Image != "":
		image, err := scope.expand(decl.Image)
		if err != nil {
			return svc, false, err
		}
		svc.image = image
	case def.image != "":
		svc.image = fmt.Sprintf(def.image, version)
	default:
		return svc, false, fmt.Errorf("type %q requires an explicit image", typ)
	}

	if decl.Port != 0 {
		svc.port = decl.Port
	}
	if svc.port == 0 {
		return svc, false, fmt.Errorf("type %q requires an explicit port", typ)
	}

	// Identity defaults to the app's own name: one app, one database, named
	// after the app is what a single-VM deployment almost always wants.
	svc.user = firstNonEmpty(decl.User, def.user, normalizeIdentifier(t.ComponentName))
	svc.database = decl.Database
	if !def.noDatabase {
		svc.database = firstNonEmpty(decl.Database, normalizeIdentifier(t.ComponentName))
	}

	changed := false
	switch {
	case decl.Password != "":
		password, err := scope.expand(decl.Password)
		if err != nil {
			return svc, false, err
		}
		svc.password = password
	case state[name].Password != "":
		svc.password = state[name].Password
	default:
		// A new password only works on an empty data directory: the database
		// writes the password into its data on first start and ignores the
		// variable after that. Data left from an earlier deployment of the
		// same name (removed before removal deleted data, or a watcher whose
		// database was lost) would make the app fail with "password
		// authentication failed", far from the cause.
		if w.serviceHasOldData(namespace, svc.component, decl, def) {
			return svc, false, fmt.Errorf("its storage already holds data from an earlier deployment of %s, "+
				"but the password that data was created with is no longer known — remove this service and "+
				"create it again to start with an empty database, or set services.%s.password to the old password",
				svc.component, name)
		}
		password, err := generatePassword()
		if err != nil {
			return svc, false, err
		}
		svc.password = password
		state[name] = serviceState{Password: password}
		changed = true
	}

	// Environment: the type's bootstrap variables, then the manifest's own.
	svc.env = make(map[string]string, len(def.envTemplate)+len(decl.Env))
	for k, v := range def.envTemplate {
		svc.env[k] = svc.substitute(v)
	}
	for k, v := range decl.Env {
		expanded, err := scope.expand(v)
		if err != nil {
			return svc, changed, fmt.Errorf("env.%s: %w", k, err)
		}
		svc.env[k] = expanded
	}

	// Storage: a stateful service always gets a volume, named after the
	// service unless the manifest says otherwise. Without this a database
	// would lose everything on redeploy, which is never what anyone means.
	dataPath := firstNonEmpty(decl.Storage.Path, def.dataPath)
	if dataPath != "" {
		volume := firstNonEmpty(decl.Storage.Volume, svc.component+"-data")
		hostPath := w.volumePath(componentRef{namespace: namespace, name: svc.component}, volume)
		if err := os.MkdirAll(hostPath, 0o755); err != nil {
			return svc, changed, fmt.Errorf("storage: %w", err)
		}
		svc.mounts = []mountSpec{{HostPath: hostPath, ContainerPath: dataPath}}
	}

	// Ports: services are private to their network by default. Publishing
	// one is opt-in, for when a human wants to point psql at it.
	for _, p := range decl.Ports {
		ps, err := parseManifestPort(p)
		if err != nil {
			return svc, changed, fmt.Errorf("ports: %w", err)
		}
		svc.ports = append(svc.ports, client.PortSpec{
			ContainerPort: ps.ContainerPort,
			HostPort:      ps.HostPort,
			Protocol:      ps.Protocol,
		})
	}

	// Health: the type knows how to check itself. This is what makes
	// "wait until the database is ready" mean something.
	switch {
	case decl.Health.HTTP != "" || len(decl.Health.Exec) > 0:
		hc, err := manifestHealthToSpec(decl.Health)
		if err != nil {
			return svc, changed, err
		}
		svc.health = healthSpecToClient(hc)
	case len(def.healthExec) > 0:
		cmd := make([]string, 0, len(def.healthExec))
		for _, arg := range def.healthExec {
			cmd = append(cmd, svc.substitute(arg))
		}
		// Probed sooner and more often than the server-side defaults (5s
		// delay, 10s interval), because for a service this probe is not only
		// liveness: it is the gate the app's deploy waits on. The defaults
		// would add most of a needless ten seconds to every first deploy to
		// answer a question a database can usually answer in one.
		svc.health = &client.HealthCheckSpec{
			Type:                "exec",
			ExecCommand:         cmd,
			InitialDelaySeconds: 1,
			IntervalSeconds:     3,
			FailureThreshold:    def.healthFailures,
		}
	}

	return svc, changed, nil
}

// substitute fills the %USER%/%PASSWORD%/%DATABASE% placeholders the defaults
// table uses.
func (s resolvedService) substitute(v string) string {
	return strings.NewReplacer(
		"%USER%", s.user,
		"%PASSWORD%", s.password,
		"%DATABASE%", s.database,
	).Replace(v)
}

// deployService upserts the service component and waits for it to be ready.
func (w *Watcher) deployService(ctx context.Context, t *Target, svc resolvedService, namespace, network, groupID string) error {
	req := client.DeployRequest{
		// A service lives in the same namespace as the app that declared it:
		// it is only ever addressed from inside that network.
		Namespace: namespace,
		Name:      svc.component,
		Image:     svc.image,
		Env:       svc.env,
		// Not compared by serviceUnchanged (a component does not report its
		// command); it is fixed per type, so a change to it arrives with a
		// change to env, as the redis password did.
		Command: svc.command,
		Ports:   svc.ports,
		Network: network,
		GroupID: groupID,
		// Services are infrastructure: they should come back after a crash
		// or a host reboot without anyone asking.
		RestartPolicy: "always",
		HealthCheck:   svc.health,
		// Never through the ingress: no public hostname for a database, and
		// the proxy has no reason to route to it.
		Ingress: &client.IngressSpec{Off: true},
	}
	// A database qooq starts for a user is as much the user's load as the
	// app is, so it gets the server's default too. qooq.yaml cannot set limits
	// for a service, so there is nothing to check against the maximum.
	req.CPULimit, req.MemLimitBytes, _ = w.limits.apply(0, 0)
	for _, m := range svc.mounts {
		req.Mounts = append(req.Mounts, client.MountSpec{
			HostPath:      m.HostPath,
			ContainerPath: m.ContainerPath,
			ReadOnly:      m.ReadOnly,
		})
	}

	// An upsert always replaces the container, so blindly upserting on every
	// app deploy would bounce the database each time the app's code changes —
	// downtime for something that did not change. Redeploy only when the
	// desired spec actually differs from what is running.
	if existing, err := w.findComponent(ctx, componentRef{namespace: namespace, name: svc.component}); err != nil {
		w.log.Warn("could not check existing service (redeploying it)",
			zap.String("component", svc.component), zap.Error(err))
	} else if existing != nil && existing.Phase == "Running" && serviceUnchanged(existing, req) {
		w.log.Info("service already up to date", zap.String("component", svc.component))
		return nil
	}

	w.log.Info("deploying service",
		zap.String("target", t.Name), zap.String("service", svc.name),
		zap.String("component", svc.component), zap.String("image", svc.image))

	comp, err := w.client.Upsert(ctx, req)
	if err != nil {
		// notTheDaemon: a runtime that does not answer is not the
		// repository's fault — missing here, a docker restart counted
		// against every project with a database (E1).
		return fmt.Errorf("upsert %s: %w", svc.component, notTheDaemon(err))
	}
	return w.waitForService(ctx, comp.ID, svc)
}

// findComponent returns the component with this (namespace, name), or nil if
// there is none. qooqd addresses components by id, so a name lookup is a list
// scan.
//
// The namespace is part of the match, not a detail: two stacks each running a
// project called "api" are two different components, and treating them as one
// would have this report someone else's component as already up to date and
// skip deploying ours entirely.
func (w *Watcher) findComponent(ctx context.Context, ref componentRef) (*client.ComponentResponse, error) {
	comps, err := w.client.ListComponents(ctx)
	if err != nil {
		return nil, err
	}
	for i := range comps {
		if comps[i].Namespace == ref.namespace && comps[i].Name == ref.name && comps[i].Phase != "Removed" {
			return &comps[i], nil
		}
	}
	return nil, nil
}

// serviceUnchanged reports whether the running component already matches the
// deploy request in every field the service resolver sets. Fields qooqd fills
// in server-side (health check timings, a normalized restart policy) are
// compared only where the request states them, so a defaulted value is not
// mistaken for a difference.
func serviceUnchanged(existing *client.ComponentResponse, req client.DeployRequest) bool {
	if existing.Image != req.Image ||
		existing.Network != req.Network ||
		existing.RestartPolicy != req.RestartPolicy {
		return false
	}
	// A service from before Ingress.Off is redeployed once to get it.
	if (existing.Ingress != nil && existing.Ingress.Off) != (req.Ingress != nil && req.Ingress.Off) {
		return false
	}
	if len(existing.Env) != len(req.Env) {
		return false
	}
	for k, v := range req.Env {
		if existing.Env[k] != v {
			return false
		}
	}
	if len(existing.Mounts) != len(req.Mounts) {
		return false
	}
	for i, m := range req.Mounts {
		if existing.Mounts[i] != m {
			return false
		}
	}
	if len(existing.Ports) != len(req.Ports) {
		return false
	}
	for i, p := range req.Ports {
		if existing.Ports[i].ContainerPort != p.ContainerPort || existing.Ports[i].HostPort != p.HostPort {
			return false
		}
	}
	return healthProbeEqual(existing.HealthCheck, req.HealthCheck)
}

// healthProbeEqual compares the identifying part of a health check — what it
// probes, not how often. Intervals are server-defaulted, so comparing them
// would report a difference on every single deploy.
func healthProbeEqual(a, b *client.HealthCheckSpec) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	if a.Type != b.Type || a.HTTPPath != b.HTTPPath || a.HTTPPort != b.HTTPPort {
		return false
	}
	if len(a.ExecCommand) != len(b.ExecCommand) {
		return false
	}
	for i := range a.ExecCommand {
		if a.ExecCommand[i] != b.ExecCommand[i] {
			return false
		}
	}
	return true
}

// waitForService blocks until the service component is running (and, when it
// has a health check, passing it). Deploying the app before its database
// accepts connections just moves the failure into the app's first seconds,
// where it looks like an app bug.
func (w *Watcher) waitForService(ctx context.Context, id string, svc resolvedService) error {
	return w.waitForComponentReady(ctx, id, svc.component)
}

// waitForComponentReady blocks until the component is ready, or gives up.
// label names it in log lines and errors.
//
// Ready means Running *and*, when the component has a health check, that the
// probe has passed at least once. Those are two different claims and only the
// second one is useful to a caller that is about to connect: a database
// process that has started is still seconds away from accepting connections,
// and an app that builds its pool during startup — Go fx, NestJS, Spring —
// fails in that window. Waiting only for Running moved the failure into the
// app's first second, where it reads as an app bug: the build says success,
// the component says Exited, and the reason is only in the app's own log.
//
// A component with no health check makes no readiness claim, so Running is
// the whole answer for it and the behaviour is unchanged.
//
// "Running" is as far as qooqd's own state machine goes: a component with a
// health check that never passes gets restarted until the restart manager
// gives up, which is what RestartExhausted reports. So the terminal answers
// are "it is ready", "it has stopped trying", and — for an http probe that
// cannot see its app — "it is running and this is as good as this is going
// to get" (see healthGraceTimeout).
//
// A stack's depends_on: ... wait: ready uses this too — one definition of
// ready for a database and for a project, because there is no useful
// difference between them.
func (w *Watcher) waitForComponentReady(ctx context.Context, id, label string) error {
	deadline := time.Now().Add(serviceReadyTimeout)
	var lastPhase, lastErr string
	var runningSince time.Time

	for {
		comp, err := w.client.GetComponent(ctx, id)
		if err != nil {
			return fmt.Errorf("wait for %s: %w", label, err)
		}
		lastPhase, lastErr = comp.Phase, comp.LastError

		if comp.Phase == "Running" {
			switch {
			case comp.HealthCheck == nil:
				w.log.Info("component ready", zap.String("component", label))
				return nil

			case !comp.HealthyAt.IsZero():
				w.log.Info("component ready",
					zap.String("component", label),
					zap.String("probe", "passed"))
				return nil
			}

			// Running but not yet proved ready. Time the grace period from
			// the first moment it was running, not from the start of the
			// wait: pulling the image is not the probe's fault.
			if runningSince.IsZero() {
				runningSince = time.Now()
			}
			if comp.HealthCheck.Type != "exec" && time.Since(runningSince) > healthGraceTimeout {
				w.log.Warn("continuing without a passing health probe; anything connecting to this may fail at first",
					zap.String("component", label),
					zap.Duration("waited", healthGraceTimeout),
					zap.String("remedy", "check that health.port names a port this container actually listens on, or use an exec probe"))
				return nil
			}
		} else {
			// A restart resets the claim: wait for the new container to
			// prove itself rather than counting the old one's grace.
			runningSince = time.Time{}
		}

		if comp.RestartExhausted {
			return fmt.Errorf("%s gave up restarting (last error: %s)", label, dashIfBlank(lastErr))
		}
		if time.Now().After(deadline) {
			if lastPhase == "Running" {
				return fmt.Errorf("%s started but its health probe never passed within %s (last error: %s)",
					label, serviceReadyTimeout, dashIfBlank(lastErr))
			}
			return fmt.Errorf("%s not ready after %s (phase=%s, last error: %s)",
				label, serviceReadyTimeout, lastPhase, dashIfBlank(lastErr))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(servicePollInterval):
		}
	}
}

// ensureGroup returns the id of the group named after the app, creating it if
// it does not exist. qooqd rejects a duplicate name, so an existing group is
// found by listing rather than by handling the create error.
func (w *Watcher) ensureGroup(ctx context.Context, name string) (string, error) {
	groups, err := w.client.ListGroups(ctx)
	if err != nil {
		return "", err
	}
	for _, g := range groups {
		if g.Name == name {
			return g.ID, nil
		}
	}
	g, err := w.client.CreateGroup(ctx, client.CreateGroupRequest{Name: name})
	if err != nil {
		return "", err
	}
	return g.ID, nil
}

// healthSpecToClient converts the watcher's internal health DTO into the
// qooqd wire type.
func healthSpecToClient(hc *healthCheckSpec) *client.HealthCheckSpec {
	if hc == nil {
		return nil
	}
	return &client.HealthCheckSpec{
		Type:                hc.Type,
		HTTPPath:            hc.HTTPPath,
		HTTPPort:            hc.HTTPPort,
		ExecCommand:         hc.ExecCommand,
		IntervalSeconds:     hc.IntervalSeconds,
		TimeoutSeconds:      hc.TimeoutSeconds,
		FailureThreshold:    hc.FailureThreshold,
		InitialDelaySeconds: hc.InitialDelaySeconds,
	}
}

func serviceTypeNames() []string {
	names := make([]string, 0, len(serviceDefaults))
	for name := range serviceDefaults {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func dashIfBlank(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// normalizeIdentifier turns a component name into something safe to use as a
// database/user name: lowercase, non-alphanumerics folded to "_".
func normalizeIdentifier(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// serviceHasOldData reports whether the service's data directory already has
// something in it. Same path rule as the storage block in resolveService.
func (w *Watcher) serviceHasOldData(namespace, component string, decl manifestService, def serviceDefinition) bool {
	if firstNonEmpty(decl.Storage.Path, def.dataPath) == "" || w.mountsDir == "" {
		return false
	}
	volume := firstNonEmpty(decl.Storage.Volume, component+"-data")
	entries, err := os.ReadDir(w.volumePath(componentRef{namespace: namespace, name: component}, volume))
	return err == nil && len(entries) > 0
}

// persistServices writes the services list into the row's topology now,
// keeping the references the row already has (those are only known once the
// whole manifest has resolved, and are rewritten on success).
func (w *Watcher) persistServices(t *Target, services []watcherclient.TopologyService) {
	if w.db == nil || t.ID == 0 {
		return
	}
	var row Target
	var refs []string
	if err := w.db.Select("topology").First(&row, t.ID).Error; err == nil {
		if topo := row.topologyForResponse(); topo != nil {
			refs = topo.References
		}
	}
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).
		Update("topology", encodeTopology(services, refs)).Error; err != nil {
		w.log.Warn("could not record the services being deployed", zap.String("target", t.Name), zap.Error(err))
	}
}
