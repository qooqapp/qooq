// Package blobstore names where qooq's own large, opaque files go.
//
// It exists to hold a seam, not to add a feature: the only implementation here
// is the local directory the callers already used, and wiring it changes no
// observable behaviour. Object storage (S3, GCS) then becomes one more
// implementation rather than a change at every call site.
//
// It is deliberately small and deliberately not a filesystem. A blob here is
// written once, listed, and deleted; there is no rename, no append, no
// directory tree, no permissions — because none of those survive the move to
// object storage, and an interface that promises them would be a local-only
// interface wearing a portable name.
//
// # What belongs here
//
// Backups. A snapshot stored on the same machine as the thing it protects is
// not a backup, which makes this the outlet where "put it somewhere else"
// actually matters.
//
// # What deliberately does not
//
// Container images: a registry already is the object store for those, and the
// watcher pushes to one when registry.prefix is set.
//
// Mounts and volumes: POSIX semantics. An app reads and writes those paths
// while it runs; object storage cannot stand in for that, and a component
// cannot move machines without them. That needs node affinity or network
// storage, which is a different problem.
//
// Build output: it lives in a database column (Target.LastBuildLog), latest
// only. Moving it here is the build-history feature, deliberately deferred
// because history is a disk-growth surface needing retention, accounting and
// a browsing UI. The interface will fit when that is wanted; it is not wired
// now, and pretending otherwise would be the behaviour change this package
// promises not to make.
//
// Process-runtime artifacts: these must end up as an *executable file on the
// node that runs them*, so their problem is not where the bytes live but how
// the node materializes them — cross-machine artifact delivery, which the
// roadmap already records as unanswered. Storing them through Put would move
// the bytes and leave the hard half untouched, so the write side stays
// filesystem code until the fetch side exists.
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// ErrNotFound is returned for a key that is not in the store. A caller
// distinguishing "absent" from "could not tell" must use errors.Is on this
// rather than matching on message text — pkg/client learned that one the hard
// way.
var ErrNotFound = errors.New("blobstore: not found")

// Entry describes one stored blob.
type Entry struct {
	Key     string
	Size    int64
	ModTime time.Time
}

// Store is where blobs go.
//
// Put takes a fill func rather than an io.Reader because the producers here
// write rather than read: bbolt's Tx.WriteTo and an HTTP response both want an
// io.Writer handed to them. An implementation must make the blob appear
// complete or not at all — a half-written snapshot that is named like a whole
// one is worse than a missing one, because it will be trusted.
//
// PutFile exists because some producers cannot write to an io.Writer at all:
// sqlite's VACUUM INTO takes a path and nothing else. Those callers write a
// temporary file and hand it over.
type Store interface {
	Put(ctx context.Context, key string, fill func(io.Writer) error) error
	PutFile(ctx context.Context, key, localPath string) error
	List(ctx context.Context, prefix string) ([]Entry, error)
	Delete(ctx context.Context, key string) error
}

// Local stores blobs as files under Root.
type Local struct {
	Root string
	// Mode is the permission for created files. Zero means 0600, which is what
	// the backup callers need: a snapshot of the watcher's database contains
	// every user's git credentials.
	Mode os.FileMode
}

// NewLocal returns a Store backed by a directory.
func NewLocal(root string) *Local { return &Local{Root: root} }

func (l *Local) mode() os.FileMode {
	if l.Mode == 0 {
		return 0o600
	}
	return l.Mode
}

// path maps a key to a file under Root, refusing anything that would land
// outside it. filepath.IsLocal rejects absolute paths and any ".." that
// escapes, which is the same string-level guard the watcher applies to
// manifest-supplied paths. Keys here come from qooq's own code rather than
// from a user, so this is defence in depth, not the only thing standing
// between a key and the filesystem.
func (l *Local) path(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("blobstore: empty key")
	}
	clean := filepath.FromSlash(key)
	if !filepath.IsLocal(clean) {
		return "", fmt.Errorf("blobstore: key %q escapes the store", key)
	}
	return filepath.Join(l.Root, clean), nil
}

// Put writes through a temporary file and renames it into place, so a crash or
// a failing fill leaves nothing behind that looks like a finished blob.
func (l *Local) Put(ctx context.Context, key string, fill func(io.Writer) error) error {
	final, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return fmt.Errorf("blobstore: create directory: %w", err)
	}
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, l.mode())
	if err != nil {
		return fmt.Errorf("blobstore: create %s: %w", key, err)
	}
	if err := fill(f); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("blobstore: close %s: %w", key, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("blobstore: finalize %s: %w", key, err)
	}
	return nil
}

// PutFile moves an existing local file into the store. For this
// implementation that is a rename when possible, so the caller's temporary
// file does not have to be copied; a remote store would upload it instead.
func (l *Local) PutFile(ctx context.Context, key, localPath string) error {
	final, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return fmt.Errorf("blobstore: create directory: %w", err)
	}
	if err := os.Rename(localPath, final); err == nil {
		return os.Chmod(final, l.mode())
	}
	// Different filesystem, or any other reason rename cannot work: fall back
	// to a copy through Put, which keeps the all-or-nothing guarantee.
	err = l.Put(ctx, key, func(w io.Writer) error {
		src, err := os.Open(localPath)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
		return err
	})
	if err != nil {
		return err
	}
	return os.Remove(localPath)
}

// List returns the blobs whose key starts with prefix, sorted by key.
// Subdirectories and partial writes (.tmp) are skipped: a listing is of blobs,
// and a blob only exists once it is whole.
func (l *Local) List(ctx context.Context, prefix string) ([]Entry, error) {
	entries, err := os.ReadDir(l.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || filepath.Ext(name) == ".tmp" {
			continue
		}
		if prefix != "" && !hasPrefix(name, prefix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{Key: name, Size: info.Size(), ModTime: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (l *Local) Delete(ctx context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if os.IsNotExist(err) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

// Prune deletes the oldest blobs under prefix, keeping the newest keep.
//
// Both backup schedulers had their own copy of this, identical down to the
// comment, which is the kind of duplication that stays correct right up until
// one of them is fixed. Ordering is by key, which the callers' timestamped
// names make equal to time order — and that is the callers' contract to keep,
// not something this function can check.
//
// keep <= 0 deletes nothing: a misread config must not be a way to erase every
// backup there is.
func Prune(ctx context.Context, s Store, prefix string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := s.List(ctx, prefix)
	if err != nil {
		return err
	}
	if len(entries) <= keep {
		return nil
	}
	var firstErr error
	for _, e := range entries[:len(entries)-keep] {
		if err := s.Delete(ctx, e.Key); err != nil && !errors.Is(err, ErrNotFound) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
