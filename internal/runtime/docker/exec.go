package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/qooqapp/qooq/internal/runtime"
)

// Exec runs cmd inside a running container and blocks until it finishes,
// returning its exit code. It attaches to combined stdout/stderr and drains
// (discards) the output — the exec API only reports completion once that
// stream is fully consumed. If ctx is cancelled (e.g. the caller's probe
// timeout) before the command finishes, the hijacked connection is closed to
// unblock the drain and ctx.Err() is returned.
func (a *Adapter) Exec(ctx context.Context, id runtime.ContainerID, cmd []string) (int, error) {
	code, _, err := a.execCapture(ctx, id, cmd, false)
	return code, err
}

// ExecOutput is Exec plus the command's combined stdout/stderr, capped at
// execOutputCap bytes. It is not part of the runtime.Runtime interface —
// callers that need the output (currently only the ingress admin-API reload,
// which has no other way to surface a Caddy config error) type-assert for it
// instead, so the health-check probe path above is untouched.
func (a *Adapter) ExecOutput(ctx context.Context, id runtime.ContainerID, cmd []string) (int, []byte, error) {
	return a.execCapture(ctx, id, cmd, true)
}

// execOutputCap bounds how much of a command's output ExecOutput keeps, so a
// runaway or chatty command cannot balloon an error message.
const execOutputCap = 16 * 1024

func (a *Adapter) execCapture(ctx context.Context, id runtime.ContainerID, cmd []string, capture bool) (int, []byte, error) {
	created, err := a.cli.ContainerExecCreate(ctx, string(id), container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return -1, nil, fmt.Errorf("exec create: %w", err)
	}

	attach, err := a.cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return -1, nil, fmt.Errorf("exec attach: %w", err)
	}
	defer attach.Close()

	drained := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			attach.Close()
		case <-drained:
		}
	}()
	var out bytes.Buffer
	var copyErr error
	if capture {
		// The exec has no TTY, so the stream is multiplexed: every chunk is
		// preceded by an 8-byte frame header. Copied raw, those headers
		// ended up in the output — the "\x02\x00\x00..." in front of every
		// caddy error in the log, and a read-back of Caddy's config that
		// could not be parsed as JSON. stdcopy takes them off (same as Logs).
		// The writer keeps the first execOutputCap bytes and discards the
		// rest, so the stream is still drained to completion.
		capped := &capWriter{buf: &out, max: execOutputCap}
		_, copyErr = stdcopy.StdCopy(capped, capped, attach.Reader)
	} else {
		_, copyErr = io.Copy(io.Discard, attach.Reader)
	}
	close(drained)

	if ctx.Err() != nil {
		return -1, out.Bytes(), ctx.Err()
	}
	if copyErr != nil {
		return -1, out.Bytes(), fmt.Errorf("exec drain output: %w", copyErr)
	}

	insp, err := a.cli.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return -1, out.Bytes(), fmt.Errorf("exec inspect: %w", err)
	}
	return insp.ExitCode, out.Bytes(), nil
}

// capWriter keeps up to max bytes and accepts (and drops) the rest.
type capWriter struct {
	buf *bytes.Buffer
	max int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); room > 0 {
		if len(p) > room {
			w.buf.Write(p[:room])
		} else {
			w.buf.Write(p)
		}
	}
	return len(p), nil
}
