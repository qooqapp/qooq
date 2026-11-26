package watcher

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// maxEnvFileBytes caps how large an env file the watcher will read. Like
// maxManifestBytes, this is a hand-written config file — the cap only exists
// so a corrupted or hostile repo cannot make the sweep loop read something
// enormous once per poll.
const maxEnvFileBytes = 1 << 20 // 1 MiB

// parseEnvFile reads a dotenv-style file: KEY=VALUE per line, blank lines and
// # comments skipped, an optional "export " prefix tolerated (people paste
// these straight out of shell scripts), and surrounding single or double
// quotes stripped from the value.
//
// No expansion happens here — neither shell ${VAR} nor qooq's own
// interpolation. An env file is data, and a value that reads as ${...} in the
// file is meant to reach the app as those literal characters. Interpolation is
// something the manifest opts into, per value.
func parseEnvFile(path string) (map[string]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size() > maxEnvFileBytes {
		return nil, fmt.Errorf("%s is %d bytes, over the %d byte limit", path, info.Size(), maxEnvFileBytes)
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]string)
	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE, got %q", path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, lineNo)
		}
		out[key] = unquoteEnvValue(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// unquoteEnvValue strips one layer of matching quotes. Anything else — inner
// quotes, escapes, trailing comments — is left alone: guessing at shell
// semantics in a file that is not run by a shell causes more surprises than it
// prevents.
func unquoteEnvValue(v string) string {
	if len(v) < 2 {
		return v
	}
	first, last := v[0], v[len(v)-1]
	if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
		return v[1 : len(v)-1]
	}
	return v
}

// resolveEnvFiles reads every entry of run.env_file, in order, and merges them
// into one map where a later file wins over an earlier one — the same
// last-one-wins rule shells use for repeated assignments.
//
// Sources are classified exactly like a mount's, so an env file can live in
// the repo, at an absolute host path, or inside a named volume
// ("appdata:/config/app.env"). That last form is why this reuses
// parseMountEntry: an env file the operator drops into a volume is a normal
// enough arrangement that it should not need its own syntax.
func (m *Watcher) resolveEnvFiles(ref componentRef, cloneDir string, entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string)
	for _, entry := range entries {
		path, err := m.resolveEnvFilePath(ref, cloneDir, entry)
		if err != nil {
			return nil, err
		}
		vars, err := parseEnvFile(path)
		if err != nil {
			return nil, fmt.Errorf("env_file %q: %w", entry, err)
		}
		for k, v := range vars {
			out[k] = v
		}
	}
	return out, nil
}

// resolveEnvFilePath turns one env_file entry into a readable path. The
// "volume:/path/inside" form addresses a file inside a named volume; every
// other form is a plain repo-relative or absolute path.
func (m *Watcher) resolveEnvFilePath(ref componentRef, cloneDir, entry string) (string, error) {
	if volume, rel, ok := strings.Cut(entry, ":"); ok && classifyMountSource(volume) == mountSourceVolume {
		if !validTargetPath(strings.TrimPrefix(rel, "/")) {
			return "", fmt.Errorf("env_file %q: path inside volume %q escapes it", entry, volume)
		}
		return resolveWithin(m.volumePath(ref, volume), strings.TrimPrefix(rel, "/"))
	}

	switch classifyMountSource(entry) {
	case mountSourceHost:
		if err := m.mountsPolicy.checkHostPath(entry); err != nil {
			return "", fmt.Errorf("env_file %q: %w", entry, err)
		}
		return entry, nil
	case mountSourceVolume:
		return "", fmt.Errorf("env_file %q: a bare name is a volume, not a file — use \"%s:/path/to/file\"", entry, entry)
	default:
		if !validTargetPath(entry) {
			return "", fmt.Errorf("env_file %q must stay inside the repo", entry)
		}
		path, err := resolveWithin(cloneDir, entry)
		if err != nil {
			return "", fmt.Errorf("env_file %q: %w", entry, err)
		}
		return path, nil
	}
}
