package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to a qooqd instance over HTTP.
type Client struct {
	baseURL string
	// http is used by do (plain JSON request/response calls); its timeout
	// defaults to 120s and is overridable via WithTimeout.
	http *http.Client
	// streamHTTP is used by doStream (LoadImage/BuildImage): these can run
	// arbitrarily long (a big tarball, a slow build), so it has no
	// client-side timeout at all — the caller's context is the only bound.
	streamHTTP *http.Client
	// streamTimeout is a safety cap for LoadImage/BuildImage applied *only*
	// when the caller's context carries no deadline of its own, so a hung
	// build can't pin a goroutine forever. It never shortens a deadline the
	// caller already set. 0 disables it (fully unbounded, the old behavior).
	streamTimeout time.Duration
	token         string
}

// defaultStreamTimeout caps LoadImage/BuildImage when the caller passes an
// unbounded context. Generous, because image builds legitimately take minutes.
const defaultStreamTimeout = 30 * time.Minute

// Option configures a Client at construction time.
type Option func(*Client)

// WithToken sets the bearer token sent as `Authorization: Bearer <token>`
// with every request (both do and doStream). Leave unset (or pass "") to
// talk to a qooqd with no -token/QOOQ_TOKEN configured.
func WithToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// WithTimeout overrides the default 120s timeout used for non-streaming
// requests (Health, Deploy, Start/Stop, etc). It does not affect
// LoadImage/BuildImage; use WithStreamTimeout for those.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// WithStreamTimeout overrides the default safety cap (30m) applied to
// LoadImage/BuildImage when the caller's context has no deadline. Pass 0 to
// disable the cap entirely (fully unbounded). A deadline the caller sets on
// the context always wins over this cap.
func WithStreamTimeout(d time.Duration) Option {
	return func(c *Client) { c.streamTimeout = d }
}

// New returns a Client for the given base URL (e.g. http://127.0.0.1:7777).
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		http:          &http.Client{Timeout: 120 * time.Second},
		streamHTTP:    &http.Client{},
		streamTimeout: defaultStreamTimeout,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// setAuth attaches the bearer token header, if one is configured.
func (c *Client) setAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

// do performs a request, JSON-encoding body (if non-nil) and decoding out (if
// non-nil). Non-2xx responses are turned into an error using ErrorResponse.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.setAuth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errFromResponse(resp, method, path)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// APIError is a non-2xx response. It carries the status code so a caller can
// tell "this does not exist" from "this failed", which several of them need to
// do — and which they previously could only attempt by matching on the message
// text, a check that silently stops working the first time the wording moves.
//
// The message is byte-for-byte what this package produced before the type
// existed, so anything printing the error is unaffected.
// BuildLogOf returns the build output carried by a failed build's error, or ""
// when there is none.
//
// A helper rather than a field access so callers do not have to know the error
// is an *APIError, and so wrapping stays free.
func BuildLogOf(err error) string {
	var e *APIError
	if errors.As(err, &e) {
		return e.BuildLog
	}
	return ""
}

type APIError struct {
	Method     string
	Path       string
	StatusCode int
	// Message is the daemon's own error text when it sent the structured
	// envelope, and the raw body otherwise.
	Message string
	// structured records which of the two forms the daemon sent, because the
	// two render differently and the rendering is what callers already print.
	structured bool
	// BuildLog is a failed build's output, when the endpoint was the build one.
	// Deliberately NOT part of Error(): it is pages long, and every existing
	// caller prints that string.
	BuildLog string
}

func (e *APIError) Error() string {
	if e.structured {
		return fmt.Sprintf("%s %s: %s (%d)", e.Method, e.Path, e.Message, e.StatusCode)
	}
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
}

// StatusCode reports the HTTP status behind err, or 0 if err did not come from
// a response. Use it instead of reading the message: IsNotFound and friends are
// built on it.
func StatusCode(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// IsNotFound reports whether err is the daemon saying the thing is not there.
func IsNotFound(err error) bool { return StatusCode(err) == http.StatusNotFound }

// IsConflict reports whether err is the daemon refusing because of the state
// something is in — a name already taken, an image still in use.
func IsConflict(err error) bool { return StatusCode(err) == http.StatusConflict }

// errFromResponse turns a non-2xx response into an *APIError, preferring the
// structured ErrorResponse envelope when present.
func errFromResponse(resp *http.Response, method, path string) error {
	var e ErrorResponse
	data, _ := io.ReadAll(resp.Body)
	if json.Unmarshal(data, &e) == nil && e.Error != "" {
		return &APIError{
			Method: method, Path: path, StatusCode: resp.StatusCode,
			Message: e.Error, structured: true,
			BuildLog: e.BuildLog,
		}
	}
	return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
}

// streamDeadline applies c.streamTimeout as a safety cap, but only when the
// caller's ctx has no deadline of its own — a caller-set deadline always wins.
// The returned cancel must be deferred by the caller.
func (c *Client) streamDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.streamTimeout <= 0 {
		return ctx, func() {}
	}
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, c.streamTimeout)
}

// doStream performs a streaming-body request (the request body is passed
// through to net/http as-is, never buffered) and decodes a JSON response.
func (c *Client) doStream(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-tar")
	c.setAuth(req)
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return errFromResponse(resp, method, path)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var out HealthResponse
	if err := c.do(ctx, http.MethodGet, "/v1/healthz", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Version(ctx context.Context) (*VersionResponse, error) {
	var out VersionResponse
	if err := c.do(ctx, http.MethodGet, "/v1/version", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Capacity returns the host's resource capacity (cpu cores, total memory,
// arch, os) — used by watcher's M14 requires validation.
func (c *Client) Capacity(ctx context.Context) (*CapacityResponse, error) {
	var out CapacityResponse
	if err := c.do(ctx, http.MethodGet, "/v1/system/capacity", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Backup streams a consistent snapshot of the daemon's datastore to w
// (GET /v1/system/backup). It uses streamHTTP (no fixed client timeout) since
// a large store can take a while; the caller's ctx is the bound.
func (c *Client) Backup(ctx context.Context, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/system/backup", nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("GET /v1/system/backup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errFromResponse(resp, http.MethodGet, "/v1/system/backup")
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("read backup stream: %w", err)
	}
	return nil
}

func (c *Client) Deploy(ctx context.Context, req DeployRequest) (*ComponentResponse, error) {
	var out ComponentResponse
	if err := c.do(ctx, http.MethodPost, "/v1/components", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Upsert creates a component (name not yet in use) or replaces an existing
// one's container in place (same id, new image/spec) via PUT /v1/components.
func (c *Client) Upsert(ctx context.Context, req DeployRequest) (*ComponentResponse, error) {
	var out ComponentResponse
	if err := c.do(ctx, http.MethodPut, "/v1/components", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PhaseReserved is the phase of a component that has an address reserved
// (Reserve) but has not been deployed yet.
const PhaseReserved = "Reserved"

// Reserve sets aside the address req would get, before it is deployed: its
// auto host ports are assigned now and kept for it. A name that is already
// deployed is answered with that component. Image is not needed.
func (c *Client) Reserve(ctx context.Context, req DeployRequest) (*ComponentResponse, error) {
	var out ComponentResponse
	if err := c.do(ctx, http.MethodPut, "/v1/reservations", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListReservations returns the reservations not yet used by a deploy, each in
// the shape of the component it will become (phase PhaseReserved).
func (c *Client) ListReservations(ctx context.Context) ([]ComponentResponse, error) {
	var out []ComponentResponse
	if err := c.do(ctx, http.MethodGet, "/v1/reservations", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ReleaseReservation drops a reservation; one that does not exist is not an error.
func (c *Client) ReleaseReservation(ctx context.Context, namespace, name string) error {
	q := url.Values{"namespace": {namespace}, "name": {name}}
	return c.do(ctx, http.MethodDelete, "/v1/reservations?"+q.Encode(), nil, nil)
}

func (c *Client) ListComponents(ctx context.Context) ([]ComponentResponse, error) {
	var out []ComponentResponse
	if err := c.do(ctx, http.MethodGet, "/v1/components", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) GetComponent(ctx context.Context, id string) (*ComponentResponse, error) {
	var out ComponentResponse
	if err := c.do(ctx, http.MethodGet, "/v1/components/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) StartComponent(ctx context.Context, id string) (*ComponentResponse, error) {
	var out ComponentResponse
	if err := c.do(ctx, http.MethodPost, "/v1/components/"+id+"/start", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) StopComponent(ctx context.Context, id string) (*ComponentResponse, error) {
	var out ComponentResponse
	if err := c.do(ctx, http.MethodPost, "/v1/components/"+id+"/stop", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RollbackComponent swaps a component's image back to its previously
// recorded PreviousImage via POST /v1/components/{id}/rollback. Calling it
// twice in a row round-trips: it undoes the previous rollback.
func (c *Client) RollbackComponent(ctx context.Context, id string) (*ComponentResponse, error) {
	var out ComponentResponse
	if err := c.do(ctx, http.MethodPost, "/v1/components/"+id+"/rollback", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Logs returns a stream of a component's container output via
// GET /v1/components/{id}/logs. The caller must Close the returned
// ReadCloser. tail<=0 requests full output; follow keeps the stream open
// (the request uses streamHTTP, which has no client-side timeout — the
// caller's ctx is the only bound). Unlike LoadImage/BuildImage this has no
// default cap: a --follow stream is meant to stay open indefinitely.
func (c *Client) Logs(ctx context.Context, id string, follow bool, tail int, timestamps bool) (io.ReadCloser, error) {
	q := url.Values{}
	if follow {
		q.Set("follow", "1")
	}
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	if timestamps {
		q.Set("timestamps", "1")
	}
	path := "/v1/components/" + id + "/logs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	c.setAuth(req)
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", http.MethodGet, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, errFromResponse(resp, http.MethodGet, path)
	}
	return resp.Body, nil
}

func (c *Client) RemoveComponent(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/components/"+id, nil, nil)
}

func (c *Client) PullImage(ctx context.Context, req PullRequest) (*PullResponse, error) {
	var out PullResponse
	if err := c.do(ctx, http.MethodPost, "/v1/images/pull", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PullImageAuth pulls ref, authenticating with username/password if given
// (both empty = anonymous, same as PullImage).
func (c *Client) PullImageAuth(ctx context.Context, ref, username, password string) error {
	_, err := c.PullImage(ctx, PullRequest{Ref: ref, Username: username, Password: password})
	return err
}

// ImageDigest queries the registry for ref's current content digest (no
// pull) via POST /v1/images/digest — used by watcher's M20a image-mode
// targets to detect upstream changes without downloading the image.
func (c *Client) ImageDigest(ctx context.Context, ref, username, password string) (string, error) {
	var out ImageDigestResponse
	req := ImageDigestRequest{Ref: ref, Username: username, Password: password}
	if err := c.do(ctx, http.MethodPost, "/v1/images/digest", req, &out); err != nil {
		return "", err
	}
	return out.Digest, nil
}

// LoadImage uploads a `docker save` tarball (streamed, never buffered fully
// in memory) and returns the ids of the images it loaded.
func (c *Client) LoadImage(ctx context.Context, tarball io.Reader) (*LoadResponse, error) {
	ctx, cancel := c.streamDeadline(ctx)
	defer cancel()
	var out LoadResponse
	if err := c.doStream(ctx, http.MethodPost, "/v1/images/load", tarball, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BuildImage uploads a build context tarball (streamed) and builds an image
// from it. dockerfile defaults to "Dockerfile" server-side when empty; tag is
// optional.
// BuildImage builds with a single tag. Kept as-is so existing callers — the
// CLI among them — need no change.
func (c *Client) BuildImage(ctx context.Context, buildCtx io.Reader, dockerfile, tag string) (*BuildResponse, error) {
	var tags []string
	if tag != "" {
		tags = []string{tag}
	}
	return c.BuildImageTags(ctx, buildCtx, dockerfile, tags)
}

// BuildImageTags builds and applies every tag in one pass.
//
// One build producing both the local name and the registry name is what lets
// registry mode avoid a separate tag round trip — and, more importantly, avoids
// the window where the two names could disagree about which image they mean.
func (c *Client) BuildImageTags(ctx context.Context, buildCtx io.Reader, dockerfile string, tags []string) (*BuildResponse, error) {
	ctx, cancel := c.streamDeadline(ctx)
	defer cancel()
	q := url.Values{}
	if dockerfile != "" {
		q.Set("dockerfile", dockerfile)
	}
	for _, tag := range tags {
		if tag != "" {
			q.Add("tag", tag)
		}
	}
	path := "/v1/images/build"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out BuildResponse
	if err := c.doStream(ctx, http.MethodPost, path, buildCtx, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListImages returns every image known to the runtime.
func (c *Client) ListImages(ctx context.Context) ([]ImageResponse, error) {
	var out []ImageResponse
	if err := c.do(ctx, http.MethodGet, "/v1/images", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// RemoveImage deletes a locally available image by ref/tag/id via
// DELETE /v1/images?ref=<ref>. ref is passed as a query param (not a path
// segment) so tags containing "/" and ":" round-trip without colliding with
// routing.
func (c *Client) RemoveImage(ctx context.Context, ref string) error {
	q := url.Values{}
	q.Set("ref", ref)
	return c.do(ctx, http.MethodDelete, "/v1/images?"+q.Encode(), nil, nil)
}

// Stats returns the latest resource-usage sample for a component.
func (c *Client) Stats(ctx context.Context, id string) (*StatsResponse, error) {
	var out StatsResponse
	if err := c.do(ctx, http.MethodGet, "/v1/components/"+id+"/stats", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WatchStats opens the SSE stats stream (GET .../stats?watch=1) and invokes fn
// for each sample until the stream ends, fn returns an error, or ctx is
// cancelled (returning ctx.Err()). Uses streamHTTP so it is not bounded by a
// client-side timeout.
func (c *Client) WatchStats(ctx context.Context, id string, fn func(StatsResponse) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/components/"+id+"/stats?watch=1", nil)
	if err != nil {
		return err
	}
	c.setAuth(req)
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("GET /v1/components/%s/stats?watch=1: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errFromResponse(resp, http.MethodGet, "/v1/components/"+id+"/stats")
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue // SSE framing lines (blank separators, comments)
		}
		var s StatsResponse
		if err := json.Unmarshal([]byte(data), &s); err != nil {
			continue // skip a malformed event rather than abort the stream
		}
		if err := fn(s); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Events returns the persisted event log for a component (oldest first).
func (c *Client) Events(ctx context.Context, id string) ([]EventResponse, error) {
	var out []EventResponse
	if err := c.do(ctx, http.MethodGet, "/v1/components/"+id+"/events", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// CreateGroup creates a logical group.
func (c *Client) CreateGroup(ctx context.Context, req CreateGroupRequest) (*GroupResponse, error) {
	var out GroupResponse
	if err := c.do(ctx, http.MethodPost, "/v1/groups", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListGroups returns all groups.
func (c *Client) ListGroups(ctx context.Context) ([]GroupResponse, error) {
	var out []GroupResponse
	if err := c.do(ctx, http.MethodGet, "/v1/groups", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetGroup returns a group with its current members.
func (c *Client) GetGroup(ctx context.Context, id string) (*GroupDetailResponse, error) {
	var out GroupDetailResponse
	if err := c.do(ctx, http.MethodGet, "/v1/groups/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteGroup deletes a group; cascade also removes its member components.
func (c *Client) DeleteGroup(ctx context.Context, id string, cascade bool) error {
	path := "/v1/groups/" + id
	if cascade {
		path += "?cascade=true"
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// StartGroup starts every member of a group.
func (c *Client) StartGroup(ctx context.Context, id string) ([]BatchResultResponse, error) {
	var out []BatchResultResponse
	if err := c.do(ctx, http.MethodPost, "/v1/groups/"+id+"/start", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// StopGroup stops every member of a group.
func (c *Client) StopGroup(ctx context.Context, id string) ([]BatchResultResponse, error) {
	var out []BatchResultResponse
	if err := c.do(ctx, http.MethodPost, "/v1/groups/"+id+"/stop", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// AddMember assigns a component to a group.
func (c *Client) AddMember(ctx context.Context, groupID string, req AddMemberRequest) (*ComponentResponse, error) {
	var out ComponentResponse
	if err := c.do(ctx, http.MethodPost, "/v1/groups/"+groupID+"/members", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveMember clears a component's group membership.
func (c *Client) RemoveMember(ctx context.Context, groupID, componentID string) (*ComponentResponse, error) {
	var out ComponentResponse
	if err := c.do(ctx, http.MethodDelete, "/v1/groups/"+groupID+"/members/"+componentID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PushImage uploads a locally built image to the registry its ref names.
//
// Needed the moment the daemon that runs an image is not the one that built it
// (multi-node setups). Until then a build already lands where it will run, and
// this would only add a disk copy and a round trip.
func (c *Client) PushImage(ctx context.Context, ref, username, password string) error {
	return c.do(ctx, http.MethodPost, "/v1/images/push",
		PushRequest{Ref: ref, Username: username, Password: password}, nil)
}
