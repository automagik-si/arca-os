package diskimport

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

// tree is one directory written into an Incus backup tarball.
type tree struct {
	dir     string
	prefix  string
	skip    []string
	replace map[string][]byte
	idmap   idMap
}

// fileKey names a file across file systems: btrfs subvolumes reuse inode numbers.
type fileKey struct {
	dev uint64
	ino uint64
}

// tarball writes an Incus backup tarball, keeping owners, modes, xattrs and hard links.
type tarball struct {
	tw    *tar.Writer
	links map[fileKey]string
}

func newTarball(w io.Writer) *tarball {
	return &tarball{tw: tar.NewWriter(w), links: map[fileKey]string{}}
}

// linkTarget returns the name a file was first written under, or records name as the first.
func (t *tarball) linkTarget(dev uint64, ino uint64, name string) (string, bool) {
	key := fileKey{dev, ino}

	first, ok := t.links[key]
	if ok {
		return first, true
	}

	t.links[key] = name

	return "", false
}

func (t *tarball) close() error {
	return t.tw.Close()
}

// addBytes writes a generated file.
func (t *tarball) addBytes(name string, content []byte) error {
	err := t.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content)), Typeflag: tar.TypeReg, Format: tar.FormatPAX})
	if err != nil {
		return err
	}

	_, err = t.tw.Write(content)

	return err
}

// addFile writes the content of path (a file or a block device) under name.
func (t *tarball) addFile(name string, path string) error {
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}

	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}

	err = t.tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: size, Typeflag: tar.TypeReg, Format: tar.FormatPAX})
	if err != nil {
		return err
	}

	_, err = io.CopyN(t.tw, f, size)

	return err
}

// unshift maps an id of the old host's container range back to the container's own id.
func unshift(set idMap, uid int64, gid int64) (int64, int64) {
	if set == nil {
		return uid, gid
	}

	return set.unshift(uid, gid)
}

// unshiftACL maps the user and group entries of a POSIX ACL xattr.
func unshiftACL(value []byte, set idMap) []byte {
	if set == nil || len(value) < 4 || (len(value)-4)%8 != 0 {
		return value
	}

	out := slices.Clone(value)

	for off := 4; off+8 <= len(out); off += 8 {
		tag := binary.LittleEndian.Uint16(out[off:])
		id := int64(binary.LittleEndian.Uint32(out[off+4:]))

		var mapped int64 = -1

		switch tag {
		case 0x02:
			mapped, _ = set.unshift(id, -1)
		case 0x08:
			_, mapped = set.unshift(-1, id)
		default:
		}

		if mapped >= 0 {
			binary.LittleEndian.PutUint32(out[off+4:], uint32(mapped)) //nolint:gosec
		}
	}

	return out
}

// unshiftCaps maps the root id of a namespaced (v3) file capability.
func unshiftCaps(value []byte, set idMap) []byte {
	if set == nil || len(value) != 24 || binary.LittleEndian.Uint32(value)&0xFF000000 != 0x03000000 {
		return value
	}

	out := slices.Clone(value)

	rootid, _ := set.unshift(int64(binary.LittleEndian.Uint32(out[20:])), -1)
	if rootid >= 0 {
		binary.LittleEndian.PutUint32(out[20:], uint32(rootid)) //nolint:gosec
	}

	return out
}

// xattrs reads every extended attribute of path without following links.
func xattrs(path string) (map[string][]byte, error) {
	size, err := unix.Llistxattr(path, nil)
	if err != nil || size == 0 {
		if errors.Is(err, unix.ENOTSUP) {
			return map[string][]byte{}, nil
		}

		return nil, err
	}

	buf := make([]byte, size)

	size, err = unix.Llistxattr(path, buf)
	if err != nil {
		return nil, err
	}

	ret := map[string][]byte{}

	for name := range strings.SplitSeq(string(buf[:size]), "\x00") {
		if name == "" {
			continue
		}

		vsize, err := unix.Lgetxattr(path, name, nil)
		if err != nil {
			return nil, err
		}

		value := make([]byte, vsize)

		vsize, err = unix.Lgetxattr(path, name, value)
		if err != nil {
			return nil, err
		}

		ret[name] = value[:vsize]
	}

	return ret, nil
}

// addTree writes every entry of a directory tree under its prefix.
func (t *tarball) addTree(src tree) error {
	return filepath.WalkDir(src.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src.dir, path)
		if err != nil {
			return err
		}

		if slices.Contains(src.skip, rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		return t.addEntry(src, path, rel)
	})
}

func (t *tarball) addEntry(src tree, path string, rel string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}

	if fi.Mode()&os.ModeSocket != 0 {
		return nil
	}

	link := ""
	if fi.Mode()&os.ModeSymlink != 0 {
		link, err = os.Readlink(path)
		if err != nil {
			return err
		}
	}

	hdr, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}

	hdr.Name = src.prefix
	if rel != "." {
		hdr.Name = src.prefix + "/" + filepath.ToSlash(rel)
	}

	hdr.Uname = ""
	hdr.Gname = ""
	hdr.Format = tar.FormatPAX

	if fi.IsDir() || link != "" {
		hdr.Size = 0
	}

	var st unix.Stat_t

	err = unix.Lstat(path, &st)
	if err != nil {
		return err
	}

	set := src.idmap
	if rel != "rootfs" && !strings.HasPrefix(rel, "rootfs/") {
		set = nil
	}

	uid, gid := unshift(set, int64(st.Uid), int64(st.Gid))
	if uid < 0 || gid < 0 {
		// Same as Incus' own export: an id outside the container's range is left out.
		return nil
	}

	hdr.Uid = int(uid)
	hdr.Gid = int(gid)

	if fi.Mode()&(os.ModeDevice|os.ModeCharDevice) != 0 {
		hdr.Devmajor = int64(unix.Major(st.Rdev))
		hdr.Devminor = int64(unix.Minor(st.Rdev))
	}

	if fi.Mode().IsRegular() && st.Nlink > 1 {
		first, ok := t.linkTarget(st.Dev, st.Ino, hdr.Name)
		if ok {
			hdr.Typeflag = tar.TypeLink
			hdr.Linkname = first
			hdr.Size = 0
		}
	}

	if link == "" {
		attrs, err := xattrs(path)
		if err != nil {
			return fmt.Errorf("read xattrs of %s: %w", path, err)
		}

		hdr.PAXRecords = map[string]string{}

		for name, value := range attrs {
			switch name {
			case "system.posix_acl_access", "system.posix_acl_default":
				value = unshiftACL(value, set)
			case "security.capability":
				value = unshiftCaps(value, set)
			default:
			}

			hdr.PAXRecords["SCHILY.xattr."+name] = string(value)
		}
	}

	content, replaced := src.replace[rel]
	if replaced && hdr.Typeflag == tar.TypeReg {
		hdr.Size = int64(len(content))
	}

	err = t.tw.WriteHeader(hdr)
	if err != nil {
		return err
	}

	if hdr.Typeflag != tar.TypeReg {
		return nil
	}

	if replaced {
		_, err = io.Copy(t.tw, bytes.NewReader(content))

		return err
	}

	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return err
	}

	defer func() { _ = f.Close() }()

	_, err = io.CopyN(t.tw, f, hdr.Size)

	return err
}

// treeSize sums the size of the regular files below dir.
func treeSize(dir string, skip []string) int64 {
	var total int64

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr
		}

		rel, _ := filepath.Rel(dir, path)
		if slices.Contains(skip, rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}

			return nil
		}

		if d.Type().IsRegular() {
			fi, err := d.Info()
			if err == nil {
				total += fi.Size()
			}
		}

		return nil
	})

	return total
}
