//go:build unix

package process

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestFollowLogSurvivesTruncate covers the interaction between follow mode
// and truncateOversizedLogs: the poller truncates an oversized log in place
// (same inode), which would otherwise leave the follow fd's offset beyond
// EOF, reading nothing until the file grew back past the old offset.
func TestFollowLogSurvivesTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stdout.log")
	if err := os.WriteFile(path, []byte("first-line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rc := followLog(t.Context(), path, 0)
	defer rc.Close()

	var mu sync.Mutex
	var got strings.Builder
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := rc.Read(buf)
			if n > 0 {
				mu.Lock()
				got.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	waitFor := func(want string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			s := got.String()
			mu.Unlock()
			if strings.Contains(s, want) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		mu.Lock()
		s := got.String()
		mu.Unlock()
		t.Fatalf("timed out waiting for %q; got %q", want, s)
	}

	waitFor("first-line\n")

	// Simulate truncateOversizedLogs: in-place truncate, then new content
	// shorter than the old fd offset.
	if err := os.Truncate(path, 0); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("hi\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	waitFor("hi\n")

	// Simulate rotateLogIfLarge: rename the log away and start a fresh file
	// (new inode) at the same path.
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("reborn\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor("reborn\n")
}
