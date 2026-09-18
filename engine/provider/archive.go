// archive.go — bounded tar/gzip extraction with path-traversal
// (tar-slip) protection. Mirrors engine/coremgr's unpack discipline;
// kept local so engine/provider stays self-contained by design.
package provider

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func newGzipReader(r io.Reader) (*gzip.Reader, error) {
	return gzip.NewReader(r)
}

func extractTar(r io.Reader, dst string) error {
	reader := tar.NewReader(r)

	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}

		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}

		clean, err := safeJoin(dst, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(clean, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(clean), 0o700); err != nil {
				return err
			}

			// Size-bounded write: a malformed archive must not
			// exhaust the disk.
			if err := writeBounded(clean, reader, header.Size); err != nil {
				return err
			}

			if header.Mode&0o111 != 0 {
				_ = os.Chmod(clean, 0o755)
			}
		case tar.TypeSymlink:
			// Symlinks are skipped: executables never need them and
			// following attacker-controlled links is unacceptable.
			continue
		default:
			continue
		}
	}
}

func writeBounded(dst string, r io.Reader, size int64) error {
	if size < 0 {
		size = 1 << 30 // hard ceiling for pathological headers
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	defer out.Close()

	_, err = io.CopyN(out, r, size)
	if err == io.EOF {
		return nil
	}

	return err
}

func safeJoin(dst, name string) (string, error) {
	target := filepath.Join(dst, filepath.Clean("/"+name))
	if !strings.HasPrefix(target, filepath.Clean(dst)+string(os.PathSeparator)) && target != filepath.Clean(dst) {
		return "", fmt.Errorf("tar-slip blocked: %q", name)
	}

	return target, nil
}
