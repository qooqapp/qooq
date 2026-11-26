//go:build unix

// Package process implements runtime.Runtime against native OS processes
// instead of a container engine, so qooqd can manage components on hosts
// that cannot run Docker. It is stdlib-only (no external dependencies).
//
// Semantics translated for this adapter (see docs/DEPLOY-CONFIG.md):
//   - "image" is the absolute path (or PATH-resolvable name) of a host
//     executable; argv is [image] + ContainerSpec.Command.
//   - Load/Build/ListImages are not supported and return a clear error.
//   - Each container instance is a directory under the configured state dir
//     (<procDir>/<id>/) holding meta.json (spec/pid/exit info) plus
//     stdout.log/stderr.log.
//   - There is no namespace isolation: processes share the host filesystem
//     and network with qooqd and each other.
//   - Resource limits (CPU/memory) are applied via Linux cgroups v2 on a
//     best-effort basis; on any other OS (or without permission) they are
//     silently skipped after a single warning.
//   - A daemon restart "adopts" processes it finds still alive (matched by
//     pid + a pid-reuse guard) and reports Failed with ExitCode -1 for any
//     that died while qooqd was down, because the real exit code is lost
//     once the original parent is gone.
package process

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

// meta is the on-disk (and in-memory) record for one instance. It is
// persisted as <procDir>/<id>/meta.json so a daemon restart can reconstruct
// state (see adoptExisting).
type meta struct {
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Command []string          `json:"command,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	// Mounts is recorded for fidelity (inspect/debugging); the files it
	// describes were already materialized into the instance dir at create
	// time and are not re-created on restart.
	Mounts    []runtime.Mount `json:"mounts,omitempty"`
	CPUNanos  int64           `json:"cpu_nanos,omitempty"`
	MemBytes  int64           `json:"mem_bytes,omitempty"`
	CreatedAt time.Time       `json:"created_at"`

	// StartAttemptedAt (F2, M17 review) is set and persisted *before*
	// cmd.Start() is called, so a crash (e.g. SIGKILL) landing between
	// Start() and the post-start meta save below leaves a breadcrumb
	// instead of a record indistinguishable from "never started" (PID==0).
	// adoptExisting warns when it finds PID==0 with this set: the child may
	// be an orphan (started, then the pid was never recorded), and since
	// the pid is unknown it cannot be adopted or reaped — this only
	// surfaces the risk, it cannot close it. There remains an
	// irreducible residual window: the instant between cmd.Start()
	// returning and the immediate post-start saveLocked() below, during
	// which a crash still loses the pid despite the child existing. That
	// window is now microseconds (one field assignment + one fsync-ish
	// rename) instead of spanning cgroup setup too.
	StartAttemptedAt time.Time `json:"start_attempted_at,omitempty"`

	PID        int       `json:"pid,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	ProcKey    string    `json:"proc_key,omitempty"`
	Exited     bool      `json:"exited"`
	ExitCode   int       `json:"exit_code"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Adopted    bool      `json:"adopted,omitempty"`
	CgroupDir  string    `json:"cgroup_dir,omitempty"`
}

// liveness is the tri-state result of a process liveness probe (see
// checkLiveness in procid_linux.go / procid_other.go). Only liveDead is hard
// evidence (kill(pid,0) failed); liveSuspect (identity key mismatch) is soft
// evidence that must be observed on consecutive checks before a process is
// declared dead — a single odd probe must never kill a healthy component.
type liveness int

const (
	liveDead liveness = iota
	liveAlive
	liveSuspect
)

// suspectThreshold is how many consecutive liveSuspect probes the adopted-
// process poller requires before treating soft evidence as death.
const suspectThreshold = 2

// maxLogBytes caps stdout.log/stderr.log size per instance (F4, M17
// review). A component that logs continuously with no rotation of its own
// would otherwise grow its log files without bound under proc-dir until the
// disk fills; qooq owns these files (it opens them, the component just
// inherits the fd), so qooq is responsible for bounding them.
const maxLogBytes = 10 * 1024 * 1024 // 10MB

// logRotateTicks is how many 2s poller ticks between log-size sweeps of
// running instances (~30s): frequent enough that a runaway logger can't
// fill much disk between sweeps, infrequent enough not to stat two files
// per instance every 2s.
const logRotateTicks = 15

// rotateLogIfLarge renames path to path+".old" (clobbering any previous
// .old) if it is currently larger than maxLogBytes. Called right before a
// fresh process's stdout/stderr log is (re-)opened in append mode, so a
// restarted component doesn't keep appending onto an already-huge log left
// by a long-lived predecessor run.
func rotateLogIfLarge(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= maxLogBytes {
		return
	}
	_ = os.Rename(path, path+".old")
}

// instance is the in-memory handle for one component's process. mu guards
// every field below it: the wait goroutine, the adopted-process poller and
// API calls (Stop/Inspect/...) all touch the same meta concurrently.
type instance struct {
	id  runtime.ContainerID
	dir string

	mu       sync.Mutex
	meta     meta
	cmd      *exec.Cmd
	done     chan struct{}
	doneOnce *sync.Once
	suspect  int  // consecutive liveSuspect poller probes (see checkAdopted)
	removed  bool // RemoveContainer ran; skip persisting to the deleted dir
}

func (inst *instance) metaPath() string { return filepath.Join(inst.dir, "meta.json") }

// saveLocked persists inst.meta. Caller must hold inst.mu. Writes go through
// a temp file + rename so a crash mid-write never leaves a corrupt meta.json.
func (inst *instance) saveLocked() error {
	data, err := json.MarshalIndent(inst.meta, "", "  ")
	if err != nil {
		return err
	}
	tmp := inst.metaPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, inst.metaPath())
}

// markDoneLocked closes inst.done exactly once. Caller must hold inst.mu.
func (inst *instance) markDoneLocked() {
	inst.doneOnce.Do(func() { close(inst.done) })
}

func loadMeta(path string) (meta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return meta{}, err
	}
	var m meta
	if err := json.Unmarshal(data, &m); err != nil {
		return meta{}, err
	}
	return m, nil
}

// Adapter is the process-backed runtime.Runtime.
type Adapter struct {
	dir string
	log *slog.Logger

	mu        sync.Mutex
	instances map[runtime.ContainerID]*instance

	bus *eventBus

	pollCancel context.CancelFunc
}

// Ensure the interface is satisfied at compile time.
var _ runtime.Runtime = (*Adapter)(nil)

// New builds an Adapter rooted at dir (created if missing), adopting any
// instance directories already present (a prior daemon's state) and starting
// the background poller that detects when an adopted process dies.
func New(dir string, log *slog.Logger) (*Adapter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("process runtime: create state dir %s: %w", dir, err)
	}

	a := &Adapter{
		dir:       dir,
		log:       log,
		instances: make(map[runtime.ContainerID]*instance),
		bus:       newEventBus(),
	}

	if err := a.adoptExisting(); err != nil {
		log.Warn("process runtime: adopt existing instances", "err", err)
	}

	pollCtx, cancel := context.WithCancel(context.Background())
	a.pollCancel = cancel
	go a.pollAdopted(pollCtx)

	return a, nil
}

// adoptExisting scans dir for instance subdirectories left by a previous
// daemon process and reconstructs in-memory state for each: still-alive
// processes are marked Adopted (the poller takes over watching them);
// processes provably dead (kill(pid,0) fails — hard evidence only) are
// recorded as exited with ExitCode -1, since the real code is unrecoverable
// once the original parent is gone. A mere identity-key mismatch at boot is
// soft evidence and does NOT kill the record here: the instance is adopted
// with a warning and the poller's consecutive-strike rule decides.
func (a *Adapter) adoptExisting() error {
	entries, err := os.ReadDir(a.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		instDir := filepath.Join(a.dir, e.Name())
		m, err := loadMeta(filepath.Join(instDir, "meta.json"))
		if err != nil {
			a.log.Warn("process runtime: skipping instance dir with unreadable meta", "dir", instDir, "err", err)
			continue
		}

		inst := &instance{
			id:       runtime.ContainerID(m.ID),
			dir:      instDir,
			meta:     m,
			done:     make(chan struct{}),
			doneOnce: &sync.Once{},
		}

		switch {
		case m.Exited:
			inst.markDoneLocked()
		case m.PID == 0:
			// Created but never started (interrupted deploy): leave the
			// record as-is; core's reconcile handles Pending/Creating drift.
			// F2 (M17 review): distinguish "never even attempted" from "an
			// attempt was made but qooqd died before the pid could be
			// recorded" — the latter may have left an orphaned, unmanaged
			// child process running with a pid we no longer know.
			if !m.StartAttemptedAt.IsZero() {
				a.log.Warn("process runtime: a start attempt was interrupted before its pid could be recorded; a child process may be orphaned and unmanaged (pid unknown, cannot adopt)",
					"container", inst.id, "name", m.Name, "start_attempted_at", m.StartAttemptedAt)
			}
		default:
			switch lv := checkLiveness(m.PID, m.ProcKey); lv {
			case liveDead:
				inst.meta.Exited = true
				inst.meta.ExitCode = -1
				inst.meta.FinishedAt = time.Now()
				inst.markDoneLocked()
				if err := inst.saveLocked(); err != nil {
					a.log.Warn("process runtime: persist adopted-dead state", "container", inst.id, "err", err)
				}
				a.log.Warn("process runtime: instance died while daemon was down, exit code unknown", "container", inst.id, "name", m.Name)
			default:
				// liveAlive, or liveSuspect (soft evidence: adopt anyway and
				// let the poller's consecutive-strike rule decide).
				inst.meta.Adopted = true
				if lv == liveSuspect {
					a.log.Warn("process runtime: adopted process identity key mismatch, will re-check",
						"container", inst.id, "pid", m.PID, "name", m.Name)
				}
				a.log.Info("process runtime: adopted running process", "container", inst.id, "pid", m.PID, "name", m.Name)
			}
		}

		a.mu.Lock()
		a.instances[inst.id] = inst
		a.mu.Unlock()
	}
	return nil
}

// pollAdopted periodically checks liveness of every adopted-and-still-running
// instance (these have no cmd.Wait() goroutine of our own to reap them) and
// reports a die event the moment one disappears.
func (a *Adapter) pollAdopted(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.checkAdopted()
			// F4 (M17 review): piggyback the log-size sweep on the same
			// ticker rather than a second goroutine/ticker — this poller
			// already wakes up every 2s for every instance.
			ticks++
			if ticks%logRotateTicks == 0 {
				a.truncateOversizedLogs()
			}
		}
	}
}

// truncateOversizedLogs scans every currently-running instance's
// stdout.log/stderr.log and truncates any that has grown past maxLogBytes
// (F4, M17 review). Truncate — not rename/rotate — is used here because the
// writer is an os.O_APPEND file handle StartContainer already has open
// (inst.cmd.Stdout/Stderr): os.Truncate on a file an O_APPEND writer still
// holds open is safe (the next Write() lands at the new, post-truncate end
// of file), whereas renaming the path out from under that handle would
// silently orphan future writes into an unlinked, invisible file.
func (a *Adapter) truncateOversizedLogs() {
	a.mu.Lock()
	insts := make([]*instance, 0, len(a.instances))
	for _, inst := range a.instances {
		insts = append(insts, inst)
	}
	a.mu.Unlock()

	for _, inst := range insts {
		inst.mu.Lock()
		running := inst.meta.PID != 0 && !inst.meta.Exited
		dir := inst.dir
		id := inst.id
		inst.mu.Unlock()
		if !running {
			continue
		}
		for _, name := range []string{"stdout.log", "stderr.log"} {
			path := filepath.Join(dir, name)
			info, err := os.Stat(path)
			if err != nil || info.Size() <= maxLogBytes {
				continue
			}
			if err := os.Truncate(path, 0); err != nil {
				a.log.Warn("process runtime: truncate oversized log failed", "container", id, "file", name, "err", err)
				continue
			}
			a.log.Info("process runtime: truncated oversized log", "container", id, "file", name, "size_bytes", info.Size())
		}
	}
}

func (a *Adapter) checkAdopted() {
	a.mu.Lock()
	insts := make([]*instance, 0, len(a.instances))
	for _, inst := range a.instances {
		insts = append(insts, inst)
	}
	a.mu.Unlock()

	for _, inst := range insts {
		inst.mu.Lock()
		if !inst.meta.Adopted || inst.meta.Exited {
			inst.mu.Unlock()
			continue
		}
		pid, key := inst.meta.PID, inst.meta.ProcKey
		inst.mu.Unlock()

		switch checkLiveness(pid, key) {
		case liveAlive:
			inst.mu.Lock()
			inst.suspect = 0
			inst.mu.Unlock()
			continue
		case liveSuspect:
			// Soft evidence only: require suspectThreshold consecutive
			// mismatches before declaring death, so one odd ps/procfs
			// reading can never kill a healthy component.
			inst.mu.Lock()
			inst.suspect++
			strikes := inst.suspect
			inst.mu.Unlock()
			if strikes < suspectThreshold {
				a.log.Warn("process runtime: adopted process identity key mismatch, re-checking next poll",
					"container", inst.id, "pid", pid, "strikes", strikes)
				continue
			}
		case liveDead:
			// Hard evidence (kill(pid,0) failed): proceed immediately.
		}

		inst.mu.Lock()
		if inst.meta.Exited { // already handled by a concurrent check
			inst.mu.Unlock()
			continue
		}
		inst.meta.Exited = true
		inst.meta.ExitCode = -1
		inst.meta.FinishedAt = time.Now()
		labels := inst.meta.Labels
		if !inst.removed {
			if err := inst.saveLocked(); err != nil {
				a.log.Warn("process runtime: persist adopted-die state", "container", inst.id, "err", err)
			}
		}
		inst.markDoneLocked()
		inst.mu.Unlock()

		a.log.Warn("process runtime: adopted process died, exit code unknown (reporting -1)", "container", inst.id)
		a.bus.publish(runtime.Event{
			ContainerID: inst.id,
			Action:      "die",
			ExitCode:    -1,
			Labels:      labels,
			Time:        time.Now(),
		})
	}
}

func (a *Adapter) getInstance(id runtime.ContainerID) (*instance, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	inst, ok := a.instances[id]
	return inst, ok
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failing is effectively unrecoverable; fall back to a
		// timestamp id rather than taking the daemon down over it.
		return fmt.Sprintf("proc%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// resolveImage validates that ref refers to an executable file (absolute
// path or a name resolvable via PATH) and returns its absolute path.
func resolveImage(ref string) (string, error) {
	path := ref
	if !filepath.IsAbs(path) {
		if p, err := exec.LookPath(ref); err == nil {
			path = p
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("process runtime: resolve image path %q: %w", ref, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("process runtime: image %q: %w", ref, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("process runtime: image %q is a directory, not an executable", ref)
	}
	if info.Mode()&0o111 == 0 {
		return "", fmt.Errorf("process runtime: image %q is not executable", ref)
	}
	return abs, nil
}

func (a *Adapter) Ping(ctx context.Context) error { return nil }

func (a *Adapter) Close() error {
	a.pollCancel()
	return nil
}

func (a *Adapter) PullImage(ctx context.Context, ref string, opts runtime.PullOptions, registryAuth string) (runtime.ImageID, error) {
	abs, err := resolveImage(ref)
	if err != nil {
		return "", err
	}
	return runtime.ImageID(abs), nil
}

func (a *Adapter) ResolveImageDigest(ctx context.Context, ref string, registryAuth string) (string, error) {
	return "", errors.New("process runtime: image digest resolution not supported (image = host executable path)")
}

func (a *Adapter) PushImage(ctx context.Context, ref string, registryAuth string) error {
	return errors.New("process runtime: image push not supported (image = host executable path)")
}

func (a *Adapter) PruneBuildCache(ctx context.Context, reservedBytes int64) (uint64, error) {
	// Nothing to prune: this runtime has no image build and therefore no cache.
	// Reported as zero rather than an error so a caller pruning periodically
	// does not have to know which runtime it is talking to.
	return 0, nil
}

func (a *Adapter) LoadImage(ctx context.Context, tarball io.Reader) ([]runtime.ImageID, error) {
	return nil, errors.New("process runtime: image load not supported (image = host executable path)")
}

func (a *Adapter) BuildImage(ctx context.Context, buildCtx io.Reader, opts runtime.BuildOptions) (runtime.ImageID, error) {
	return "", errors.New("process runtime: image build not supported (image = host executable path)")
}

func (a *Adapter) ListImages(ctx context.Context) ([]runtime.ImageSummary, error) {
	return nil, errors.New("process runtime: image listing not supported (image = host executable path)")
}

func (a *Adapter) RemoveImage(ctx context.Context, ref string) error {
	return errors.New("process runtime: image removal not supported (image = host executable path)")
}

// EnsureNetwork is a no-op: processes share the host's network stack, so
// components can already reach each other (on 127.0.0.1) with no network to
// create. Returning nil rather than an error keeps a manifest that declares a
// network deployable under either runtime.
func (a *Adapter) EnsureNetwork(ctx context.Context, name string) error { return nil }

// RemoveNetworkIfUnused is a no-op: EnsureNetwork never created anything.
func (a *Adapter) RemoveNetworkIfUnused(ctx context.Context, name string) error { return nil }

// ConnectNetwork is a no-op for the same reason as EnsureNetwork: processes
// already share the host's network stack, so they are as connected as they can
// get.
func (a *Adapter) ConnectNetwork(ctx context.Context, id runtime.ContainerID, network string, aliases []string) error {
	return nil
}

// DisconnectNetwork is a no-op: there is no network to leave.
func (a *Adapter) DisconnectNetwork(ctx context.Context, id runtime.ContainerID, network string) error {
	return nil
}

// CreateContainer validates the image and persists a new instance record; it
// does not start the process (StartContainer does), matching Docker's
// create-then-start split.
func (a *Adapter) CreateContainer(ctx context.Context, spec runtime.ContainerSpec) (runtime.ContainerID, error) {
	imgPath, err := resolveImage(spec.Image)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	id := runtime.ContainerID(newID())
	instDir := filepath.Join(a.dir, string(id))
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		return "", fmt.Errorf("process runtime: create instance dir: %w", err)
	}

	// Mounts are realized once, at create time: the instance dir persists
	// across restarts, so StartContainer must not redo this (it would fight
	// with a running process's open files for no benefit).
	if err := materializeMounts(instDir, spec.Mounts); err != nil {
		os.RemoveAll(instDir)
		return "", err
	}

	inst := &instance{
		id:  id,
		dir: instDir,
		meta: meta{
			ID:        string(id),
			Name:      spec.Name,
			Image:     imgPath,
			Command:   spec.Command,
			Env:       spec.Env,
			Labels:    spec.Labels,
			Mounts:    spec.Mounts,
			CPUNanos:  spec.CPUNanos,
			MemBytes:  spec.MemBytes,
			CreatedAt: time.Now(),
		},
		done:     make(chan struct{}),
		doneOnce: &sync.Once{},
	}
	if err := inst.saveLocked(); err != nil {
		os.RemoveAll(instDir)
		return "", fmt.Errorf("process runtime: persist meta: %w", err)
	}

	a.mu.Lock()
	a.instances[id] = inst
	a.mu.Unlock()

	return id, nil
}

// StartContainer execs the instance's image+command. It is also the
// "restart in place" path: calling it again on an instance that previously
// exited (e.g. `component start` after a manual stop) spawns a fresh process
// reusing the same instance directory and id, mirroring how Docker restarts
// an existing (non-recreated) container.
func (a *Adapter) StartContainer(ctx context.Context, id runtime.ContainerID) error {
	inst, ok := a.getInstance(id)
	if !ok {
		return fmt.Errorf("process runtime: container %s not found", id)
	}

	inst.mu.Lock()

	if inst.meta.PID != 0 && !inst.meta.Exited {
		inst.mu.Unlock()
		return nil // already running; idempotent like Docker's start.
	}

	argv := append([]string{inst.meta.Image}, inst.meta.Command...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = inst.dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = mergeEnv(inst.meta.Env)

	// F4 (M17 review): a previous run's log may already be oversized (the
	// periodic truncateOversizedLogs sweep only runs while the process is
	// running, and only every logRotateTicks — a process that exited
	// between sweeps can leave a large file behind). Rotate it out of the
	// way before appending more onto it, same as logrotate's copytruncate.
	stdoutPath := filepath.Join(inst.dir, "stdout.log")
	stderrPath := filepath.Join(inst.dir, "stderr.log")
	rotateLogIfLarge(stdoutPath)
	rotateLogIfLarge(stderrPath)

	outFile, err := os.OpenFile(stdoutPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		inst.mu.Unlock()
		return fmt.Errorf("process runtime: open stdout log: %w", err)
	}
	errFile, err := os.OpenFile(stderrPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		outFile.Close()
		inst.mu.Unlock()
		return fmt.Errorf("process runtime: open stderr log: %w", err)
	}
	cmd.Stdout = outFile
	cmd.Stderr = errFile

	// F2 (M17 review): persist the start-attempted marker *before*
	// cmd.Start() so a crash between here and the post-start save below is
	// visible on the next boot (see meta.StartAttemptedAt doc comment).
	inst.meta.StartAttemptedAt = time.Now()
	if err := inst.saveLocked(); err != nil {
		a.log.Warn("process runtime: persist start-attempted marker", "container", id, "err", err)
	}

	if err := cmd.Start(); err != nil {
		outFile.Close()
		errFile.Close()
		inst.mu.Unlock()
		return fmt.Errorf("process runtime: start %s: %w", inst.meta.Image, err)
	}

	// F2: persist the pid immediately — before cgroup setup — to keep the
	// crash-loses-pid window as small as possible.
	inst.meta.PID = cmd.Process.Pid
	inst.meta.StartedAt = time.Now()
	inst.meta.ProcKey = procKey(cmd.Process.Pid)
	inst.meta.Exited = false
	inst.meta.ExitCode = 0
	inst.meta.FinishedAt = time.Time{}
	inst.meta.Adopted = false
	inst.meta.CgroupDir = "" // reset any stale value from a prior run of this same instance
	inst.suspect = 0
	if err := inst.saveLocked(); err != nil {
		a.log.Warn("process runtime: persist meta after start", "container", id, "err", err)
	}

	cgDir := setupCgroup(string(id), inst.meta.CPUNanos, inst.meta.MemBytes, a.log)
	if cgDir != "" {
		addPidToCgroup(cgDir, cmd.Process.Pid, a.log)
		inst.meta.CgroupDir = cgDir
		if err := inst.saveLocked(); err != nil {
			a.log.Warn("process runtime: persist cgroup dir after start", "container", id, "err", err)
		}
	}

	inst.cmd = cmd
	inst.done = make(chan struct{})
	inst.doneOnce = &sync.Once{}
	labels := inst.meta.Labels
	inst.mu.Unlock()

	// F5 (M17 review): publish the start event, then spawn the wait
	// goroutine — not the other way around. A process that exits
	// immediately (e.g. a misconfigured artifact) must never have its
	// "die" event observed by a consumer before its "start" event; the
	// previous ordering (goroutine spawned first) raced waitForExit's
	// publish against this one.
	a.bus.publish(runtime.Event{
		ContainerID: id,
		Action:      "start",
		Labels:      labels,
		Time:        time.Now(),
	})

	go a.waitForExit(inst, outFile, errFile)

	return nil
}

// mergeEnv layers overrides on top of qooqd's own environment (PATH etc.),
// so components need only specify what they want to add or change.
func mergeEnv(overrides map[string]string) []string {
	base := os.Environ()
	if len(overrides) == 0 {
		return base
	}
	idx := make(map[string]int, len(base))
	for i, kv := range base {
		if eq := strings.IndexByte(kv, '='); eq >= 0 {
			idx[kv[:eq]] = i
		}
	}
	out := append([]string(nil), base...)
	for k, v := range overrides {
		if i, ok := idx[k]; ok {
			out[i] = k + "=" + v
		} else {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// waitForExit reaps the child (preventing a zombie) and records/publishes
// its exit. It runs for every process we spawned ourselves; adopted
// processes (reparented across a daemon restart) are instead watched by
// checkAdopted, since they are no longer our child and cmd.Wait() would fail.
func (a *Adapter) waitForExit(inst *instance, outFile, errFile *os.File) {
	inst.mu.Lock()
	cmd := inst.cmd
	doneCh := inst.done
	inst.mu.Unlock()

	err := cmd.Wait()
	outFile.Close()
	errFile.Close()
	code := exitCodeFromWaitErr(err)

	inst.mu.Lock()
	if inst.cmd != cmd {
		// Defensive: instance moved on to a different process already.
		inst.mu.Unlock()
		return
	}
	inst.meta.Exited = true
	inst.meta.ExitCode = code
	inst.meta.FinishedAt = time.Now()
	cgDir := inst.meta.CgroupDir
	labels := inst.meta.Labels
	if !inst.removed { // the instance dir is gone once RemoveContainer ran
		if err := inst.saveLocked(); err != nil {
			a.log.Warn("process runtime: persist meta after exit", "container", inst.id, "err", err)
		}
	}
	if inst.done == doneCh {
		inst.markDoneLocked()
	}
	inst.mu.Unlock()

	removeCgroup(cgDir)

	a.bus.publish(runtime.Event{
		ContainerID: inst.id,
		Action:      "die",
		ExitCode:    code,
		Labels:      labels,
		Time:        time.Now(),
	})
}

func exitCodeFromWaitErr(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal())
			}
			return ws.ExitStatus()
		}
		return ee.ExitCode()
	}
	return -1
}

// StopContainer sends SIGTERM to the instance's process group, waits up to
// timeout, then SIGKILLs it. It blocks until the process is confirmed dead
// (or a grace period elapses for an adopted process the poller hasn't yet
// caught up with).
func (a *Adapter) StopContainer(ctx context.Context, id runtime.ContainerID, timeout time.Duration) error {
	inst, ok := a.getInstance(id)
	if !ok {
		return fmt.Errorf("process runtime: container %s not found", id)
	}

	inst.mu.Lock()
	if inst.meta.Exited || inst.meta.PID == 0 {
		inst.mu.Unlock()
		return nil // already stopped; idempotent like Docker's stop.
	}
	pid := inst.meta.PID
	key := inst.meta.ProcKey
	doneCh := inst.done
	inst.mu.Unlock()

	// F6 (M17 review): verify the recorded pid is still the process we
	// started/adopted before signaling it. kill(pid,0) succeeding is not
	// enough on its own — pids get reused — so this checks hard evidence
	// of death (liveDead) and skips straight to cleanup rather than sending
	// SIGTERM/SIGKILL to whatever unrelated process may have inherited the
	// pid in the meantime (most likely for adopted instances, where the
	// gap between our last liveness check and this call can be seconds).
	if checkLiveness(pid, key) == liveDead {
		return nil
	}

	if timeout <= 0 {
		timeout = 10 * time.Second
	}

	_ = syscall.Kill(-pid, syscall.SIGTERM)

	select {
	case <-doneCh:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
	}

	_ = syscall.Kill(-pid, syscall.SIGKILL)

	select {
	case <-doneCh:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		// An adopted process with no cmd.Wait() of our own: the 2s poller
		// will catch up shortly and publish the die event.
	}
	return nil
}

// RemoveContainer deletes the instance's on-disk state (and, if force, kills
// it first). A non-force remove of a still-running instance is refused, like
// Docker's.
func (a *Adapter) RemoveContainer(ctx context.Context, id runtime.ContainerID, force bool) error {
	inst, ok := a.getInstance(id)
	if !ok {
		return fmt.Errorf("process runtime: container %s not found", id)
	}

	inst.mu.Lock()
	running := inst.meta.PID != 0 && !inst.meta.Exited
	pid := inst.meta.PID
	key := inst.meta.ProcKey
	cgDir := inst.meta.CgroupDir
	doneCh := inst.done
	if running && !force {
		inst.mu.Unlock()
		return fmt.Errorf("process runtime: container %s is still running, use force", id)
	}
	// Mark removed before killing so the wait goroutine does not try to
	// persist meta.json into (or race RemoveAll recreating files inside) a
	// directory that is about to be deleted.
	inst.removed = true
	inst.mu.Unlock()

	// F6 (M17 review): same pid-identity check as StopContainer — don't
	// SIGKILL a pid that hard evidence says is no longer our process.
	if running && checkLiveness(pid, key) == liveDead {
		running = false
	}

	if running {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		// Wait briefly for the death to be confirmed before deleting the
		// directory: for our own children the wait goroutine closes done;
		// an adopted process has no reaper of ours, so fall back to
		// kill(pid,0) polling.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			select {
			case <-doneCh:
				deadline = time.Time{}
			case <-time.After(50 * time.Millisecond):
				if syscall.Kill(pid, 0) != nil {
					deadline = time.Time{}
				}
			}
			if deadline.IsZero() {
				break
			}
		}
	}

	a.mu.Lock()
	delete(a.instances, id)
	a.mu.Unlock()

	removeCgroup(cgDir)
	if err := os.RemoveAll(inst.dir); err != nil {
		a.log.Warn("process runtime: remove instance dir", "container", id, "err", err)
	}
	return nil
}

func (a *Adapter) InspectContainer(ctx context.Context, id runtime.ContainerID) (runtime.ContainerStatus, error) {
	inst, ok := a.getInstance(id)
	if !ok {
		return runtime.ContainerStatus{}, fmt.Errorf("process runtime: container %s not found", id)
	}
	inst.mu.Lock()
	defer inst.mu.Unlock()
	return statusFromMeta(inst.meta), nil
}

func (a *Adapter) ListContainers(ctx context.Context, filter runtime.Filter) ([]runtime.ContainerStatus, error) {
	a.mu.Lock()
	insts := make([]*instance, 0, len(a.instances))
	for _, inst := range a.instances {
		insts = append(insts, inst)
	}
	a.mu.Unlock()

	out := make([]runtime.ContainerStatus, 0, len(insts))
	for _, inst := range insts {
		inst.mu.Lock()
		m := inst.meta
		inst.mu.Unlock()

		if !labelsMatch(filter.Labels, m.Labels) {
			continue
		}
		st := statusFromMeta(m)
		if !filter.All && st.Phase != runtime.PhaseRunning {
			continue
		}
		out = append(out, st)
	}
	return out, nil
}

func statusFromMeta(m meta) runtime.ContainerStatus {
	return runtime.ContainerStatus{
		ID:         runtime.ContainerID(m.ID),
		Name:       m.Name,
		Image:      m.Image,
		Phase:      phaseFromMeta(m),
		ExitCode:   m.ExitCode,
		Created:    m.CreatedAt,
		StartedAt:  m.StartedAt,
		FinishedAt: m.FinishedAt,
		Labels:     m.Labels,
	}
}

// phaseFromMeta mirrors the docker adapter's mapPhase: never-started ->
// Creating, running -> Running, exited cleanly -> Exited, exited non-zero
// (including adopted processes whose real code is unknown and reported -1)
// -> Failed.
func phaseFromMeta(m meta) runtime.Phase {
	switch {
	case m.PID == 0:
		return runtime.PhaseCreating
	case !m.Exited:
		return runtime.PhaseRunning
	case m.ExitCode != 0:
		return runtime.PhaseFailed
	default:
		return runtime.PhaseExited
	}
}

func labelsMatch(want, have map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
