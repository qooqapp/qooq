package watcher

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qooqapp/qooq/pkg/client"
	"github.com/qooqapp/qooq/pkg/watcherclient"
)

// Per-service usage for the operator.
//
// Nothing is stopped for being idle — a user's service is not turned
// off because nobody visited it. This is the page the operator decides from:
// every component qooqd runs, with its CPU and memory right now, the data it
// keeps, and when it was last deployed (the stand-in for "idle" until there is
// traffic to measure).
//
// CPU and memory are qooqd's latest sample, one call per component. The page
// is opened by a person, not polled by a loop, so the calls are made when it
// asks; a few at a time, and a component whose sample does not come back in
// time is listed without one rather than holding up the rest.
const (
	usageStatsParallel = 8
	usageStatsTimeout  = 3 * time.Second
)

func (w *Watcher) handleComponentUsage(c *gin.Context) {
	ctx := c.Request.Context()
	if w.client == nil {
		c.JSON(http.StatusOK, watcherclient.ComponentUsageResponse{Components: []watcherclient.ComponentUsage{}})
		return
	}
	comps, err := w.client.ListComponents(ctx)
	if err != nil {
		respondErr(c, http.StatusBadGateway, err)
		return
	}
	ts, err := listTargets(w.db)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	_, byComp, measuredAt := w.disk.snapshotAll()

	// Which target each component belongs to — its own, or as a service the
	// target's build started — keyed by (namespace, name) as everywhere else.
	type key struct{ ns, name string }
	type owner struct {
		target   string
		service  bool
		deployAt *time.Time
	}
	owners := map[key]owner{}
	for i := range ts {
		r := ts[i].toResponse()
		ns := client.NormalizeNamespace(r.Namespace)
		owners[key{ns, r.ComponentName}] = owner{target: r.Name, deployAt: r.LastBuildAt}
		if r.Topology != nil {
			for _, s := range r.Topology.Services {
				owners[key{ns, s.Component}] = owner{target: r.Name, service: true, deployAt: r.LastBuildAt}
			}
		}
	}

	out := make([]watcherclient.ComponentUsage, 0, len(comps))
	for _, comp := range comps {
		if comp.Phase == "Removed" {
			continue
		}
		ns := client.NormalizeNamespace(comp.Namespace)
		u := watcherclient.ComponentUsage{
			Namespace: ns, Component: comp.Name, Phase: comp.Phase, DesiredState: comp.DesiredState,
			CPULimit: comp.CPULimit, MemLimit: comp.MemLimitBytes,
		}
		if !comp.StartedAt.IsZero() {
			at := comp.StartedAt
			u.StartedAt = &at
		}
		if o, ok := owners[key{ns, comp.Name}]; ok {
			u.Target, u.Service, u.LastDeployAt = o.target, o.service, o.deployAt
		}
		if !measuredAt.IsZero() {
			n := byComp[ns+"/"+comp.Name]
			u.DataBytes = &n
		}
		out = append(out, u)
	}

	w.attachStats(ctx, comps, out)
	resp := watcherclient.ComponentUsageResponse{Components: out}
	if !measuredAt.IsZero() {
		resp.DataMeasuredAt = &measuredAt
	}
	c.JSON(http.StatusOK, resp)
}

// attachStats fills CPU and memory for the running components of out, which
// lists the non-removed members of comps in the same order.
func (w *Watcher) attachStats(ctx context.Context, comps []client.ComponentResponse, out []watcherclient.ComponentUsage) {
	ids := make([]string, 0, len(out))
	for _, comp := range comps {
		if comp.Phase != "Removed" {
			ids = append(ids, comp.ID)
		}
	}
	sem := make(chan struct{}, usageStatsParallel)
	var wg sync.WaitGroup
	for i := range out {
		if out[i].Phase != "Running" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			sctx, cancel := context.WithTimeout(ctx, usageStatsTimeout)
			defer cancel()
			s, err := w.client.Stats(sctx, ids[i])
			if err != nil || s.Time.IsZero() {
				return
			}
			cpu, mem := s.CPUPercent, s.MemUsage
			out[i].CPUPercent, out[i].MemUsage = &cpu, &mem
		}(i)
	}
	wg.Wait()
}
