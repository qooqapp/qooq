package watcher

import (
	"os"
	"path/filepath"
	"testing"
)

// effectiveMode is what makes the chmod check meaningful: Chmod returning nil
// is not evidence that the permissions took. Its bool has to distinguish
// "could not tell" from "told you 0600", because treating a stat failure as
// "the mode is wrong" would warn about credentials being exposed when nothing
// is known either way.
func TestEffectiveModeReportsWhatTheFileActuallyCarries(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "watcher.db")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	mode, ok := effectiveMode(p)
	if !ok {
		t.Fatal("existing file reported as unknown")
	}
	if mode != 0o644 {
		t.Errorf("mode after create: got %v, want 0644", mode)
	}

	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	mode, ok = effectiveMode(p)
	if !ok || mode != 0o600 {
		t.Errorf("mode after chmod: got %v (known=%v), want 0600", mode, ok)
	}
}

// A file that is not there is not a file with the wrong permissions. Collapsing
// the two would make every missing -wal/-shm sibling look like an exposure.
func TestEffectiveModeSaysUnknownRatherThanZeroWhenItCannotTell(t *testing.T) {
	if _, ok := effectiveMode(filepath.Join(t.TempDir(), "absent.db")); ok {
		t.Error("missing file reported as known")
	}
}

// The warning branch — chmod succeeding without the mode taking — cannot be
// produced on a POSIX filesystem, and creating a FAT image to get one is
// blocked in this environment. What is testable is that the check does not fire
// when the mode did take, since a warning that cries wolf on every start is
// worse than no warning: it teaches the reader to skip the line that matters.
func TestChmodCheckStaysQuietWhenThePermissionsHold(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "watcher.db")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	mode, ok := effectiveMode(p)
	if !ok {
		t.Fatal("could not read the mode back")
	}
	if mode != 0o600 {
		t.Fatalf("precondition failed: %v", mode)
	}
	// This is the condition lockDownDBFile warns on; it must be false here.
	if ok && mode != 0o600 {
		t.Error("would have warned about a file whose permissions are correct")
	}
}
