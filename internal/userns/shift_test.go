//go:build linux

package userns

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/checkpoint-restore/go-criu/v8/crit"
	usernsimg "github.com/checkpoint-restore/go-criu/v8/crit/images/userns"
	"github.com/containerd/errdefs"
	"golang.org/x/sys/unix"
)

const (
	from = uint32(firstBase + 2*Size)
	to   = uint32(firstBase + 5*Size)
)

func TestShiftTree(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown")
	}
	root := t.TempDir()
	path := func(p string) string { return filepath.Join(root, p) }
	if err := os.MkdirAll(path("dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"dir/file", "suid", "capped", "acl", "host-owned"} {
		if err := os.WriteFile(path(f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("dir/file", path("link")); err != nil {
		t.Fatal(err)
	}
	own := map[string][2]uint32{
		"dir": {from, from}, "dir/file": {from + 1000, from + 1000}, "suid": {from, from},
		"capped": {from, from}, "acl": {from + 1, from + 1}, "link": {from + 2, from + 2},
	}
	for p, ids := range own {
		if err := unix.Lchown(path(p), int(ids[0]), int(ids[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := unix.Chmod(path("suid"), 0o4755); err != nil {
		t.Fatal(err)
	}
	// A capability set in the container's namespace: revision 3, with
	// the host ID of its root.
	capV3 := make([]byte, 24)
	binary.LittleEndian.PutUint32(capV3, 0x03000000)
	binary.LittleEndian.PutUint32(capV3[4:], 1<<unix.CAP_NET_BIND_SERVICE)
	binary.LittleEndian.PutUint32(capV3[20:], from)
	if err := unix.Setxattr(path("capped"), "security.capability", capV3, 0); err != nil {
		t.Skipf("file capabilities: %v", err)
	}
	// user::rw-, user:(from+7):r--, group::r--, mask::r--, other::r--
	acl := []byte{2, 0, 0, 0}
	for _, e := range []struct {
		tag, perm uint16
		id        uint32
	}{{0x01, 6, 0xffffffff}, {0x02, 4, from + 7}, {0x04, 4, 0xffffffff}, {0x10, 4, 0xffffffff}, {0x20, 4, 0xffffffff}} {
		acl = binary.LittleEndian.AppendUint16(acl, e.tag)
		acl = binary.LittleEndian.AppendUint16(acl, e.perm)
		acl = binary.LittleEndian.AppendUint32(acl, e.id)
	}
	if err := unix.Setxattr(path("acl"), "system.posix_acl_access", acl, 0); err != nil {
		t.Skipf("ACLs: %v", err)
	}

	if err := ShiftTree(root, from, to); err != nil {
		t.Fatal(err)
	}
	for p, ids := range own {
		var st unix.Stat_t
		if err := unix.Lstat(path(p), &st); err != nil {
			t.Fatal(err)
		}
		if st.Uid != ids[0]-from+to || st.Gid != ids[1]-from+to {
			t.Errorf("%s: owned by %d:%d, want %d:%d", p, st.Uid, st.Gid, ids[0]-from+to, ids[1]-from+to)
		}
	}
	var st unix.Stat_t
	if err := unix.Stat(path("host-owned"), &st); err != nil || st.Uid != 0 {
		t.Errorf("a file outside the range moved: uid %d, %v", st.Uid, err)
	}
	if err := unix.Stat(path("suid"), &st); err != nil || st.Mode&unix.S_ISUID == 0 {
		t.Errorf("setuid bit lost: mode %o, %v", st.Mode, err)
	}
	if v, err := getxattr(path("capped"), "security.capability"); err != nil || len(v) != 24 ||
		binary.LittleEndian.Uint32(v[20:]) != to || binary.LittleEndian.Uint32(v[4:]) != 1<<unix.CAP_NET_BIND_SERVICE {
		t.Errorf("capability after the shift: %v, %v", v, err)
	}
	if v, err := getxattr(path("acl"), "system.posix_acl_access"); err != nil || binary.LittleEndian.Uint32(v[12+4:]) != to+7 {
		t.Errorf("ACL after the shift: %v, %v", v, err)
	}
}

func TestShiftImages(t *testing.T) {
	dir := t.TempDir()
	ext := func(first, lower, count uint32) *usernsimg.UidGidExtent {
		return &usernsimg.UidGidExtent{First: &first, LowerFirst: &lower, Count: &count}
	}
	img := &crit.CriuImage{Magic: "USERNS", Entries: []*crit.CriuEntry{{Message: &usernsimg.UsernsEntry{
		UidMap: []*usernsimg.UidGidExtent{ext(0, from, Size)},
		GidMap: []*usernsimg.UidGidExtent{ext(0, from, Size), ext(Size, 5, 1)},
	}}}}
	path := filepath.Join(dir, "userns-13.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := crit.New(nil, f, "", false, false).Encode(img); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := ShiftImages(dir, from, to); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := crit.New(f, nil, "", false, false).Decode(&usernsimg.UsernsEntry{})
	if err != nil {
		t.Fatal(err)
	}
	ue := got.Entries[0].Message.(*usernsimg.UsernsEntry)
	if ue.UidMap[0].GetLowerFirst() != to || ue.GidMap[0].GetLowerFirst() != to {
		t.Fatalf("maps after the shift: uid %d, gid %d; want %d", ue.UidMap[0].GetLowerFirst(), ue.GidMap[0].GetLowerFirst(), to)
	}
	if ue.GidMap[1].GetLowerFirst() != 5 || ue.UidMap[0].GetFirst() != 0 || ue.UidMap[0].GetCount() != Size {
		t.Fatalf("an extent outside the range, or the rest, changed: %v", ue)
	}
}

func TestCheckImages(t *testing.T) {
	write := func(t *testing.T, host uint32) string {
		t.Helper()
		dir := t.TempDir()
		ext := func(first, lower, count uint32) *usernsimg.UidGidExtent {
			return &usernsimg.UidGidExtent{First: &first, LowerFirst: &lower, Count: &count}
		}
		img := &crit.CriuImage{Magic: "USERNS", Entries: []*crit.CriuEntry{{Message: &usernsimg.UsernsEntry{
			UidMap: []*usernsimg.UidGidExtent{ext(0, host, Size)},
			GidMap: []*usernsimg.UidGidExtent{ext(0, host, Size)},
		}}}}
		f, err := os.Create(filepath.Join(dir, "userns-13.img"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := crit.New(nil, f, "", false, false).Encode(img); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	own := []Extent{{0, from, Size}}
	if err := CheckImages(write(t, from), own, own); err != nil {
		t.Fatalf("the container's own maps: %v", err)
	}
	// Images mapping the container's root to the host's.
	if err := CheckImages(write(t, 0), own, own); !errdefs.IsPermissionDenied(err) {
		t.Fatalf("maps to host root: %v", err)
	}
	if err := CheckImages(write(t, from), nil, nil); !errdefs.IsPermissionDenied(err) {
		t.Fatalf("a user namespace in images for a container with none: %v", err)
	}
	if err := CheckImages(t.TempDir(), nil, nil); err != nil {
		t.Fatalf("no user namespace either side: %v", err)
	}
}
