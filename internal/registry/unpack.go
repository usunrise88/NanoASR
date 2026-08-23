package registry

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Unpacking a downloaded archive.
//
// The archive arrived over the network, so every entry in it is untrusted
// input. Three things are refused outright rather than sanitised: paths that
// leave the destination, entries that are not plain files or directories, and
// anything that would exceed the size limits. Sanitising a hostile path is how
// traversal bugs get written; refusing it is not.

// Unpack extracts archive into dest, which must not already exist.
func Unpack(archive, dest string, limits UnpackLimits) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 64<<10)
	magic, err := br.Peek(4)
	if err != nil {
		return fmt.Errorf("archive is too small to identify: %w", err)
	}

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	switch {
	case bytes.HasPrefix(magic, []byte("BZh")):
		return extractTar(tar.NewReader(bzip2.NewReader(br)), dest, limits)
	case bytes.HasPrefix(magic, []byte{0x1F, 0x8B}):
		zr, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer zr.Close()
		return extractTar(tar.NewReader(zr), dest, limits)
	case bytes.HasPrefix(magic, []byte("PK\x03\x04")):
		return extractZip(archive, dest, limits)
	default:
		// Uncompressed tar has its magic 257 bytes in, past a useful peek;
		// try it rather than guessing from the file name.
		return extractTar(tar.NewReader(br), dest, limits)
	}
}

func extractTar(tr *tar.Reader, dest string, limits UnpackLimits) error {
	var files int
	var total int64

	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading archive: %w", err)
		}

		target, err := safeJoin(dest, h.Name)
		if err != nil {
			return err
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		case tar.TypeReg:
		default:
			// Symlinks, hard links and device nodes have no place in a model
			// archive, and a symlink is the classic way out of a destination
			// directory that path checks alone would have protected.
			return fmt.Errorf("archive entry %q has unsupported type %q; "+
				"only regular files and directories are accepted", h.Name, string(h.Typeflag))
		}

		files++
		if files > limits.MaxFiles {
			return fmt.Errorf("archive contains more than %d files", limits.MaxFiles)
		}
		if h.Size > limits.MaxFileBytes {
			return fmt.Errorf("archive entry %q is %d bytes, over the %d byte limit",
				h.Name, h.Size, limits.MaxFileBytes)
		}

		written, err := writeEntry(tr, target, limits.MaxUnpackedBytes-total)
		if err != nil {
			return fmt.Errorf("extracting %q: %w", h.Name, err)
		}
		total += written
	}
}

func extractZip(archive, dest string, limits UnpackLimits) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer zr.Close()

	var files int
	var total int64

	for _, entry := range zr.File {
		target, err := safeJoin(dest, entry.Name)
		if err != nil {
			return err
		}

		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if !entry.Mode().IsRegular() {
			return fmt.Errorf("archive entry %q is not a regular file", entry.Name)
		}

		files++
		if files > limits.MaxFiles {
			return fmt.Errorf("archive contains more than %d files", limits.MaxFiles)
		}
		// The declared size is a claim; writeEntry enforces the real one.
		if int64(entry.UncompressedSize64) > limits.MaxFileBytes {
			return fmt.Errorf("archive entry %q declares %d bytes, over the %d byte limit",
				entry.Name, entry.UncompressedSize64, limits.MaxFileBytes)
		}

		rc, err := entry.Open()
		if err != nil {
			return err
		}
		written, err := writeEntry(rc, target, limits.MaxUnpackedBytes-total)
		rc.Close()
		if err != nil {
			return fmt.Errorf("extracting %q: %w", entry.Name, err)
		}
		total += written
	}
	return nil
}

// writeEntry copies one entry, refusing to write more than remaining bytes.
//
// The limit is enforced against what actually arrives, not against the size the
// archive declares: a decompression bomb declares whatever it likes.
func writeEntry(src io.Reader, target string, remaining int64) (int64, error) {
	if remaining <= 0 {
		return 0, fmt.Errorf("archive exceeds the total unpacked size limit")
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}

	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	// One byte over the allowance is enough to tell the difference between
	// "fits" and "was truncated".
	written, err := io.Copy(f, io.LimitReader(src, remaining+1))
	if err != nil {
		return written, err
	}
	if written > remaining {
		return written, fmt.Errorf("archive exceeds the total unpacked size limit")
	}
	return written, nil
}

// safeJoin resolves an archive entry name inside dest, or refuses.
func safeJoin(dest, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("archive contains an entry with no name")
	}
	// Archive paths are always slash-separated, whatever the host uses.
	clean := filepath.Clean(filepath.FromSlash(name))

	if filepath.IsAbs(clean) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("archive entry %q is an absolute path", name)
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination directory", name)
	}

	target := filepath.Join(dest, clean)
	rel, err := filepath.Rel(dest, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination directory", name)
	}
	return target, nil
}
