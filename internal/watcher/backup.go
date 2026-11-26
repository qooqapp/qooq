package watcher

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
)

// handleBackup streams a consistent snapshot of the watcher's sqlite database.
//
// sqlite has no in-process "copy to writer" primitive, so we use VACUUM INTO
// to write a point-in-time, self-contained snapshot to a fresh file, then
// stream that file to the client and delete it. The temp dir is 0700 (owner
// only) because the snapshot contains git/webhook credentials exactly as
// stored (encrypted if QOOQ_WATCHER_SECRET_KEY is set, plaintext otherwise) —
// a backup must be faithfully restorable, so credentials are not masked.
func (w *Watcher) handleBackup(c *gin.Context) {
	tmpDir, err := os.MkdirTemp("", "qooq-watcher-backup-")
	if err != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("create temp dir: %w", err))
		return
	}
	defer os.RemoveAll(tmpDir)

	snapPath := filepath.Join(tmpDir, "watcher-snapshot.db")
	if err := w.snapshotDB(snapPath); err != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("sqlite snapshot: %w", err))
		return
	}

	f, err := os.Open(snapPath)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("open snapshot: %w", err))
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}

	c.Header("Content-Disposition", `attachment; filename="watcher-backup.db"`)
	c.DataFromReader(http.StatusOK, fi.Size(), "application/octet-stream", f, nil)
}

// snapshotDB writes a consistent, self-contained copy of the sqlite database
// to destPath via VACUUM INTO. destPath must not already exist. VACUUM INTO
// cannot run inside a transaction; gorm's Exec issues it directly. Shared by
// the on-demand /v1/backup handler and the scheduled backup loop.
func (w *Watcher) snapshotDB(destPath string) error {
	return w.db.Exec("VACUUM INTO ?", destPath).Error
}
