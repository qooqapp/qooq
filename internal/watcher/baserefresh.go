package watcher

import (
	"bufio"
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Base images are pulled once and then built from cache forever: the docker
// build is not asked to pull its parents, so a FROM nginx:alpine keeps the
// nginx it found the first time even after a security release replaces that
// tag (NGINX2Root, 2026-11-18: the cached alpine tag was 1.31.8 a day after
// 1.31.9 shipped). Pulling on every build would fix that and cost a registry
// round trip per FROM per build — on Docker Hub's anonymous rate limit, with
// many users, that is a way for builds to start failing — and an offline
// machine could not build at all.
//
// So each base is re-pulled at most once per interval, before the build that
// uses it, on the machine that builds. A failed pull is a warning and the
// build goes on with what is cached: a registry blip must not turn into a
// build failure, and a private base the build node cannot pull anonymously
// is still built from whatever it has (as it was before this existed).

const (
	configBaseRefreshInterval  = "build.base_refresh_interval"
	defaultBaseRefreshInterval = 24 * time.Hour
	basePullTimeout            = 5 * time.Minute
)

type baseRefresher struct {
	interval time.Duration
	mu       sync.Mutex
	last     map[string]time.Time // ref -> last attempt, success or not
}

func newBaseRefresher(interval time.Duration) *baseRefresher {
	return &baseRefresher{interval: interval, last: map[string]time.Time{}}
}

// due reports whether ref should be pulled now, and marks it attempted so a
// concurrent build of another project with the same base does not pull it too.
func (b *baseRefresher) due(ref string, now time.Time) bool {
	if b == nil || b.interval <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.last[ref]; ok && now.Sub(t) < b.interval {
		return false
	}
	b.last[ref] = now
	return true
}

// refreshBaseImages pulls the stale bases the Dockerfile at path builds FROM.
func (w *Watcher) refreshBaseImages(ctx context.Context, path string) {
	refs, err := dockerfileBases(path)
	if err != nil {
		return // the build reads the same file and reports the real error
	}
	for _, ref := range refs {
		if !w.baseRefresh.due(ref, time.Now()) {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, basePullTimeout)
		err := w.buildNode().PullImageAuth(pctx, ref, "", "")
		cancel()
		if err != nil {
			w.log.Warn("could not refresh base image; building from the cached one",
				zap.String("image", ref), zap.Error(err))
			continue
		}
		w.log.Info("refreshed base image", zap.String("image", ref))
	}
}

// dockerfileBases lists the external images a Dockerfile builds FROM, in
// order and without repeats. A FROM naming an earlier stage, scratch, or a
// reference built from ARGs is skipped: the first is not an image, the second
// has nothing to pull, and the third cannot be resolved without the build.
func dockerfileBases(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	stages := map[string]bool{}
	seen := map[string]bool{}
	var refs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		args := fields[1:]
		for len(args) > 0 && strings.HasPrefix(args[0], "--") {
			args = args[1:]
		}
		if len(args) == 0 {
			continue
		}
		ref := args[0]
		earlierStage := stages[strings.ToLower(ref)]
		if len(args) >= 3 && strings.EqualFold(args[1], "AS") {
			stages[strings.ToLower(args[2])] = true
		}
		if earlierStage || strings.EqualFold(ref, "scratch") || strings.Contains(ref, "$") || seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return refs, sc.Err()
}
