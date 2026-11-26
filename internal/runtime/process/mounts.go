package process

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/qooqapp/qooq/internal/runtime"
)

// reservedInstanceFiles are the names the instance directory owns. A mount
// landing on one of them would overwrite the adapter's own state — meta.json
// in particular is the entire process-runtime state machine — so a collision
// is a hard error rather than something to silently rename around.
var reservedInstanceFiles = map[string]struct{}{
	"meta.json":  {},
	"stdout.log": {},
	"stderr.log": {},
}

// materializeMounts realizes a container spec's mounts for the process
// runtime, which has no namespaces and therefore nothing to bind-mount into.
// The equivalent of "this file is visible at /app/config.toml" for a process
// whose working directory is the instance dir (see StartContainer's cmd.Dir)
// is "this file sits in the instance dir under its base name" — which is
// exactly what an app doing viper.AddConfigPath("./") then finds.
//
// Files are copied; directories are symlinked. The distinction matters:
// RemoveContainer does os.RemoveAll(inst.dir) and an Upsert version change
// goes through RemoveContainer, so anything copied into the instance dir is
// deleted on the next deploy. A symlink means only the link dies and the
// data — which for a directory mount is the whole point — survives outside.
func materializeMounts(instDir string, mounts []runtime.Mount) error {
	for _, m := range mounts {
		base := filepath.Base(filepath.Clean(m.Target))
		if base == "." || base == string(filepath.Separator) {
			return fmt.Errorf("process runtime: mount target %q has no usable name", m.Target)
		}
		if _, reserved := reservedInstanceFiles[base]; reserved {
			return fmt.Errorf("process runtime: mount target %q collides with the instance's own %s", m.Target, base)
		}

		info, err := os.Stat(m.Source)
		if err != nil {
			return fmt.Errorf("process runtime: mount source %s: %w", m.Source, err)
		}

		dest := filepath.Join(instDir, base)
		if info.IsDir() {
			if err := os.Symlink(m.Source, dest); err != nil {
				return fmt.Errorf("process runtime: link mount %s: %w", m.Target, err)
			}
			continue
		}
		if err := copyFile(m.Source, dest); err != nil {
			return fmt.Errorf("process runtime: copy mount %s: %w", m.Target, err)
		}
	}
	return nil
}

// copyFile copies src to dst with 0644. Mounted files are config/data, not
// executables — the artifact binary has its own copy path in the watcher.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(0o644)
}
