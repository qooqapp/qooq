package watcherclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to a qooq-watcher instance over HTTP.
type Client struct {
	baseURL string
	http    *http.Client
	// streamHTTP carries request bodies that are streams (UploadSource). It
	// has no client-side timeout at all: a large archive on a slow link takes
	// as long as it takes, and the 30s cap on http would cut every such
	// upload off mid-body. The caller's context is the only bound — the same
	// arrangement as pkg/client's streamHTTP for image loads and builds.
	streamHTTP *http.Client
	token      string
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithTimeout overrides the default 30s request timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// WithToken sets the bearer token sent as `Authorization: Bearer <token>`
// with every request (mirrors pkg/client's WithToken for qooqd). Leave
// unset (or pass "") to talk to a qooq-watcher with no http.token/
// QOOQ_WATCHER_HTTP_TOKEN configured.
func WithToken(token string) Option {
	return func(c *Client) { c.token = token }
}

// New returns a Client for the given qooq-watcher base URL (e.g.
// http://127.0.0.1:7778).
func New(baseURL string, opts ...Option) *Client {
	c := &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		http:       &http.Client{Timeout: 30 * time.Second},
		streamHTTP: &http.Client{},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
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
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e ErrorResponse
		data, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Message: e.Error, structured: true}
		}
		return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// CreateTarget registers a new GitOps watch target.
func (c *Client) CreateTarget(ctx context.Context, req CreateTargetRequest) (*TargetResponse, error) {
	var out TargetResponse
	if err := c.do(ctx, http.MethodPost, "/v1/targets", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListTargets returns every watch target.
func (c *Client) ListTargets(ctx context.Context) ([]TargetResponse, error) {
	var out []TargetResponse
	if err := c.do(ctx, http.MethodGet, "/v1/targets", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetTarget returns one watch target by name.
func (c *Client) GetTarget(ctx context.Context, name string) (*TargetResponse, error) {
	var out TargetResponse
	if err := c.do(ctx, http.MethodGet, "/v1/targets/"+name, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteTarget removes a watch target and the deployment it made.
//
// keepComponent leaves the deployment running and removes only the watching.
// It is a parameter rather than a second method, and not defaulted, because
// the behaviour this chooses between used to be decided silently the wrong
// way: deleting a target left a container running and an address answering
// with nothing watching or listing it. A caller should have to say which of
// the two it means.
func (c *Client) DeleteTarget(ctx context.Context, name string, keepComponent bool) error {
	path := "/v1/targets/" + name
	if keepComponent {
		path += "?keep_component=1"
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// DeleteService deletes a service the target's manifest no longer declares,
// with its container and data. The watcher refuses (409) one that is still
// declared.
func (c *Client) DeleteService(ctx context.Context, target, service string) error {
	return c.do(ctx, http.MethodDelete, "/v1/targets/"+target+"/services/"+url.PathEscape(service), nil, nil)
}

// BuildLog returns the most recent build's output for a target.
//
// A string rather than a stream: the daemon already caps it, so there is a
// known-small amount of it, and a caller that wants to show it needs the whole
// thing anyway.
func (c *Client) BuildLog(ctx context.Context, name string) (string, error) {
	body, err := c.doRaw(ctx, http.MethodGet, "/v1/targets/"+name+"/build-log", nil)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// TriggerTarget schedules a watch target for the next sweep tick, bypassing
// its poll interval.
//
// force also rebuilds when the remote has not moved. Without it a trigger only
// means "look now", and looking finds the same commit — fine for "did I miss a
// push?", useless when the commit is right and something around it changed.
func (c *Client) TriggerTarget(ctx context.Context, name string, force bool) error {
	path := "/v1/targets/" + name + "/trigger"
	if force {
		path += "?force=1"
	}
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// StopTarget and StartTarget act on what is running, leaving the target, its
// settings and its images where they are (POST /v1/targets/{name}/stop|start).
//
// Separate from PauseTarget below, which stops the watching rather than the
// process. A project that is stopped but still watched comes back the next
// time somebody pushes — a deploy starts what it deploys — and a caller that
// treats these as one verb will tell somebody the wrong thing about why their
// app is back up.
func (c *Client) StopTarget(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/v1/targets/"+name+"/stop", nil, nil)
}

func (c *Client) StartTarget(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/v1/targets/"+name+"/start", nil, nil)
}

// RollbackTarget puts the previous version back (POST
// /v1/targets/{name}/rollback). 409: nothing to go back to; 423: a deploy is
// in progress. Calling it twice goes forward again.
func (c *Client) RollbackTarget(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/v1/targets/"+name+"/rollback", nil, nil)
}

// PauseTarget and ResumeTarget stop and restart automatic deploys. What is
// running keeps running; an explicit TriggerTarget still builds.
func (c *Client) PauseTarget(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/v1/targets/"+name+"/pause", nil, nil)
}

func (c *Client) ResumeTarget(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/v1/targets/"+name+"/resume", nil, nil)
}

// SetBranch switches an existing target to another branch and rebuilds it
// from that branch's HEAD right away (PUT /v1/targets/{name}). The component
// is kept — same name, same address — only what it is built from changes.
// Stack-owned targets are refused: their branch comes from the stack manifest.
func (c *Client) SetBranch(ctx context.Context, name, branch string) (*TargetResponse, error) {
	var out TargetResponse
	if err := c.do(ctx, http.MethodPut, "/v1/targets/"+name, UpdateTargetRequest{Branch: &branch}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UseAppAuth moves an unauthenticated target to GitHub App auth
// (PUT /v1/targets/{name} with auth_type=github-app). A no-op for a target
// already on it; refused for any other auth type.
func (c *Client) UseAppAuth(ctx context.Context, name string) (*TargetResponse, error) {
	authType := "github-app"
	var out TargetResponse
	if err := c.do(ctx, http.MethodPut, "/v1/targets/"+name, UpdateTargetRequest{AuthType: &authType}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetDomain assigns the target's hostname (PUT /v1/targets/{name} with domain)
// and rebuilds it so the address takes effect. An empty domain clears it.
// Only the domain changes; the rest of the deploy_spec is kept as it was.
func (c *Client) SetDomain(ctx context.Context, name, domain string) (*TargetResponse, error) {
	var out TargetResponse
	if err := c.do(ctx, http.MethodPut, "/v1/targets/"+name, UpdateTargetRequest{Domain: &domain}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeployImage (M20b) tells the watcher to deploy image on the named target
// right now, bypassing the poll/build pipeline entirely — typically called by
// a CI pipeline that just built and pushed image. Returns the target's
// updated state.
func (c *Client) DeployImage(ctx context.Context, name, image string) (*TargetResponse, error) {
	var out TargetResponse
	req := DeployTargetRequest{Image: image}
	if err := c.do(ctx, http.MethodPost, "/v1/targets/"+name+"/deploy", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Backup streams a consistent snapshot of the watcher's sqlite database to w
// (GET /v1/backup). The snapshot contains credentials as stored — protect the
// output file accordingly.
func (c *Client) Backup(ctx context.Context, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/backup", nil)
	if err != nil {
		return err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("GET /v1/backup: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e ErrorResponse
		data, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("GET /v1/backup: %s (%d)", e.Error, resp.StatusCode)
		}
		return fmt.Errorf("GET /v1/backup: status %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("read backup stream: %w", err)
	}
	return nil
}

// GetManifest returns the manifest qooq generated for a target whose repo has
// no qooq.yaml, as raw YAML. Errors when the target has one of its own or has
// not been built yet.
func (c *Client) GetManifest(ctx context.Context, name string) ([]byte, error) {
	return c.doRaw(ctx, http.MethodGet, "/v1/targets/"+name+"/manifest", nil)
}

// PutManifest replaces a target's generated manifest. The watcher validates it
// before storing, and never regenerates over a stored copy — so a correction
// made here survives every later sweep.
func (c *Client) PutManifest(ctx context.Context, name string, yaml []byte) ([]byte, error) {
	return c.doRaw(ctx, http.MethodPut, "/v1/targets/"+name+"/manifest", yaml)
}

// SetInputs supplies values for the inputs a target's manifest declares.
// Values merge into what is already stored (an empty value clears one), and
// setting any of them triggers an immediate redeploy — the point of supplying
// a value is to unblock a deployment that is stuck now.
func (c *Client) SetInputs(ctx context.Context, name string, values map[string]string) (*TargetResponse, error) {
	var out TargetResponse
	if err := c.do(ctx, http.MethodPut, "/v1/targets/"+name+"/inputs", SetInputsRequest{Values: values}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// CreateStack registers a manifest repository as a stack: several projects
// deployed together, in the order the manifest declares.
func (c *Client) CreateStack(ctx context.Context, req CreateStackRequest) (*StackResponse, error) {
	var out StackResponse
	if err := c.do(ctx, http.MethodPost, "/v1/stacks", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListStacks(ctx context.Context) ([]StackResponse, error) {
	var out []StackResponse
	if err := c.do(ctx, http.MethodGet, "/v1/stacks", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetStack returns the stack with its projects expanded.
func (c *Client) GetStack(ctx context.Context, name string) (*StackResponse, error) {
	var out StackResponse
	if err := c.do(ctx, http.MethodGet, "/v1/stacks/"+name, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteStack removes the stack and its projects' watch targets. With prune it also
// removes the deployed components; without it they keep running.
func (c *Client) DeleteStack(ctx context.Context, name string, prune bool) error {
	path := "/v1/stacks/" + name
	if prune {
		path += "?prune=true"
	}
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

func (c *Client) TriggerStack(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodPost, "/v1/stacks/"+name+"/trigger", nil, nil)
}

// ResumeStack lifts a pause and gives every project a fresh failure budget.
func (c *Client) ResumeStack(ctx context.Context, name string) (*StackResponse, error) {
	var out StackResponse
	if err := c.do(ctx, http.MethodPost, "/v1/stacks/"+name+"/resume", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StackManifest returns the manifest the watcher is working from — the last one
// that parsed, which is not necessarily what is on the branch right now.
func (c *Client) StackManifest(ctx context.Context, name string) ([]byte, error) {
	return c.doRaw(ctx, http.MethodGet, "/v1/stacks/"+name+"/manifest", nil)
}

// doRaw performs a request whose body and response are opaque bytes (YAML
// here) rather than the JSON envelope do() speaks. Errors still arrive as that
// envelope, so they are unwrapped the same way.
func (c *Client) doRaw(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/yaml")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e ErrorResponse
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Message: e.Error, structured: true}
		}
		return nil, fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

// UploadSource sends a gzip'd tar of a source tree to an upload target
// (PUT /v1/targets/{name}/source) and queues a build of it. body is streamed,
// never buffered; bound it with ctx. force builds even when the archive is
// identical to what is already deployed.
//
// The response's BuildGeneration is the baseline to wait above, and
// BuildQueued false means nothing will be built (same content as deployed).
func (c *Client) UploadSource(ctx context.Context, name string, body io.Reader, force bool) (*UploadSourceResponse, error) {
	path := "/v1/targets/" + url.PathEscape(name) + "/source"
	if force {
		path += "?force=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/gzip")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e ErrorResponse
		data, _ := io.ReadAll(resp.Body)
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, &APIError{Method: http.MethodPut, Path: path, StatusCode: resp.StatusCode, Message: e.Error, structured: true}
		}
		return nil, &APIError{Method: http.MethodPut, Path: path, StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	var out UploadSourceResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("PUT %s: decode response: %w", path, err)
	}
	return &out, nil
}

// AppLogs streams what the target's running app writes to stdout/stderr
// (GET /v1/targets/{name}/logs). tail limits it to the last n lines (0: the
// daemon's default); follow keeps the stream open for new lines until ctx ends.
// The caller closes the reader. 409: nothing deployed yet.
//
// A stream, on the client with no total timeout: a followed log is open for
// as long as somebody is reading it.
func (c *Client) AppLogs(ctx context.Context, name string, tail int, follow bool) (io.ReadCloser, error) {
	q := url.Values{}
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	if follow {
		q.Set("follow", "1")
	}
	path := "/v1/targets/" + url.PathEscape(name) + "/logs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var e ErrorResponse
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return nil, &APIError{Method: http.MethodGet, Path: path, StatusCode: resp.StatusCode, Message: e.Error, structured: true}
		}
		return nil, &APIError{Method: http.MethodGet, Path: path, StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	return resp.Body, nil
}

// DiskUsageResponse is GET /v1/usage/disk: bytes of service data per
// namespace, as last measured. MeasuredAt is nil until the first measurement.
type DiskUsageResponse struct {
	Namespaces map[string]int64 `json:"namespaces"`
	MeasuredAt *time.Time       `json:"measured_at,omitempty"`
}

// DiskUsage returns the watcher's last data measurement.
func (c *Client) DiskUsage(ctx context.Context) (*DiskUsageResponse, error) {
	var out DiskUsageResponse
	if err := c.do(ctx, http.MethodGet, "/v1/usage/disk", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ComponentUsage is one component on GET /v1/usage/components: what it uses
// now and when it was last deployed. CPUPercent and MemUsage are nil when
// qooqd had no sample (not running, or it did not answer in time); DataBytes
// is nil until the first data measurement.
type ComponentUsage struct {
	Namespace string `json:"namespace"`
	Component string `json:"component"`
	// Target is the target the component belongs to, empty when none does;
	// Service says it is a database or cache that target's build started.
	Target       string     `json:"target,omitempty"`
	Service      bool       `json:"service,omitempty"`
	Phase        string     `json:"phase"`
	DesiredState string     `json:"desired_state,omitempty"`
	CPUPercent   *float64   `json:"cpu_percent,omitempty"`
	CPULimit     float64    `json:"cpu_limit,omitempty"`
	MemUsage     *uint64    `json:"mem_usage,omitempty"`
	MemLimit     int64      `json:"mem_limit,omitempty"`
	DataBytes    *int64     `json:"data_bytes,omitempty"`
	StartedAt    *time.Time `json:"started_at,omitempty"`
	LastDeployAt *time.Time `json:"last_deploy_at,omitempty"`
}

// ComponentUsageResponse is GET /v1/usage/components.
type ComponentUsageResponse struct {
	Components     []ComponentUsage `json:"components"`
	DataMeasuredAt *time.Time       `json:"data_measured_at,omitempty"`
}

// ComponentUsage returns every component's current usage.
func (c *Client) ComponentUsage(ctx context.Context) (*ComponentUsageResponse, error) {
	var out ComponentUsageResponse
	if err := c.do(ctx, http.MethodGet, "/v1/usage/components", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetQuotaHold holds (non-empty reason) or releases (empty) a target over its
// account's plan limit. Idempotent.
func (c *Client) SetQuotaHold(ctx context.Context, name, reason string) error {
	return c.do(ctx, http.MethodPut, "/v1/targets/"+url.PathEscape(name)+"/quota-hold", map[string]string{"reason": reason}, nil)
}

// PutSecret stores (or replaces) a secret in a namespace's store, where
// ${secret.NAME} in that namespace's manifests resolves it. The value is never
// returned by any endpoint afterwards.
func (c *Client) PutSecret(ctx context.Context, namespace, name, value string) error {
	return c.do(ctx, http.MethodPut, "/v1/namespaces/"+url.PathEscape(namespace)+"/secrets/"+url.PathEscape(name),
		map[string]string{"value": value}, nil)
}

// ListSecrets returns the names (only) of the secrets a namespace holds.
func (c *Client) ListSecrets(ctx context.Context, namespace string) ([]string, error) {
	var out struct {
		Names []string `json:"names"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/namespaces/"+url.PathEscape(namespace)+"/secrets", nil, &out); err != nil {
		return nil, err
	}
	return out.Names, nil
}

// DeleteSecret removes one secret from a namespace's store.
func (c *Client) DeleteSecret(ctx context.Context, namespace, name string) error {
	return c.do(ctx, http.MethodDelete, "/v1/namespaces/"+url.PathEscape(namespace)+"/secrets/"+url.PathEscape(name), nil, nil)
}
