package watcher

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// Limits on an uploaded source archive.
//
// Exceeding any of them rejects the whole upload. Truncating instead would
// deploy something that is not what was sent, and the difference would show up
// as a build failure about a missing file, far from its cause.
//
// They are vars rather than consts only so tests can lower them; product code
// must not write them.
var (
	maxUploadCompressedBytes int64 = 100 << 20 // 100 MiB, what the client sends
	maxUploadUnpackedBytes   int64 = 500 << 20 // 500 MiB, sum of file contents
	maxUploadEntries               = 50_000

	// maxUploadStreamOverhead bounds the decompressed bytes that are NOT file
	// contents — tar headers, padding, PAX records. 50,000 entries cost at
	// most ~1 KiB each in headers and padding, so this is generous; it exists
	// so a gzip bomb made of something other than file bodies (a run of
	// zero blocks, an endless PAX record) is cut off too.
	maxUploadStreamOverhead int64 = 64 << 20
)

// errUploadTooLarge marks a rejection for size, so the API can answer 413
// rather than 400: the archive is well-formed, there is just too much of it.
var errUploadTooLarge = errors.New("upload too large")

// errUnsafeArchive marks a rejection for content that may not be extracted —
// an escaping path, a device, a link — as opposed to a corrupt stream.
var errUnsafeArchive = errors.New("unsafe archive entry")

// uploadRevisionVersion prefixes the canonical form that is hashed, so the
// format can change later without a new revision colliding with an old one.
const uploadRevisionVersion = "qooq-upload-v1\n"

// uploadEntry is one file or symlink as it contributes to the revision.
type uploadEntry struct {
	path   string
	link   bool   // symlink rather than regular file
	exec   bool   // any execute bit set (files only)
	digest string // hex sha256 of the content (files) or the link target (symlinks)
}

// extractUpload unpacks a gzip'd tar from src into dest and returns the
// archive's revision.
//
// dest must exist and be empty. On ANY error dest is removed entirely: a
// half-extracted tree must never be mistaken for a source tree, and the caller
// is not trusted to remember.
//
// Rules (this is the one new attack surface of upload-as-source, so they are
// strict and spelled out):
//   - only regular files, directories and symlinks; hardlinks, devices,
//     fifos and anything else are refused;
//   - no absolute paths, no ".." component anywhere in a name;
//   - nothing is written through a symlink: an entry whose parent path
//     contains a symlink that the archive itself created is refused (that is
//     how "symlink dir -> /tmp, then write dir/file" escapes);
//   - every symlink, once everything is on disk, must resolve inside dest —
//     the same question resolveWithin answers for repo paths, but asked with
//     a resolver that also refuses dangling links pointing outside;
//   - a path may appear only once (later entries silently overwriting
//     earlier ones would make what gets hashed differ from what is built);
//   - the size/entry limits above.
//
// The revision is computed here, while the contents stream through, over a
// normalized form: entries sorted by path, each contributing only its path,
// its content hash, and (for files) whether it is executable. mtime, uid, gid,
// the other permission bits and the archive's own ordering are ignored, so the
// same files packed twice — on another day, by another machine, in another
// order — have the same revision, the way the same tree has the same git
// commit. Hashing the gzip bytes instead would change on every pack. Empty
// directories do not contribute: whether a packer emits directory entries at
// all is incidental, and git makes the same choice.
func extractUpload(src io.Reader, dest string) (revision string, err error) {
	// Checked before the cleanup below is armed: a directory that was not
	// empty is somebody else's, and removing it would be the worst possible
	// response to being handed the wrong path.
	if names, rerr := os.ReadDir(dest); rerr != nil {
		return "", fmt.Errorf("extract: %w", rerr)
	} else if len(names) > 0 {
		return "", errors.New("extract: destination is not empty")
	}
	defer func() {
		if err != nil {
			os.RemoveAll(dest)
		}
	}()

	compressed := &limitedReader{r: src, remaining: maxUploadCompressedBytes, what: "compressed archive"}
	gz, err := gzip.NewReader(compressed)
	if err != nil {
		return "", fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	// Bounds everything decompressed, file bodies and headers alike — the
	// per-file accounting below only sees bodies.
	stream := &limitedReader{r: gz, remaining: maxUploadUnpackedBytes + maxUploadStreamOverhead, what: "decompressed archive"}
	tr := tar.NewReader(stream)

	var (
		entries  []uploadEntry
		seen     = map[string]bool{}
		symlinks = map[string]bool{}
		unpacked int64
		count    int
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("read archive: %w", err)
		}
		count++
		if count > maxUploadEntries {
			return "", fmt.Errorf("%w: more than %d entries", errUploadTooLarge, maxUploadEntries)
		}

		name, skip, err := cleanArchivePath(hdr.Name)
		if err != nil {
			return "", err
		}
		if skip {
			continue
		}
		if seen[name] {
			return "", fmt.Errorf("%w: %q appears more than once", errUnsafeArchive, name)
		}
		seen[name] = true
		// Writing below a symlink this archive planted is the "symlink then
		// write through it" escape. Checked against what the archive created,
		// which in a fresh directory is every symlink there is.
		for dir := path.Dir(name); dir != "."; dir = path.Dir(dir) {
			if symlinks[dir] {
				return "", fmt.Errorf("%w: %q is inside symlink %q", errUnsafeArchive, name, dir)
			}
		}
		target := filepath.Join(dest, filepath.FromSlash(name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return "", fmt.Errorf("extract %q: %w", name, err)
			}
		case tar.TypeReg, tar.TypeRegA: //nolint:staticcheck // TypeRegA is what old packers write
			if hdr.Size < 0 || hdr.Size > maxUploadUnpackedBytes-unpacked {
				return "", fmt.Errorf("%w: contents exceed %d bytes unpacked", errUploadTooLarge, maxUploadUnpackedBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", fmt.Errorf("extract %q: %w", name, err)
			}
			exec := hdr.Mode&0o111 != 0
			mode := os.FileMode(0o644)
			if exec {
				mode = 0o755
			}
			// O_EXCL: never follow or reuse whatever is already at this path.
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return "", fmt.Errorf("extract %q: %w", name, err)
			}
			h := sha256.New()
			n, err := io.Copy(io.MultiWriter(f, h), tr)
			cerr := f.Close()
			if err != nil {
				return "", fmt.Errorf("extract %q: %w", name, err)
			}
			if cerr != nil {
				return "", fmt.Errorf("extract %q: %w", name, cerr)
			}
			unpacked += n
			entries = append(entries, uploadEntry{path: name, exec: exec, digest: hex.EncodeToString(h.Sum(nil))})
		case tar.TypeSymlink:
			if err := checkLinkTargetLexically(name, hdr.Linkname); err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return "", fmt.Errorf("extract %q: %w", name, err)
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return "", fmt.Errorf("extract %q: %w", name, err)
			}
			symlinks[name] = true
			sum := sha256.Sum256([]byte(hdr.Linkname))
			entries = append(entries, uploadEntry{path: name, link: true, digest: hex.EncodeToString(sum[:])})
		case tar.TypeLink:
			return "", fmt.Errorf("%w: %q is a hard link", errUnsafeArchive, name)
		default:
			return "", fmt.Errorf("%w: %q has unsupported type %q (only files, directories and symlinks)",
				errUnsafeArchive, name, string(hdr.Typeflag))
		}
	}

	// Now that every link exists, follow each one the way the kernel would —
	// through other links — and refuse any that lands outside dest. A link
	// that looks local on its own can still escape through another one
	// ("d -> ." then "x -> d/.."), which no per-entry check can see.
	for name := range symlinks {
		if err := linkStaysWithin(dest, name); err != nil {
			return "", err
		}
	}

	return uploadRevision(entries), nil
}

// uploadRevision hashes the normalized form described on extractUpload.
func uploadRevision(entries []uploadEntry) string {
	sorted := append([]uploadEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].path < sorted[j].path })
	h := sha256.New()
	io.WriteString(h, uploadRevisionVersion)
	for _, e := range sorted {
		kind, exec := "f", "-"
		if e.link {
			kind = "l"
		} else if e.exec {
			exec = "x"
		}
		// NUL cannot appear in a tar path, so it separates fields unambiguously.
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\n", kind, e.path, exec, e.digest)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// cleanArchivePath validates a tar entry name and returns it slash-separated
// and relative. skip is true for the archive's own root ("." or "./").
func cleanArchivePath(raw string) (name string, skip bool, err error) {
	if raw == "" {
		return "", false, fmt.Errorf("%w: empty name", errUnsafeArchive)
	}
	if strings.HasPrefix(raw, "/") {
		return "", false, fmt.Errorf("%w: %q is an absolute path", errUnsafeArchive, raw)
	}
	// Checked on the raw name, not after Clean: Clean would quietly turn
	// "a/../../x" into "../x" and "a/../b" into "b", and the second is still
	// an archive trying something.
	for _, part := range strings.Split(raw, "/") {
		if part == ".." {
			return "", false, fmt.Errorf("%w: %q contains \"..\"", errUnsafeArchive, raw)
		}
	}
	name = path.Clean(raw)
	if name == "." {
		return "", true, nil
	}
	if !filepath.IsLocal(filepath.FromSlash(name)) {
		return "", false, fmt.Errorf("%w: %q is not a local path", errUnsafeArchive, raw)
	}
	return name, false, nil
}

// checkLinkTargetLexically refuses a symlink whose target is absolute or
// climbs out of the root on its face. It is not sufficient alone (see
// linkStaysWithin), but it rejects the obvious cases with the clearest message
// and before anything is created.
func checkLinkTargetLexically(name, target string) error {
	if target == "" {
		return fmt.Errorf("%w: symlink %q has an empty target", errUnsafeArchive, name)
	}
	if path.IsAbs(target) {
		return fmt.Errorf("%w: symlink %q points to absolute path %q", errUnsafeArchive, name, target)
	}
	joined := path.Join(path.Dir(name), target)
	if joined == ".." || strings.HasPrefix(joined, "../") {
		return fmt.Errorf("%w: symlink %q points outside the upload (%q)", errUnsafeArchive, name, target)
	}
	return nil
}

// maxLinkHops bounds symlink chains, matching the kernel's own ELOOP limit.
const maxLinkHops = 40

// linkStaysWithin resolves the symlink at rel (relative to root) through every
// link along the way, treating root as "/", and reports an error if the walk
// ever climbs above root or reaches an absolute target.
//
// This is resolveWithin's question, but resolveWithin cannot answer it for a
// link whose target does not exist: filepath.EvalSymlinks fails, and its
// fallback resolves only the parent — so "x -> d/../nope" with "d -> ." would
// pass. Walking component by component keeps going past missing ones, which is
// what makes a dangling link that points outside fail too.
func linkStaysWithin(root, rel string) error {
	hops := 0
	var stack []string
	rest := strings.Split(rel, "/")
	for len(rest) > 0 {
		part := rest[0]
		rest = rest[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if len(stack) == 0 {
				return fmt.Errorf("%w: symlink %q resolves outside the upload", errUnsafeArchive, rel)
			}
			stack = stack[:len(stack)-1]
			continue
		}
		cur := append(append([]string(nil), stack...), part)
		fi, err := os.Lstat(filepath.Join(root, filepath.Join(cur...)))
		if err != nil || fi.Mode()&os.ModeSymlink == 0 {
			// A real entry, or one that does not exist: either way it is
			// a plain name from here on, and only ".." can climb.
			stack = cur
			continue
		}
		hops++
		if hops > maxLinkHops {
			return fmt.Errorf("%w: symlink %q: too many levels of links", errUnsafeArchive, rel)
		}
		target, err := os.Readlink(filepath.Join(root, filepath.Join(cur...)))
		if err != nil {
			return fmt.Errorf("extract: read symlink %q: %w", rel, err)
		}
		if path.IsAbs(target) {
			return fmt.Errorf("%w: symlink %q resolves to absolute path %q", errUnsafeArchive, rel, target)
		}
		// The link's target replaces it, relative to its own directory.
		rest = append(strings.Split(target, "/"), rest...)
	}
	return nil
}

// limitedReader is io.LimitReader that fails loudly instead of reporting a
// clean EOF at the limit — a silent EOF would turn "too large" into "corrupt
// archive", or worse, into a truncated tree that extracts fine.
type limitedReader struct {
	r         io.Reader
	remaining int64
	what      string
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining < 0 {
		return 0, fmt.Errorf("%w: %s exceeds its limit", errUploadTooLarge, l.what)
	}
	if int64(len(p)) > l.remaining+1 {
		p = p[:l.remaining+1]
	}
	n, err := l.r.Read(p)
	l.remaining -= int64(n)
	if l.remaining < 0 {
		return n, fmt.Errorf("%w: %s exceeds its limit", errUploadTooLarge, l.what)
	}
	return n, err
}
