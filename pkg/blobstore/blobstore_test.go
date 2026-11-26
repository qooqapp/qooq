package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPutIsAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	s := NewLocal(dir)

	want := errors.New("the snapshot failed halfway")
	err := s.Put(context.Background(), "qooq-20260101-000000.db", func(w io.Writer) error {
		_, _ = w.Write([]byte("half a database"))
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("Put error = %v, want the fill error through", err)
	}

	entries, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed Put left %d blobs behind: a truncated snapshot named like a whole one is worse than none, because it will be trusted", len(entries))
	}
	// Nor may the temporary file survive under a name a human would mistake
	// for a backup.
	left, _ := os.ReadDir(dir)
	for _, e := range left {
		t.Fatalf("leftover file %q after a failed Put", e.Name())
	}
}

func TestPutThenList(t *testing.T) {
	s := NewLocal(t.TempDir())
	ctx := context.Background()
	for _, k := range []string{"qooq-20260101-000000.db", "qooq-20260102-000000.db", "other.txt"} {
		if err := s.Put(ctx, k, func(w io.Writer) error { _, err := w.Write([]byte(k)); return err }); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}
	all, err := s.List(ctx, "qooq-")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d entries for prefix qooq-, want 2 (other.txt must not match)", len(all))
	}
	if all[0].Key > all[1].Key {
		t.Fatal("List must be sorted by key: Prune's ordering depends on it")
	}
	if all[0].Size == 0 {
		t.Fatal("Size was not reported")
	}
}

func TestModeIsRestrictive(t *testing.T) {
	dir := t.TempDir()
	s := NewLocal(dir)
	if err := s.Put(context.Background(), "secret.db", func(w io.Writer) error { return nil }); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fi, err := os.Stat(filepath.Join(dir, "secret.db"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600: a watcher snapshot holds every user's git credentials", fi.Mode().Perm())
	}
}

// TestAKeyCannotEscapeTheStore: keys are qooq's own, so this is defence in
// depth — but a store whose keys can address the whole filesystem is not a
// store, and the next caller may not be qooq's own code.
func TestAKeyCannotEscapeTheStore(t *testing.T) {
	dir := t.TempDir()
	s := NewLocal(filepath.Join(dir, "store"))
	for _, bad := range []string{"../escaped", "/etc/passwd", "a/../../escaped", ""} {
		err := s.Put(context.Background(), bad, func(w io.Writer) error { return nil })
		if err == nil {
			t.Fatalf("key %q was accepted", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped")); err == nil {
		t.Fatal("a rejected key still wrote a file outside the store")
	}
}

func TestPutFileMovesItIn(t *testing.T) {
	dir := t.TempDir()
	s := NewLocal(filepath.Join(dir, "store"))
	src := filepath.Join(dir, "vacuumed.db")
	if err := os.WriteFile(src, []byte("snapshot"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := s.PutFile(context.Background(), "watcher-20260101-000000.db", src); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("PutFile left the caller's temporary file behind")
	}
	got, err := os.ReadFile(filepath.Join(dir, "store", "watcher-20260101-000000.db"))
	if err != nil {
		t.Fatalf("read stored: %v", err)
	}
	if string(got) != "snapshot" {
		t.Fatalf("stored %q, want %q", got, "snapshot")
	}
}

func TestPruneKeepsTheNewest(t *testing.T) {
	s := NewLocal(t.TempDir())
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		k := fmt.Sprintf("qooq-2026010%d-000000.db", i)
		if err := s.Put(ctx, k, func(w io.Writer) error { return nil }); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := Prune(ctx, s, "qooq-", 2); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	left, err := s.List(ctx, "qooq-")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(left) != 2 {
		t.Fatalf("%d left, want 2", len(left))
	}
	if left[0].Key != "qooq-20260104-000000.db" || left[1].Key != "qooq-20260105-000000.db" {
		t.Fatalf("kept %v, want the two newest", []string{left[0].Key, left[1].Key})
	}
}

// TestPruneKeepingNothingDeletesNothing: keep comes from configuration, and a
// zero that reached here by a misread config must not be the one instruction
// that erases every backup in the store.
func TestPruneKeepingNothingDeletesNothing(t *testing.T) {
	s := NewLocal(t.TempDir())
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		_ = s.Put(ctx, fmt.Sprintf("qooq-2026010%d-000000.db", i), func(w io.Writer) error { return nil })
	}
	for _, keep := range []int{0, -1} {
		if err := Prune(ctx, s, "qooq-", keep); err != nil {
			t.Fatalf("Prune(keep=%d): %v", keep, err)
		}
		left, _ := s.List(ctx, "qooq-")
		if len(left) != 3 {
			t.Fatalf("keep=%d deleted backups: %d left, want 3", keep, len(left))
		}
	}
}

func TestListOnAMissingDirectoryIsEmptyNotAnError(t *testing.T) {
	s := NewLocal(filepath.Join(t.TempDir(), "never-created"))
	got, err := s.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List on a missing root should be empty, not an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d entries", len(got))
	}
}

func TestDeleteMissingSaysNotFound(t *testing.T) {
	s := NewLocal(t.TempDir())
	err := s.Delete(context.Background(), "nope.db")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete of a missing key = %v, want ErrNotFound so callers can tell absent from broken", err)
	}
}

// TestTheFinalNameOnlyAppearsWhenComplete is the property tmp-and-rename
// actually buys, and the one the all-or-nothing test above cannot reach: while
// the bytes are still being written, nothing is visible under the finished
// name. A reader — prune, an operator, a future uploader — must never be able
// to see a partial blob as a whole one.
//
// Note what is still not covered: a process killed mid-write. The error path
// cleans up either way, so only this mid-flight check distinguishes writing
// through a temporary name from writing straight to the final one.
func TestTheFinalNameOnlyAppearsWhenComplete(t *testing.T) {
	s := NewLocal(t.TempDir())
	ctx := context.Background()
	const key = "qooq-20260101-000000.db"

	err := s.Put(ctx, key, func(w io.Writer) error {
		if _, err := w.Write([]byte("first half")); err != nil {
			return err
		}
		// Mid-write: the store must not admit to having this blob yet.
		entries, err := s.List(ctx, "")
		if err != nil {
			return err
		}
		for _, e := range entries {
			if e.Key == key {
				return fmt.Errorf("the finished name %q was visible while it was still being written", key)
			}
		}
		_, err = w.Write([]byte(" second half"))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.List(ctx, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Key != key {
		t.Fatalf("after Put: %v, want just %q", got, key)
	}
}
