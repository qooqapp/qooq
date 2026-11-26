package controlclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProjectCalls(t *testing.T) {
	var gotMethod, gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth = r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization")
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		switch {
		case r.Method == "GET" && r.URL.Path == "/apis/v1/accounts/acme/projects":
			w.Write([]byte(`{"projects":[{"name":"web","namespace":"prod","last_build_status":"success","build_generation":3,"endpoints":["http://x:1"],"services":[{"name":"db","type":"postgres"}]}]}`))
		case r.Method == "POST" && r.URL.Path == "/apis/v1/accounts/acme/projects":
			w.WriteHeader(201)
			w.Write([]byte(`{"name":"web","namespace":"prod","last_build_status":"never"}`))
		case r.URL.Path == "/apis/v1/accounts/acme/namespaces/prod/projects/web/build-log":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("line1\nline2\n"))
		case r.Method == "POST":
			w.WriteHeader(202)
			w.Write([]byte(`{"ok":true}`))
		case r.Method == "DELETE":
			w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"code":"not_found","message":"nope"}}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, WithToken("tok"))
	ctx := context.Background()

	ps, err := c.ListProjects(ctx, "acme")
	if err != nil || len(ps) != 1 || ps[0].BuildGeneration != 3 || ps[0].Services[0].Type != "postgres" || ps[0].Endpoints[0] != "http://x:1" {
		t.Fatalf("list: %+v %v", ps, err)
	}
	if gotAuth != "Bearer tok" || gotMethod != "GET" {
		t.Fatalf("auth/method: %q %q", gotAuth, gotMethod)
	}

	p, err := c.CreateProject(ctx, "acme", CreateProjectRequest{Name: "web", Namespace: "prod", RepoURL: "https://g/x"})
	if err != nil || p.LastBuildStatus != "never" || gotBody["repo_url"] != "https://g/x" || gotBody["name"] != "web" {
		t.Fatalf("create: %+v %v %v", p, err, gotBody)
	}

	if err := c.DeployProject(ctx, "acme", "prod", "web"); err != nil || gotMethod != "POST" || gotPath != "/apis/v1/accounts/acme/namespaces/prod/projects/web/deploy" {
		t.Fatalf("deploy: %v %s %s", err, gotMethod, gotPath)
	}
	if gotBody != nil {
		t.Fatalf("a plain deploy sent a body (the server would read it as an image deploy): %v", gotBody)
	}
	if _, err := c.DeployProjectImage(ctx, "acme", "prod", "web", "reg/app:pinned"); err != nil ||
		gotPath != "/apis/v1/accounts/acme/namespaces/prod/projects/web/deploy" || gotBody["image"] != "reg/app:pinned" {
		t.Fatalf("image deploy: %v %s %v", err, gotPath, gotBody)
	}
	log, err := c.BuildLog(ctx, "acme", "prod", "web")
	if err != nil || log != "line1\nline2\n" {
		t.Fatalf("log: %q %v", log, err)
	}
	if err := c.DeleteProject(ctx, "acme", "prod", "web"); err != nil || gotMethod != "DELETE" {
		t.Fatalf("delete: %v %s", err, gotMethod)
	}
	_, err = c.BuildLog(ctx, "acme", "prod", "other")
	if StatusCode(err) != 404 {
		t.Fatalf("want 404 APIError, got %v", err)
	}
}

func TestUploadProjectSourceStreamsTheBodyWithoutTheShortTimeout(t *testing.T) {
	var gotBody, gotType, gotAuth, gotQuery string
	var gotLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotLen = r.ContentLength
		gotBody, gotType, gotAuth, gotQuery = string(b), r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.URL.RawQuery
		if r.Method != "PUT" || r.URL.Path != "/apis/v1/accounts/acme/namespaces/prod/projects/web/source" {
			w.WriteHeader(404)
			return
		}
		if string(b) == "BAD" {
			w.WriteHeader(409)
			w.Write([]byte(`{"error":{"code":"project_not_upload_source","message":"nope"}}`))
			return
		}
		w.WriteHeader(202)
		w.Write([]byte(`{"revision":"abc","build_queued":true,"build_generation":4,"created":true}`))
	}))
	defer srv.Close()
	c := New(srv.URL, WithToken("tok"))
	if c.stream.Timeout != 0 {
		t.Fatalf("the upload client has a total timeout (%v), which would cut a large upload off", c.stream.Timeout)
	}
	res, err := c.UploadProjectSource(context.Background(), "acme", "prod", "web", strings.NewReader("TARGZ"), 5, true)
	if err != nil || res.Revision != "abc" || !res.BuildQueued || res.BuildGeneration != 4 || !res.Created {
		t.Fatalf("upload: %+v %v", res, err)
	}
	if gotBody != "TARGZ" || gotType != "application/gzip" || gotAuth != "Bearer tok" || gotQuery != "force=1" || gotLen != 5 {
		t.Errorf("body=%q type=%q auth=%q query=%q", gotBody, gotType, gotAuth, gotQuery)
	}
	_, err = c.UploadProjectSource(context.Background(), "acme", "prod", "web", strings.NewReader("BAD"), -1, false)
	if StatusCode(err) != 409 || !strings.Contains(err.Error(), "project_not_upload_source") {
		t.Errorf("err = %v", err)
	}
}
