package docker

import (
	"errors"
	"strings"
	"testing"

	"github.com/qooqapp/qooq/internal/runtime"
)

// TestClassifyStartErr pins the two daemon message shapes that mean "host port
// taken" to runtime.ErrPortConflict, since docker gives them no errdefs class
// and internal/api relies on the sentinel to answer 409 instead of a generic
// 500 that hides the cause.
func TestClassifyStartErr(t *testing.T) {
	tests := []struct {
		name       string
		msg        string
		wantConfl  bool
		wantInText string // substring the operator needs to see
	}{
		{
			name:       "port is already allocated",
			msg:        "Error response from daemon: driver failed programming external connectivity on endpoint web (abc123): Bind for 0.0.0.0:8080 failed: port is already allocated",
			wantConfl:  true,
			wantInText: "host port 0.0.0.0:8080",
		},
		{
			name:       "address already in use",
			msg:        "Error response from daemon: driver failed programming external connectivity on endpoint web (abc123): listen tcp4 0.0.0.0:9090: bind: address already in use",
			wantConfl:  true,
			wantInText: "host port 0.0.0.0:9090",
		},
		{
			name:       "conflict without a parseable address still flagged",
			msg:        "Error response from daemon: port is already allocated",
			wantConfl:  true,
			wantInText: "one of its published host ports",
		},
		{
			name:      "unrelated failure stays unclassified",
			msg:       "Error response from daemon: no such image: nginx:absent",
			wantConfl: false,
		},
		{
			name:      "oci runtime error stays unclassified",
			msg:       "Error response from daemon: OCI runtime create failed: exec: \"/app\": permission denied",
			wantConfl: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyStartErr(runtime.ContainerID("cid123"), errors.New(tc.msg))

			if isConfl := errors.Is(got, runtime.ErrPortConflict); isConfl != tc.wantConfl {
				t.Fatalf("errors.Is(err, ErrPortConflict) = %v, want %v (err=%q)", isConfl, tc.wantConfl, got)
			}
			// The raw daemon text must survive in every case so logs keep
			// full fidelity.
			if !strings.Contains(got.Error(), tc.msg) {
				t.Errorf("original daemon message lost:\n got %q\nwant it to contain %q", got, tc.msg)
			}
			if tc.wantInText != "" && !strings.Contains(got.Error(), tc.wantInText) {
				t.Errorf("missing operator-facing detail %q in %q", tc.wantInText, got)
			}
		})
	}
}
