//go:build unix

package process

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"github.com/qooqapp/qooq/internal/runtime"
)

// Exec runs cmd on the host (resolved via PATH like any shell command),
// cwd set to the instance's state directory, and waits for it to finish,
// returning its exit code. Used by the "exec" health-check probe. There is
// no containment here — the command runs with qooqd's own privileges — which
// matches the process runtime's documented lack of namespace isolation.
func (a *Adapter) Exec(ctx context.Context, id runtime.ContainerID, cmd []string) (int, error) {
	code, _, err := a.exec(ctx, id, cmd, false)
	return code, err
}

// ExecOutput is Exec plus the command's combined stdout/stderr. Not part of
// the runtime.Runtime interface — see the docker adapter's ExecOutput for why
// it exists as an optional, type-asserted extra instead.
func (a *Adapter) ExecOutput(ctx context.Context, id runtime.ContainerID, cmd []string) (int, []byte, error) {
	return a.exec(ctx, id, cmd, true)
}

func (a *Adapter) exec(ctx context.Context, id runtime.ContainerID, cmd []string, capture bool) (int, []byte, error) {
	if len(cmd) == 0 {
		return -1, nil, errors.New("process runtime: exec: empty command")
	}
	inst, ok := a.getInstance(id)
	if !ok {
		return -1, nil, fmt.Errorf("process runtime: exec: container %s not found", id)
	}

	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = inst.dir

	var out []byte
	var err error
	if capture {
		out, err = c.CombinedOutput()
	} else {
		err = c.Run()
	}
	if err == nil {
		return 0, out, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok {
			if ws.Signaled() {
				return 128 + int(ws.Signal()), out, nil
			}
			return ws.ExitStatus(), out, nil
		}
		return ee.ExitCode(), out, nil
	}
	if ctx.Err() != nil {
		return -1, out, ctx.Err()
	}
	return -1, out, fmt.Errorf("process runtime: exec: %w", err)
}
