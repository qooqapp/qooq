//go:build !darwin && !linux

package api

import (
	"fmt"
	"runtime"
)

// hostMemoryBytes has no implementation outside darwin/linux (v1 scope).
func hostMemoryBytes() (int64, error) {
	return 0, fmt.Errorf("memory detection not supported on %s", runtime.GOOS)
}
