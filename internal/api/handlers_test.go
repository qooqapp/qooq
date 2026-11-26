package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime"
	"github.com/qooqapp/qooq/internal/store/bbolt"
	"github.com/qooqapp/qooq/pkg/client"
)

// --- test scaffolding -------------------------------------------------
//
// These tests exercise internal/api/handlers.go's error-mapping and
// request-handling decisions through the real HTTP router, backed by a real
// core.Service over a real bbolt store (temp dir per test) and a
// fakeRuntime (fake_runtime_test.go) whose hooks force the specific runtime
// errors handlers.go is supposed to translate. router_test.go and
// advertise_test.go already cover the Host/Content-Type middleware and the
// endpoint-composition helpers; this file is about what individual handlers
// do with what core.Service returns them.

// newTestService builds a core.Service over a fresh temp-dir bbolt store and
// the given runtime (a &fakeRuntime{} if nil suffices).
func newTestService(t *testing.T, rt runtime.Runtime) *core.Service {
	t.Helper()
	if rt == nil {
		rt = &fakeRuntime{}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := bbolt.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return core.NewService(rt, st, "fake", "", log)
}

// newTestHandler wires svc into a router with no auth/ingress, logging to
// discard.
func newTestHandler(svc *core.Service) http.Handler {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewRouter(svc, log, "", "127.0.0.1:7777", nil, NewAdvertiseHost(""), nil)
}

// newTestHandlerWithLog is the same, but routes log output to buf so a test
// can assert on what actually got logged (vs. what the client sees).
func newTestHandlerWithLog(svc *core.Service, buf *bytes.Buffer) http.Handler {
	log := slog.New(slog.NewTextHandler(buf, nil))
	return NewRouter(svc, log, "", "127.0.0.1:7777", nil, NewAdvertiseHost(""), nil)
}

func jsonReq(t *testing.T, method, target string, body any) *http.Request {
	t.Helper()
	var r io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, target, r)
	req.Host = "127.0.0.1:7777"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func serve(h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeComponent(t *testing.T, rec *httptest.ResponseRecorder) client.ComponentResponse {
	t.Helper()
	var out client.ComponentResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode component response: %v; body=%s", err, rec.Body.String())
	}
	return out
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var out client.ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error response: %v; body=%s", err, rec.Body.String())
	}
	return out.Error
}

func deployBody(name string) client.DeployRequest {
	return client.DeployRequest{Name: name, Image: "fake:latest"}
}

// --- decision 1: core.ErrNameConflict -> 409 ---------------------------

func TestNameConflictReturns409(t *testing.T) {
	svc := newTestService(t, nil)
	h := newTestHandler(svc)

	rec := serve(h, jsonReq(t, http.MethodPost, "/v1/components/", deployBody("web")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("first deploy: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}

	rec = serve(h, jsonReq(t, http.MethodPost, "/v1/components/", deployBody("web")))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate name deploy: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	if msg := decodeErr(t, rec); !strings.Contains(msg, "already") {
		t.Errorf("409 body should keep the specific conflict detail, got %q", msg)
	}
}

// --- decision 2: runtime.ErrPortConflict -> 409, not masked as 500 -----

func TestPortConflictReturns409NotMaskedAs500(t *testing.T) {
	// Mimic the docker adapter's classifyStartErr: a start failure that
	// wraps the neutral sentinel inside adapter-specific detail text, the
	// same way real "port is already allocated" errors arrive.
	startErr := fmt.Errorf("bind for 0.0.0.0:19000 failed: %w", runtime.ErrPortConflict)
	svc := newTestService(t, &fakeRuntime{startErr: startErr})
	h := newTestHandler(svc)

	rec := serve(h, jsonReq(t, http.MethodPost, "/v1/components/", deployBody("web")))
	if rec.Code != http.StatusConflict {
		t.Fatalf("port conflict: got %d, want 409 (the deliberate exception to the 5xx-masking rule); body=%s",
			rec.Code, rec.Body.String())
	}
	// This is the whole point of the exception: the operator needs the
	// which-port detail, not a generic "internal error".
	if msg := decodeErr(t, rec); !strings.Contains(msg, "19000") {
		t.Errorf("409 body should carry the port-conflict detail, got %q", msg)
	}
}

// A failure while the container runtime itself does not answer is 503, not
// 500: qooqd is fine, docker is not, and the watcher must not count it
// against the repository. With the runtime up the same
// failure stays a plain 500 — which is what a Dockerfile that does not build
// looks like, and that one has to keep counting.
func TestARuntimeThatDoesNotAnswerIs503(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pingErr error
		want    int
	}{
		{"runtime down", errors.New("Cannot connect to the Docker daemon"), http.StatusServiceUnavailable},
		{"runtime up", nil, http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t, &fakeRuntime{startErr: errors.New("boom"), pingErr: tc.pingErr})
			rec := serve(newTestHandler(svc), jsonReq(t, http.MethodPost, "/v1/components/", deployBody("web")))
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d; body=%s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// --- decision 3: core.ErrNotFound -> 404 --------------------------------

func TestNotFoundReturns404(t *testing.T) {
	svc := newTestService(t, nil)
	h := newTestHandler(svc)

	rec := serve(h, jsonReq(t, http.MethodGet, "/v1/components/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown component id: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// --- decision 4: core.ErrNothingToRollback -> 409 -----------------------

func TestNothingToRollbackReturns409(t *testing.T) {
	svc := newTestService(t, nil)
	h := newTestHandler(svc)

	rec := serve(h, jsonReq(t, http.MethodPost, "/v1/components/", deployBody("web")))
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	comp := decodeComponent(t, rec)
	if comp.PreviousImage != "" {
		t.Fatalf("freshly deployed component should have no previous image, got %q", comp.PreviousImage)
	}

	rec = serve(h, jsonReq(t, http.MethodPost, "/v1/components/"+comp.ID+"/rollback", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("rollback with nothing to roll back to: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// --- decision 5: DELETE /v1/images error mapping via errors.Is ---------

// opaqueWrappedErr wraps a sentinel behind a message that shares none of the
// sentinel's words, so a handler that classified errors by substring
// matching (e.g. strings.Contains(err.Error(), "not found")) would fail
// this test even though errors.Is(err, sentinel) succeeds. This is the
// specific thing "靠 errors.Is，不是比對字串" is guarding against.
type opaqueWrappedErr struct {
	msg     string
	wrapped error
}

func (e *opaqueWrappedErr) Error() string { return e.msg }
func (e *opaqueWrappedErr) Unwrap() error { return e.wrapped }

func TestRemoveImageNotFoundReturns404ViaErrorsIs(t *testing.T) {
	err := &opaqueWrappedErr{msg: "registry hiccup xyz123", wrapped: runtime.ErrImageNotFound}
	if strings.Contains(err.Error(), "not found") {
		t.Fatal("test setup bug: opaque message must not contain the sentinel's own words")
	}
	svc := newTestService(t, &fakeRuntime{removeImageErr: err})
	h := newTestHandler(svc)

	rec := serve(h, jsonReq(t, http.MethodDelete, "/v1/images?ref=missing:latest", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("remove missing image: got %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestRemoveImageInUseReturns409ViaErrorsIs(t *testing.T) {
	err := &opaqueWrappedErr{msg: "registry hiccup abc789", wrapped: runtime.ErrImageInUse}
	if strings.Contains(err.Error(), "in use") {
		t.Fatal("test setup bug: opaque message must not contain the sentinel's own words")
	}
	svc := newTestService(t, &fakeRuntime{removeImageErr: err})
	h := newTestHandler(svc)

	rec := serve(h, jsonReq(t, http.MethodDelete, "/v1/images?ref=busy:latest", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("remove in-use image: got %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
}

// A removeImage failure that is neither sentinel still falls back to the
// generic 5xx path (proving the switch has a default, not just two cases).
func TestRemoveImageUnrelatedErrorReturns500Generic(t *testing.T) {
	var buf bytes.Buffer
	secret := "disk exploded at /var/lib/docker/overlay2/deadbeef"
	svc := newTestService(t, &fakeRuntime{removeImageErr: errors.New(secret)})
	h := newTestHandlerWithLog(svc, &buf)

	rec := serve(h, jsonReq(t, http.MethodDelete, "/v1/images?ref=x:latest", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("unrelated remove-image error: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if msg := decodeErr(t, rec); msg != "internal error" {
		t.Errorf("5xx body must be the generic message, got %q", msg)
	}
	if !strings.Contains(buf.String(), secret) {
		t.Errorf("original error text must reach the log; log=%s", buf.String())
	}
}

// --- decision 6: 5xx only ever gets a generic message; 4xx keeps its own ---

func TestFiveXXBodyIsGenericButLogKeepsOriginal(t *testing.T) {
	var buf bytes.Buffer
	secret := "leaked-internal-path: /home/op/.docker/config.json unreadable: permission denied"
	svc := newTestService(t, &fakeRuntime{listImagesErr: errors.New(secret)})
	h := newTestHandlerWithLog(svc, &buf)

	rec := serve(h, jsonReq(t, http.MethodGet, "/v1/images", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("5xx response body leaked the internal error text: %s", rec.Body.String())
	}
	if msg := decodeErr(t, rec); msg != "internal error" {
		t.Errorf("5xx body must be exactly the generic message, got %q", msg)
	}
	if !strings.Contains(buf.String(), secret) {
		t.Errorf("the real error text must still reach the daemon log, log=%s", buf.String())
	}
}

// The flip side: a 4xx (bad request) keeps its specific, useful message —
// this is what the CLI matches on, and what an operator needs to fix their
// own mistake.
func TestFourXXKeepsSpecificMessage(t *testing.T) {
	svc := newTestService(t, nil)
	h := newTestHandler(svc)

	rec := serve(h, jsonReq(t, http.MethodPost, "/v1/components/", client.DeployRequest{Name: "", Image: ""}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing name/image: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if msg := decodeErr(t, rec); msg != "name and image are required" {
		t.Errorf("4xx body should keep its specific message, got %q", msg)
	}
}

// --- decision 7: Content-Type enforcement is per-route, not global -----

func TestNonJSONContentTypeRejectedOnJSONDecodingRoutes(t *testing.T) {
	svc := newTestService(t, nil)
	h := newTestHandler(svc)

	req := jsonReq(t, http.MethodPost, "/v1/components/", deployBody("web"))
	req.Header.Set("Content-Type", "text/plain")
	rec := serve(h, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("POST /v1/components with text/plain: got %d, want 415; body=%s", rec.Code, rec.Body.String())
	}
}

// images/load and images/build are tar streams, not JSON, and must not be
// subject to the same Content-Type guard — sending them without (or with a
// non-JSON) Content-Type must reach the handler rather than being rejected
// at the router layer with 415.
func TestTarStreamRoutesExemptFromJSONContentTypeGuard(t *testing.T) {
	svc := newTestService(t, nil)
	h := newTestHandler(svc)

	req := httptest.NewRequest(http.MethodPost, "/v1/images/load", strings.NewReader("not-a-real-tar"))
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := serve(h, req)
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("images/load must not be gated by the JSON Content-Type guard; got 415")
	}

	req = httptest.NewRequest(http.MethodPost, "/v1/images/build", strings.NewReader("not-a-real-tar"))
	req.Host = "127.0.0.1:7777"
	// No Content-Type at all.
	rec = serve(h, req)
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Fatalf("images/build must not be gated by the JSON Content-Type guard; got 415")
	}
}

// --- decision 8: namespace normalization, "" <-> "default", idempotent ---

func TestNamespaceDefaultsToDefaultAndIsIdempotent(t *testing.T) {
	svc := newTestService(t, nil)
	h := newTestHandler(svc)

	body := deployBody("api")
	body.Namespace = "" // omitted
	rec := serve(h, jsonReq(t, http.MethodPost, "/v1/components/", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("deploy with empty namespace: got %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	comp := decodeComponent(t, rec)
	if comp.Namespace != client.DefaultNamespace {
		t.Fatalf("empty namespace should normalize to %q, got %q", client.DefaultNamespace, comp.Namespace)
	}

	// Applying the normalization again (as if re-reading a value that went
	// through it once already) must not change it — the same identity check
	// that name-conflict detection relies on for "" and "default" being one
	// namespace, not two.
	if got := client.NormalizeNamespace(comp.Namespace); got != client.DefaultNamespace {
		t.Fatalf("NormalizeNamespace must be idempotent, got %q", got)
	}

	// Proof it isn't just cosmetic: a second component explicitly named
	// "default" collides with the first one's implicit "default", because
	// both went through the same normalization on write.
	body2 := deployBody("api")
	body2.Namespace = client.DefaultNamespace
	rec = serve(h, jsonReq(t, http.MethodPost, "/v1/components/", body2))
	if rec.Code != http.StatusConflict {
		t.Fatalf("explicit %q namespace should collide with the earlier implicit default, got %d; body=%s",
			client.DefaultNamespace, rec.Code, rec.Body.String())
	}
}
