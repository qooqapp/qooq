package buildctx

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TarDir is what turns a cloned repo into something the docker daemon will
// build, and it is imported by two repos. Its failure mode is quiet: a file
// that stops being included produces a build error about a missing file
// somewhere deep in a Dockerfile, and an included .git turns a small context
// into a large one for no benefit.

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return dir
}

// tarEntries returns name -> content for regular files, plus the set of
// directory entries seen.
func tarEntries(t *testing.T, r io.Reader) (map[string]string, map[string]bool) {
	t.Helper()
	files := map[string]string{}
	dirs := map[string]bool{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return files, dirs
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			dirs[hdr.Name] = true
		case tar.TypeReg:
			body, err := io.ReadAll(tr)
			if err != nil {
				t.Fatalf("read %s: %v", hdr.Name, err)
			}
			files[hdr.Name] = string(body)
		}
	}
}

func TestTarDirIncludesTheTreeWithSlashPaths(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"Dockerfile":      "FROM scratch\n",
		"cmd/app/main.go": "package main\n",
		"web/index.html":  "<h1>hi</h1>",
	})

	var buf bytes.Buffer
	if err := TarDir(dir, &buf); err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	files, dirs := tarEntries(t, &buf)

	for name, want := range map[string]string{
		"Dockerfile":      "FROM scratch\n",
		"cmd/app/main.go": "package main\n",
		"web/index.html":  "<h1>hi</h1>",
	} {
		got, ok := files[name]
		if !ok {
			t.Fatalf("%s missing from the context (have %v)", name, files)
		}
		if got != want {
			t.Errorf("%s content: got %q, want %q", name, got, want)
		}
	}
	// Directories are entries too, and tar paths are always slash-separated —
	// a Windows-style separator here produces a context docker cannot use.
	if !dirs["cmd/"] || !dirs["cmd/app/"] {
		t.Errorf("directory entries missing: %v", dirs)
	}
}

func TestTarDirExcludesGit(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"Dockerfile":         "FROM scratch\n",
		".git/config":        "[core]\n",
		".git/objects/ab/cd": "binary",
		// A file whose name merely starts with .git is not the git directory.
		".gitignore": "node_modules\n",
	})

	var buf bytes.Buffer
	if err := TarDir(dir, &buf); err != nil {
		t.Fatalf("TarDir: %v", err)
	}
	files, dirs := tarEntries(t, &buf)

	for name := range files {
		if name == ".git" || len(name) > 5 && name[:5] == ".git/" {
			t.Errorf("%s should not be in the build context", name)
		}
	}
	if dirs[".git/"] {
		t.Error(".git directory entry should not be in the build context")
	}
	if _, ok := files[".gitignore"]; !ok {
		t.Error(".gitignore is a normal file and belongs in the context")
	}
}

func TestTarDirEmptyDirectory(t *testing.T) {
	var buf bytes.Buffer
	if err := TarDir(t.TempDir(), &buf); err != nil {
		t.Fatalf("an empty directory is a valid, if useless, context: %v", err)
	}
	files, _ := tarEntries(t, &buf)
	if len(files) != 0 {
		t.Errorf("expected no entries, got %v", files)
	}
}

func TestTarDirMissingDirectoryErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := TarDir(filepath.Join(t.TempDir(), "not-there"), &buf); err == nil {
		t.Fatal("a missing context directory must be an error, not an empty build")
	}
}

func TestTarFilesPacksOnlyTheListedFilesSortedWithSymlinksAsLinks(t *testing.T) {
	dir := writeTree(t, map[string]string{
		"b.txt": "b", "a.txt": "a", "skip.txt": "no", "sub/c.sh": "#!/bin/sh\n",
	})
	if err := os.Chmod(filepath.Join(dir, "sub/c.sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A link pointing outside the tree is packed as a link; its target's
	// contents must never appear in the archive.
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret"), []byte("TOPSECRET"), 0o644)
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "lnk")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := TarFiles(dir, []string{"sub/c.sh", "b.txt", "lnk", "a.txt", "a.txt"}, &buf); err != nil {
		t.Fatalf("TarFiles: %v", err)
	}
	tr := tar.NewReader(&buf)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
		switch h.Name {
		case "lnk":
			if h.Typeflag != tar.TypeSymlink {
				t.Errorf("lnk is type %c, want a symlink", h.Typeflag)
			}
		case "sub/c.sh":
			if h.Mode&0o111 == 0 {
				t.Errorf("the execute bit was lost: %o", h.Mode)
			}
		}
		if body, _ := io.ReadAll(tr); strings.Contains(string(body), "TOPSECRET") {
			t.Errorf("%s carries the content a link points at", h.Name)
		}
	}
	want := []string{"a.txt", "b.txt", "lnk", "sub/c.sh"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want %v", names, want)
	}
}

func TestTarFilesRefusesAPathOutsideTheTree(t *testing.T) {
	dir := writeTree(t, map[string]string{"a": "a"})
	for _, bad := range []string{"../a", "/etc/passwd", "x/../a", "."} {
		if err := TarFiles(dir, []string{bad}, io.Discard); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
