// Package buildctx builds Docker build-context tar streams. It is shared by
// qooq-watcher's pipeline (which tars a cloned repo before calling
// pkg/client.BuildImage) and the qooq CLI (now in the separate qooq-cli repo,
// `component deploy --build` / `image build`). It has no dependency on cobra,
// the qooq CLI, or any qooqd-internal package, so both sides can import it
// freely — which is why it lives under pkg/, not internal/.
package buildctx

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// TarDir streams dir as a tar archive into w (build-context format), skipping
// any .git directory. It never writes a temp file: files are read and copied
// straight into the tar stream as they are walked.
func TarDir(dir string, w io.Writer) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if info.IsDir() && info.Name() == ".git" {
			return filepath.SkipDir
		}
		if strings.HasPrefix(filepath.ToSlash(rel), ".git/") {
			return nil
		}

		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// TarFiles streams exactly the named files of dir as a tar archive.
//
// files are slash-separated paths relative to dir. They are written sorted, so
// the same set of files always produces the same archive order. Regular files
// are written with their contents and a normalized mode (the execute bit is
// kept, everything else is 0644/0755) and symlinks are written as symlinks —
// the target is NOT followed, so a link pointing outside dir cannot pull
// foreign content into the archive. Anything else (directories, devices,
// sockets) is skipped: directories exist implicitly through their files.
//
// A path that would leave dir ("..", absolute) is an error rather than being
// skipped: the caller built the list, and a list that reaches outside the tree
// is a bug to see, not to paper over.
func TarFiles(dir string, files []string, w io.Writer) error {
	sorted := append([]string(nil), files...)
	sort.Strings(sorted)

	tw := tar.NewWriter(w)
	for i, rel := range sorted {
		if i > 0 && sorted[i-1] == rel {
			continue
		}
		clean := path.Clean(rel)
		if clean != rel || path.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			return fmt.Errorf("unsafe path in the file list: %q", rel)
		}
		full := filepath.Join(dir, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			hdr := &tar.Header{Typeflag: tar.TypeSymlink, Name: rel, Linkname: filepath.ToSlash(target), Mode: 0o777, ModTime: info.ModTime()}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case info.Mode().IsRegular():
			mode := int64(0o644)
			if info.Mode().Perm()&0o111 != 0 {
				mode = 0o755
			}
			hdr := &tar.Header{Typeflag: tar.TypeReg, Name: rel, Size: info.Size(), Mode: mode, ModTime: info.ModTime()}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if err := copyFile(tw, full, info.Size()); err != nil {
				return err
			}
		}
	}
	return tw.Close()
}

// copyFile copies exactly size bytes: a file that grew since Lstat would
// otherwise corrupt the tar, and one that shrank is an error worth stopping on.
func copyFile(w io.Writer, full string, size int64) error {
	f, err := os.Open(full)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := io.Copy(w, io.LimitReader(f, size))
	if err != nil {
		return err
	}
	if n != size {
		return fmt.Errorf("%s changed while it was being packed", full)
	}
	return nil
}
