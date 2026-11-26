//go:build darwin

package api

import "golang.org/x/sys/unix"

// hostMemoryBytes returns total physical memory on macOS via the hw.memsize
// sysctl. stdlib's syscall.Sysctl treats the result as a NUL-terminated C
// string, which mangles a raw uint64 — golang.org/x/sys/unix.SysctlUint64
// reads it correctly. x/sys is already a transitive dependency of this
// module (pulled in by the docker SDK et al.), so this doesn't add a new
// dependency to qooqd, just a direct import of one already in go.sum.
func hostMemoryBytes() (int64, error) {
	v, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0, err
	}
	return int64(v), nil
}
