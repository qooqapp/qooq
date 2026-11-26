package watcher

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestHandleCreateTarget_RequiresJSONContentType verifies the 415 guard fires
// before any body parsing / db access (the check is the first line of the
// handler), so a browser no-cors POST (text/plain) is rejected.
func TestHandleCreateTarget_RequiresJSONContentType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := &Watcher{}
	r := gin.New()
	r.POST("/v1/targets", w.handleCreateTarget)

	req := httptest.NewRequest(http.MethodPost, "/v1/targets", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain POST /v1/targets: got %d, want 415; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHostGuard covers the no-auth drive-by defense: a foreign Host is
// rejected, the allowlisted localhost Host passes through.
func TestHostGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", hostGuard([]string{"127.0.0.1:7778"}), func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Host = "evil.com"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Host evil.com: got %d, want 403", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Host = "127.0.0.1:7778"
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("Host 127.0.0.1:7778: got %d, want 200", rec.Code)
	}
}

// validTargetPath / validManifestPath are the string-level path-traversal
// defense on target fields (dockerfile, context_subdir). They must reject
// anything that could point the build outside the cloned repo root; the
// symlink-level defense is resolveWithin (see pipeline_test.go / M17 F3).
func TestValidTargetPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"", true},                  // empty means "use default"
		{"Dockerfile", true},        // plain file in root
		{"svc/Dockerfile", true},    // nested inside repo
		{"a/../b", true},            // stays within after cleaning
		{"/etc/passwd", false},      // absolute escapes
		{"../secrets", false},       // parent traversal escapes
		{"../../etc/passwd", false}, // deeper traversal escapes
		{"sub/../../etc", false},    // net-upward traversal escapes
	}
	for _, c := range cases {
		if got := validTargetPath(c.path); got != c.want {
			t.Errorf("validTargetPath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestValidManifestPath(t *testing.T) {
	// Everything validTargetPath allows, plus the "-" opt-out sentinel.
	if !validManifestPath("-") {
		t.Error(`validManifestPath("-") = false, want true (opt-out sentinel)`)
	}
	if !validManifestPath("qooq.yaml") {
		t.Error(`validManifestPath("qooq.yaml") = false, want true`)
	}
	if validManifestPath("../qooq.yaml") {
		t.Error(`validManifestPath("../qooq.yaml") = true, want false (escapes root)`)
	}
	if validManifestPath("/abs/qooq.yaml") {
		t.Error(`validManifestPath("/abs/qooq.yaml") = true, want false (absolute)`)
	}
}
