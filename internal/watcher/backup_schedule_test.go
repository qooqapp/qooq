package watcher

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The watcher's scheduled backup had no test before it was moved onto
// pkg/blobstore, and moving it also fixed something: it used to VACUUM INTO the
// final filename directly, so an interrupted snapshot left a truncated file
// named exactly like a finished backup — prune would keep it and whoever
// needed it would restore from it. The snapshot now lands on a temporary name
// and is only renamed once it is whole.

func TestScheduledBackupWritesACompleteFile(t *testing.T) {
	w := testWatcherWithDB(t)
	dir := t.TempDir()

	if err := w.writeScheduledBackup(dir); err != nil {
		t.Fatalf("writeScheduledBackup: %v", err)
	}
	names := watcherBackupNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("got %v, want one backup", names)
	}
	fi, err := os.Stat(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("the snapshot is empty")
	}
	// A sqlite file starts with this; it proves VACUUM INTO really ran rather
	// than an empty file being created and renamed.
	head := make([]byte, 15)
	f, _ := os.Open(filepath.Join(dir, names[0]))
	defer f.Close()
	_, _ = f.Read(head)
	if !strings.HasPrefix(string(head), "SQLite format 3") {
		t.Fatalf("snapshot does not look like a sqlite database: %q", head)
	}
}

// TestNoTemporaryFileIsLeftBehind pins what it can actually see: a successful
// backup leaves no scratch file in the directory, so repeated backups do not
// accumulate one per run alongside the real ones.
//
// It deliberately does not claim to prove crash-safety. Writing to a temporary
// name and renaming is what makes an interrupted VACUUM invisible, but a unit
// test cannot kill the process mid-snapshot, and the error path cleans up
// either way — so this test passes with or without the temporary name. The
// property is real; this is not the thing that proves it.
func TestNoTemporaryFileIsLeftBehind(t *testing.T) {
	w := testWatcherWithDB(t)
	dir := t.TempDir()
	if err := w.writeScheduledBackup(dir); err != nil {
		t.Fatalf("writeScheduledBackup: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".vacuum") || strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("a temporary file survived a successful backup: %q", e.Name())
		}
	}
}

func TestPruneWatcherBackupsKeepsTheNewest(t *testing.T) {
	dir := t.TempDir()
	for _, stamp := range []string{"20260101-000000", "20260102-000000", "20260103-000000"} {
		if err := os.WriteFile(filepath.Join(dir, backupPrefix+stamp+backupSuffix), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// Not ours, must survive.
	if err := os.WriteFile(filepath.Join(dir, "operator-notes.txt"), []byte("mine"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if err := pruneWatcherBackups(dir, 1); err != nil {
		t.Fatalf("pruneWatcherBackups: %v", err)
	}
	names := watcherBackupNames(t, dir)
	if len(names) != 1 || names[0] != backupPrefix+"20260103-000000"+backupSuffix {
		t.Fatalf("kept %v, want only the newest", names)
	}
	if _, err := os.Stat(filepath.Join(dir, "operator-notes.txt")); err != nil {
		t.Fatal("prune deleted a file that was not a backup")
	}
}

func watcherBackupNames(t *testing.T, dir string) []string {
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
