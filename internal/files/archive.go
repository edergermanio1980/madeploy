package files

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// WritePackage writes a tar.gz with the given relative paths from root to out
// and returns its sha256.
func WritePackage(root string, paths []string, out string) (string, error) {
	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	gz := gzip.NewWriter(io.MultiWriter(f, h))
	tw := tar.NewWriter(gz)
	for _, rel := range paths {
		if err := addFile(tw, root, rel); err != nil {
			return "", fmt.Errorf("empacotando %s: %w", rel, err)
		}
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func addFile(tw *tar.Writer, root, rel string) error {
	src, err := os.Open(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return err
	}
	mode := int64(0o644)
	if st.Mode()&0o111 != 0 {
		mode = 0o755
	}
	hdr := &tar.Header{Name: rel, Mode: mode, Size: st.Size(), ModTime: st.ModTime(), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = io.Copy(tw, src)
	return err
}

// Extract unpacks a tar.gz stream into dest, skipping excluded paths and
// refusing entries that escape dest. Only regular files and directories are written.
func Extract(r io.Reader, dest string, m *Matcher) (int, error) {
	return ExtractProgress(r, dest, m, nil)
}

// HumanBytes renders a size as "12.3 MB".
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// Progress is reported after each extracted file. Totals are 0 when unknown.
type Progress struct {
	Files, TotalFiles int
	Bytes, TotalBytes int64
	Name              string
	Done              bool
}

// ExtractProgress is Extract calling fn (if not nil) after every regular file.
func ExtractProgress(r io.Reader, dest string, m *Matcher, fn func(Progress)) (int, error) {
	var bytes int64
	dest, err := filepath.Abs(dest) // "." would never prefix-match the joined target
	if err != nil {
		return 0, err
	}
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	tr := tar.NewReader(gz)
	n := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		rel := strings.TrimPrefix(path.Clean("/"+hdr.Name), "/")
		if rel == "" || (m != nil && m.Excluded(rel)) {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) {
			return n, fmt.Errorf("entrada fora do destino: %s", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return n, err
			}
		case tar.TypeReg:
			if err := writeEntry(tr, target, hdr.Mode); err != nil {
				return n, err
			}
			n++
			bytes += hdr.Size
			if fn != nil {
				fn(Progress{Files: n, Bytes: bytes, Name: rel})
			}
		}
	}
}

func writeEntry(r io.Reader, target string, hdrMode int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if hdrMode&0o111 != 0 {
		mode = 0o755
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
