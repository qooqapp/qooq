package watcher

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/qooqapp/qooq/pkg/blobstore"
	"go.uber.org/zap"
)

// Naming for scheduled backup files, e.g. watcher-20260711-013000.db. The
// prefix is what prune matches on, so it never touches unrelated files.
const (
	backupPrefix  = "watcher-"
	backupSuffix  = ".db"
	backupTimeFmt = "20060102-150405"
)

// runBackupLoop periodically snapshots the sqlite database into dir, keeping
// only the newest keep files. It returns immediately (does nothing) when dir
// is empty, preserving the no-config = no-backup default. A snapshot failure
// is logged and retried next tick; it never stops the watcher.
func (m *Watcher) runBackupLoop(ctx context.Context, dir string, interval time.Duration, keep int) {
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
		m.log.Error("backup scheduler: create dir failed, disabling", zap.String("dir", dir), zap.Error(err))
		return
	}
	m.log.Info("backup scheduler started", zap.String("dir", dir), zap.Duration("interval", interval), zap.Int("keep", keep))

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := m.writeScheduledBackup(dir); err != nil {
				m.log.Error("scheduled backup failed", zap.Error(err))
				continue
			}
			if err := pruneWatcherBackups(dir, keep); err != nil {
				m.log.Error("prune old backups failed", zap.Error(err))
			}
		}
	}
}

// writeScheduledBackup VACUUMs a fresh timestamped snapshot into dir. VACUUM
// INTO writes a complete file atomically from sqlite's view (it fails if the
// path exists), so there is no truncated-file window to guard against.
func (m *Watcher) writeScheduledBackup(dir string) error {
	name := backupPrefix + time.Now().Format(backupTimeFmt) + backupSuffix
	// sqlite's VACUUM INTO takes a path and will not write to an io.Writer, so
	// the snapshot lands on a temporary file and is handed over — which is also
	// the shape a remote store needs. It used to VACUUM straight onto the final
	// name, so a crash mid-snapshot left a truncated file named exactly like a
	// finished backup, which prune would keep and an operator would trust.
	tmp := filepath.Join(dir, name+".vacuum")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := m.snapshotDB(tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return blobstore.NewLocal(dir).PutFile(context.Background(), name, tmp)
}

// pruneWatcherBackups deletes the oldest snapshots under dir, keeping the
// newest keep. Timestamped names sort lexically in chronological order, so a
// key sort is a time sort — the contract blobstore.Prune relies on.
func pruneWatcherBackups(dir string, keep int) error {
	return blobstore.Prune(context.Background(), blobstore.NewLocal(dir), backupPrefix, keep)
}
