package watcherclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// This package is the contract between three things that are built separately:
// qooq-watcher serves it, internal/watcher type-aliases the DTOs, and qooq-cli
// lives in another repository and decodes them. Nothing in this repo's build
// fails when a method starts hitting the wrong path or a field is renamed —
// the CLI just stops working, in a way that looks like a watcher bug.

// capture records what the client sent, so a test can assert on the request
// rather than only on what came back.
type capture struct {
	method      string
	path        string
	rawQuery    string
	auth        string
	contentType string
	body        []byte
}

// newServer returns a client pointed at a test server that records the request
// and replies with status and body.
func newServer(t *testing.T, status int, body string, opts ...Option) (*Client, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.rawQuery = r.URL.RawQuery
		got.auth = r.Header.Get("Authorization")
		got.contentType = r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, opts...), got
}

func TestNewTrimsTrailingSlash(t *testing.T) {
	// Without this, every path becomes //v1/targets, which some routers treat
	// as a different route entirely.
	if got := New("http://127.0.0.1:7778/").baseURL; got != "http://127.0.0.1:7778" {
		t.Fatalf("baseURL = %q, want no trailing slash", got)
	}
}

func TestRequestsCarryTheToken(t *testing.T) {
	// Three separate code paths build requests — do, doRaw and Backup — and
	// each sets the header itself. A method that forgets it fails only against
	// a watcher that has auth turned on, which is not the one anybody develops
	// against.
	t.Run("json", func(t *testing.T) {
		c, got := newServer(t, http.StatusOK, `[]`, WithToken("s3cret"))
		if _, err := c.ListTargets(context.Background()); err != nil {
			t.Fatalf("ListTargets: %v", err)
		}
		if got.auth != "Bearer s3cret" {
			t.Errorf("Authorization = %q", got.auth)
		}
	})

	t.Run("raw yaml", func(t *testing.T) {
		c, got := newServer(t, http.StatusOK, "version: 1\n", WithToken("s3cret"))
		if _, err := c.GetManifest(context.Background(), "web"); err != nil {
			t.Fatalf("GetManifest: %v", err)
		}
		if got.auth != "Bearer s3cret" {
			t.Errorf("Authorization = %q", got.auth)
		}
	})

	t.Run("backup stream", func(t *testing.T) {
		c, got := newServer(t, http.StatusOK, "sqlite-bytes", WithToken("s3cret"))
		if err := c.Backup(context.Background(), io.Discard); err != nil {
			t.Fatalf("Backup: %v", err)
		}
		if got.auth != "Bearer s3cret" {
			t.Errorf("Authorization = %q", got.auth)
		}
	})
}

func TestNoTokenSendsNoAuthorizationHeader(t *testing.T) {
	// A watcher with no token configured should see a bare request, not
	// "Bearer ".
	c, got := newServer(t, http.StatusOK, `[]`)
	if _, err := c.ListTargets(context.Background()); err != nil {
		t.Fatalf("ListTargets: %v", err)
	}
	if got.auth != "" {
		t.Errorf("Authorization = %q, want none", got.auth)
	}
}

func TestMethodsHitTheirRoutes(t *testing.T) {
	cases := []struct {
		name       string
		call       func(*Client) error
		wantMethod string
		wantPath   string
		wantQuery  string
	}{
		{"CreateTarget", func(c *Client) error {
			_, err := c.CreateTarget(context.Background(), CreateTargetRequest{Name: "web"})
			return err
		}, http.MethodPost, "/v1/targets", ""},
		{"GetTarget", func(c *Client) error {
			_, err := c.GetTarget(context.Background(), "web")
			return err
		}, http.MethodGet, "/v1/targets/web", ""},
		{"DeleteTarget", func(c *Client) error {
			return c.DeleteTarget(context.Background(), "web", false)
		}, http.MethodDelete, "/v1/targets/web", ""},
		// keep_component is a query parameter for the same reason force is:
		// removing the watch and removing the deployment are one request with
		// one more fact, not two endpoints.
		{"DeleteTargetKeepingComponent", func(c *Client) error {
			return c.DeleteTarget(context.Background(), "web", true)
		}, http.MethodDelete, "/v1/targets/web", "keep_component=1"},
		{"StopTarget", func(c *Client) error {
			return c.StopTarget(context.Background(), "web")
		}, http.MethodPost, "/v1/targets/web/stop", ""},
		{"StartTarget", func(c *Client) error {
			return c.StartTarget(context.Background(), "web")
		}, http.MethodPost, "/v1/targets/web/start", ""},
		// Stop and pause are near-identical calls that do very different
		// things, so each one's path is pinned: wiring two of them to the same
		// endpoint is invisible until somebody's site is off.
		{"PauseTarget", func(c *Client) error {
			return c.PauseTarget(context.Background(), "web")
		}, http.MethodPost, "/v1/targets/web/pause", ""},
		{"ResumeTarget", func(c *Client) error {
			return c.ResumeTarget(context.Background(), "web")
		}, http.MethodPost, "/v1/targets/web/resume", ""},
		{"TriggerTarget", func(c *Client) error {
			return c.TriggerTarget(context.Background(), "web", false)
		}, http.MethodPost, "/v1/targets/web/trigger", ""},
		// force is a query parameter, not a different endpoint: a plain
		// trigger and a forced one are the same request with one more fact.
		{"TriggerTargetForce", func(c *Client) error {
			return c.TriggerTarget(context.Background(), "web", true)
		}, http.MethodPost, "/v1/targets/web/trigger", "force=1"},
		{"DeployImage", func(c *Client) error {
			_, err := c.DeployImage(context.Background(), "web", "nginx:1.27")
			return err
		}, http.MethodPost, "/v1/targets/web/deploy", ""},
		{"SetInputs", func(c *Client) error {
			_, err := c.SetInputs(context.Background(), "web", map[string]string{"K": "v"})
			return err
		}, http.MethodPut, "/v1/targets/web/inputs", ""},
		{"GetManifest", func(c *Client) error {
			_, err := c.GetManifest(context.Background(), "web")
			return err
		}, http.MethodGet, "/v1/targets/web/manifest", ""},
		{"PutManifest", func(c *Client) error {
			_, err := c.PutManifest(context.Background(), "web", []byte("version: 1\n"))
			return err
		}, http.MethodPut, "/v1/targets/web/manifest", ""},
		{"CreateStack", func(c *Client) error {
			_, err := c.CreateStack(context.Background(), CreateStackRequest{Name: "acme"})
			return err
		}, http.MethodPost, "/v1/stacks", ""},
		{"GetStack", func(c *Client) error {
			_, err := c.GetStack(context.Background(), "acme")
			return err
		}, http.MethodGet, "/v1/stacks/acme", ""},
		{"TriggerStack", func(c *Client) error {
			return c.TriggerStack(context.Background(), "acme")
		}, http.MethodPost, "/v1/stacks/acme/trigger", ""},
		{"ResumeStack", func(c *Client) error {
			_, err := c.ResumeStack(context.Background(), "acme")
			return err
		}, http.MethodPost, "/v1/stacks/acme/resume", ""},
		{"StackManifest", func(c *Client) error {
			_, err := c.StackManifest(context.Background(), "acme")
			return err
		}, http.MethodGet, "/v1/stacks/acme/manifest", ""},
		{"DeleteStack", func(c *Client) error {
			return c.DeleteStack(context.Background(), "acme", false)
		}, http.MethodDelete, "/v1/stacks/acme", ""},
		{"DeleteStack prune", func(c *Client) error {
			return c.DeleteStack(context.Background(), "acme", true)
		}, http.MethodDelete, "/v1/stacks/acme", "prune=true"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// "{}" decodes into every response type these methods return, and
			// into nothing for the ones that decode nothing.
			c, got := newServer(t, http.StatusOK, `{}`)
			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got.method != tc.wantMethod || got.path != tc.wantPath {
				t.Errorf("got %s %s, want %s %s", got.method, got.path, tc.wantMethod, tc.wantPath)
			}
			if got.rawQuery != tc.wantQuery {
				t.Errorf("query = %q, want %q", got.rawQuery, tc.wantQuery)
			}
		})
	}
}

func TestRequestBodies(t *testing.T) {
	t.Run("SetInputs wraps values", func(t *testing.T) {
		c, got := newServer(t, http.StatusOK, `{}`)
		if _, err := c.SetInputs(context.Background(), "web", map[string]string{"STRIPE_KEY": "sk_x"}); err != nil {
			t.Fatalf("SetInputs: %v", err)
		}
		var body SetInputsRequest
		if err := json.Unmarshal(got.body, &body); err != nil {
			t.Fatalf("body is not the expected envelope: %v (%s)", err, got.body)
		}
		if body.Values["STRIPE_KEY"] != "sk_x" {
			t.Errorf("values did not survive: %v", body.Values)
		}
		if got.contentType != "application/json" {
			t.Errorf("Content-Type = %q", got.contentType)
		}
	})

	t.Run("DeployImage names the image", func(t *testing.T) {
		c, got := newServer(t, http.StatusOK, `{}`)
		if _, err := c.DeployImage(context.Background(), "web", "ghcr.io/acme/api@sha256:abc"); err != nil {
			t.Fatalf("DeployImage: %v", err)
		}
		var body DeployTargetRequest
		if err := json.Unmarshal(got.body, &body); err != nil {
			t.Fatalf("body: %v (%s)", err, got.body)
		}
		if body.Image != "ghcr.io/acme/api@sha256:abc" {
			t.Errorf("image = %q", body.Image)
		}
	})

	t.Run("PutManifest sends yaml unchanged", func(t *testing.T) {
		c, got := newServer(t, http.StatusOK, "version: 1\n")
		yaml := []byte("version: 1\napp: web\n")
		if _, err := c.PutManifest(context.Background(), "web", yaml); err != nil {
			t.Fatalf("PutManifest: %v", err)
		}
		// The watcher parses this as YAML: JSON-wrapping or re-encoding it
		// would turn a valid manifest into a 400.
		if !bytes.Equal(got.body, yaml) {
			t.Errorf("body = %q, want it byte for byte", got.body)
		}
		if got.contentType != "application/yaml" {
			t.Errorf("Content-Type = %q, want application/yaml", got.contentType)
		}
	})

	t.Run("DeploySpec stays a JSON object", func(t *testing.T) {
		c, got := newServer(t, http.StatusOK, `{}`)
		req := CreateTargetRequest{
			Name:       "web",
			DeploySpec: json.RawMessage(`{"namespace":"acme","ports":["8080:80"]}`),
		}
		if _, err := c.CreateTarget(context.Background(), req); err != nil {
			t.Fatalf("CreateTarget: %v", err)
		}
		// json.RawMessage is embedded as-is; a plain []byte would be
		// base64-encoded here and the watcher would reject it as not an object.
		if !strings.Contains(string(got.body), `"deploy_spec":{"namespace":"acme"`) {
			t.Fatalf("deploy_spec was not embedded as an object: %s", got.body)
		}
	})
}

func TestErrorsCarryTheServersMessageAndStatus(t *testing.T) {
	t.Run("error envelope", func(t *testing.T) {
		c, _ := newServer(t, http.StatusConflict, `{"error":"target \"web\" already exists"}`)
		_, err := c.CreateTarget(context.Background(), CreateTargetRequest{Name: "web"})
		if err == nil {
			t.Fatal("409 must be an error")
		}
		// Both halves matter: the message is what the operator reads, the
		// status is how a script tells "already there" from "broken".
		if !strings.Contains(err.Error(), `already exists`) || !strings.Contains(err.Error(), "409") {
			t.Errorf("error = %q", err)
		}
	})

	t.Run("body that is not the envelope", func(t *testing.T) {
		// A proxy or a panic page: the status and whatever text there was still
		// have to reach the caller instead of an empty error.
		c, _ := newServer(t, http.StatusBadGateway, "upstream is down\n")
		_, err := c.ListTargets(context.Background())
		if err == nil {
			t.Fatal("502 must be an error")
		}
		if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "upstream is down") {
			t.Errorf("error = %q", err)
		}
	})

	t.Run("raw path", func(t *testing.T) {
		c, _ := newServer(t, http.StatusNotFound, `{"error":"no generated manifest"}`)
		_, err := c.GetManifest(context.Background(), "web")
		if err == nil {
			t.Fatal("404 must be an error")
		}
		if !strings.Contains(err.Error(), "no generated manifest") || !strings.Contains(err.Error(), "404") {
			t.Errorf("error = %q", err)
		}
	})
}

func TestBackupStreamsBytesAndLeavesTheWriterAloneOnError(t *testing.T) {
	t.Run("streams", func(t *testing.T) {
		c, _ := newServer(t, http.StatusOK, "sqlite-snapshot-bytes")
		var buf bytes.Buffer
		if err := c.Backup(context.Background(), &buf); err != nil {
			t.Fatalf("Backup: %v", err)
		}
		if buf.String() != "sqlite-snapshot-bytes" {
			t.Errorf("got %q", buf.String())
		}
	})

	t.Run("writes nothing on error", func(t *testing.T) {
		// A backup file containing an error envelope would look like a backup
		// until the day someone tries to restore it.
		c, _ := newServer(t, http.StatusUnauthorized, `{"error":"unauthorized"}`)
		var buf bytes.Buffer
		if err := c.Backup(context.Background(), &buf); err == nil {
			t.Fatal("401 must be an error")
		}
		if buf.Len() != 0 {
			t.Errorf("wrote %q to the backup file", buf.String())
		}
	})
}

func TestResponsesDecode(t *testing.T) {
	body := `{
		"name": "web",
		"deploy_mode": "source",
		"repo_url": "https://example.com/acme/web.git",
		"branch": "main",
		"auth_credential": "***",
		"component_name": "web",
		"namespace": "acme",
		"deploy_spec": {"namespace": "acme"},
		"poll_interval_seconds": 30,
		"last_build_status": "needs_input",
		"missing_inputs": ["STRIPE_KEY"],
		"inputs": [{"name": "STRIPE_KEY", "description": "Stripe secret key", "required": true, "secret": true, "set": false}],
		"next_check_at": "2026-08-05T10:00:00Z",
		"created_at": "2026-08-05T09:00:00Z",
		"updated_at": "2026-08-05T09:30:00Z"
	}`
	c, _ := newServer(t, http.StatusOK, body)
	got, err := c.GetTarget(context.Background(), "web")
	if err != nil {
		t.Fatalf("GetTarget: %v", err)
	}

	if got.Name != "web" || got.Namespace != "acme" || got.ComponentName != "web" {
		t.Errorf("identity fields: %+v", got)
	}
	if got.PollIntervalSeconds != 30 || got.LastBuildStatus != "needs_input" {
		t.Errorf("status fields: %+v", got)
	}
	if len(got.MissingInputs) != 1 || got.MissingInputs[0] != "STRIPE_KEY" {
		t.Errorf("missing_inputs: %+v", got.MissingInputs)
	}
	if len(got.Inputs) != 1 || !got.Inputs[0].Required || !got.Inputs[0].Secret || got.Inputs[0].Set {
		t.Errorf("inputs: %+v", got.Inputs)
	}
	// The CLI prints this to tell the user when the next check happens; a
	// missing pointer here is the difference between a time and "-".
	if got.NextCheckAt == nil || got.NextCheckAt.IsZero() {
		t.Errorf("next_check_at did not decode: %v", got.NextCheckAt)
	}
	if string(got.DeploySpec) == "" {
		t.Error("deploy_spec did not decode")
	}
}

func TestStackResponseDecodesItsProjects(t *testing.T) {
	body := `{
		"name": "acme",
		"repo_url": "https://example.com/acme/stack.git",
		"status": "paused",
		"paused_reason": "project \"api\" failed 3 times in a row",
		"projects": [
			{"name": "db", "source": "postgres", "target_name": "acme-db", "last_build_status": "success"},
			{"name": "api", "source": "https://example.com/acme/api.git", "depends_on": ["db (started)"],
			 "target_name": "acme-api", "last_build_status": "failed", "consecutive_failures": 3, "orphaned": true}
		],
		"created_at": "2026-08-05T09:00:00Z",
		"updated_at": "2026-08-05T09:30:00Z"
	}`
	c, _ := newServer(t, http.StatusOK, body)
	got, err := c.GetStack(context.Background(), "acme")
	if err != nil {
		t.Fatalf("GetStack: %v", err)
	}

	if got.Status != "paused" || got.PausedReason == "" {
		t.Errorf("a paused stack has to say why: %+v", got)
	}
	if len(got.Projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(got.Projects))
	}
	api := got.Projects[1]
	if api.Name != "api" || api.TargetName != "acme-api" {
		t.Errorf("project identity: %+v", api)
	}
	if len(api.DependsOn) != 1 || api.DependsOn[0] != "db (started)" {
		t.Errorf("depends_on: %+v", api.DependsOn)
	}
	if api.ConsecutiveFailures != 3 || !api.Orphaned {
		t.Errorf("failure/orphan state: %+v", api)
	}
}

// The names below are what qooq-watcher serves and what qooq-cli, in a
// different repository, decodes. Renaming a Go field is free; renaming the tag
// silently breaks the other side, and nothing in this build would notice.
func TestWireFieldNamesAreStable(t *testing.T) {
	t.Run("CreateTargetRequest", func(t *testing.T) {
		data, err := json.Marshal(CreateTargetRequest{
			Name: "web", DeployMode: "source", RepoURL: "u", Branch: "main",
			AuthType: "token", AuthCredential: "c", Dockerfile: "d", ContextSubdir: "s",
			ManifestPath: "qooq.yaml", ComponentName: "web", ImageRef: "r",
			PollIntervalSeconds: 30, WebhookSecret: "w",
			Inputs: map[string]string{"K": "v"},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		assertKeys(t, data, []string{
			"name", "deploy_mode", "repo_url", "branch", "auth_type", "auth_credential",
			"dockerfile", "context_subdir", "manifest_path", "component_name", "image_ref",
			"poll_interval_seconds", "webhook_secret", "inputs",
		})
	})

	t.Run("TargetResponse", func(t *testing.T) {
		data, err := json.Marshal(TargetResponse{
			Name: "web", ComponentName: "web", Namespace: "acme", LastBuildStatus: "success",
			LastCommit: "abc1234", LastImageDigest: "sha256:x", LastBuildError: "boom",
			StackName: "acme", ProjectName: "api", MissingInputs: []string{"K"},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		assertKeys(t, data, []string{
			"name", "component_name", "namespace", "last_build_status", "last_commit",
			"last_image_digest", "last_build_error", "stack_name", "project_name",
			"missing_inputs", "created_at", "updated_at",
		})
	})

	t.Run("StackResponse", func(t *testing.T) {
		data, err := json.Marshal(StackResponse{
			Name: "acme", RepoURL: "u", Status: "ready", PausedReason: "r",
			LastManifestCommit: "abc", Projects: []StackProjectResponse{{Name: "api", TargetName: "acme-api"}},
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		assertKeys(t, data, []string{
			"name", "repo_url", "status", "paused_reason", "last_manifest_commit", "projects",
		})
	})

	t.Run("InputResponse", func(t *testing.T) {
		data, err := json.Marshal(InputResponse{Name: "K", Description: "d", Example: "e", Default: "f", Required: true, Secret: true, Set: true, Value: "***"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		assertKeys(t, data, []string{"name", "description", "example", "default", "required", "secret", "set", "value"})
	})

	t.Run("ErrorResponse", func(t *testing.T) {
		// Shared with qooqd; both daemons' handlers write this shape.
		data, err := json.Marshal(ErrorResponse{Error: "boom"})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(data) != `{"error":"boom"}` {
			t.Fatalf("error envelope = %s", data)
		}
	})
}

func assertKeys(t *testing.T, data []byte, want []string) {
	t.Helper()
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("wire field %q is missing; the other side decodes it by that name (have %v)", key, keysOf(got))
		}
	}
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
