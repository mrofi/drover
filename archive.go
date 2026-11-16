package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// maxReleaseFiles caps the number of entries in one folder release.
const maxReleaseFiles = 200000

// cleanRel turns an archive entry name into a path relative to the archive root, with every "..", leading
// slash and backslash taken out. The root itself is "".
func cleanRel(name string) string {
	return strings.TrimPrefix(path.Clean("/"+strings.ReplaceAll(name, `\`, "/")), "/")
}

// unpacker writes archive entries under dest. Links are only recorded while entries are read and created
// at the end, so nothing is ever written through a link that an earlier entry planted.
type unpacker struct {
	dest     string
	prefix   string // only entries under this folder of the archive are kept, with the prefix removed
	maxBytes int64
	written  int64
	entries  int
	symlinks [][2]string // path, target
	hardlink [][2]string // path, path of the file it is a link to
}

// rel maps an entry name to its path inside the release; ok is false for entries outside the prefix and
// for the root itself.
func (u *unpacker) rel(name string) (string, bool) {
	n := cleanRel(name)
	if u.prefix != "" {
		if !strings.HasPrefix(n, u.prefix+"/") {
			return "", false
		}
		n = n[len(u.prefix)+1:]
	}
	return n, n != ""
}

func (u *unpacker) count() error {
	if u.entries++; u.entries > maxReleaseFiles {
		return fmt.Errorf("more than %d entries", maxReleaseFiles)
	}
	return nil
}

func (u *unpacker) dir(rel string, mode fs.FileMode) error {
	if err := u.count(); err != nil {
		return err
	}
	return os.MkdirAll(filepath.Join(u.dest, rel), mode.Perm()|0o700)
}

func (u *unpacker) file(rel string, mode fs.FileMode, r io.Reader) error {
	if err := u.count(); err != nil {
		return err
	}
	target := filepath.Join(u.dest, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	_ = os.Remove(target) // a later entry replaces an earlier one of the same name
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, mode.Perm()|0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, u.maxBytes-u.written+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if u.written += n; err == nil && u.written > u.maxBytes {
		err = fmt.Errorf("unpacks to more than max_binary_bytes (%d)", u.maxBytes)
	}
	return err
}

func (u *unpacker) symlink(rel, target string) error {
	if err := u.count(); err != nil {
		return err
	}
	if path.IsAbs(target) || escapes(path.Join(path.Dir(rel), target)) {
		return fmt.Errorf("the link %s points outside the release (to %s)", rel, target)
	}
	u.symlinks = append(u.symlinks, [2]string{rel, target})
	return nil
}

func escapes(p string) bool {
	p = path.Clean(p)
	return p == ".." || strings.HasPrefix(p, "../")
}

// finish creates the recorded links.
func (u *unpacker) finish() error {
	for _, l := range u.hardlink {
		target := filepath.Join(u.dest, l[0])
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		_ = os.Remove(target)
		if err := os.Link(filepath.Join(u.dest, l[1]), target); err != nil {
			return fmt.Errorf("hard link %s: %w", l[0], err)
		}
	}
	for _, l := range u.symlinks {
		target := filepath.Join(u.dest, l[0])
		_ = os.MkdirAll(filepath.Dir(target), 0o755)
		if st, err := os.Lstat(target); err == nil && st.IsDir() {
			// Something was unpacked "through" the link's name. Refused, not guessed at.
			return fmt.Errorf("%s is both a link and a folder in the archive", l[0])
		}
		_ = os.Remove(target)
		if err := os.Symlink(l[1], target); err != nil {
			return fmt.Errorf("link %s: %w", l[0], err)
		}
	}
	return nil
}

// extractArchive unpacks a tar, tar.gz, tar.bz2 or zip file into dest, keeping only what is under subdir
// (the whole archive when subdir is empty).
func extractArchive(archivePath, dest, subdir string, maxBytes int64) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	head := make([]byte, 6)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	u := &unpacker{dest: dest, prefix: cleanRel(subdir), maxBytes: maxBytes}
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		err = u.zip(archivePath)
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		var zr *gzip.Reader
		if zr, err = gzip.NewReader(bufio.NewReader(f)); err == nil {
			err = u.tar(zr)
		}
	case bytes.HasPrefix(head, []byte("BZh")):
		err = u.tar(bzip2.NewReader(bufio.NewReader(f)))
	case bytes.HasPrefix(head, []byte{0xfd, '7', 'z', 'X', 'Z'}), bytes.HasPrefix(head, []byte{0x28, 0xb5, 0x2f, 0xfd}):
		err = errors.New("xz and zstd archives are not supported: use tar, tar.gz, tar.bz2 or zip")
	default:
		err = u.tar(bufio.NewReader(f))
	}
	if err == nil {
		err = u.finish()
	}
	if err != nil {
		return fmt.Errorf("unpacking the archive: %w", err)
	}
	return nil
}

func (u *unpacker) tar(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("not a readable tar, tar.gz, tar.bz2 or zip archive: %w", err)
		}
		rel, ok := u.rel(hdr.Name)
		if !ok {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = u.dir(rel, hdr.FileInfo().Mode())
		case tar.TypeReg:
			err = u.file(rel, hdr.FileInfo().Mode(), tr)
		case tar.TypeSymlink:
			err = u.symlink(rel, hdr.Linkname)
		case tar.TypeLink:
			// A hard link names its file by the path in the archive; one to a file outside what is kept is dropped.
			if src, ok := u.rel(hdr.Linkname); ok {
				u.hardlink = append(u.hardlink, [2]string{rel, src})
			}
		}
		if err != nil {
			return err
		}
	}
}

func (u *unpacker) zip(archivePath string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("not a readable zip archive: %w", err)
	}
	defer zr.Close()
	for _, zf := range zr.File {
		rel, ok := u.rel(zf.Name)
		if !ok {
			continue
		}
		mode := zf.Mode()
		switch {
		case mode.IsDir():
			err = u.dir(rel, mode)
		case mode&fs.ModeSymlink != 0:
			var rc io.ReadCloser
			if rc, err = zf.Open(); err == nil {
				target, _ := io.ReadAll(io.LimitReader(rc, 4096))
				rc.Close()
				err = u.symlink(rel, string(target))
			}
		case mode.IsRegular():
			var rc io.ReadCloser
			if rc, err = zf.Open(); err == nil {
				err = u.file(rel, mode, rc)
				rc.Close()
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkTree walks an unpacked release: it counts files and bytes, makes everything removable by drover,
// and refuses a tree with a link that leads outside it or with anything that is not a file, folder or link.
func checkTree(root string, maxBytes int64) (files int, size int64, err error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return 0, 0, err
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			// A link to something that does not exist leads nowhere; one that resolves must stay inside.
			if resolved, err := filepath.EvalSymlinks(p); err == nil &&
				resolved != realRoot && !strings.HasPrefix(resolved, realRoot+string(filepath.Separator)) {
				return fmt.Errorf("the link %s points outside the release", rel)
			}
		case info.IsDir():
			if info.Mode().Perm()&0o700 != 0o700 {
				return os.Chmod(p, info.Mode().Perm()|0o700)
			}
		case info.Mode().IsRegular():
			files++
			if size += info.Size(); size > maxBytes {
				return fmt.Errorf("larger than max_binary_bytes (%d)", maxBytes)
			}
		default:
			return fmt.Errorf("%s is not a file, folder or link", rel)
		}
		if files > maxReleaseFiles {
			return fmt.Errorf("more than %d files", maxReleaseFiles)
		}
		return nil
	})
	if err == nil && files == 0 {
		err = errors.New("there are no files in it")
	}
	return files, size, err
}

// tarGzDir writes the content of dir as a gzipped tar, for uploading a folder from the command line.
func tarGzDir(dir string, w io.Writer) error {
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return nil // sockets, devices and pipes are left out
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}
