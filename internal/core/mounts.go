package core

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/qooqapp/qooq/internal/runtime"
)

// ErrInvalidMount is returned by Deploy/Upsert when a requested mount cannot
// be expressed: paths must both be absolute (the declarative repo-relative and
// named-volume forms are resolved to host paths by the watcher long before
// they reach qooqd) and each container path may only be claimed once.
var ErrInvalidMount = errors.New("mount: source and target must be absolute paths, and each target may appear only once")

// validateMounts checks a spec's mounts. A nil/empty slice is valid — that is
// what every component created before M23 decodes to.
func validateMounts(mounts []runtime.Mount) error {
	seen := make(map[string]struct{}, len(mounts))
	for _, m := range mounts {
		if !filepath.IsAbs(m.Source) {
			return fmt.Errorf("%w (source %q is not absolute)", ErrInvalidMount, m.Source)
		}
		if !filepath.IsAbs(m.Target) {
			return fmt.Errorf("%w (target %q is not absolute)", ErrInvalidMount, m.Target)
		}
		// Mounting over "/" replaces the container's whole filesystem; it is
		// never what anyone means and the resulting container is unbootable.
		target := filepath.Clean(m.Target)
		if target == "/" {
			return fmt.Errorf("%w (target %q would replace the container root)", ErrInvalidMount, m.Target)
		}
		if _, dup := seen[target]; dup {
			return fmt.Errorf("%w (target %q is mounted twice)", ErrInvalidMount, m.Target)
		}
		seen[target] = struct{}{}
	}
	return nil
}
