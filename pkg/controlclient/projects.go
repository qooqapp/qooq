package controlclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Project mirrors the external manager's project object, limited to what a CLI
// needs. The JSON tags must match the external manager's Project object (the external manager is not in this repo); a
// field this struct does not name is simply dropped on decode.
type Project struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	RepoURL   string `json:"repo_url"`
	Branch    string `json:"branch"`

	Source string `json:"source"`
	// UploadRevision and LastUploadAt: set for source "upload".
	UploadRevision  string     `json:"upload_revision,omitempty"`
	LastUploadAt    *time.Time `json:"last_upload_at,omitempty"`
	ImageRef        string     `json:"image_ref,omitempty"`
	LastImageDigest string     `json:"last_image_digest,omitempty"`

	LastBuildStatus   string `json:"last_build_status"`
	LastBuildError    string `json:"last_build_error,omitempty"`
	LastBuildRevision string `json:"last_build_revision,omitempty"`
	DeployedRevision  string `json:"deployed_revision,omitempty"`
	// RunningVersion is the tag that is running; PreviousVersion what
	// RollbackProject would put back, empty when there is none.
	RunningVersion  string     `json:"running_version,omitempty"`
	PreviousVersion string     `json:"previous_version,omitempty"`
	BuildGeneration uint64     `json:"build_generation,omitempty"`
	BuildStartedAt  *time.Time `json:"build_started_at,omitempty"`
	LastBuildAt     *time.Time `json:"last_build_at,omitempty"`

	// URL is the address the platform gave the service; it answers once
	// Endpoints lists it. Empty when the server hands out none.
	URL string `json:"url,omitempty"`

	Endpoints  []string         `json:"endpoints,omitempty"`
	Services   []ProjectService `json:"services,omitempty"`
	References []string         `json:"references,omitempty"`
	Phase      string           `json:"phase,omitempty"`
	PhaseError string           `json:"phase_error,omitempty"`

	DeployPaused bool `json:"deploy_paused,omitempty"`
	Stopped      bool `json:"stopped,omitempty"`
	Paused       bool `json:"paused,omitempty"`

	// Inputs are what qooq.yaml's inputs: declares; MissingInputs the
	// required ones with no value yet (status needs_input).
	Inputs        []ProjectInput `json:"inputs,omitempty"`
	MissingInputs []string       `json:"missing_inputs,omitempty"`
}

// ProjectInput is one declared input. A secret's value is never returned.
type ProjectInput struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Example     string `json:"example,omitempty"`
	Default     string `json:"default,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Set         bool   `json:"set"`
	Value       string `json:"value,omitempty"`
}

// ProjectService is a database or cache qooq started for a project.
type ProjectService struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	Type      string `json:"type"`
	Version   string `json:"version"`
	Phase     string `json:"phase"`
	// Dropped: qooq.yaml no longer declares it. Its container is stopped and
	// its data kept until it is deleted from the external manager or the project is
	// removed.
	Dropped bool `json:"dropped,omitempty"`
}

// CreateProjectRequest is the body of POST /accounts/{slug}/projects. Source
// is "git" (default) or "image"; with "image" only ImageRef, RegistryAuth and
// ContainerPort apply.
type CreateProjectRequest struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	RepoURL   string `json:"repo_url,omitempty"`
	Branch    string `json:"branch,omitempty"`
	AuthToken string `json:"auth_token,omitempty"`

	Source        string `json:"source,omitempty"`
	ImageRef      string `json:"image_ref,omitempty"`
	RegistryAuth  string `json:"registry_auth,omitempty"`
	ContainerPort int    `json:"container_port,omitempty"`
}

func projectsPath(account string) string {
	return "/apis/v1/accounts/" + url.PathEscape(account) + "/projects"
}

func projectPath(account, ns, name string) string {
	return "/apis/v1/accounts/" + url.PathEscape(account) + "/namespaces/" +
		url.PathEscape(ns) + "/projects/" + url.PathEscape(name)
}

// ListProjects returns every project of the account.
func (c *Client) ListProjects(ctx context.Context, account string) ([]Project, error) {
	var out struct {
		Projects []Project `json:"projects"`
	}
	if err := c.do(ctx, http.MethodGet, projectsPath(account), true, nil, &out); err != nil {
		return nil, err
	}
	return out.Projects, nil
}

// CreateProject connects a repository or image.
func (c *Client) CreateProject(ctx context.Context, account string, req CreateProjectRequest) (*Project, error) {
	var out Project
	if err := c.do(ctx, http.MethodPost, projectsPath(account), true, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeployProject queues a redeploy (202).
func (c *Client) DeployProject(ctx context.Context, account, ns, name string) error {
	return c.do(ctx, http.MethodPost, projectPath(account, ns, name)+"/deploy", true, nil, nil)
}

// DeployProjectImage deploys the given image now — how a CI pipeline picks the
// version. Unlike DeployProject it is synchronous: the answer is the project
// after the deploy.
func (c *Client) DeployProjectImage(ctx context.Context, account, ns, name, image string) (*Project, error) {
	var out Project
	in := map[string]string{"image": image}
	if err := c.do(ctx, http.MethodPost, projectPath(account, ns, name)+"/deploy", true, in, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// SetProjectInputs supplies values for the inputs the project declares, and
// redeploys it. An empty value clears one.
func (c *Client) SetProjectInputs(ctx context.Context, account, ns, name string, values map[string]string) (*Project, error) {
	var out Project
	if err := c.do(ctx, http.MethodPut, projectPath(account, ns, name)+"/inputs", true, map[string]any{"values": values}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StopProject stops the running service; the project and its settings stay,
// and automatic deploys are untouched (a push still deploys it again — pause
// for that). StartProject starts it again.
func (c *Client) StopProject(ctx context.Context, account, ns, name string) error {
	return c.do(ctx, http.MethodPost, projectPath(account, ns, name)+"/stop", true, nil, nil)
}

func (c *Client) StartProject(ctx context.Context, account, ns, name string) error {
	return c.do(ctx, http.MethodPost, projectPath(account, ns, name)+"/start", true, nil, nil)
}

// PauseProject stops automatic deploys; what runs keeps running.
// ResumeProject turns them back on.
func (c *Client) PauseProject(ctx context.Context, account, ns, name string) error {
	return c.do(ctx, http.MethodPost, projectPath(account, ns, name)+"/pause", true, nil, nil)
}

func (c *Client) ResumeProject(ctx context.Context, account, ns, name string) error {
	return c.do(ctx, http.MethodPost, projectPath(account, ns, name)+"/resume", true, nil, nil)
}

// RollbackResult says which version a rollback went from and to.
type RollbackResult struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// RollbackProject puts the version before the running one back. A second call
// goes forward again. 409 nothing_to_roll_back / deploy_in_progress.
func (c *Client) RollbackProject(ctx context.Context, account, ns, name string) (*RollbackResult, error) {
	var out RollbackResult
	if err := c.do(ctx, http.MethodPost, projectPath(account, ns, name)+"/rollback", true, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// BuildLog returns the last build's output as plain text.
func (c *Client) BuildLog(ctx context.Context, account, ns, name string) (string, error) {
	var out string
	if err := c.do(ctx, http.MethodGet, projectPath(account, ns, name)+"/build-log", true, nil, &out); err != nil {
		return "", err
	}
	return out, nil
}

// DeleteProject removes the project and its running service.
func (c *Client) DeleteProject(ctx context.Context, account, ns, name string) error {
	return c.do(ctx, http.MethodDelete, projectPath(account, ns, name), true, nil, nil)
}

// UploadSourceResult is the 202 answer to an upload. BuildGeneration is the
// baseline to wait above; BuildQueued false means the content is identical to
// what is deployed and nothing will be built.
type UploadSourceResult struct {
	Revision        string `json:"revision"`
	BuildQueued     bool   `json:"build_queued"`
	BuildGeneration uint64 `json:"build_generation"`
	Created         bool   `json:"created,omitempty"`
}

// UploadProjectSource sends a gzip'd tar of a source tree to the project,
// creating the project if it does not exist (PUT .../source). body is
// streamed; bound it with ctx — there is deliberately no total timeout. size
// is the body length when known (it lets the server refuse an oversized upload
// before reading it, and keeps the request out of chunked encoding), or -1.
func (c *Client) UploadProjectSource(ctx context.Context, account, ns, name string, body io.Reader, size int64, force bool) (*UploadSourceResult, error) {
	path := projectPath(account, ns, name) + "/source"
	if force {
		path += "?force=1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+path, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Content-Type", "application/gzip")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("control PUT %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Method: http.MethodPut, Path: path, StatusCode: resp.StatusCode, Message: messageFrom(raw, resp.Status)}
	}
	var out UploadSourceResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("control PUT %s: response is not JSON: %w", path, err)
	}
	return &out, nil
}

// AppLogs streams what the project's running app prints (GET .../logs): the
// last tail lines (0: the server's default), and with follow, new lines until
// ctx ends. The caller closes the reader. Not the build output — that is
// BuildLog.
func (c *Client) AppLogs(ctx context.Context, account, ns, name string, tail int, follow bool) (io.ReadCloser, error) {
	q := url.Values{}
	if tail > 0 {
		q.Set("tail", strconv.Itoa(tail))
	}
	if follow {
		q.Set("follow", "1")
	}
	path := projectPath(account, ns, name) + "/logs"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	// The streaming client: a followed log stays open as long as somebody
	// reads it, and the ordinary client's total timeout would cut it off.
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, fmt.Errorf("control GET %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, &APIError{Method: http.MethodGet, Path: path, StatusCode: resp.StatusCode, Message: messageFrom(raw, resp.Status)}
	}
	return resp.Body, nil
}
