//go:build unix

package process

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/qooqapp/qooq/internal/runtime"
)

// followPollInterval is how often the follow-mode reader checks stdout.log
// for new bytes. A simple poll-tail is sufficient here (v1 scope): process
// instances are local files, not a network stream, so sub-second latency from
// polling is an acceptable trade for not needing inotify/kqueue plumbing.
const followPollInterval = 300 * time.Millisecond

// Logs returns the instance's stdout.log/stderr.log as plain text. Non-follow
// mode concatenates both files (stdout first, then stderr), each prefixed
// with a "=== stdout ===" / "=== stderr ===" header, tailed to opts.Tail
// lines (0 = everything). Follow mode only follows stdout.log (see package
// doc): it emits the tailed starting content then polls for
// growth until ctx is cancelled or the returned reader is closed.
func (a *Adapter) Logs(ctx context.Context, id runtime.ContainerID, opts runtime.LogOptions) (io.ReadCloser, error) {
	inst, ok := a.getInstance(id)
	if !ok {
		return nil, fmt.Errorf("process runtime: container %s not found", id)
	}
	inst.mu.Lock()
	dir := inst.dir
	inst.mu.Unlock()

	stdoutPath := filepath.Join(dir, "stdout.log")
	stderrPath := filepath.Join(dir, "stderr.log")

	if opts.Follow {
		return followLog(ctx, stdoutPath, opts.Tail), nil
	}
	return staticLogs(stdoutPath, stderrPath, opts.Tail), nil
}

// staticLogs reads the tailed content of both log files into memory (they
// are capped at maxLogBytes=10MB each — see rotateLogIfLarge/
// truncateOversizedLogs — so this is bounded) and returns it as a single
// ReadCloser.
func staticLogs(stdoutPath, stderrPath string, tail int) io.ReadCloser {
	var buf bytes.Buffer
	buf.WriteString("=== stdout ===\n")
	buf.Write(tailFile(stdoutPath, tail))
	buf.WriteString("=== stderr ===\n")
	buf.Write(tailFile(stderrPath, tail))
	return io.NopCloser(&buf)
}

// tailFile returns the last n lines of the file at path, or its entire
// content when n<=0. A missing file yields no error and no output (an
// instance that never wrote to stderr, for instance).
func tailFile(path string, n int) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if n <= 0 {
		return data
	}
	return lastNLines(data, n)
}

// lastNLines returns the suffix of data starting at the beginning of its
// (len(lines)-n)-th line, i.e. its last n lines (trailing newline preserved).
func lastNLines(data []byte, n int) []byte {
	if n <= 0 || len(data) == 0 {
		return data
	}
	// Walk backwards counting newlines; stop once n+1 are seen (the split
	// point is right after the (n+1)-th-from-last '\n').
	count := 0
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			count++
			if count == n+1 {
				return data[i+1:]
			}
		}
	}
	return data
}

// followLog returns a reader that first emits the tailed content of path,
// then polls for growth (simple poll-tail, see followPollInterval) until ctx
// is cancelled. Cancelling ctx is what guarantees the goroutine exits;
// closing the reader alone only fails the next pipe Write, which never comes
// while the log is idle — so follow callers must cancel ctx when giving up
// (see the runtime.Runtime.Logs contract).
func followLog(ctx context.Context, path string, tail int) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()

		if _, err := pw.Write([]byte("=== stdout ===\n")); err != nil {
			return
		}

		f, err := os.Open(path)
		if err != nil {
			// The instance may not have produced any stdout yet, or the log
			// file genuinely does not exist: not a hard error for follow
			// mode, just nothing to show until it appears.
			f = nil
		}
		var offset int64
		if f != nil {
			if tail > 0 {
				if data, rerr := os.ReadFile(path); rerr == nil {
					tailed := lastNLines(data, tail)
					offset = int64(len(data) - len(tailed))
				}
			}
			if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
				f.Close()
				f = nil
			}
		}

		buf := make([]byte, 32*1024)
		ticker := time.NewTicker(followPollInterval)
		defer ticker.Stop()
		for {
			if f == nil {
				// Retry opening in case the instance is about to start.
				if opened, oerr := os.Open(path); oerr == nil {
					f = opened
				}
			}
			if f != nil {
				// The log can be rotated out from under us: rotateLogIfLarge
				// renames it away on instance start (different inode appears at
				// path), and truncateOversizedLogs shrinks it in place (same
				// inode, size drops below our offset). Either way the fd would
				// otherwise read nothing forever; detect both and restart from
				// the top of the fresh content.
				fi, ferr := f.Stat()
				if cur, cerr := os.Stat(path); cerr == nil && ferr == nil && !os.SameFile(cur, fi) {
					f.Close()
					f, offset = nil, 0
					if opened, oerr := os.Open(path); oerr == nil {
						f = opened
					}
				} else if ferr == nil && fi.Size() < offset {
					if _, serr := f.Seek(0, io.SeekStart); serr == nil {
						offset = 0
					}
				}
			}
			if f != nil {
				for {
					n, rerr := f.Read(buf)
					if n > 0 {
						offset += int64(n)
						if _, werr := pw.Write(buf[:n]); werr != nil {
							f.Close()
							return
						}
					}
					if rerr != nil {
						break
					}
				}
			}
			select {
			case <-ctx.Done():
				if f != nil {
					f.Close()
				}
				return
			case <-ticker.C:
			}
		}
	}()
	return pr
}
