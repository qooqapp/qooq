package watcher

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Data usage per namespace.
//
// What grows without bound is the data qooq keeps for services — a database's
// files under mounts.dir/<namespace>/<component>/. Images, logs and build
// leftovers already have limits. This measures that tree, one total per
// namespace, so the external manager can add them up per account and hold an
// account over its plan's limit.
//
// Measured on a timer and served from memory: walking a database's files is
// slow enough that doing it inside a request would make the request the slow
// part of whatever asked.
const diskUsageInterval = 10 * time.Minute

type diskUsage struct {
	mu   sync.Mutex
	byNS map[string]int64
	// byComp is the same measurement one level down, keyed
	// "<namespace>/<component>" — the operator's usage page shows which
	// service holds the data, not only which namespace.
	byComp     map[string]int64
	measuredAt time.Time
}

func (d *diskUsage) snapshot() (map[string]int64, time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make(map[string]int64, len(d.byNS))
	for k, v := range d.byNS {
		out[k] = v
	}
	return out, d.measuredAt
}

// snapshotAll is snapshot plus the per-component totals.
func (d *diskUsage) snapshotAll() (byNS, byComp map[string]int64, at time.Time) {
	byNS, at = d.snapshot()
	d.mu.Lock()
	defer d.mu.Unlock()
	byComp = make(map[string]int64, len(d.byComp))
	for k, v := range d.byComp {
		byComp[k] = v
	}
	return byNS, byComp, at
}

// measureMounts totals the regular files under each namespace directory of
// root, and under each component directory inside it (byComp, keyed
// "<namespace>/<component>"). Links are not followed — a link is the size of
// the link — so nothing outside the tree is ever counted, or walked into.
func measureMounts(root string) (byNS, byComp map[string]int64, err error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]int64{}, map[string]int64{}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	byNS, byComp = map[string]int64{}, map[string]int64{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		nsDir := filepath.Join(root, e.Name())
		var total int64
		// Errors inside one namespace (a file removed mid-walk) skip that
		// entry rather than the whole measurement.
		_ = filepath.WalkDir(nsDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.Type().IsRegular() {
				if info, ierr := d.Info(); ierr == nil {
					total += info.Size()
					if rel, rerr := filepath.Rel(nsDir, path); rerr == nil {
						if comp, _, ok := strings.Cut(filepath.ToSlash(rel), "/"); ok {
							byComp[e.Name()+"/"+comp] += info.Size()
						}
					}
				}
			}
			return nil
		})
		byNS[e.Name()] = total
	}
	return byNS, byComp, nil
}

// runDiskUsageLoop measures now, then every diskUsageInterval, until ctx ends.
func (w *Watcher) runDiskUsageLoop(ctx context.Context) {
	measure := func() {
		byNS, byComp, err := measureMounts(w.mountsDir)
		if err != nil {
			w.log.Warn("disk usage: measuring mounts failed", zap.String("dir", w.mountsDir), zap.Error(err))
			return
		}
		w.disk.mu.Lock()
		w.disk.byNS, w.disk.byComp, w.disk.measuredAt = byNS, byComp, time.Now()
		w.disk.mu.Unlock()
	}
	measure()
	t := time.NewTicker(diskUsageInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			measure()
		}
	}
}

// handleDiskUsage serves the last measurement. measured_at is absent until
// the first one finishes, so a caller can tell "nothing yet" from "nothing used".
func (w *Watcher) handleDiskUsage(c *gin.Context) {
	byNS, at := w.disk.snapshot()
	resp := gin.H{"namespaces": byNS}
	if !at.IsZero() {
		resp["measured_at"] = at
	}
	c.JSON(http.StatusOK, resp)
}

// errQuotaHold is a deploy refused because the target's account is over a
// plan limit. The hold's own reason is the message.
type errQuotaHold struct{ reason string }

func (e errQuotaHold) Error() string { return e.reason }

// handleQuotaHold sets (non-empty reason) or clears (empty) a target's quota
// hold. Idempotent, so the external manager can reassert it every round.
func (w *Watcher) handleQuotaHold(c *gin.Context) {
	var body struct {
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		respondErr(c, http.StatusBadRequest, err)
		return
	}
	t, err := getTargetByName(w.db, c.Param("name"))
	if err != nil {
		respondErr(c, http.StatusNotFound, err)
		return
	}
	if t.QuotaHold == body.Reason {
		c.Status(http.StatusNoContent)
		return
	}
	if err := w.db.Model(t).Update("quota_hold", body.Reason).Error; err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	if body.Reason == "" {
		w.log.Info("quota hold lifted", zap.String("target", t.Name))
	} else {
		w.log.Info("quota hold set", zap.String("target", t.Name), zap.String("reason", body.Reason))
	}
	c.Status(http.StatusNoContent)
}
