package watcher

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// Upload as a source: a deploy_mode "upload"
// target gets its source from an archive PUT to /v1/targets/{name}/source
// instead of a git clone. The archive is stored, its revision computed, and a
// build queued; the pipeline then extracts it where a clone would have gone
// (fetchSource) and every later step is the git path unchanged.

// uploadTargetName is what an upload target's name must look like, because it
// names a directory under uploads.dir. Target names are otherwise free-form;
// this is the one mode where the name reaches the filesystem.
var uploadTargetName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

func validUploadTargetName(name string) bool {
	return uploadTargetName.MatchString(name) && name != "." && name != ".."
}

// staleUploadTempAge is how old a leftover partial upload must be before a
// later upload to the same target removes it. Younger ones may belong to an
// upload still streaming in — those are written outside the target lock.
const staleUploadTempAge = time.Hour

const uploadTempPattern = ".upload-*.tmp"

// uploadDirFor is where one target's archives live.
func (w *Watcher) uploadDirFor(name string) (string, error) {
	if w.uploadsDir == "" {
		return "", errors.New("uploads.dir is not configured")
	}
	if !validUploadTargetName(name) {
		return "", fmt.Errorf("target name %q cannot name an upload directory", name)
	}
	return filepath.Join(w.uploadsDir, name), nil
}

// handleUploadSource handles PUT /v1/targets/:name/source.
//
// The body is streamed to a temp file (never held in memory), then extracted
// once into a scratch directory under exactly the rules the build will apply
// (extractUpload). That pass is what computes the revision, and it also means
// an archive the pipeline would refuse is refused now, as a 400 to whoever
// sent it, rather than minutes later as a build failure on a target.
//
// Only the final step — naming the file, recording the revision, queueing the
// build — runs under the target lock, so a build in progress does not stall
// the stream; it does mean the 202 waits for that build to finish, because the
// lock is what keeps an upload from pruning the archive a build is reading.
//
// ?force=1 builds even when the archive is identical to what is deployed,
// like `trigger?force=1` for a commit that has not moved.
func (w *Watcher) handleUploadSource(c *gin.Context) {
	name := c.Param("name")
	switch ct := c.ContentType(); ct {
	case "", "application/gzip", "application/x-gzip", "application/octet-stream":
	default:
		respondErr(c, http.StatusUnsupportedMediaType, fmt.Errorf("Content-Type must be application/gzip (got %q)", ct))
		return
	}

	t, err := getTargetByName(w.db, name)
	if err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	if t.DeployMode != DeployModeUpload {
		respondErr(c, http.StatusConflict, fmt.Errorf("target %q has deploy_mode %q; only %q targets accept an uploaded source", name, t.DeployMode, DeployModeUpload))
		return
	}
	if t.QuotaHold != "" {
		// Refused before a byte is stored: a held target would keep the
		// upload and never build it.
		respondErr(c, http.StatusConflict, errQuotaHold{t.QuotaHold})
		return
	}
	dir, err := w.uploadDirFor(name)
	if err != nil {
		respondErr(c, http.StatusBadRequest, err)
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("create upload dir: %w", err))
		return
	}

	tmp, err := os.CreateTemp(dir, uploadTempPattern)
	if err != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("create upload file: %w", err))
		return
	}
	tmpPath := tmp.Name()
	// After a successful rename this removes nothing; on every other path it
	// is what keeps a partial archive from looking like a stored one.
	defer os.Remove(tmpPath)

	body := http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadCompressedBytes)
	_, copyErr := io.Copy(tmp, body)
	closeErr := tmp.Close()
	if copyErr != nil {
		var tooBig *http.MaxBytesError
		if errors.As(copyErr, &tooBig) {
			respondErr(c, http.StatusRequestEntityTooLarge, fmt.Errorf("%w: the compressed archive exceeds %d bytes", errUploadTooLarge, maxUploadCompressedBytes))
			return
		}
		respondErr(c, http.StatusBadRequest, fmt.Errorf("read upload: %w", copyErr))
		return
	}
	if closeErr != nil {
		respondErr(c, http.StatusInternalServerError, fmt.Errorf("write upload: %w", closeErr))
		return
	}

	revision, err := checkUploadArchive(tmpPath)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errUploadTooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		respondErr(c, status, err)
		return
	}

	force := c.Query("force") == "1"
	unlock := w.lockTarget(name)
	defer unlock()

	// Re-read inside the lock: a build that finished while the body streamed
	// has moved LastCommit, and the answer below depends on it.
	t, err = getTargetByName(w.db, name)
	if err != nil {
		if errors.Is(err, ErrTargetNotFound) {
			respondErr(c, http.StatusNotFound, err)
			return
		}
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	if t.DeployMode != DeployModeUpload {
		respondErr(c, http.StatusConflict, fmt.Errorf("target %q is no longer an upload target", name))
		return
	}

	final := filepath.Join(dir, revision+".tar.gz")
	if _, err := os.Stat(final); err != nil {
		if err := os.Rename(tmpPath, final); err != nil {
			respondErr(c, http.StatusInternalServerError, fmt.Errorf("store upload: %w", err))
			return
		}
	}
	// else: the same content is already stored under its revision; the new
	// copy is redundant and the deferred Remove drops it.

	// The sweep's own rule (checkAndMaybeBuild), answered here so the caller
	// knows whether to wait for a build at all.
	queued := force ||
		(revision != t.LastCommit && revision != t.RejectedCommit) ||
		(revision == t.LastCommit && w.componentIsGone(c.Request.Context(), t))

	previous := t.UploadRevision
	now := time.Now()
	updates := map[string]any{"upload_revision": revision, "last_upload_at": now}
	if force {
		updates["force_rebuild"] = true
	}
	if err := w.db.Model(&Target{}).Where("id = ?", t.ID).Updates(updates).Error; err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	if err := triggerNow(w.db, name); err != nil {
		respondErr(c, http.StatusInternalServerError, err)
		return
	}
	w.pruneUploads(dir, revision, previous)

	w.log.Info("source uploaded", zap.String("target", name), zap.String("revision", revision),
		zap.String("previous", previous), zap.Bool("build_queued", queued))
	c.JSON(http.StatusAccepted, UploadSourceResponse{
		Revision:        revision,
		BuildGeneration: t.BuildGeneration,
		BuildQueued:     queued,
	})
}

// checkUploadArchive extracts the archive at path into a throwaway directory
// and returns its revision. Nothing is kept: this exists to apply the build's
// own extraction rules to the upload before it is accepted.
func checkUploadArchive(path string) (string, error) {
	scratch, err := os.MkdirTemp("", "qooq-upload-check-")
	if err != nil {
		return "", fmt.Errorf("create scratch dir: %w", err)
	}
	defer os.RemoveAll(scratch)
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return extractUpload(f, scratch)
}

// extractStoredUpload is fetchSource's upload branch: it extracts the target's
// current archive into dir (a fresh, empty temp dir) and returns its revision.
//
// The revision is recomputed rather than trusted from the file name, and a
// mismatch fails the build: a stored archive that no longer hashes to its name
// has changed on disk, and building it would deploy something nobody uploaded.
func (w *Watcher) extractStoredUpload(t *Target, dir string) (string, error) {
	if t.UploadRevision == "" {
		return "", errors.New("extract upload: nothing has been uploaded to this target")
	}
	udir, err := w.uploadDirFor(t.Name)
	if err != nil {
		return "", fmt.Errorf("extract upload: %w", err)
	}
	f, err := os.Open(filepath.Join(udir, t.UploadRevision+".tar.gz"))
	if err != nil {
		return "", fmt.Errorf("extract upload: %w", err)
	}
	defer f.Close()
	revision, err := extractUpload(f, dir)
	if err != nil {
		return "", fmt.Errorf("extract upload: %w", err)
	}
	if revision != t.UploadRevision {
		return "", fmt.Errorf("extract upload: stored archive %s now hashes to %s; it changed on disk, upload it again", shortSHA(t.UploadRevision), shortSHA(revision))
	}
	return revision, nil
}

// pruneUploads keeps the archives named in keep (the current upload and the
// one it replaced — the same current+previous rule as images and artifacts)
// and removes every other archive in dir, plus partial uploads old enough to
// be abandoned. Called under the target lock, so no build is reading what it
// removes. Failures are logged, never returned: the upload itself succeeded.
func (w *Watcher) pruneUploads(dir string, keep ...string) {
	keepSet := map[string]bool{}
	for _, k := range keep {
		if k != "" {
			keepSet[k+".tar.gz"] = true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		w.log.Warn("prune uploads: list failed", zap.String("dir", dir), zap.Error(err))
		return
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".tar.gz") && !keepSet[name]:
		case strings.HasPrefix(name, ".upload-") && strings.HasSuffix(name, ".tmp"):
			info, err := e.Info()
			if err != nil || time.Since(info.ModTime()) < staleUploadTempAge {
				continue
			}
		default:
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			w.log.Warn("prune uploads: remove failed", zap.String("file", name), zap.Error(err))
		}
	}
}

// removeStoredUploads deletes every archive of a target that no longer exists.
func (w *Watcher) removeStoredUploads(name string) {
	dir, err := w.uploadDirFor(name)
	if err != nil {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		w.log.Warn("remove uploads of deleted target failed", zap.String("target", name), zap.Error(err))
	}
}
