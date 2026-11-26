package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	goruntime "runtime"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/ingress"
	"github.com/qooqapp/qooq/internal/runtime"
	"github.com/qooqapp/qooq/pkg/client"
)

// statsStreamInterval is how often the ?watch=1 SSE stream emits the latest
// stats sample. It matches the monitor's ~2s sampling cadence, so each event
// is a fresh sample rather than a repeat.
const statsStreamInterval = 2 * time.Second

type handlers struct {
	svc *core.Service
	log *slog.Logger
	// advertiseHost is the address other machines use to reach this host,
	// used to compose component endpoints. Empty means "unknown": endpoints
	// fall back to 127.0.0.1.
	advertiseHost *AdvertiseHost
	// ingress describes the reverse proxy in front of components, when one is
	// running. Nil means no ingress, and endpoints are host-port URLs only.
	// It is the same Config the proxy runs with — the shape of the ingress,
	// not a handle on it — so the reported URLs follow the same rules.
	ingress *ingress.Config
}

func (h *handlers) health(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	if err := h.svc.Ping(r.Context()); err != nil {
		status = "degraded"
		h.log.Warn("healthz: runtime ping failed", "err", err)
	}
	writeJSON(w, http.StatusOK, client.HealthResponse{
		Status:  status,
		Runtime: h.svc.RuntimeName(),
	})
}

// capacity reports host resource capacity — cpu cores, total memory, arch,
// os — independent of docker/the runtime. Consumed by qooq-watcher's M14
// `requires` validation before it builds an app-manifest-described component.
func (h *handlers) capacity(w http.ResponseWriter, r *http.Request) {
	mem, err := hostMemoryBytes()
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, client.CapacityResponse{
		CPUCores:      goruntime.NumCPU(),
		MemoryBytes:   mem,
		Arch:          goruntime.GOARCH,
		OS:            goruntime.GOOS,
		Runtime:       h.svc.RuntimeName(),
		AdvertiseHost: h.advertiseHost.Get(),
	})
}

func (h *handlers) backup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="qooq-backup.db"`)
	// Content-Length is set from the snapshot size (known inside the read tx,
	// before any bytes flow). Once bytes flow the 200 is committed, so a
	// mid-stream failure can only be logged here — but with a declared length
	// net/http aborts the connection on the short write, so the client sees an
	// unexpected EOF instead of a silently truncated "successful" backup.
	if err := h.svc.Backup(w, func(size int64) {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}); err != nil {
		h.log.Error("backup: snapshot failed", "err", err)
	}
}

func (h *handlers) version(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, client.VersionResponse{
		Version:    Version,
		Runtime:    h.svc.RuntimeName(),
		APIVersion: "v1",
	})
}

func (h *handlers) deploy(w http.ResponseWriter, r *http.Request) {
	var req client.DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Image == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name and image are required"))
		return
	}

	comp, err := h.svc.Deploy(r.Context(), specFromRequest(req))
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, h.toResponse(comp))
}

// upsertComponent handles PUT /v1/components: create-if-absent, replace
// (same ULID, new container) if a non-removed component already owns the
// name. Name is decoded from the body rather than a path param specifically
// to avoid colliding with the /{id} routes.
func (h *handlers) upsertComponent(w http.ResponseWriter, r *http.Request) {
	var req client.DeployRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" || req.Image == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name and image are required"))
		return
	}

	comp, created, err := h.svc.Upsert(r.Context(), specFromRequest(req))
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, h.toResponse(comp))
}

func (h *handlers) listComponents(w http.ResponseWriter, r *http.Request) {
	comps, err := h.svc.List(r.Context())
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	out := make([]client.ComponentResponse, 0, len(comps))
	for _, c := range comps {
		out = append(out, h.toResponseAmong(c, comps))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) getComponent(w http.ResponseWriter, r *http.Request) {
	comp, err := h.svc.Get(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(comp))
}

func (h *handlers) removeComponent(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Remove(r.Context(), chi.URLParam(r, "id")); err != nil {
		h.writeComponentErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) startComponent(w http.ResponseWriter, r *http.Request) {
	comp, err := h.svc.Start(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(comp))
}

func (h *handlers) stopComponent(w http.ResponseWriter, r *http.Request) {
	comp, err := h.svc.Stop(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(comp))
}

// rollbackComponent handles POST /v1/components/{id}/rollback.
func (h *handlers) rollbackComponent(w http.ResponseWriter, r *http.Request) {
	comp, err := h.svc.Rollback(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(comp))
}

// componentLogs handles GET /v1/components/{id}/logs?follow=1&tail=N. The
// runtime's ReadCloser is copied straight to the response body (chunked,
// flushed after every read when follow is set) — no buffering, matching
// core.Service.Logs's "no io buffering in core" contract. The stream ends
// when the runtime closes it (non-follow: EOF once existing output is
// drained) or when the client disconnects (r.Context() is cancelled by
// net/http, which propagates into the runtime adapter's ctx and unblocks its
// Read).
func (h *handlers) componentLogs(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	follow := false
	if f := r.URL.Query().Get("follow"); f != "" {
		follow = f == "1" || f == "true"
	}
	tail := 0
	if t := r.URL.Query().Get("tail"); t != "" {
		n, err := strconv.Atoi(t)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, errors.New("invalid tail: must be a non-negative integer"))
			return
		}
		tail = n
	}
	timestamps := false
	if ts := r.URL.Query().Get("timestamps"); ts != "" {
		timestamps = ts == "1" || ts == "true"
	}

	rc, err := h.svc.Logs(r.Context(), id, runtime.LogOptions{Follow: follow, Tail: tail, Timestamps: timestamps})
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	defer rc.Close()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flusher, canFlush := w.(http.Flusher)

	buf := make([]byte, 32*1024)
	for {
		n, rerr := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				h.log.Warn("component logs: stream ended with error", "id", id, "err", rerr)
			}
			return
		}
	}
}

func (h *handlers) pushImage(w http.ResponseWriter, r *http.Request) {
	var req client.PushRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Ref == "" {
		writeErr(w, http.StatusBadRequest, errors.New("ref is required"))
		return
	}
	auth, err := encodeRegistryAuth(req.Username, req.Password)
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	if err := h.svc.PushImage(r.Context(), req.Ref, auth); err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) pullImage(w http.ResponseWriter, r *http.Request) {
	var req client.PullRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Ref == "" {
		writeErr(w, http.StatusBadRequest, errors.New("ref is required"))
		return
	}
	auth, err := encodeRegistryAuth(req.Username, req.Password)
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	id, err := h.svc.PullImageAuth(r.Context(), req.Ref, req.Platform, auth)
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, client.PullResponse{ImageID: id})
}

// digestImage handles POST /v1/images/digest (M20a): queries the registry
// for ref's current content digest without pulling it, so watcher's
// image-mode targets can cheaply poll for upstream changes.
func (h *handlers) digestImage(w http.ResponseWriter, r *http.Request) {
	var req client.ImageDigestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Ref == "" {
		writeErr(w, http.StatusBadRequest, errors.New("ref is required"))
		return
	}
	auth, err := encodeRegistryAuth(req.Username, req.Password)
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	digest, err := h.svc.ResolveImageDigest(r.Context(), req.Ref, auth)
	if err != nil {
		if errors.Is(err, runtime.ErrImageNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, client.ImageDigestResponse{Ref: req.Ref, Digest: digest})
}

// encodeRegistryAuth builds a docker-style base64url(JSON) registry auth
// string from a username/password pair, matching the wire format the Docker
// SDK's registry.EncodeAuthConfig produces (and DistributionInspect/
// ImagePull's RegistryAuth parameter expects) — reproduced by hand here
// rather than importing the docker SDK's registry package, since
// internal/api may not depend on Docker (see the dependency
// wall in docs/ARCHITECTURE.md). Both empty returns "" (anonymous, same as before M20a).
func encodeRegistryAuth(username, password string) (string, error) {
	if username == "" && password == "" {
		return "", nil
	}
	buf, err := json.Marshal(struct {
		Username string `json:"username,omitempty"`
		Password string `json:"password,omitempty"`
	}{Username: username, Password: password})
	if err != nil {
		return "", fmt.Errorf("encode registry auth: %w", err)
	}
	return base64.URLEncoding.EncodeToString(buf), nil
}

// loadImage handles POST /v1/images/load. The request body is a `docker
// save` tar stream; it is passed straight through to the runtime adapter
// without being buffered in memory.
func (h *handlers) loadImage(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	ids, err := h.svc.LoadImage(r.Context(), r.Body)
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	writeJSON(w, http.StatusOK, client.LoadResponse{ImageIDs: ids})
}

// buildImage handles POST /v1/images/build?dockerfile=&tag=. The request
// body is a build-context tar stream, passed straight through unbuffered.
func (h *handlers) buildImage(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	dockerfile := r.URL.Query().Get("dockerfile")
	// Every ?tag=, not just the first. docker can apply several names in one
	// build, which is how an image gets its local name and its registry name
	// at once without a separate tag call. A single ?tag= still
	// behaves exactly as before.
	var tags []string
	for _, tag := range r.URL.Query()["tag"] {
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	res, err := h.svc.BuildImage(r.Context(), r.Body, dockerfile, tags)
	if err != nil {
		// The status still comes from the mapper — a build stopped by the
		// machine's timeout has to say so — but the output goes back with it.
		//
		// This is the case the log exists for. A failed build's output is the
		// answer to "why", and it lives nowhere else: docker does not keep it,
		// and the stream is closed by the time this returns. Without it the
		// caller gets a sentence and has to go and reproduce the build.
		h.writeBuildErr(w, err, res.Log)
		return
	}
	writeJSON(w, http.StatusOK, client.BuildResponse{ImageID: res.ImageID, Log: res.Log})
}

func (h *handlers) listImages(w http.ResponseWriter, r *http.Request) {
	images, err := h.svc.ListImages(r.Context())
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	out := make([]client.ImageResponse, 0, len(images))
	for _, img := range images {
		out = append(out, client.ImageResponse{
			ID:      string(img.ID),
			Tags:    img.Tags,
			Created: img.Created,
			Size:    img.Size,
			Built:   img.Labels[core.LabelBuilt] == "true",
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// removeImage handles DELETE /v1/images?ref=<ref>. ref is a query param
// (rather than a path segment) so tags containing "/" and ":" don't collide
// with chi's routing. Not-found and in-use are mapped to distinct, non-500
// statuses so callers (qooq-watcher's prune step, manual curl) can tell
// "nothing to do" from a real failure.
func (h *handlers) removeImage(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeErr(w, http.StatusBadRequest, errors.New("ref is required"))
		return
	}
	if err := h.svc.RemoveImage(r.Context(), ref); err != nil {
		switch {
		case errors.Is(err, runtime.ErrImageNotFound):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, runtime.ErrImageInUse):
			writeErr(w, http.StatusConflict, err)
		default:
			writeInternalErr(w, h.log, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) componentStats(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if _, err := h.svc.Get(r.Context(), id); err != nil {
		h.writeComponentErr(w, err)
		return
	}

	if watch := r.URL.Query().Get("watch"); watch == "1" || watch == "true" {
		h.streamComponentStats(w, r, id)
		return
	}

	sample, ok := h.svc.LatestStat(id)
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("no stats sample yet for this component"))
		return
	}
	writeJSON(w, http.StatusOK, statsResponse(id, sample))
}

func statsResponse(id string, sample runtime.StatSample) client.StatsResponse {
	return client.StatsResponse{
		ComponentID: id,
		CPUPercent:  sample.CPUPercent,
		MemUsage:    sample.MemUsage,
		MemLimit:    sample.MemLimit,
		Time:        sample.Time,
	}
}

// streamComponentStats emits the latest stats sample as Server-Sent Events
// every statsStreamInterval until the client disconnects or the component is
// removed. Each event is `data: <StatsResponse JSON>\n\n`.
func (h *handlers) streamComponentStats(w http.ResponseWriter, r *http.Request, id string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeInternalErr(w, h.log, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// emit sends the current sample if one exists. It returns false when the
	// stream should end (write error, or the component no longer exists).
	emit := func() bool {
		if _, err := h.svc.Get(r.Context(), id); err != nil {
			return false // component gone; end the stream
		}
		sample, ok := h.svc.LatestStat(id)
		if !ok {
			return true // no sample yet; keep the stream open
		}
		payload, err := json.Marshal(statsResponse(id, sample))
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	if !emit() {
		return
	}
	ticker := time.NewTicker(statsStreamInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if !emit() {
				return
			}
		}
	}
}

func (h *handlers) componentEvents(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	events, err := h.svc.ListEvents(r.Context(), id)
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	out := make([]client.EventResponse, 0, len(events))
	for _, e := range events {
		out = append(out, client.EventResponse{
			ComponentID: e.ComponentID,
			Type:        e.Type,
			Message:     e.Message,
			ExitCode:    e.ExitCode,
			Time:        e.Time,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) createGroup(w http.ResponseWriter, r *http.Request) {
	var req client.CreateGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("name is required"))
		return
	}
	g, err := h.svc.CreateGroup(r.Context(), req.Name)
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	writeJSON(w, http.StatusCreated, toGroupResponse(g))
}

func (h *handlers) listGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.svc.ListGroups(r.Context())
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	out := make([]client.GroupResponse, 0, len(groups))
	for _, g := range groups {
		out = append(out, toGroupResponse(g))
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) getGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	g, err := h.svc.GetGroup(r.Context(), id)
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	members, err := h.svc.GroupMembers(r.Context(), id)
	if err != nil {
		writeInternalErr(w, h.log, err)
		return
	}
	mout := make([]client.ComponentResponse, 0, len(members))
	for _, m := range members {
		mout = append(mout, h.toResponse(m))
	}
	writeJSON(w, http.StatusOK, client.GroupDetailResponse{
		GroupResponse: toGroupResponse(g),
		Members:       mout,
	})
}

func (h *handlers) deleteGroup(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	cascade := r.URL.Query().Get("cascade") == "true"
	if _, err := h.svc.DeleteGroup(r.Context(), id, cascade); err != nil {
		h.writeComponentErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *handlers) startGroup(w http.ResponseWriter, r *http.Request) {
	h.groupBatch(w, r, h.svc.StartGroup)
}

func (h *handlers) stopGroup(w http.ResponseWriter, r *http.Request) {
	h.groupBatch(w, r, h.svc.StopGroup)
}

func (h *handlers) groupBatch(w http.ResponseWriter, r *http.Request, op func(context.Context, string) ([]core.BatchResult, error)) {
	results, err := op(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toBatchResults(results))
}

func (h *handlers) addMember(w http.ResponseWriter, r *http.Request) {
	var req client.AddMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.ComponentID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("component_id is required"))
		return
	}
	comp, err := h.svc.AddMember(r.Context(), chi.URLParam(r, "id"), req.ComponentID)
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(comp))
}

func (h *handlers) removeMember(w http.ResponseWriter, r *http.Request) {
	comp, err := h.svc.RemoveMember(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "componentID"))
	if err != nil {
		h.writeComponentErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.toResponse(comp))
}

func toGroupResponse(g *core.Group) client.GroupResponse {
	return client.GroupResponse{ID: g.ID, Name: g.Name, CreatedAt: g.CreatedAt}
}

func toBatchResults(results []core.BatchResult) []client.BatchResultResponse {
	out := make([]client.BatchResultResponse, 0, len(results))
	for _, r := range results {
		out = append(out, client.BatchResultResponse{
			ComponentID: r.ComponentID,
			Name:        r.Name,
			Phase:       string(r.Phase),
			Err:         r.Err,
		})
	}
	return out
}

// endpoints lists every URL this component answers on: the ingress hostnames
// first when there is an ingress, then any published host port.
//
// The proxy itself is left with its host-port URL only. It has no route of its
// own, and telling an operator to visit qooq-ingress.localhost would send them
// to a 404 from the very thing that produced the address.
//
// all is every component, because which hostnames a component keeps depends
// on the others (ingress.Assign); nil means look them up.
func (h *handlers) endpoints(c *core.Component, ports []client.PortSpec, all []*core.Component) []string {
	adv := h.advertiseHost.Get()
	// A port bound to loopback answers nobody at the advertise host; listing
	// it would be an address that does not work.
	loopback := map[int]bool{}
	for _, p := range c.Spec.Ports {
		if ip := net.ParseIP(p.HostIP); ip != nil && ip.IsLoopback() {
			loopback[p.HostPort] = true
		}
	}
	reachable := make([]client.PortSpec, 0, len(ports))
	for _, p := range ports {
		if !loopback[p.HostPort] {
			reachable = append(reachable, p)
		}
	}
	direct := endpointsFor(adv, reachable)
	if h.ingress == nil || c.Spec.Name == ingress.ComponentName || len(c.Spec.Ports) == 0 ||
		(c.Spec.Ingress != nil && c.Spec.Ingress.Off) {
		return direct
	}
	if all == nil {
		var err error
		if all, err = h.svc.List(context.Background()); err != nil {
			// Without the others, this component's names cannot be checked
			// against theirs; reporting ones it may not hold is the error
			// the check exists to prevent.
			h.log.Warn("endpoints: list components", "err", err)
			return direct
		}
	}
	return append(ingressEndpoints(c, adv, *h.ingress, all), direct...)
}

// toResponse is a method rather than a plain function because the endpoints it
// reports depend on the daemon's advertise host, which is per-daemon config.
func (h *handlers) toResponse(c *core.Component) client.ComponentResponse {
	return h.toResponseAmong(c, nil)
}

// toResponseAmong is toResponse for a caller already holding every component,
// so a list does not look them all up again once per entry.
func (h *handlers) toResponseAmong(c *core.Component, all []*core.Component) client.ComponentResponse {
	ports := make([]client.PortSpec, 0, len(c.Spec.Ports))
	for _, p := range c.Spec.Ports {
		// AutoHost is deliberately not echoed: by the time a component exists
		// the request has been answered, and the answer is HostPort. Reporting
		// "auto" alongside a real port would leave a reader guessing which one
		// the component is actually published on.
		ports = append(ports, client.PortSpec{
			ContainerPort: p.ContainerPort,
			HostPort:      p.HostPort,
			Protocol:      p.Protocol,
		})
	}
	mounts := make([]client.MountSpec, 0, len(c.Spec.Mounts))
	for _, m := range c.Spec.Mounts {
		mounts = append(mounts, client.MountSpec{
			HostPath:      m.Source,
			ContainerPath: m.Target,
			ReadOnly:      m.ReadOnly,
		})
	}
	resp := client.ComponentResponse{
		ID:               c.ID,
		Namespace:        c.Spec.Namespace,
		Name:             c.Spec.Name,
		ContainerName:    core.ContainerName(c.Spec),
		Image:            c.Spec.Image,
		Phase:            string(c.Status.Phase),
		ContainerID:      c.Status.ContainerID,
		ExitCode:         c.Status.LastExitCode,
		CrashCount:       c.Status.CrashCount,
		GroupID:          c.Spec.GroupID,
		Ports:            ports,
		Endpoints:        h.endpoints(c, ports, all),
		Mounts:           mounts,
		Network:          c.Spec.Network,
		Env:              c.Spec.Env,
		DesiredState:     c.Status.DesiredState,
		RestartPolicy:    c.Spec.RestartPolicy,
		RestartCount:     c.Status.RestartCount,
		PreviousImage:    c.Status.PreviousImage,
		CPULimit:         c.Spec.CPULimit,
		MemLimitBytes:    c.Spec.MemLimitBytes,
		Domain:           c.Spec.Domain,
		Ingress:          toIngressSpec(c.Spec.Ingress),
		LastRestartAt:    c.Status.LastRestartAt,
		BackoffUntil:     c.Status.BackoffUntil,
		RestartExhausted: c.Status.RestartExhausted,
		LastError:        c.Status.LastError,
		HealthCheck:      toHealthCheckSpec(c.Spec.HealthCheck),
		HealthyAt:        c.Status.HealthyAt,
		StartedAt:        c.Status.StartedAt,
		CreatedAt:        c.CreatedAt,
		UpdatedAt:        c.UpdatedAt,
	}
	// Reported as a pair so a reader does not have to fetch the daemon's
	// current address separately and compare it themselves; both are empty
	// unless this component genuinely carries the old one.
	resp.AdvertiseHostAtDeploy, resp.AdvertiseHostNow = staleAddress(c, h.advertiseHost.Get())
	return resp
}

// fromHealthCheckSpec converts the wire DTO into the core domain type.
func fromHealthCheckSpec(hc *client.HealthCheckSpec) *core.HealthCheck {
	if hc == nil {
		return nil
	}
	return &core.HealthCheck{
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

// toHealthCheckSpec converts the core domain type into the wire DTO.
func toHealthCheckSpec(hc *core.HealthCheck) *client.HealthCheckSpec {
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

// specFromRequest translates the wire DTO into a core spec. Deploy and Upsert
// accept the exact same body, so they share this one translation: keeping two
// copies in sync is how a new field ends up silently working on one verb and
// not the other.
func specFromRequest(req client.DeployRequest) core.ComponentSpec {
	ports := make([]runtime.PortMapping, 0, len(req.Ports))
	for _, p := range req.Ports {
		ports = append(ports, runtime.PortMapping{
			ContainerPort: p.ContainerPort,
			HostPort:      p.HostPort,
			Protocol:      p.Protocol,
			AutoHost:      p.AutoHost,
		})
	}
	mounts := make([]runtime.Mount, 0, len(req.Mounts))
	for _, m := range req.Mounts {
		mounts = append(mounts, runtime.Mount{
			Source:   m.HostPath,
			Target:   m.ContainerPath,
			ReadOnly: m.ReadOnly,
		})
	}
	return core.ComponentSpec{
		// Filled in here because this is the single door every deploy and
		// upsert comes through: past it, a component always has a namespace
		// and identity comparisons cannot disagree about what "none" means.
		Namespace:     client.NormalizeNamespace(req.Namespace),
		Name:          req.Name,
		Image:         req.Image,
		Command:       req.Command,
		Env:           req.Env,
		Ports:         ports,
		Mounts:        mounts,
		Network:       req.Network,
		GroupID:       req.GroupID,
		RestartPolicy: req.RestartPolicy,
		CPULimit:      req.CPULimit,
		MemLimitBytes: req.MemLimitBytes,
		Domain:        req.Domain,
		Ingress:       fromIngressSpec(req.Ingress),
		HealthCheck:   fromHealthCheckSpec(req.HealthCheck),
	}
}

// fromIngressSpec converts the wire DTO into the core domain type.
func fromIngressSpec(in *client.IngressSpec) *core.IngressSpec {
	if in == nil {
		return nil
	}
	return &core.IngressSpec{
		Hosts:     in.Hosts,
		Port:      in.Port,
		Path:      in.Path,
		StripPath: in.StripPath,
		TLS:       in.TLS,
		Off:       in.Off,
	}
}

// toIngressSpec converts the core domain type back out to the wire.
func toIngressSpec(in *core.IngressSpec) *client.IngressSpec {
	if in == nil {
		return nil
	}
	return &client.IngressSpec{
		Hosts:     in.Hosts,
		Port:      in.Port,
		Path:      in.Path,
		StripPath: in.StripPath,
		TLS:       in.TLS,
		Off:       in.Off,
	}
}

func (h *handlers) writeComponentErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, core.ErrNameConflict):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, core.ErrInvalidRestartPolicy):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, core.ErrInvalidName):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, core.ErrInvalidMount):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, core.ErrInvalidHealthCheck):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, core.ErrNothingToRollback):
		writeErr(w, http.StatusConflict, err)
	case errors.Is(err, core.ErrBuildTimeout):
		// The operator's build timeout took effect. Reported with its detail,
		// and as 504 rather than 500, for the same reason as the port conflict
		// below: a masked 5xx here sends the author looking at their Dockerfile's
		// last line instead of at the clock.
		writeErr(w, http.StatusGatewayTimeout, err)
	case errors.Is(err, core.ErrLowDisk):
		// The machine, not the request: 507 so the caller can tell it from a
		// build that failed, and with the detail, which says how full it is.
		writeErr(w, http.StatusInsufficientStorage, err)
	case errors.Is(err, core.ErrImageTooLarge):
		// The repository's doing, and the message says what to change.
		writeErr(w, http.StatusRequestEntityTooLarge, err)
	case errors.Is(err, runtime.ErrPortConflict):
		// An operator configuration error (the requested host port is taken),
		// not a server fault: keep the detail so the caller learns which port
		// to change instead of getting an opaque 500.
		writeErr(w, http.StatusConflict, err)
	default:
		// Before calling it a server fault: is the runtime there at all? With
		// docker gone qooqd itself is fine and answers 500 for everything,
		// which reads exactly like "your Dockerfile failed" — and the watcher
		// counts those against the repository until it pauses it. 503 says
		// "not now, not your fault". Asked only on the
		// way to a 500, so a working daemon pays nothing for it.
		if perr := h.runtimeDown(); perr != nil {
			h.log.Error("container runtime unavailable", "err", err, "ping", perr)
			writeErr(w, http.StatusServiceUnavailable, fmt.Errorf("the container runtime is not available: %w", perr))
			return
		}
		writeInternalErr(w, h.log, err)
	}
}

// runtimeDown is the same question /v1/healthz asks, with its own short
// bound: a hung runtime must not hold up the error response.
func (h *handlers) runtimeDown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return h.svc.Ping(ctx)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, client.ErrorResponse{Error: err.Error()})
}

// writeBuildErr reports a failed build with its output attached.
//
// Status and message are decided exactly as anywhere else — including the 5xx
// masking, which still applies, because the masking exists to keep server
// internals out of a response and the build log is not a server internal. It
// is the caller's own Dockerfile talking.
func (h *handlers) writeBuildErr(w http.ResponseWriter, err error, log string) {
	if log == "" {
		h.writeComponentErr(w, err)
		return
	}
	rec := &buildErrRecorder{ResponseWriter: w, log: log}
	h.writeComponentErr(rec, err)
}

// buildErrRecorder attaches the build log to whatever error envelope the
// mapper decided to write.
//
// Wrapping rather than duplicating the mapper's status table: a second copy of
// "which error is which status" would be a second thing to keep in step, and
// the one nobody updates is the one that goes wrong.
type buildErrRecorder struct {
	http.ResponseWriter
	log    string
	status int
}

func (r *buildErrRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *buildErrRecorder) Write(p []byte) (int, error) {
	var env client.ErrorResponse
	if err := json.Unmarshal(p, &env); err != nil || env.Error == "" {
		// Not the envelope this expected. Passed through untouched rather than
		// guessed at — a mangled error body would be worse than a missing log.
		return r.ResponseWriter.Write(p)
	}
	env.BuildLog = r.log
	out, err := json.Marshal(env)
	if err != nil {
		return r.ResponseWriter.Write(p)
	}
	out = append(out, '\n')
	return r.ResponseWriter.Write(out)
}

// writeInternalErr logs the real error and returns a generic 500 envelope, so
// unexpected server-side failure details never leak to clients. Only for 5xx
// — 4xx keeps its specific message (the CLI matches on those).
func writeInternalErr(w http.ResponseWriter, log *slog.Logger, err error) {
	if log != nil {
		log.Error("internal error", "err", err)
	}
	writeJSON(w, http.StatusInternalServerError, client.ErrorResponse{Error: "internal error"})
}
