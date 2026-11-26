// Command qooqd is the qooq orchestrator daemon: the composition root that
// wires the Docker runtime adapter, the bbolt store, the core service and the
// REST API, then serves HTTP on localhost.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qooqapp/qooq/internal/api"
	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/ingress"
	"github.com/qooqapp/qooq/internal/monitor"
	"github.com/qooqapp/qooq/internal/runtime"
	"github.com/qooqapp/qooq/internal/runtime/docker"
	"github.com/qooqapp/qooq/internal/runtime/process"
	"github.com/qooqapp/qooq/internal/store/bbolt"
	"github.com/qooqapp/qooq/internal/version"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7777", "HTTP listen address")
	dataPath := flag.String("data", "qooq.db", "bbolt data file path")
	tokenFlag := flag.String("token", "", "bearer token required for /v1 API access, except /healthz (default: QOOQ_TOKEN env; empty disables auth)")
	runtimeFlag := flag.String("runtime", "docker", "container runtime backend: docker or process")
	procDirFlag := flag.String("proc-dir", "", "process runtime instance state directory (default: qooq-proc next to -data; only used when -runtime=process)")
	backupDirFlag := flag.String("backup-dir", "", "if set, periodically snapshot the datastore into this directory (default: QOOQ_BACKUP_DIR env; empty disables automatic backups)")
	backupIntervalFlag := flag.Duration("backup-interval", time.Hour, "interval between automatic backups (default: QOOQ_BACKUP_INTERVAL env, else 1h)")
	backupKeepFlag := flag.Int("backup-keep", 5, "number of newest automatic backups to retain (default: QOOQ_BACKUP_KEEP env, else 5)")
	allowedHostsFlag := flag.String("allowed-hosts", "", "comma-separated extra Host header values accepted when auth is disabled (default: QOOQ_ALLOWED_HOSTS env; localhost forms of -listen are always allowed)")
	// Build policy. Set on the daemon, not per request: what submits a build
	// on a shared node is a repository you do not control, and a limit the submitter can
	// raise is not a limit.
	buildTimeoutFlag := flag.Duration("build-timeout", 30*time.Minute, "abort an image build after this long; 0 disables (a Dockerfile that never finishes otherwise holds a build slot forever)")
	buildMemoryFlag := flag.String("build-memory", "", "memory limit for image builds, e.g. 2g or 512m (default: unlimited, which is right when every build is your own)")
	buildCPUsFlag := flag.Float64("build-cpus", 0, "CPU cores an image build may use, e.g. 1.5 (default: 0 = unlimited)")
	buildImageMaxFlag := flag.String("build-image-max", "", "refuse (and remove) a built image larger than this, e.g. 5g (default: empty = no limit)")
	buildMinFreeFlag := flag.String("build-min-free-disk", "", "refuse every build while less than this share of the disk is free, e.g. 15% (default: empty = no check); measured on -build-disk-path")
	buildDiskPathFlag := flag.String("build-disk-path", "/var/lib/docker", "where builds write — docker's data root — for -build-min-free-disk")
	buildCacheMaxFlag := flag.String("build-cache-max", "", "keep build cache under this size after each build, e.g. 10g (default: empty = never prune; docker has no per-build disk quota, so this is the only enforceable disk bound)")
	// Container policy. Same split as the build flags: set on the daemon,
	// because the component spec comes from a user's manifest.
	pidsLimitFlag := flag.Int64("container-pids-limit", core.DefaultContainerPidsLimit, "max processes+threads per container, a fork-bomb guard (default: QOOQ_CONTAINER_PIDS_LIMIT env, else 2048; 0 = unlimited). Threads count, so a thread-per-request server with thousands of live threads is the one app that can notice it")
	containerMemMaxFlag := flag.String("container-memory-max", "", "ceiling on any container's memory, e.g. 2g or 512m: components asking for nothing get it, components asking for more are clamped (default: QOOQ_CONTAINER_MEMORY_MAX env, else no ceiling)")
	containerCPUsMaxFlag := flag.Float64("container-cpus-max", 0, "ceiling on any container's CPU cores, e.g. 2: same rule as -container-memory-max (default: QOOQ_CONTAINER_CPUS_MAX env, else no ceiling)")
	hardenFlag := flag.Bool("harden-containers", false, "harden every container for a machine that runs strangers' code: no-new-privileges, NET_RAW/MKNOD/AUDIT_WRITE/SYS_CHROOT/SETFCAP dropped, and a seccomp profile that also refuses SCTP sockets and socketcall (default: QOOQ_HARDEN_CONTAINERS env, else off). Docker runtime only")
	instanceFlag := flag.String("instance", "", "qooqd instance id: containers are labeled qooq.instance=<id> and reconcile/events/orphan-adoption only ever touch this instance's own containers, so multiple independent qooqd can share one docker host without interfering (default: QOOQ_INSTANCE env; empty = default instance, pre-M20 behavior)")
	advertiseHostFlag := flag.String("advertise-host", "", "host or IP other machines use to reach this daemon's components; reported as each component's endpoints (default: QOOQ_ADVERTISE_HOST env, else the primary network interface's IPv4)")
	publishIPFlag := flag.String("publish-ip", "", "host address published ports bind to when a component does not name one (default: QOOQ_PUBLISH_IP env, else 0.0.0.0); 127.0.0.1 on a shared machine behind the ingress, so other users cannot reach each other's ports through the bridge gateway")
	autoPortRangeFlag := flag.String("auto-port-range", fmt.Sprintf("%d-%d", core.DefaultAutoPortLow, core.DefaultAutoPortHigh), "host port range that ports written \"auto:<container>\" are drawn from")
	ingressFlag := flag.Bool("ingress", false, "run a reverse proxy in front of components so each is reachable at <name>.localhost (and at <name>.<advertise-host>.nip.io from the LAN) instead of a host port; off by default because it binds -ingress-http-port on this machine")
	ingressPortFlag := flag.Int("ingress-http-port", 80, "host port the ingress publishes for HTTP (only used with -ingress)")
	ingressTLSFlag := flag.Bool("ingress-tls", false, "let the ingress obtain certificates automatically for hostnames a public CA can issue for; requires the domain's DNS to point at this host and port 443 to be reachable (only used with -ingress)")
	ingressTLSIssuerFlag := flag.String("ingress-tls-issuer", ingress.IssuerACME, `who signs ingress certificates: "acme" (a public CA; needs public DNS pointing here and reachable 443) or "internal" (Caddy own CA; works offline for any hostname including .localhost, but the root must be trusted on every device that connects)`)
	ingressHTTPSPortFlag := flag.Int("ingress-https-port", 443, "host port the ingress listens on and publishes for HTTPS (only used with -ingress)")
	ingressAppDomainFlag := flag.String("ingress-app-domain", "", "the domain shared-node app services are served under (like apps.example.com): a namespace shared with other users is then routed only on its assigned <label>.<this domain>, over https, never via a certificate authority; empty (the default) leaves routing as it is (only used with -ingress)")
	ingressAppDomainRedirectFlag := flag.String("ingress-app-domain-redirect", "", "URL the bare -ingress-app-domain answers with a 301 to (like https://example.com); empty leaves the bare domain unhandled")
	ingressCertFileFlag := flag.String("ingress-cert-file", "", "a fixed certificate (PEM) the ingress serves -ingress-app-domain names with, e.g. a Cloudflare Origin CA certificate for *.<app domain>; needs -ingress-key-file")
	ingressKeyFileFlag := flag.String("ingress-key-file", "", "the private key (PEM) for -ingress-cert-file")
	ingressBindIPFlag := flag.String("ingress-bind-ip", "", "host address the ingress's own ports bind to (default 0.0.0.0, reachable from the network; 127.0.0.1 keeps it to this machine)")
	ingressCloudflareFlag := flag.Bool("ingress-cloudflare", false, "the ingress sits behind Cloudflare: trust Cloudflare's address ranges as proxies and take the client address from CF-Connecting-IP")
	versionFlag := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if err := applyBuildFlagEnv(); err != nil {
		fmt.Fprintln(os.Stderr, "qooqd:", err)
		os.Exit(1)
	}

	// Answered before anything else is set up: "which build is this" has to be
	// answerable on a machine where docker is not running and the datastore is
	// not openable, since that is often exactly when it is asked.
	if *versionFlag {
		fmt.Println("qooqd", version.String())
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	token := *tokenFlag
	if token == "" {
		token = os.Getenv("QOOQ_TOKEN")
	}

	instance := *instanceFlag
	if instance == "" {
		instance = os.Getenv("QOOQ_INSTANCE")
	}

	// Automatic backup config: flags win when explicitly passed, otherwise the
	// QOOQ_BACKUP_* env vars fill in (mirrors -token / QOOQ_TOKEN). An empty
	// backup dir keeps the historical behaviour: no scheduled backups at all.
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	backupDir := *backupDirFlag
	if !set["backup-dir"] {
		backupDir = os.Getenv("QOOQ_BACKUP_DIR")
	}
	backupInterval := *backupIntervalFlag
	if !set["backup-interval"] {
		if v := os.Getenv("QOOQ_BACKUP_INTERVAL"); v != "" {
			if d, err := time.ParseDuration(v); err == nil {
				backupInterval = d
			} else {
				log.Warn("invalid QOOQ_BACKUP_INTERVAL, using default", "value", v, "default", backupInterval)
			}
		}
	}
	backupKeep := *backupKeepFlag
	if !set["backup-keep"] {
		if v := os.Getenv("QOOQ_BACKUP_KEEP"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				backupKeep = n
			} else {
				log.Warn("invalid QOOQ_BACKUP_KEEP, using default", "value", v, "default", backupKeep)
			}
		}
	}

	// Extra Host-header allowlist entries for the no-auth drive-by guard
	// (flag wins, else QOOQ_ALLOWED_HOSTS). Empty is fine: the localhost forms
	// derived from -listen are always allowed by the router itself.
	allowedHostsRaw := *allowedHostsFlag
	if !set["allowed-hosts"] {
		allowedHostsRaw = os.Getenv("QOOQ_ALLOWED_HOSTS")
	}
	var extraHosts []string
	for _, h := range strings.Split(allowedHostsRaw, ",") {
		if h = strings.TrimSpace(h); h != "" {
			extraHosts = append(extraHosts, h)
		}
	}

	warnExposedWithoutAuth(log, *listen, token, extraHosts)

	// The address components are advertised on. Detection is a heuristic and
	// gets it wrong on machines with VPNs or several NICs, so the choice is
	// logged: an operator who sees the wrong address here knows to pass
	// -advertise-host, instead of wondering why the URL qooq handed them
	// doesn't answer.
	advertiseHost := *advertiseHostFlag
	if !set["advertise-host"] {
		advertiseHost = os.Getenv("QOOQ_ADVERTISE_HOST")
	}
	advertiseSource := "configured"
	if advertiseHost == "" {
		advertiseHost = api.DetectAdvertiseHost()
		advertiseSource = "detected"
	}
	if advertiseHost == "" {
		log.Warn("no advertise host found; component endpoints will use 127.0.0.1")
	} else {
		log.Info("advertise host", "host", advertiseHost, "source", advertiseSource)
	}
	advertise := api.NewAdvertiseHost(advertiseHost)

	var rt runtime.Runtime
	switch *runtimeFlag {
	case "docker":
		d, err := docker.New()
		if err != nil {
			log.Error("init docker runtime", "err", err)
			os.Exit(1)
		}
		if err := d.Ping(context.Background()); err != nil {
			log.Error("docker runtime unreachable", "err", err)
			os.Exit(1)
		}
		rt = d
	case "process":
		procDir := *procDirFlag
		if procDir == "" {
			procDir = filepath.Join(filepath.Dir(*dataPath), "qooq-proc")
		}
		p, err := process.New(procDir, log)
		if err != nil {
			log.Error("init process runtime", "err", err)
			os.Exit(1)
		}
		rt = p
	default:
		log.Error("unknown -runtime value (want docker or process)", "runtime", *runtimeFlag)
		os.Exit(1)
	}

	st, err := bbolt.Open(*dataPath)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// The ingress proxies to containers by name on a shared docker network,
	// which the process runtime has no equivalent of. Refusing at startup
	// beats running half the feature and leaving the operator to work out why
	// nothing routes.
	if *ingressFlag && *runtimeFlag != "docker" {
		log.Error("-ingress requires the docker runtime", "runtime", *runtimeFlag)
		os.Exit(1)
	}
	if *ingressFlag && *ingressTLSIssuerFlag != ingress.IssuerACME && *ingressTLSIssuerFlag != ingress.IssuerInternal {
		log.Error("unknown -ingress-tls-issuer value", "issuer", *ingressTLSIssuerFlag, "want", ingress.IssuerACME+" or "+ingress.IssuerInternal)
		os.Exit(1)
	}

	// The app-domain settings are what stop a user from routing a name it
	// was not given. Any of them wrong means refusing to start: running
	// without them would be running without the limit.
	var ingressCfg *ingress.Config
	if *ingressFlag {
		if err := checkIngressAppDomain(*ingressHTTPSPortFlag, *ingressAppDomainFlag, *ingressAppDomainRedirectFlag, *ingressCertFileFlag, *ingressKeyFileFlag); err != nil {
			log.Error("invalid ingress settings", "err", err)
			os.Exit(1)
		}
		ingressCfg = &ingress.Config{
			HTTPPort:          *ingressPortFlag,
			HTTPSPort:         *ingressHTTPSPortFlag,
			TLS:               *ingressTLSFlag,
			TLSIssuer:         *ingressTLSIssuerFlag,
			StateDir:          filepath.Join(filepath.Dir(*dataPath), "qooq-ingress"),
			AdvertiseHost:     advertise.Get,
			AppDomain:         *ingressAppDomainFlag,
			AppDomainRedirect: *ingressAppDomainRedirectFlag,
			CertFile:          *ingressCertFileFlag,
			KeyFile:           *ingressKeyFileFlag,
			Cloudflare:        *ingressCloudflareFlag,
			BindIP:            *ingressBindIPFlag,
		}
	}

	svc := core.NewService(rt, st, *runtimeFlag, instance, log)
	if *ingressFlag {
		svc.SetIngressNetwork(ingress.Network)
	}
	svc.SetAdvertiseHost(advertise.Get)

	buildMem, err := parseSizeFlag(*buildMemoryFlag)
	if err != nil {
		log.Error("invalid -build-memory", "err", err)
		os.Exit(1)
	}
	buildCacheMax, err := parseSizeFlag(*buildCacheMaxFlag)
	if err != nil {
		log.Error("invalid -build-cache-max", "err", err)
		os.Exit(1)
	}
	buildImageMax, err := parseSizeFlag(*buildImageMaxFlag)
	if err != nil {
		log.Error("invalid -build-image-max", "err", err)
		os.Exit(1)
	}
	minFree, err := parseShareFlag(*buildMinFreeFlag)
	if err != nil {
		log.Error("invalid -build-min-free-disk", "err", err)
		os.Exit(1)
	}
	var diskFree func() (uint64, uint64, error)
	if minFree > 0 {
		diskPath := *buildDiskPathFlag
		// Checked once here so a wrong path is said at start, not as a warning
		// before every build. On a Mac, docker's data root lives inside a VM
		// and is not a path on this machine at all.
		free, total, err := freeDisk(diskPath)
		if err != nil {
			log.Error("invalid -build-disk-path: cannot read free space", "path", diskPath, "err", err)
			os.Exit(1)
		}
		// Said at start: what is being measured, so a guard watching the
		// wrong disk is visible without waiting for a disk to fill.
		dev, _ := deviceOf(diskPath)
		log.Info("build disk guard",
			"disk_path", diskPath, "device", dev, "total_bytes", total,
			"free_percent", fmt.Sprintf("%.0f", 100*float64(free)/float64(max(total, 1))))
		// And whether that is where images are written. Docker 29 keeps
		// layers in containerd's root, not its data root, so the default
		// /var/lib/docker can be a directory on a disk that never fills —
		// seen for real: a 600 MB build went through with the real disk at
		// 2% free (E4). Warned only when the image store's location is
		// KNOWN: the first version guessed containerd's default root, and on
		// a machine whose root had been moved it warned against the right
		// path and stayed quiet about the wrong one. A warning that fires on
		// correct settings is the kind people learn to ignore.
		checkImageStoreDisk(log, rt, diskPath)
		diskFree = func() (uint64, uint64, error) { return freeDisk(diskPath) }
	}
	svc.SetBuildLimits(core.BuildLimits{
		Timeout:            *buildTimeoutFlag,
		MemoryBytes:        buildMem,
		CPUs:               *buildCPUsFlag,
		CacheReservedBytes: buildCacheMax,
		ImageMaxBytes:      buildImageMax,
		MinFreeDisk:        minFree,
		FreeDisk:           diskFree,
	})
	log.Info("build limits",
		"timeout", *buildTimeoutFlag,
		"memory_bytes", buildMem,
		"cpus", *buildCPUsFlag,
		"cache_reserved_bytes", buildCacheMax,
		"image_max_bytes", buildImageMax,
		"min_free_disk", minFree,
		"disk_path", *buildDiskPathFlag)
	containerLimits, err := resolveContainerLimits(*pidsLimitFlag, *containerMemMaxFlag, *containerCPUsMaxFlag)
	if err != nil {
		log.Error("invalid container limit", "err", err)
		os.Exit(1)
	}
	containerLimits.Harden = *hardenFlag
	if containerLimits.Harden && *runtimeFlag != "docker" {
		log.Warn("-harden-containers has no effect on the process runtime: there is no container to harden")
	} else if containerLimits.Harden {
		// Here, not at the first deploy: a profile that cannot be built would
		// otherwise surface as every container failing to create.
		if err := docker.HardeningReady(); err != nil {
			log.Error("-harden-containers", "err", err)
			os.Exit(1)
		}
	}
	svc.SetContainerLimits(containerLimits)
	log.Info("container limits",
		"pids_limit", containerLimits.PidsLimit,
		"memory_max_bytes", containerLimits.MemoryMaxBytes,
		"cpus_max", containerLimits.CPUsMax,
		"harden", containerLimits.Harden)
	low, high, err := parsePortRange(*autoPortRangeFlag)
	if err != nil {
		log.Error("invalid -auto-port-range", "value", *autoPortRangeFlag, "err", err)
		os.Exit(1)
	}
	if err := svc.SetAutoPortRange(low, high); err != nil {
		log.Error("invalid -auto-port-range", "value", *autoPortRangeFlag, "err", err)
		os.Exit(1)
	}
	publishIP := *publishIPFlag
	if publishIP == "" {
		publishIP = os.Getenv("QOOQ_PUBLISH_IP")
	}
	if err := svc.SetPublishIP(publishIP); err != nil {
		log.Error("invalid -publish-ip", "value", publishIP, "err", err)
		os.Exit(1)
	}
	if publishIP != "" {
		log.Info("published ports bind to", "address", publishIP)
	}

	if err := svc.Reconcile(context.Background()); err != nil {
		// Reconcile failure is non-fatal: the daemon can still serve requests.
		log.Error("reconcile at boot", "err", err)
	}

	// Start monitoring: an events goroutine tracks lifecycle/crash events and
	// stats streams follow container start/stop. adoptRunning inside Start
	// re-attaches stats to containers that survived a daemon restart.
	monCtx, monCancel := context.WithCancel(context.Background())
	mon := monitor.New(rt, svc, log)
	go mon.Start(monCtx)

	// Keep the advertise host current. Only when it was detected: an address
	// the operator configured is theirs, and they may well know something this
	// process cannot see (a NAT, a tunnel, a name that resolves differently
	// from outside). Detection is cheap — it reads the interface list — so a
	// minute is frequent enough to catch a laptop changing networks long
	// before anyone has finished wondering why a URL stopped working.
	if advertiseSource == "detected" {
		go advertise.Refresh(monCtx, advertiseRefreshInterval, log)
	}

	// Persistent reconcile loop: catches crashed/missing/drifted containers
	// that the events stream missed, and is the fallback driver for a
	// scheduled restart whose in-process timer was lost (e.g. this daemon
	// itself restarted mid-backoff).
	go svc.ReconcileLoop(monCtx, 15*time.Second)

	// Scheduled datastore backups (no-op unless -backup-dir/QOOQ_BACKUP_DIR set).
	go runBackupScheduler(monCtx, svc, backupDir, backupInterval, backupKeep, log)

	// Ingress: deploys the proxy, then keeps its routes in step with whatever
	// components are running. Started after the monitor so the proxy's own
	// lifecycle is observed like any other component's.
	if ingressCfg != nil {
		ing := ingress.New(svc, rt, *ingressCfg, log)
		go ing.Run(monCtx)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           api.NewRouter(svc, log, token, *listen, extraHosts, advertise, ingressCfg),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Info("qooqd listening", "addr", *listen, "data", *dataPath, "auth", token != "", "instance", instance)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			os.Exit(1)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig

	log.Info("shutting down")
	monCancel()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Error("graceful shutdown", "err", err)
	}
	if err := rt.Close(); err != nil {
		log.Error("close runtime", "err", err)
	}
	log.Info("qooqd stopped")
}

// parsePortRange reads the "low-high" form of -auto-port-range. A bad range is
// fatal rather than a fallback to the default: a daemon told to use particular
// ports and quietly using different ones is how an app ends up published
// somewhere nobody is looking.
func parsePortRange(s string) (low, high int, err error) {
	lo, hi, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("want the form low-high, e.g. 19000-19999")
	}
	if low, err = strconv.Atoi(strings.TrimSpace(lo)); err != nil {
		return 0, 0, fmt.Errorf("low port: %w", err)
	}
	if high, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil {
		return 0, 0, fmt.Errorf("high port: %w", err)
	}
	return low, high, nil
}

// advertiseRefreshInterval is how often a detected advertise host is checked
// for change.
const advertiseRefreshInterval = time.Minute

// resolveContainerLimits applies the QOOQ_CONTAINER_* env fallbacks for flags
// not passed explicitly (an explicit flag always wins, including an explicit 0)
// and rejects bad values. A bad value is fatal rather than ignored: a typo'd
// ceiling that quietly falls back to "no ceiling" is the failure this exists to
// prevent.
func resolveContainerLimits(pids int64, mem string, cpus float64) (core.ContainerLimits, error) {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	if v := os.Getenv("QOOQ_CONTAINER_PIDS_LIMIT"); v != "" && !set["container-pids-limit"] {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return core.ContainerLimits{}, fmt.Errorf("QOOQ_CONTAINER_PIDS_LIMIT: not an integer: %q", v)
		}
		pids = n
	}
	if v := os.Getenv("QOOQ_CONTAINER_MEMORY_MAX"); v != "" && !set["container-memory-max"] {
		mem = v
	}
	if v := os.Getenv("QOOQ_CONTAINER_CPUS_MAX"); v != "" && !set["container-cpus-max"] {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return core.ContainerLimits{}, fmt.Errorf("QOOQ_CONTAINER_CPUS_MAX: not a number: %q", v)
		}
		cpus = f
	}
	return buildContainerLimits(pids, mem, cpus)
}

func buildContainerLimits(pids int64, mem string, cpus float64) (core.ContainerLimits, error) {
	if pids < 0 {
		return core.ContainerLimits{}, fmt.Errorf("-container-pids-limit must not be negative: %d", pids)
	}
	// NaN passes every comparison, so "< 0" alone would read it as "no
	// ceiling" — exactly the typo-means-unlimited this refuses.
	if cpus < 0 || math.IsNaN(cpus) || math.IsInf(cpus, 0) {
		return core.ContainerLimits{}, fmt.Errorf("-container-cpus-max must be a non-negative number: %v", cpus)
	}
	// Below docker's own minimums every container create would fail; say so
	// once at boot rather than on each deploy.
	if cpus > 0 && cpus < 0.01 {
		return core.ContainerLimits{}, fmt.Errorf("-container-cpus-max %v is below docker's minimum of 0.01", cpus)
	}
	memBytes, err := parseSizeFlag(mem)
	if err != nil {
		return core.ContainerLimits{}, fmt.Errorf("-container-memory-max: %w", err)
	}
	if memBytes > 0 && memBytes < 6<<20 {
		return core.ContainerLimits{}, fmt.Errorf("-container-memory-max %s is below docker's minimum of 6m", mem)
	}
	return core.ContainerLimits{PidsLimit: pids, MemoryMaxBytes: memBytes, CPUsMax: cpus}, nil
}

// parseSizeFlag reads a size like "2g", "512m" or a bare byte count.
//
// 1024-based, matching the CLI's --mem flag rather than the decimal units a
// manifest's requests.memory accepts. The two differ because one is a docker
// convention and the other is Kubernetes'; a flag on this daemon follows the
// daemon's own.
// buildFlagEnv maps the build-limit flags to environment variables, so a
// deployment can keep every limit in the one env file preflight reads, instead
// of half of them on a systemd ExecStart line nothing checks. An explicit flag
// wins, as for the QOOQ_CONTAINER_* ones.
var buildFlagEnv = map[string]string{
	"build-timeout":       "QOOQ_BUILD_TIMEOUT",
	"build-memory":        "QOOQ_BUILD_MEMORY",
	"build-cpus":          "QOOQ_BUILD_CPUS",
	"build-cache-max":     "QOOQ_BUILD_CACHE_MAX",
	"build-image-max":     "QOOQ_BUILD_IMAGE_MAX",
	"build-min-free-disk": "QOOQ_BUILD_MIN_FREE_DISK",
	"build-disk-path":     "QOOQ_BUILD_DISK_PATH",
	// The ingress too: a shared node gets its settings from an EnvironmentFile,
	// and with QOOQ_PUBLISH_IP=127.0.0.1 a node without the ingress gives
	// users no address at all — so it must be switchable from the same file.
	"ingress":            "QOOQ_INGRESS",
	"ingress-http-port":  "QOOQ_INGRESS_HTTP_PORT",
	"ingress-tls":        "QOOQ_INGRESS_TLS",
	"ingress-tls-issuer": "QOOQ_INGRESS_TLS_ISSUER",
	"ingress-https-port": "QOOQ_INGRESS_HTTPS_PORT",
	// The user hostname rule and its certificate: a shared node must have
	// them, and preflight reads the same env file.
	"ingress-app-domain":          "QOOQ_INGRESS_APP_DOMAIN",
	"ingress-app-domain-redirect": "QOOQ_INGRESS_APP_DOMAIN_REDIRECT",
	"ingress-cert-file":           "QOOQ_INGRESS_CERT_FILE",
	"ingress-key-file":            "QOOQ_INGRESS_KEY_FILE",
	"ingress-cloudflare":          "QOOQ_INGRESS_CLOUDFLARE",
	"ingress-bind-ip":             "QOOQ_INGRESS_BIND_IP",
	// A shared node must have it on, and preflight reads the env file.
	"harden-containers": "QOOQ_HARDEN_CONTAINERS",
}

func applyBuildFlagEnv() error {
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	for name, env := range buildFlagEnv {
		v := os.Getenv(env)
		if v == "" || set[name] {
			continue
		}
		if err := flag.Set(name, v); err != nil {
			return fmt.Errorf("%s: %w", env, err)
		}
	}
	return nil
}

// parseShareFlag reads "15%" or "0.15" as 0.15. Empty is 0 (off).
func parseShareFlag(v string) (float64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	pct := strings.HasSuffix(v, "%")
	f, err := strconv.ParseFloat(strings.TrimSuffix(v, "%"), 64)
	if err != nil {
		return 0, fmt.Errorf("not a share: %q", v)
	}
	if pct {
		f /= 100
	}
	if f < 0 || f >= 1 {
		return 0, fmt.Errorf("must be between 0%% and 100%%: %q", v)
	}
	return f, nil
}

func parseSizeFlag(v string) (int64, error) {
	v = strings.TrimSpace(strings.ToLower(v))
	if v == "" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(v, "g"):
		mult, v = 1<<30, strings.TrimSuffix(v, "g")
	case strings.HasSuffix(v, "m"):
		mult, v = 1<<20, strings.TrimSuffix(v, "m")
	case strings.HasSuffix(v, "k"):
		mult, v = 1<<10, strings.TrimSuffix(v, "k")
	case strings.HasSuffix(v, "b"):
		v = strings.TrimSuffix(v, "b")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("not a size (use 2g, 512m, or a byte count): %q", v)
	}
	if n < 0 {
		return 0, fmt.Errorf("must not be negative: %q", v)
	}
	return n * mult, nil
}

// checkImageStoreDisk warns when -build-disk-path is on a different filesystem
// from the one docker writes images to — but only when that location is known
// and is on this machine. Otherwise it says it could not check.
func checkImageStoreDisk(log *slog.Logger, rt runtime.Runtime, diskPath string) {
	unsure := func(why string) {
		log.Info("build disk guard: could not check that -build-disk-path is where images are written; confirm it yourself (docker info, and containerd's root)",
			"reason", why)
	}
	// A remote docker writes to a disk this machine cannot measure.
	if h := os.Getenv("DOCKER_HOST"); h != "" && !strings.HasPrefix(h, "unix://") {
		unsure("DOCKER_HOST is not a local socket")
		return
	}
	d, ok := rt.(interface {
		ImageStoreRoot(context.Context) (string, bool, error)
	})
	if !ok {
		return
	}
	root, known, err := d.ImageStoreRoot(context.Background())
	if err != nil || !known {
		unsure("the image store's location is not reported (containerd's config could not be read)")
		return
	}
	same, err := sameFilesystem(diskPath, root)
	if err != nil {
		unsure("cannot stat " + root)
		return
	}
	if !same {
		log.Warn("-build-disk-path is not on the filesystem docker writes images to; the free-disk check is watching another disk",
			"disk_path", diskPath, "image_store", root)
	}
}
