//go:build linux

package userns

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	usernsimg "github.com/checkpoint-restore/go-criu/v8/crit/images/userns"
	"golang.org/x/sys/unix"
)

// A snapshot moved to another host may have to map another range there:
// its files and CRIU images carry host IDs of the range it was taken in.
// CRIU writes most IDs in its images as the container sees them; the
// host's are in the files' owners, in two kinds of xattrs (file
// capabilities record their namespace's root, POSIX ACLs name users and
// groups), and in the user namespace's maps (userns-*.img), which CRIU
// recreates the namespace from on restore.

// ShiftTree moves the owners of the files under root, and the IDs in
// their capability and ACL xattrs, from the range at from to the one at
// to. IDs outside the range stay as they are.
func ShiftTree(root string, from, to uint32) error {
	shift := func(id uint32) (uint32, bool) {
		if id >= from && id-from < Size {
			return id - from + to, true
		}
		return id, false
	}
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			return err
		}
		uid, uidMoved := shift(st.Uid)
		gid, gidMoved := shift(st.Gid)
		isLink := st.Mode&unix.S_IFMT == unix.S_IFLNK
		// Read before the chown, which drops file capabilities.
		var xattrs map[string][]byte
		if !isLink {
			var err error
			if xattrs, err = shiftedXattrs(path, shift); err != nil {
				return err
			}
		}
		if uidMoved || gidMoved {
			if err := unix.Lchown(path, int(uid), int(gid)); err != nil {
				return fmt.Errorf("chown %s: %w", path, err)
			}
			// chown clears setuid and setgid bits.
			if !isLink && st.Mode&(unix.S_ISUID|unix.S_ISGID) != 0 {
				if err := unix.Chmod(path, st.Mode&07777); err != nil {
					return err
				}
			}
		}
		for name, v := range xattrs {
			if err := unix.Setxattr(path, name, v, 0); err != nil {
				return fmt.Errorf("%s: set %s: %w", path, name, err)
			}
		}
		return nil
	})
}

// shiftedXattrs returns path's capability and ACL xattrs whose IDs move
// (all its capability xattr, which the chown drops), rewritten.
func shiftedXattrs(path string, shift func(uint32) (uint32, bool)) (map[string][]byte, error) {
	out := map[string][]byte{}
	for _, name := range []string{"security.capability", "system.posix_acl_access", "system.posix_acl_default"} {
		v, err := getxattr(path, name)
		if err != nil {
			return nil, err
		}
		if v == nil {
			continue
		}
		switch name {
		case "security.capability":
			// Revision 3 (struct vfs_ns_cap_data) ends with the host ID
			// of its namespace's root.
			if len(v) == 24 && binary.LittleEndian.Uint32(v)&0xff000000 == 0x03000000 {
				if id, ok := shift(binary.LittleEndian.Uint32(v[20:])); ok {
					binary.LittleEndian.PutUint32(v[20:], id)
				}
			}
			out[name] = v
		default:
			// A version 2 header, then {tag u16, perm u16, id u32}s.
			moved := false
			for off := 4; off+8 <= len(v); off += 8 {
				tag := binary.LittleEndian.Uint16(v[off:])
				const aclUser, aclGroup = 0x02, 0x08
				if tag != aclUser && tag != aclGroup {
					continue
				}
				if id, ok := shift(binary.LittleEndian.Uint32(v[off+4:])); ok {
					binary.LittleEndian.PutUint32(v[off+4:], id)
					moved = true
				}
			}
			if moved {
				out[name] = v
			}
		}
	}
	return out, nil
}

func getxattr(path, name string) ([]byte, error) {
	for size := 64; ; size *= 4 {
		buf := make([]byte, size)
		n, err := unix.Lgetxattr(path, name, buf)
		switch {
		case errors.Is(err, unix.ENODATA), errors.Is(err, unix.ENOTSUP):
			return nil, nil
		case errors.Is(err, unix.ERANGE) && size < 1<<20:
			continue
		case err != nil:
			return nil, fmt.Errorf("%s: get %s: %w", path, name, err)
		}
		return buf[:n], nil
	}
}

// ShiftImages moves the user namespace maps in the CRIU images in dir
// from the range at from to the one at to.
func ShiftImages(dir string, from, to uint32) error {
	imgs, err := filepath.Glob(filepath.Join(dir, "userns-*.img"))
	if err != nil {
		return err
	}
	for _, path := range imgs {
		if err := shiftImage(path, from, to); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func shiftImage(path string, from, to uint32) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	img, err := crit.New(in, nil, "", false, false).Decode(&usernsimg.UsernsEntry{})
	in.Close()
	if err != nil {
		return err
	}
	for _, e := range img.Entries {
		ue, ok := e.Message.(*usernsimg.UsernsEntry)
		if !ok {
			return fmt.Errorf("entry is %T", e.Message)
		}
		for _, ext := range append(append([]*usernsimg.UidGidExtent{}, ue.UidMap...), ue.GidMap...) {
			if l := ext.GetLowerFirst(); l >= from && l-from < Size {
				moved := l - from + to
				ext.LowerFirst = &moved
			}
		}
	}
	tmp := path + ".shift"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	err = crit.New(nil, out, "", false, false).Encode(img)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
