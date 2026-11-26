package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These two functions had no tests before they were moved onto pkg/blobstore,
// which is the wrong order: a refactor of untested code is a rewrite with a
// hopeful name. What matters is that a failed snapshot leaves nothing a human
// would mistake for a backup, and that prune keeps the newest rather than
// whatever the filesystem happens to list first.

type fakeSource struct {
	payload string
	err     error
}

func (f fakeSource) Backup(w io.Writer, _ func(int64)) error {
	if _, err := io.WriteString(w, f.payload); err != nil {
		return err
	}
	return f.err
}

func TestWriteBackupStoresTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := writeBackup(fakeSource{payload: "bbolt bytes"}, dir); err != nil {
		t.Fatalf("writeBackup: %v", err)
	}
	names := backupNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("got %v, want one backup", names)
	}
	got, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "bbolt bytes" {
		t.Fatalf("content = %q", got)
	}
	fi, _ := os.Stat(filepath.Join(dir, names[0]))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

// TestAFailedSnapshotLeavesNothing is the property worth having. A truncated
// file carrying a real backup's name is worse than no file, because the next
// person to need it will restore from it.
func TestAFailedSnapshotLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	err := writeBackup(fakeSource{payload: "half", err: errors.New("disk went away")}, dir)
	if err == nil {
		t.Fatal("writeBackup reported success for a failed snapshot")
	}
	if names := backupNames(t, dir); len(names) != 0 {
		t.Fatalf("a failed snapshot left %v behind", names)
	}
	left, _ := os.ReadDir(dir)
	for _, e := range left {
		t.Fatalf("leftover file %q", e.Name())
	}
}

func TestPruneBackupsKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	for _, stamp := range []string{"20260101-000000", "20260102-000000", "20260103-000000"} {
		write(t, filepath.Join(dir, backupPrefix+stamp+backupSuffix), "x")
	}
	// Files that are not ours must survive: this directory may be the
	// operator's, not qooq's alone.
	write(t, filepath.Join(dir, "notes.txt"), "mine")

	if err := pruneBackups(dir, 2); err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}
	names := backupNames(t, dir)
	if len(names) != 2 || names[0] != backupPrefix+"20260102-000000"+backupSuffix {
		t.Fatalf("kept %v, want the two newest", names)
	}
	if _, err := os.Stat(filepath.Join(dir, "notes.txt")); err != nil {
		t.Fatal("prune deleted a file that was not a backup")
	}
}

func TestPruneBackupsKeepingZeroDeletesNothing(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, backupPrefix+"20260101-000000"+backupSuffix), "x")
	if err := pruneBackups(dir, 0); err != nil {
		t.Fatalf("pruneBackups: %v", err)
	}
	if len(backupNames(t, dir)) != 1 {
		t.Fatal("keep=0 erased the only backup there was")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func backupNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), backupPrefix) && strings.HasSuffix(e.Name(), backupSuffix) {
			out = append(out, e.Name())
		}
	}
	return out
}
