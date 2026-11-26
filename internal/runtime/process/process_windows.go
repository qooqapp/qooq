//go:build !unix

// The process runtime relies on unix process-group and signal semantics
// (Setpgid, SIGTERM/SIGKILL, kill(pid,0) liveness checks); on other
// platforms it is compiled out and New reports a clear error so cmd/qooqd
// still builds everywhere.
package process

import (
	"errors"
	"log/slog"

	"github.com/qooqapp/qooq/internal/runtime"
)

// Adapter is a placeholder on non-unix platforms; New never returns one.
// Embedding the interface satisfies runtime.Runtime at compile time; the
// methods are unreachable because New always errors.
type Adapter struct{ runtime.Runtime }

func New(dir string, log *slog.Logger) (*Adapter, error) {
	return nil, errors.New("process runtime is only supported on unix platforms")
}
