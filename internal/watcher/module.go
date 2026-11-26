// Package watcher is the domain module for qooq-watcher: watch targets CRUD,
// git new-commit polling, and the build/upsert pipeline against qooqd (M12).
//
// Wiring follows the weedbox `weedbox.Module[P]` pattern: a generic
// base (lifecycle hooks, named zap logger, config-path helper) plus a
// concrete Params struct that pulls in this module's fx dependencies.
package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/viper"
	"github.com/weedbox/common-modules/database"
	"github.com/weedbox/common-modules/http_server"
	"github.com/weedbox/weedbox"
	"go.uber.org/fx"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"github.com/qooqapp/qooq/pkg/client"
)

const ModuleName = "Watcher"

// Config keys for reaching qooqd. Unscoped (not under the module's own
// "watcher.*" namespace) since they describe an external service address,
// not this module's own tunables — mirrors how sqlite.path/http.port are
// named after the thing they configure rather than the module that reads them.
const (
	configQooqdAddr  = "qooqd.addr"
	configQooqdToken = "qooqd.token"

	// configQooqdBuildAddr/Token point at a SECOND qooqd that only builds.
	// Empty (the default, and every offline install) means builds run on the
	// same daemon as deployments, exactly as before. Why the split exists, and
	// what it does and does not fence, is in buildnode.go.
	configQooqdBuildAddr  = "qooqd.build_addr"
	configQooqdBuildToken = "qooqd.build_token"

	// configControlAddr/configControlToken point at a qooq external manager, and
	// are set only in a managed deployment. Their sole use is fetching clone
	// credentials for AuthTypeGitHubApp targets (controlauth.go); everything
	// else the watcher does is unchanged when they are empty, which is every
	// offline install.
	configControlAddr  = "control.addr"
	configControlToken = "control.token"

	defaultQooqdAddr = "http://127.0.0.1:7777"

	// configArtifactsDir (M16) is where process-runtime build artifacts are
	// copied to, one versioned subdir per component+sha:
	// <artifacts.dir>/<component>/<sha7>/<basename>. Relative paths resolve
	// against the watcher process's cwd.
	configArtifactsDir  = "artifacts.dir"
	defaultArtifactsDir = "artifacts"

	// configMountsDir (M23) is where run.files copies and named run.volumes
	// live: <mounts.dir>/<component>/files/<sha7>/... and
	// <mounts.dir>/<component>/volumes/<name>/. Relative paths resolve
	// against the watcher process's cwd.
	//
	// IMPORTANT when the watcher itself runs in a container: these paths are
	// handed to the docker daemon as bind sources, and the daemon resolves
	// them on the *host*, not inside the watcher's filesystem. mounts.dir
	// must therefore be a host bind path mounted into the watcher container
	// at the exact same path — otherwise the watcher writes files the daemon
	// cannot find.
	configMountsDir  = "mounts.dir"
	defaultMountsDir = "mounts"

	// configUploadsDir is where uploaded source archives (deploy_mode
	// "upload") are kept: <uploads.dir>/<target>/<revision>.tar.gz, the two
	// most recent per target. Empty means "uploads/ next to sqlite.path", or
	// under the cwd when there is no sqlite file (a postgres deployment) —
	// the archives are state, and state belongs beside the rest of it.
	configUploadsDir  = "uploads.dir"
	defaultUploadsDir = "uploads"

	// configMountsPolicy (M23) constrains which mount sources a repo's
	// qooq.yaml may use: "open" (default), "repo-only" or
	// "allowlist:/prefix,...". See mountsPolicy in mounts.go for why the
	// default is permissive.
	configMountsPolicy  = "mounts.policy"
	defaultMountsPolicy = mountsPolicyOpen

	// configHTTPToken (M18) gates every /v1/targets* and /v1/targets/*/trigger
	// route behind `Authorization: Bearer <token>` when non-empty. The webhook
	// endpoint (/v1/webhook/:name) is deliberately exempt: it authenticates
	// via its own per-target HMAC signature (see webhook.go), and external git
	// hosts calling in have no way to attach this bearer token. Empty (the
	// default) leaves the API exactly as before (no auth).
	configHTTPToken = "http.token"

	// configSecretKey (M18) is the passphrase AES-256-GCM-derives a key from
	// to encrypt Target.AuthCredential/WebhookSecret at rest. Empty (the
	// default) keeps the M12 plaintext stance.
	configSecretKey = "secret.key"
	// configAllowProcessEnv gates ${env.*} and the QOOQ_SECRET_<NAME> fallback.
	configAllowProcessEnv = "secrets.allow_process_env"
	// configUntrustedRepos declares that the repositories this watcher builds
	// are not the operator's own. It is the master switch for build isolation:
	// see trust.go.
	configUntrustedRepos = "untrusted_repos"
	// Registry mode.
	configRegistryPrefix   = "registry.prefix"
	configRegistryUsername = "registry.username"
	configRegistryPassword = "registry.password"

	// configHTTPAllowedHosts is a comma-separated list of extra Host header
	// values accepted by the drive-by guard when no http.token is set. The
	// localhost forms of http.port are always allowed; this widens that set
	// for proxy/non-localhost access. Ignored entirely when a token is set.
	configHTTPAllowedHosts = "http.allowed_hosts"

	// configGitKnownHosts (P1-9), when non-empty, points ssh-key clones at a
	// known_hosts file for host key verification. Empty (the default) leaves
	// go-git's own strict known_hosts DB in force.
	configGitKnownHosts = "git.known_hosts"

	// configGitInsecureHostKey (P1-9), when true, disables SSH host key
	// verification entirely (dev/test only; logs a warning at startup).
	configGitInsecureHostKey = "git.insecure_host_key"

	// configBackupDir/Interval/Keep (risk #6) drive scheduled sqlite backups.
	// An empty dir (the default) disables automatic backups entirely, matching
	// the pre-existing manual-only /v1/backup behaviour.
	configBackupDir      = "backup.dir"
	configBackupInterval = "backup.interval"
	configBackupKeep     = "backup.keep"

	defaultBackupInterval = time.Hour
	defaultBackupKeep     = 5

	// configRequestsDefaultArch (M22, renamed from configResourceDefaultArch
	// when resource: split into requests:/limits:) is the fallback
	// requests.arch enforced by checkRequests when a manifest doesn't set one
	// itself (see effectiveArch in pipeline.go). Empty (the default) means
	// "don't enforce an arch requirement absent an explicit one in the
	// manifest" — today's behavior, unchanged. An operator running a fleet of
	// same-arch hosts may want to set this so every target implicitly
	// requires that arch without every qooq.yaml having to say so.
	configRequestsDefaultArch = "requests.default_arch"

	// configLimitsMultiplier (M22) is the factor applied to a requests: field
	// to derive the corresponding container cgroup cap (limits.cpu/
	// limits.memory) whenever the manifest's limits: section leaves that
	// field unset — see deriveContainerLimits in manifest.go. Defaults to
	// 2.0 (k8s-style "limits = 2x requests" convention). Env
	// QOOQ_WATCHER_LIMITS_MULTIPLIER. A configured value <= 0 is invalid and
	// falls back to the 2.0 default (see OnStart) rather than producing a
	// nonsensical zero/negative cap.
	configLimitsMultiplier = "limits.multiplier"
	// configBuildWorkers caps how many targets build at once
	// (QOOQ_WATCHER_SWEEP_WORKERS).
	configBuildWorkers = "sweep.workers"

	// configNamespaceBuilds caps concurrent builds per namespace. 0 (the
	// default) means no cap, which is correct for a single-user install:
	// every namespace-less target normalizes to "default", so there a cap
	// would act as a global one. See newBuildQueue.
	configNamespaceBuilds = "sweep.builds_per_namespace"

	defaultLimitsMultiplier = 2.0
)

type Params struct {
	weedbox.Params

	Database   database.DatabaseConnector
	HTTPServer *http_server.HTTPServer
}

type Watcher struct {
	weedbox.Module[*Params]
	scope string

	db     *gorm.DB
	client *client.Client
	// buildClient is nil unless qooqd.build_addr names a different daemon;
	// read it through buildNode(), never directly.
	buildClient *client.Client
	// baseRefresh re-pulls the images builds start FROM (baserefresh.go).
	baseRefresh  *baseRefresher
	log          *zap.Logger
	cancel       context.CancelFunc
	backupCancel context.CancelFunc
	// disk is the last per-namespace data measurement (diskusage.go).
	disk         diskUsage
	artifactsDir string

	// mountsDir (M23) is the absolute root of the per-component mounts tree
	// (see configMountsDir); mountsPolicy gates which sources a manifest may
	// name (see configMountsPolicy).
	mountsDir    string
	mountsPolicy mountsPolicy

	// uploadsDir is the absolute root of stored source uploads (see
	// configUploadsDir and upload.go).
	uploadsDir string

	// httpToken (M18), when non-empty, is required as a Bearer token on every
	// /v1/targets* route (see api.go's registerRoutes/bearerAuth).
	httpToken string
	// allowedHosts is the Host-header allowlist enforced by the drive-by guard
	// when httpToken is empty (see api.go's protectChain/hostGuard).
	allowedHosts []string
	// secretKey (M18), when non-empty, is the passphrase used to
	// AES-256-GCM-encrypt AuthCredential/WebhookSecret at rest (see crypto.go).
	secretKey string
	// control (managed deployments only) is how clone credentials for AuthTypeGitHubApp
	// targets are obtained. nil on every offline watcher.
	control *controlLink

	// allowProcessEnv (M3) lets ${env.NAME} and the QOOQ_SECRET_<NAME> fallback
	// read this process's environment.
	//
	// True by default, because for a single operator the watcher's environment
	// is their own and reading it is the intended convenience. It MUST be false
	// on a watcher serving more than one user: there the environment belongs
	// to the deployment, and one user's manifest reading it is the most
	// severe of the three shared-node blockers.
	//
	// Note the zero value is false, the opposite of the configured default.
	// That is the right way round: a Watcher constructed without reading its
	// configuration denies rather than allows, so the failure mode of a missed
	// initialisation is a reference that will not resolve, not a user reading
	// the environment.
	allowProcessEnv bool
	// untrustedRepos (build isolation) says the repositories this watcher
	// builds belong to someone other than the operator. See trust.go for what
	// it turns off and why each one is a hole without it.
	//
	// Zero value false means "trusted", which is the permissive direction — the
	// opposite of allowProcessEnv above. That is deliberate and it is the only
	// safe way round for THIS flag: the offline product is the default, and a
	// watcher that defaulted to untrusted would refuse build.command for every
	// existing single-operator install.
	untrustedRepos bool
	// registry, when its prefix is set, is where built images
	// are pushed so a qooqd on another machine can pull them. Empty means
	// single-machine behaviour: build straight into the daemon that will run
	// it, which is the deliberate default.
	registry registryConfig

	// sshKnownHosts / sshInsecure (P1-9) form the SSH host key verification
	// policy applied to every ssh-key clone (see git.go's sshHostKeyPolicy).
	sshKnownHosts string
	sshInsecure   bool

	// defaultArch (M21) is the fallback requests.arch checkRequests enforces
	// when a manifest doesn't set requests.arch itself (see effectiveArch).
	// Empty means no fallback (v1 default: unchanged behavior).
	defaultArch string

	// limitsMultiplier (M22) is the factor applied to a requests: field to
	// derive the corresponding container cgroup cap when limits: leaves that
	// field unset — see deriveContainerLimits in manifest.go and
	// configLimitsMultiplier above. Always > 0 (OnStart falls back to
	// defaultLimitsMultiplier for a configured value <= 0).
	limitsMultiplier float64
	// limits are the server-wide default and maximum container limits
	// (limits.go). Zero value: no default, no maximum.
	limits containerLimits

	// targetLocks serializes all "act on target X" work between the sweep
	// goroutine and the M20b /deploy request handler (per target name; names
	// are unique). Before M20b every deploy ran on the single sweep goroutine
	// and no lock was needed; /deploy runs pull+upsert inline on the request
	// goroutine, so an in-flight sweep working from a pre-/deploy row
	// snapshot could otherwise redeploy the old image right over it. Entries
	// are never removed (a deleted target leaves one idle mutex behind —
	// negligible for the single-VM target counts qooq aims at).
	targetLocks sync.Map // target name -> *sync.Mutex

	// stackLocks does for stacks what targetLocks does for targets: one deploy of
	// a given stack at a time, so a trigger arriving mid-deploy queues behind the
	// walk in progress instead of racing it through the same projects.
	stackLocks sync.Map // stack name -> *sync.Mutex

	// queue holds which targets are being built right now, so a build no
	// longer blocks the sweep from noticing every other target. The durable
	// "needs attention" record stays in the targets table — see buildQueue.
	queue *buildQueue
	// buildWorkers is how many targets may build at once (config
	// sweep.workers). Always > 0; OnStart falls back to the default.
	buildWorkers int
	// namespaceBuilds caps how many of one namespace's targets may build at
	// once (config sweep.builds_per_namespace); 0 means no cap.
	namespaceBuilds int
	// instanceID names this watcher process in targets.claimed_by, so two
	// watchers sharing a database can tell each other's work apart. Set once
	// in OnStart and never changed — a claim renewed under a different id
	// would look like someone else's.
	instanceID string

	// stackWake asks the stack worker for a pass; stackBusy keeps at most one
	// pass queued, since a pass already looks at every stack.
	stackWake chan struct{}
	stackBusy atomic.Bool
}

// lockTarget acquires the per-target mutex for name and returns its unlock
// func. Hold it for the entire read-decide-act sequence on that target: the
// sweep path holds it across digest-check/clone/build/deploy, so a concurrent
// POST /v1/targets/{name}/deploy simply blocks until the in-flight check
// finishes (and then acts on the fresh row), instead of racing it.
func (w *Watcher) lockTarget(name string) (unlock func()) {
	v, _ := w.targetLocks.LoadOrStore(name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// tryLockTarget is lockTarget for callers that must not wait: ok is false
// when somebody else holds it.
func (w *Watcher) tryLockTarget(name string) (unlock func(), ok bool) {
	v, _ := w.targetLocks.LoadOrStore(name, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// sshHostKey returns the SSH host key verification policy for git operations,
// resolved from config at OnStart.
func (m *Watcher) sshHostKey() sshHostKeyPolicy {
	return sshHostKeyPolicy{KnownHostsPath: m.sshKnownHosts, Insecure: m.sshInsecure}
}

// gitAuth bundles everything the git helpers need to authenticate: the host key
// policy, and (managed deployments only) the way to fetch a GitHub App clone credential.
func (m *Watcher) gitAuth() gitAuth {
	return gitAuth{hostKey: m.sshHostKey(), appToken: m.appCloneAuth()}
}

func Module(scope string) fx.Option {
	m := &Watcher{scope: scope}

	return fx.Module(
		scope,
		fx.Supply(
			fx.Annotated{
				Name:   scope,
				Target: m,
			},
		),
		fx.Invoke(func(p Params) {
			weedbox.InitModule(scope, &p, m)
		}),
	)
}

func (m *Watcher) InitDefaultConfigs() {
	viper.SetDefault(configQooqdAddr, defaultQooqdAddr)
	viper.SetDefault(configQooqdToken, "")
	viper.SetDefault(configQooqdBuildAddr, "")
	viper.SetDefault(configQooqdBuildToken, "")
	viper.SetDefault(configControlAddr, "")
	viper.SetDefault(configControlToken, "")
	viper.SetDefault(configArtifactsDir, defaultArtifactsDir)
	viper.SetDefault(configMountsDir, defaultMountsDir)
	viper.SetDefault(configUploadsDir, "")
	viper.SetDefault(configMountsPolicy, defaultMountsPolicy)
	viper.SetDefault(configHTTPToken, "")
	viper.SetDefault(configHTTPAllowedHosts, "")
	viper.SetDefault(configSecretKey, "")
	viper.SetDefault(configUntrustedRepos, false)
	viper.SetDefault(configRegistryPrefix, "")
	viper.SetDefault(configRegistryUsername, "")
	viper.SetDefault(configRegistryPassword, "")
	viper.SetDefault(configGitKnownHosts, "")
	viper.SetDefault(configGitInsecureHostKey, false)
	viper.SetDefault(configBackupDir, "")
	viper.SetDefault(configBackupInterval, defaultBackupInterval)
	viper.SetDefault(configBaseRefreshInterval, defaultBaseRefreshInterval)
	viper.SetDefault(configBackupKeep, defaultBackupKeep)
	viper.SetDefault(configRequestsDefaultArch, "")
	viper.SetDefault(configLimitsMultiplier, defaultLimitsMultiplier)
}

func (m *Watcher) OnStart(ctx context.Context) error {
	m.log = m.Logger()
	m.log.Info("Starting " + ModuleName)

	m.db = m.Params().Database.GetDB()
	if err := m.db.AutoMigrate(&Target{}, &Stack{}, &Secret{}); err != nil {
		return fmt.Errorf("watcher: automigrate: %w", err)
	}
	// Upload targets created before createTarget cleared it carry the column
	// default "main". Idempotent, so it runs every boot.
	if err := m.db.Model(&Target{}).Where("deploy_mode = ? AND branch <> ''", DeployModeUpload).
		Update("branch", "").Error; err != nil {
		return fmt.Errorf("watcher: clear upload branches: %w", err)
	}

	// Logged on every boot because it is the only way to read targets.claimed_by
	// afterwards and tell which process a claim belongs to — the same reason the
	// advertise-host detection logs its choice.
	m.instanceID = newInstanceID()
	m.log.Info("watcher instance id", zap.String("instance", m.instanceID))

	if err := m.lockDownDBFile(); err != nil {
		// Not fatal: credentials being world-readable is a security warning,
		// not a reason to refuse to start (v1 stance).
		m.log.Warn("could not chmod sqlite db file to 0600", zap.Error(err))
	}

	m.untrustedRepos = viper.GetBool(configUntrustedRepos)

	m.baseRefresh = newBaseRefresher(viper.GetDuration(configBaseRefreshInterval))

	m.registry = registryConfig{
		Prefix:   viper.GetString(configRegistryPrefix),
		Username: viper.GetString(configRegistryUsername),
		Password: viper.GetString(configRegistryPassword),
	}
	if m.registry.enabled() {
		m.log.Info("registry mode: built images are pushed and deployed by their remote ref",
			zap.String("prefix", m.registry.Prefix))
	}

	// allow_process_env follows untrusted_repos unless it was set explicitly.
	//
	// Composed rather than defaulted independently so that turning on
	// untrusted_repos cannot leave the other half open. A two-setting secure
	// configuration is one setting away from being half-done, and the half that
	// gets forgotten is the one whose absence is invisible.
	if viper.IsSet(configAllowProcessEnv) {
		m.allowProcessEnv = viper.GetBool(configAllowProcessEnv)
	} else {
		m.allowProcessEnv = !m.untrustedRepos
	}

	if m.untrustedRepos {
		m.log.Info("repositories are treated as untrusted: build.command is refused, " +
			"mounts are restricted to the repo, and ${env.*} is disabled")
	}
	m.warnIfProcessEnvIsShared()

	m.secretKey = viper.GetString(configSecretKey)
	if m.secretKey == "" {
		m.log.Warn("watcher stores git/webhook credentials in plaintext in sqlite (QOOQ_WATCHER_SECRET_KEY not set); restrict access to the db file")
	} else {
		if err := m.upgradePlaintextCredentials(); err != nil {
			m.log.Warn("upgrade plaintext credentials to encrypted storage failed", zap.Error(err))
		}
	}

	m.httpToken = viper.GetString(configHTTPToken)
	if m.httpToken == "" {
		m.log.Warn("watcher API has no auth token configured (QOOQ_WATCHER_HTTP_TOKEN); localhost-only binding + Host guard are the only protection")
	}

	// Host-header allowlist for the no-auth drive-by guard: the localhost
	// forms of the bound port, plus any extras from http.allowed_hosts.
	m.allowedHosts = defaultAllowedHosts(viper.GetInt("http.port"))
	if extra := viper.GetString(configHTTPAllowedHosts); extra != "" {
		for _, h := range strings.Split(extra, ",") {
			if h = strings.TrimSpace(h); h != "" {
				m.allowedHosts = append(m.allowedHosts, h)
			}
		}
	}

	m.sshKnownHosts = viper.GetString(configGitKnownHosts)
	m.sshInsecure = viper.GetBool(configGitInsecureHostKey)
	if m.sshInsecure {
		m.log.Warn("SSH host key verification is DISABLED (QOOQ_WATCHER_GIT_INSECURE_HOST_KEY); clones are vulnerable to MITM — dev/test only")
	}

	m.defaultArch = viper.GetString(configRequestsDefaultArch)

	m.limitsMultiplier = viper.GetFloat64(configLimitsMultiplier)
	if m.limitsMultiplier <= 0 {
		m.log.Warn("limits.multiplier must be > 0, falling back to default",
			zap.Float64("configured", m.limitsMultiplier), zap.Float64("default", defaultLimitsMultiplier))
		m.limitsMultiplier = defaultLimitsMultiplier
	}

	limits, err := newContainerLimits(
		viper.GetFloat64(configLimitsDefaultCPU), viper.GetString(configLimitsDefaultMemory),
		viper.GetFloat64(configLimitsMaxCPU), viper.GetString(configLimitsMaxMemory))
	if err != nil {
		return fmt.Errorf("watcher: %w", err)
	}
	m.limits = limits
	if limits != (containerLimits{}) {
		m.log.Info("container limits",
			zap.Float64("default_cpu", limits.defaultCPU), zap.Int64("default_memory_bytes", limits.defaultMem),
			zap.Float64("max_cpu", limits.maxCPU), zap.Int64("max_memory_bytes", limits.maxMem))
	}

	addr := viper.GetString(configQooqdAddr)
	token := viper.GetString(configQooqdToken)
	var opts []client.Option
	if token != "" {
		opts = append(opts, client.WithToken(token))
	}
	m.client = client.New(addr, opts...)
	m.log.Info("qooqd client configured", zap.String("addr", addr))

	// Same address means the same node, whatever the token says: pointing
	// build_addr at qooqd.addr must not produce two clients, or image
	// retention would walk the one daemon twice and log every prune twice.
	if err := checkBuildNodeRegistry(viper.GetString(configQooqdBuildAddr), addr, m.registry); err != nil {
		return err
	}
	if buildAddr := viper.GetString(configQooqdBuildAddr); buildAddr != "" && buildAddr != addr {
		buildOpts := []client.Option(nil)
		// Falls back to the deploy node's token rather than to no token: one
		// daemon per machine with a shared token is the ordinary setup, and a
		// silently unauthenticated build client would fail every build with a
		// 401 that says nothing about which address was missing a token.
		buildToken := viper.GetString(configQooqdBuildToken)
		if buildToken == "" {
			buildToken = token
		}
		if buildToken != "" {
			buildOpts = append(buildOpts, client.WithToken(buildToken))
		}
		m.buildClient = client.New(buildAddr, buildOpts...)
		m.log.Info("builds run on a separate qooqd",
			zap.String("build_addr", buildAddr),
			zap.String("deploy_addr", addr),
			zap.String("note", "the build node's disk is expendable; deployments do not share it"))
	}

	m.initControlLink(viper.GetString(configControlAddr), viper.GetString(configControlToken))

	m.artifactsDir = viper.GetString(configArtifactsDir)
	if m.artifactsDir == "" {
		m.artifactsDir = defaultArtifactsDir
	}

	// mountsDir is resolved to an absolute path once, here: every value that
	// leaves this package ends up as a docker bind source, and a relative
	// bind source is meaningless to the daemon.
	mountsDir := viper.GetString(configMountsDir)
	if mountsDir == "" {
		mountsDir = defaultMountsDir
	}
	absMountsDir, err := filepath.Abs(mountsDir)
	if err != nil {
		return fmt.Errorf("resolve %s %q: %w", configMountsDir, mountsDir, err)
	}
	m.mountsDir = absMountsDir

	uploadsDir := viper.GetString(configUploadsDir)
	if uploadsDir == "" {
		uploadsDir = defaultUploadsDir
		if db := viper.GetString("sqlite.path"); db != "" {
			uploadsDir = filepath.Join(filepath.Dir(db), defaultUploadsDir)
		}
	}
	absUploadsDir, err := filepath.Abs(uploadsDir)
	if err != nil {
		return fmt.Errorf("resolve %s %q: %w", configUploadsDir, uploadsDir, err)
	}
	m.uploadsDir = absUploadsDir

	policy, err := parseMountsPolicy(viper.GetString(configMountsPolicy))
	if err != nil {
		return err
	}
	m.mountsPolicy = m.effectiveMountsPolicy(policy)
	if m.mountsPolicy.raw != policy.raw {
		m.log.Warn("mounts.policy narrowed because repositories are untrusted",
			zap.String("configured", policy.raw), zap.String("using", m.mountsPolicy.raw))
	}
	m.log.Info("mounts configured", zap.String("dir", m.mountsDir), zap.String("policy", m.mountsPolicy.raw))

	m.registerRoutes(m.Params().HTTPServer.GetRouter())

	sweepCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.namespaceBuilds = viper.GetInt(configNamespaceBuilds)
	if m.namespaceBuilds < 0 {
		m.log.Warn("sweep.builds_per_namespace must be >= 0, treating as no limit",
			zap.Int("configured", m.namespaceBuilds))
		m.namespaceBuilds = 0
	}
	m.queue = newBuildQueue(m.namespaceBuilds)
	m.stackWake = make(chan struct{}, 1)
	m.buildWorkers = viper.GetInt(configBuildWorkers)
	if m.buildWorkers <= 0 {
		m.buildWorkers = defaultBuildWorkers
	}
	if m.namespaceBuilds > 0 {
		// Logged because a throttled user has no other symptom: the target
		// keeps its previous status and simply starts later.
		m.log.Info("per-namespace build limit active",
			zap.Int("builds_per_namespace", m.namespaceBuilds),
			zap.Int("workers", m.buildWorkers))
	}
	m.runBuildWorkers(sweepCtx, m.buildWorkers)
	m.runStackWorker(sweepCtx)
	go m.runSweepLoop(sweepCtx)
	go m.runDiskUsageLoop(sweepCtx)

	// Scheduled sqlite backups (no-op unless backup.dir/QOOQ_WATCHER_BACKUP_DIR
	// is set). Runs on its own context so OnStop stops it cleanly.
	backupCtx, backupCancel := context.WithCancel(context.Background())
	m.backupCancel = backupCancel
	go m.runBackupLoop(backupCtx,
		viper.GetString(configBackupDir),
		viper.GetDuration(configBackupInterval),
		viper.GetInt(configBackupKeep),
	)

	m.log.Info("watcher module started")
	return nil
}

func (m *Watcher) OnStop(ctx context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}
	if m.backupCancel != nil {
		m.backupCancel()
	}
	m.log.Info("Stopped " + ModuleName)
	return nil
}

// lockDownDBFile chmods the sqlite db file (and its WAL/SHM siblings, if
// present) to 0600, since Target.AuthCredential/WebhookSecret are stored in
// plaintext.
//
// It reads the mode back rather than trusting Chmod's nil, because Chmod
// succeeding does not mean the permissions took. On Windows only the 0o200 bit
// is used at all — the rest are documented as ignored — and on a filesystem
// without POSIX permissions (FAT/exFAT, some network mounts) the call is a
// no-op that still reports success. Either way the credentials end up readable
// with nothing in the log to say so, which is the failure this whole function
// exists to prevent.
func (m *Watcher) lockDownDBFile() error {
	path := viper.GetString("sqlite.path")
	if path == "" {
		return nil
	}
	var firstErr error
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err != nil {
			continue
		}
		if err := os.Chmod(p, 0o600); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if mode, ok := effectiveMode(p); ok && mode != 0o600 {
			// Not an error: there is nothing to retry and refusing to start
			// would be worse than running with a warning. But it has to be
			// loud, and it has to name the way out.
			m.log.Warn("database file permissions were not applied; its contents are readable by other users on this machine",
				zap.String("path", p),
				zap.String("requested", "0600"),
				zap.String("actual", mode.String()),
				zap.String("remedy", "store it on a filesystem with POSIX permissions, or set QOOQ_WATCHER_SECRET_KEY so credentials are encrypted at rest"))
		}
	}
	return firstErr
}

// effectiveMode reports the permission bits a file actually carries. The bool
// distinguishes "could not tell" from "told you 0600" so a stat failure never
// produces a false alarm.
func effectiveMode(path string) (os.FileMode, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return fi.Mode().Perm(), true
}

// warnIfProcessEnvIsShared reports the one case where allowing ${env.*} is
// actually dangerous.
//
// Not warned about unconditionally: on a single-namespace install reading the
// watcher's own environment is the intended behaviour, and a warning every
// start would be exactly the routinely-harmless kind this project has twice
// decided not to emit — the duplicate-deployment guard and manifest validate
// both chose to block or stay silent rather than cry wolf.
//
// More than one namespace on one watcher is the first concrete evidence that
// the environment is shared between users, which is when it stops being a
// convenience and becomes a way to read somebody else's credentials.
func (m *Watcher) warnIfProcessEnvIsShared() {
	if !m.allowProcessEnv {
		m.log.Info("${env.*} and the QOOQ_SECRET_ fallback are disabled; secrets resolve only from the per-namespace store")
		return
	}
	n, err := m.distinctTargetNamespaces()
	if err != nil || n <= 1 {
		return
	}
	m.log.Warn("this watcher serves more than one namespace and ${env.*} can still read its environment; "+
		"a manifest in one namespace can read another's credentials",
		zap.Int("namespaces", n),
		zap.String("set", "QOOQ_WATCHER_SECRETS_ALLOW_PROCESS_ENV=false"))
}
