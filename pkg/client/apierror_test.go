package client

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The message must not change: callers have been printing it since before
// there was a type, and the whole point of the type is that it costs them
// nothing.
func TestAPIErrorMessageIsUnchanged(t *testing.T) {
	structured := &APIError{Method: "DELETE", Path: "/v1/images", StatusCode: 404, Message: "no such image: x", structured: true}
	if got, want := structured.Error(), "DELETE /v1/images: no such image: x (404)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	raw := &APIError{Method: "GET", Path: "/v1/images", StatusCode: 502, Message: "bad gateway"}
	if got, want := raw.Error(), "GET /v1/images: status 502: bad gateway"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// StatusCode has to survive wrapping, because that is how a caller several
// layers up asks the question.
func TestStatusCodeThroughWrapping(t *testing.T) {
	err := fmt.Errorf("remove image: %w", &APIError{StatusCode: http.StatusConflict})
	if got := StatusCode(err); got != http.StatusConflict {
		t.Errorf("StatusCode = %d, want %d", got, http.StatusConflict)
	}
	if !IsConflict(err) {
		t.Error("IsConflict = false, want true")
	}
	if IsNotFound(err) {
		t.Error("IsNotFound = true for a 409")
	}
	// An error that never came from a response answers 0, not a status that
	// happens to be falsy in some other way.
	if got := StatusCode(errors.New("dial tcp: connection refused")); got != 0 {
		t.Errorf("StatusCode of a non-API error = %d, want 0", got)
	}
	if IsNotFound(nil) {
		t.Error("IsNotFound(nil) = true")
	}
}

// End to end against a real response, so the classification is tied to what
// the daemon actually sends rather than to a hand-built value.
func TestErrFromResponseClassifies(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no such image: ghost:1"}`)
	}))
	defer srv.Close()

	err := New(srv.URL).RemoveImage(t.Context(), "ghost:1")
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound = false for a 404 response (err = %v)", err)
	}
	if got, want := err.Error(), "DELETE /v1/images?ref=ghost%3A1: no such image: ghost:1 (404)"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
