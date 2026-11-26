package watcherclient

import (
	"errors"
	"fmt"
	"net/http"
)

// APIError is a non-2xx response from the watcher.
//
// Mirrors pkg/client's APIError, and exists for the same reason that one does:
// without it the only way to tell "already exists" from "failed" is to match
// text inside the message, and a reworded message breaks that silently. The
// qooq repository has made this mistake once already and recorded it.
//
// Error() renders byte-for-byte what the previous fmt.Errorf calls produced, so
// every existing caller that prints an error is unaffected.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	// Message is the watcher's own error text when it sent the structured
	// envelope, and the raw body otherwise.
	Message string
	// structured records which of the two forms the watcher sent, because they
	// render differently and the rendering is what callers already print.
	structured bool
}

func (e *APIError) Error() string {
	if e.structured {
		return fmt.Sprintf("%s %s: %s (%d)", e.Method, e.Path, e.Message, e.StatusCode)
	}
	return fmt.Sprintf("%s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
}

// StatusCode reports the HTTP status behind err, or 0 if err did not come from a
// response. Use it instead of reading the message.
func StatusCode(err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// IsNotFound reports whether err is the watcher saying the thing is not there.
func IsNotFound(err error) bool { return StatusCode(err) == http.StatusNotFound }

// IsConflict reports whether err is the watcher refusing because of the state
// something is in — a target name already taken, a component already claimed.
func IsConflict(err error) bool { return StatusCode(err) == http.StatusConflict }
