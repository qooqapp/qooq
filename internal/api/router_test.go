package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qooqapp/qooq/internal/core"
	"github.com/qooqapp/qooq/internal/runtime/process"
	"github.com/qooqapp/qooq/internal/store/bbolt"
)

// testRouter builds a real router over a bbolt store + process runtime (no
// docker needed). These back the drive-by hardening middleware tests below;
// the process runtime is never actually exercised by the routes under test
// (List reads the store; deploy rejects an empty body before touching it).
func testRouter(t *testing.T, token string) http.Handler {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := bbolt.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	rt, err := process.New(filepath.Join(t.TempDir(), "proc"), log)
	if err != nil {
		t.Fatalf("init process runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	svc := core.NewService(rt, st, "process", "", log)
	return NewRouter(svc, log, token, "127.0.0.1:7777", nil, NewAdvertiseHost(""), nil)
}

func do(t *testing.T, h http.Handler, method, target, host, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	req.Host = host
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// (a) No auth: a foreign Host header is rejected (browser drive-by defense).
func TestHostGuard_RejectsForeignHost(t *testing.T) {
	h := testRouter(t, "")
	rec := do(t, h, http.MethodGet, "/v1/components", "evil.com", "", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Host evil.com: got %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
}

// (b) No auth: the canonical localhost Host is allowed through.
func TestHostGuard_AllowsLocalhost(t *testing.T) {
	h := testRouter(t, "")
	rec := do(t, h, http.MethodGet, "/v1/components", "127.0.0.1:7777", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("Host 127.0.0.1:7777: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// (c) A JSON-body route rejects a non-JSON Content-Type with 415.
func TestContentType_RejectsNonJSON(t *testing.T) {
	h := testRouter(t, "")
	rec := do(t, h, http.MethodPost, "/v1/components", "127.0.0.1:7777", "text/plain", "{}")
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain POST: got %d, want 415; body=%s", rec.Code, rec.Body.String())
	}
}

// (d) application/json passes the Content-Type guard and reaches the handler
// (which then rejects the empty body with 400 name/image required).
func TestContentType_AllowsJSONReachesHandler(t *testing.T) {
	h := testRouter(t, "")
	rec := do(t, h, http.MethodPost, "/v1/components", "127.0.0.1:7777", "application/json", "{}")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("application/json POST {}: got %d, want 400 (into handler); body=%s", rec.Code, rec.Body.String())
	}
}

// A charset suffix on application/json is still accepted.
func TestContentType_AllowsCharsetSuffix(t *testing.T) {
	h := testRouter(t, "")
	rec := do(t, h, http.MethodPost, "/v1/components", "127.0.0.1:7777", "application/json; charset=utf-8", "{}")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("application/json;charset POST {}: got %d, want 400 (into handler); body=%s", rec.Code, rec.Body.String())
	}
}

// (e) With a token configured, the Host guard is not mounted: a foreign Host
// plus the correct bearer token still reaches the handler (200).
func TestHostGuard_NotMountedWhenTokenSet(t *testing.T) {
	const token = "s3cret"
	h := testRouter(t, token)
	req := httptest.NewRequest(http.MethodGet, "/v1/components", nil)
	req.Host = "evil.com"
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("token set + foreign Host + bearer: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
