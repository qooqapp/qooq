package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/qooqapp/qooq/pkg/blobstore"
)

// backupPrefix / backupSuffix bracket the timestamped filenames the scheduler
// writes (e.g. qooq-20260711-013000.db). The prefix is what prune matches on
// so it never touches unrelated files sharing the directory.
const (
	backupPrefix  = "qooq-"
	backupSuffix  = ".db"
	backupTimeFmt = "20060102-150405"
)

// backupSource is the minimal slice of core.Service the scheduler needs: a
// consistent snapshot writer. Declared here (consumer-defined) so this file
// stays decoupled from the rest of the service surface.
type backupSource interface {
	Backup(w io.Writer, sizeHint func(int64)) error
}

// runBackupScheduler periodically snapshots the datastore into dir, keeping
// only the newest keep files. It returns immediately (does nothing) when dir
// is empty, preserving the no-config = no-backup default. A snapshot failure
// is logged and retried on the next tick — it never crashes the daemon.
func runBackupScheduler(ctx context.Context, src backupSource, dir string, interval time.Duration, keep int, log *slog.Logger) {
	if dir == "" {
		return
	}
	if interval <= 0 {
		interval = time.Hour
	}
	if keep < 1 {
		keep = 1
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Error("backup scheduler: create dir failed, disabling", "dir", dir, "err", err)
		return
	}
	log.Info("backup scheduler started", "dir", dir, "interval", interval, "keep", keep)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := writeBackup(src, dir); err != nil {
				log.Error("scheduled backup failed", "err", err)
				continue
			}
			if err := pruneBackups(dir, keep); err != nil {
				log.Error("prune old backups failed", "err", err)
			}
		}
	}
}

// writeBackup snapshots src into a fresh timestamped file under dir. It writes
// to a .tmp sibling first and renames on success so a mid-write failure never
// leaves a truncated file that prune might mistake for a good backup.
func writeBackup(src backupSource, dir string) error {
	name := backupPrefix + time.Now().Format(backupTimeFmt) + backupSuffix
	// blobstore.Local.Put is the .tmp-and-rename this function used to do
	// itself, and it is shared with the watcher's scheduler so there is one
	// answer to "what does a half-written snapshot look like" (nothing).
	return blobstore.NewLocal(dir).Put(context.Background(), name, func(w io.Writer) error {
		return src.Backup(w, nil)
	})
}

// pruneBackups deletes the oldest snapshots under dir, keeping the newest
// keep. The timestamped names sort lexically in chronological order, so a key
// sort is a time sort — which is the contract blobstore.Prune relies on.
func pruneBackups(dir string, keep int) error {
	return blobstore.Prune(context.Background(), blobstore.NewLocal(dir), backupPrefix, keep)
}
