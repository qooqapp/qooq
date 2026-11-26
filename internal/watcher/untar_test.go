package watcher

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tarEntry describes one entry of a test archive.
type tarEntry struct {
	name     string
	typ      byte
	body     string
	mode     int64
	link     string
	size     int64 // overrides len(body) in the header when non-zero
	mtime    time.Time
	uid, gid int
}

func file(name, body string) tarEntry {
	return tarEntry{name: name, typ: tar.TypeReg, body: body, mode: 0o644}
}
func dirEntry(name string) tarEntry { return tarEntry{name: name, typ: tar.TypeDir, mode: 0o755} }
func symlink(name, to string) tarEntry {
	return tarEntry{name: name, typ: tar.TypeSymlink, link: to, mode: 0o777}
}

func makeTarGz(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		size := int64(len(e.body))
		if e.size != 0 {
			size = e.size
		}
		if e.typ != tar.TypeReg {
			size = 0
		}
		mt := e.mtime
		if mt.IsZero() {
			mt = time.Unix(1_700_000_000, 0)
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Size: size, Linkname: e.link,
			ModTime: mt, Uid: e.uid, Gid: e.gid, Format: tar.FormatPAX}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write header %q: %v", e.name, err)
		}
		if e.typ == tar.TypeReg && e.size == 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// extract runs extractUpload into a fresh dest under a parent the test owns,
// so "dest was removed" and "nothing escaped" are both checkable.
func extract(t *testing.T, archive []byte) (dest string, rev string, err error) {
	t.Helper()
	dest = filepath.Join(t.TempDir(), "src")
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	rev, err = extractUpload(bytes.NewReader(archive), dest)
	return dest, rev, err
}

func TestExtractUploadWritesTheTree(t *testing.T) {
	exe := file("bin/run.sh", "#!/bin/sh\necho hi\n")
	exe.mode = 0o755
	archive := makeTarGz(t,
		dirEntry("./"),
		file("./index.html", "<h1>hi</h1>"),
		exe,
		symlink("latest", "bin/run.sh"),
		symlink("nested/up", "../index.html"),
	)
	dest, rev, err := extract(t, archive)
	if err != nil {
		t.Fatalf("extractUpload: %v", err)
	}
	if len(rev) != 64 {
		t.Fatalf("revision %q is not a sha256 hex", rev)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "index.html")); string(b) != "<h1>hi</h1>" {
		t.Fatalf("index.html = %q", b)
	}
	info, err := os.Stat(filepath.Join(dest, "bin/run.sh"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("bin/run.sh mode = %v, %v; want 0755", info.Mode(), err)
	}
	if info, _ := os.Stat(filepath.Join(dest, "index.html")); info.Mode().Perm() != 0o644 {
		t.Fatalf("index.html mode = %v; want 0644", info.Mode())
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "nested/up")); string(b) != "<h1>hi</h1>" {
		t.Fatalf("nested/up should read through to index.html, got %q", b)
	}
}

// The revision is what makes "the same upload again" a no-op, so it must not
// depend on anything a packer varies from one run to the next.
func TestUploadRevisionIgnoresIncidentals(t *testing.T) {
	base := func() []tarEntry {
		exe := file("run", "x")
		exe.mode = 0o755
		return []tarEntry{file("a.txt", "A"), file("dir/b.txt", "B"), exe, symlink("l", "a.txt")}
	}
	_, want, err := extract(t, makeTarGz(t, base()...))
	if err != nil {
		t.Fatal(err)
	}

	// Different mtime, owner, non-exec permission bits, order, and explicit
	// directory entries — none of which is content.
	e := base()
	for i := range e {
		e[i].mtime = time.Unix(1_800_000_000+int64(i), 0)
		e[i].uid, e[i].gid = 1000+i, 2000+i
	}
	e[0].mode = 0o600 // a.txt: other bits differ, still not executable
	e[2].mode = 0o700 // run: different bits, still executable
	shuffled := []tarEntry{dirEntry("dir/"), e[3], e[2], e[1], dirEntry("empty/"), e[0]}
	_, got, err := extract(t, makeTarGz(t, shuffled...))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("same files, different incidentals: revision %s != %s", got, want)
	}

	// And it does move for each thing that IS content.
	changes := map[string]func([]tarEntry) []tarEntry{
		"content":   func(e []tarEntry) []tarEntry { e[0].body = "A2"; return e },
		"path":      func(e []tarEntry) []tarEntry { e[1].name = "dir/c.txt"; return e },
		"exec bit":  func(e []tarEntry) []tarEntry { e[0].mode = 0o755; return e },
		"link":      func(e []tarEntry) []tarEntry { e[3].link = "dir/b.txt"; return e },
		"new file":  func(e []tarEntry) []tarEntry { return append(e, file("z", "")) },
		"file→link": func(e []tarEntry) []tarEntry { e[1] = symlink("dir/b.txt", "../a.txt"); return e },
	}
	for name, change := range changes {
		_, got, err := extract(t, makeTarGz(t, change(base())...))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == want {
			t.Errorf("%s changed but the revision did not", name)
		}
	}
}

func TestExtractUploadRefusesUnsafeEntries(t *testing.T) {
	outside := t.TempDir() // where an escape would land, checked afterwards
	hardlink := tarEntry{name: "h", typ: tar.TypeLink, link: "a"}
	fifo := tarEntry{name: "p", typ: tar.TypeFifo}
	chardev := tarEntry{name: "c", typ: tar.TypeChar}
	blockdev := tarEntry{name: "b", typ: tar.TypeBlock}

	cases := map[string][]tarEntry{
		"parent traversal":          {file("../evil", "x")},
		"nested parent traversal":   {file("a/../../evil", "x")},
		"dot-dot even if it stays":  {file("a/../b", "x")},
		"absolute path":             {file("/etc/x", "x")},
		"absolute into test dir":    {file(filepath.Join(outside, "x"), "x")},
		"symlink absolute":          {symlink("l", "/etc/passwd")},
		"symlink out":               {symlink("l", "../x")},
		"symlink out from subdir":   {symlink("a/l", "../../x")},
		"symlink chain escape":      {symlink("d", "."), symlink("x", "d/..")},
		"dangling chain escape":     {symlink("d", "."), symlink("x", "d/../nope")},
		"symlink loop":              {symlink("a", "b"), symlink("b", "a")},
		"write through symlink out": {symlink("d", outside), file("d/pwned", "x")},
		"write through symlink in":  {dirEntry("sub/"), symlink("d", "sub"), file("d/f", "x")},
		"hardlink":                  {file("a", "x"), hardlink},
		"fifo":                      {fifo},
		"char device":               {chardev},
		"block device":              {blockdev},
		"duplicate path":            {file("a", "1"), file("a", "2")},
		"file then link over it":    {file("a", "1"), symlink("a", "b")},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			dest, _, err := extract(t, makeTarGz(t, entries...))
			if !errors.Is(err, errUnsafeArchive) {
				// Refused for the stated reason, not by some unrelated failure
				// that a later change could make go away.
				t.Fatalf("err = %v, want errUnsafeArchive", err)
			}
			if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
				t.Fatalf("a refused archive left %s behind (stat err %v)", dest, statErr)
			}
		})
	}
	if left, _ := os.ReadDir(outside); len(left) != 0 {
		t.Fatalf("something was written outside the destination: %v", left)
	}
}

// withUploadLimits lowers the size limits for one test.
func withUploadLimits(t *testing.T, compressed, unpacked int64, entries int) {
	t.Helper()
	oc, ou, oe, oo := maxUploadCompressedBytes, maxUploadUnpackedBytes, maxUploadEntries, maxUploadStreamOverhead
	maxUploadCompressedBytes, maxUploadUnpackedBytes, maxUploadEntries = compressed, unpacked, entries
	maxUploadStreamOverhead = 64 << 10
	t.Cleanup(func() {
		maxUploadCompressedBytes, maxUploadUnpackedBytes, maxUploadEntries, maxUploadStreamOverhead = oc, ou, oe, oo
	})
}

func TestExtractUploadLimits(t *testing.T) {
	t.Run("too many entries", func(t *testing.T) {
		withUploadLimits(t, 1<<20, 1<<20, 5)
		var es []tarEntry
		for i := 0; i < 6; i++ {
			es = append(es, file(strings.Repeat("f", i+1), ""))
		}
		dest, _, err := extract(t, makeTarGz(t, es...))
		if !errors.Is(err, errUploadTooLarge) {
			t.Fatalf("err = %v, want errUploadTooLarge", err)
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Fatal("left the half-extracted tree behind")
		}
	})
	t.Run("exactly at the entry limit", func(t *testing.T) {
		withUploadLimits(t, 1<<20, 1<<20, 5)
		var es []tarEntry
		for i := 0; i < 5; i++ {
			es = append(es, file(strings.Repeat("f", i+1), ""))
		}
		if _, _, err := extract(t, makeTarGz(t, es...)); err != nil {
			t.Fatalf("5 entries with a limit of 5: %v", err)
		}
	})
	t.Run("unpacked total", func(t *testing.T) {
		withUploadLimits(t, 1<<20, 1000, 100)
		dest, _, err := extract(t, makeTarGz(t, file("a", strings.Repeat("x", 600)), file("b", strings.Repeat("y", 600))))
		if !errors.Is(err, errUploadTooLarge) {
			t.Fatalf("err = %v, want errUploadTooLarge", err)
		}
		if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
			t.Fatal("left the half-extracted tree behind")
		}
	})
	t.Run("compressed size", func(t *testing.T) {
		withUploadLimits(t, 512, 10<<20, 100)
		// Random content does not compress, so the gzip stream is large.
		noise := make([]byte, 8000)
		if _, err := rand.Read(noise); err != nil {
			t.Fatal(err)
		}
		_, _, err := extract(t, makeTarGz(t, file("a", string(noise))))
		if !errors.Is(err, errUploadTooLarge) {
			t.Fatalf("err = %v, want errUploadTooLarge", err)
		}
	})
	t.Run("gzip bomb", func(t *testing.T) {
		// 64 MiB of zeros is ~64 KiB compressed: well under the compressed
		// limit, far over the unpacked one. It must stop at the limit, not
		// after decompressing everything.
		withUploadLimits(t, 1<<20, 1<<20, 100)
		archive := makeTarGz(t, file("zeros", string(make([]byte, 64<<20))))
		if int64(len(archive)) > maxUploadCompressedBytes {
			t.Fatalf("test archive is %d bytes compressed, not a bomb", len(archive))
		}
		_, _, err := extract(t, archive)
		if !errors.Is(err, errUploadTooLarge) {
			t.Fatalf("err = %v, want errUploadTooLarge", err)
		}
	})
}

func TestExtractUploadRefusesNonGzipAndNonEmptyDest(t *testing.T) {
	if _, _, err := extract(t, []byte("not gzip at all")); err == nil {
		t.Fatal("accepted a non-gzip body")
	}

	dest := t.TempDir()
	keep := filepath.Join(dest, "precious")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := extractUpload(bytes.NewReader(makeTarGz(t, file("a", "1"))), dest); err == nil {
		t.Fatal("extracted into a non-empty directory")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("refusing a non-empty destination removed what was in it: %v", err)
	}
}
