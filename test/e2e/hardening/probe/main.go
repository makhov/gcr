//go:build e2e && linux

// probe runs inside a container. It makes system calls gcr's hardening
// refuses and prints "name result" for each, where result is OK or the
// errno's name, plus control calls that must keep working. Without the
// filter the refused calls fail with other errors (EINVAL, EFAULT,
// EBADF...), so EPERM (ENOSYS for clone3 and the keyring) means the filter
// refused them.
//
// "probe mount" (run with CAP_SYS_ADMIN) checks mounts gcr performs for the
// container by their exact return value: a refusal must be -1 with errno
// set, and paths through /proc/self must mean the caller.
//
// "probe nested" starts a child in new user, mount, PID and network
// namespaces, as Chrome's sandbox does, and the child reports what root
// there can do, including which link kinds it can create (gcr's netlink
// policy).
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func main() {
	// /proc/thread-self and gettid() must name the thread that mounts.
	runtime.LockOSThread()
	if len(os.Args) > 1 && os.Args[1] == "true" {
		return
	}
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "mount":
			mounts()
			return
		case "nested":
			nested()
			return
		case "nested-child":
			nestedChild()
			return
		}
	}
	refused()
}

func result(r uintptr, errno unix.Errno) string {
	if errno != 0 {
		return unix.ErrnoName(errno)
	}
	return "OK"
}

func report(name string, r uintptr, errno unix.Errno) {
	fmt.Println(name, result(r, errno))
}

// exact reports a call that must return exactly 0, or -1 with errno set.
func exact(name string, r uintptr, errno unix.Errno) {
	if errno == 0 && r != 0 {
		fmt.Printf("%s BADRET(%d)\n", name, r)
		return
	}
	report(name, r, errno)
}

func sys(nr uintptr, args ...uintptr) (uintptr, unix.Errno) {
	var a [6]uintptr
	copy(a[:], args)
	r, _, errno := unix.Syscall6(nr, a[0], a[1], a[2], a[3], a[4], a[5])
	return r, errno
}

func socket(domain, typ, proto int) (uintptr, unix.Errno) {
	fd, err := unix.Socket(domain, typ, proto)
	if err != nil {
		return 0, err.(unix.Errno)
	}
	unix.Close(fd)
	return 0, 0
}

func refused() {
	minus1 := ^uintptr(0)
	calls := []struct {
		name string
		nr   uintptr
		args []uintptr
	}{
		{"io_uring_setup", unix.SYS_IO_URING_SETUP, []uintptr{0, 0}},
		{"bpf", unix.SYS_BPF, []uintptr{minus1, 0, 0}},
		{"perf_event_open", unix.SYS_PERF_EVENT_OPEN, []uintptr{0, 0, minus1, minus1, 0}},
		{"userfaultfd", unix.SYS_USERFAULTFD, []uintptr{minus1}},
		{"keyctl", unix.SYS_KEYCTL, []uintptr{minus1, 0, 0, 0, 0}},
		{"fsopen", unix.SYS_FSOPEN, []uintptr{0, 0}},
		{"open_tree", unix.SYS_OPEN_TREE, []uintptr{minus1, 0, 0}},
		{"init_module", unix.SYS_INIT_MODULE, []uintptr{0, 0, 0}},
		{"open_by_handle_at", unix.SYS_OPEN_BY_HANDLE_AT, []uintptr{minus1, 0, 0}},
		{"unshare_user", unix.SYS_UNSHARE, []uintptr{unix.CLONE_NEWUSER}},
		{"clone3", unix.SYS_CLONE3, []uintptr{0, 0}},
	}
	for _, c := range calls {
		r, errno := sys(c.nr, c.args...)
		report(c.name, r, errno)
	}
	for _, s := range []struct {
		name               string
		domain, typ, proto int
	}{
		{"socket_packet", unix.AF_PACKET, unix.SOCK_RAW, 0},
		{"socket_raw", unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_ICMP},
		{"socket_vsock", unix.AF_VSOCK, unix.SOCK_STREAM, 0},
		{"socket_alg", unix.AF_ALG, unix.SOCK_SEQPACKET, 0},
		// Controls: must keep working.
		{"control_socket_tcp", unix.AF_INET, unix.SOCK_STREAM, 0},
		{"control_socket_ping", unix.AF_INET, unix.SOCK_DGRAM, unix.IPPROTO_ICMP},
	} {
		r, errno := socket(s.domain, s.typ, s.proto)
		report(s.name, r, errno)
	}
	// A new process (clone without CLONE_NEWUSER) must keep working.
	p, err := os.StartProcess("/proc/self/exe", []string{"probe", "true"}, &os.ProcAttr{})
	if err == nil {
		_, err = p.Wait()
	}
	fmt.Println("control_fork", map[bool]string{true: "OK", false: fmt.Sprint(err)}[err == nil])
}

func mount(source, target, fstype string, flags uintptr) (uintptr, unix.Errno) {
	ptr := func(s string) uintptr {
		if s == "" {
			return 0
		}
		p, _ := unix.BytePtrFromString(s)
		return uintptr(unsafe.Pointer(p))
	}
	return sys(unix.SYS_MOUNT, ptr(source), ptr(target), ptr(fstype), flags, 0)
}

// sameFile reports whether two paths name the same inode.
func sameFile(a, b string) string {
	var x, y unix.Stat_t
	if unix.Stat(a, &x) != nil || unix.Stat(b, &y) != nil {
		return "MISSING"
	}
	if x.Dev == y.Dev && x.Ino == y.Ino {
		return "OK"
	}
	return "DIFFERENT"
}

func touch(path string) {
	if f, err := os.Create(path); err == nil {
		f.Close()
	}
}

func mounts() {
	_ = os.MkdirAll("/mnt", 0o755)
	r, errno := mount("none", "/mnt", "ramfs", 0)
	exact("mount_ramfs", r, errno)
	r, errno = mount("none", "/mnt", "tmpfs", 0)
	exact("mount_tmpfs", r, errno)

	// dockerd keeps network namespaces by bind-mounting them so.
	touch("/mnt/netns")
	r, errno = mount("/proc/self/task/"+strconv.Itoa(unix.Gettid())+"/ns/net", "/mnt/netns", "", unix.MS_BIND)
	exact("mount_self_task_ns", r, errno)
	fmt.Println("mount_self_task_ns_same", sameFile("/mnt/netns", "/proc/self/ns/net"))
	touch("/mnt/netns2")
	r, errno = mount("/proc/thread-self/ns/net", "/mnt/netns2", "", unix.MS_BIND)
	exact("mount_thread_self_ns", r, errno)
	fmt.Println("mount_thread_self_ns_same", sameFile("/mnt/netns2", "/proc/self/ns/net"))

	// runc names mount sources and targets by file descriptor.
	_ = os.MkdirAll("/mnt/src", 0o755)
	_ = os.MkdirAll("/mnt/dst", 0o755)
	touch("/mnt/src/marker")
	fd, err := unix.Open("/mnt/dst", unix.O_PATH|unix.O_DIRECTORY, 0)
	if err != nil {
		fmt.Println("mount_fd_target", err)
		return
	}
	r, errno = mount("/mnt/src", "/proc/self/fd/"+strconv.Itoa(fd), "", unix.MS_BIND)
	exact("mount_fd_target", r, errno)
	fmt.Println("mount_fd_target_same", sameFile("/mnt/dst/marker", "/mnt/src/marker"))
}

func nested() {
	cmd := exec.Command("/proc/self/exe", "nested-child")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	if err := cmd.Run(); err != nil {
		var errno unix.Errno
		if errors.As(err, &errno) {
			fmt.Println("nested_userns", unix.ErrnoName(errno))
		} else {
			fmt.Println("nested_userns", err)
		}
	}
}

func nestedChild() {
	fmt.Println("nested_userns OK")
	r, errno := socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_NETFILTER)
	report("nested_netfilter", r, errno)
	r, errno = socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_ROUTE)
	report("nested_netlink_route", r, errno)
	_ = os.MkdirAll("/mnt", 0o755)
	r, errno = mount("none", "/mnt", "ramfs", 0)
	exact("nested_mount_ramfs", r, errno)
	r, errno = mount("none", "/mnt", "tmpfs", 0)
	exact("nested_mount_tmpfs", r, errno)

	// Link kinds, over netlink: what gcr's netlink policy allows (veth,
	// dummy, bridge) and what it refuses (bonding, macvlan, which has to
	// sit on another link).
	for _, k := range []string{"veth", "dummy", "bridge", "bond"} {
		fmt.Println("link_"+k, newLink("p-"+k, 0, k))
	}
	// Two kinds in one request: the kernel goes by the last, so must the
	// policy.
	fmt.Println("link_dummy_then_bond", newLink("p-dup", 0, "dummy", "bond"))
	_, err := net.InterfaceByName("p-dup")
	fmt.Println("link_dup_exists", map[bool]string{true: "YES", false: "NO"}[err == nil])
	parent, err := net.InterfaceByName("p-dummy")
	if err != nil {
		fmt.Println("link_macvlan NOPARENT")
		return
	}
	fmt.Println("link_macvlan", newLink("p-macvlan", parent.Index, "macvlan"))
	// A refused request must have made nothing.
	for _, k := range []string{"bond", "macvlan"} {
		_, err := net.InterfaceByName("p-" + k)
		fmt.Println("link_"+k+"_exists", map[bool]string{true: "YES", false: "NO"}[err == nil])
	}
}

// newLink asks the kernel for a link of kind (RTM_NEWLINK with
// IFLA_LINKINFO/IFLA_INFO_KIND, on parent if not 0) and reports OK or the
// errno's name, from sendmsg or the kernel's acknowledgement. Given more
// than one kind, it sends an IFLA_INFO_KIND for each.
func newLink(name string, parent int, kinds ...string) string {
	attr := func(typ uint16, payload []byte) []byte {
		b := make([]byte, 4+len(payload))
		binary.LittleEndian.PutUint16(b[0:], uint16(4+len(payload)))
		binary.LittleEndian.PutUint16(b[2:], typ)
		copy(b[4:], payload)
		return append(b, make([]byte, (4-len(b)%4)%4)...)
	}
	var attrs []byte
	attrs = append(attrs, attr(unix.IFLA_IFNAME, append([]byte(name), 0))...)
	if parent != 0 {
		p := make([]byte, 4)
		binary.LittleEndian.PutUint32(p, uint32(parent))
		attrs = append(attrs, attr(unix.IFLA_LINK, p)...)
	}
	var info []byte
	for _, kind := range kinds {
		info = append(info, attr(unix.IFLA_INFO_KIND, append([]byte(kind), 0))...)
	}
	attrs = append(attrs, attr(unix.IFLA_LINKINFO|unix.NLA_F_NESTED, info)...)
	msg := make([]byte, unix.NLMSG_HDRLEN+unix.SizeofIfInfomsg, unix.NLMSG_HDRLEN+unix.SizeofIfInfomsg+len(attrs))
	msg = append(msg, attrs...)
	h := (*unix.NlMsghdr)(unsafe.Pointer(&msg[0]))
	h.Len, h.Type, h.Seq = uint32(len(msg)), unix.RTM_NEWLINK, 1
	h.Flags = unix.NLM_F_REQUEST | unix.NLM_F_ACK | unix.NLM_F_CREATE | unix.NLM_F_EXCL

	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return "NOSOCKET"
	}
	defer unix.Close(fd)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return unix.ErrnoName(err.(unix.Errno))
	}
	reply := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, reply, 0)
	if err != nil {
		return "NOREPLY"
	}
	// The acknowledgement: an NLMSG_ERROR whose payload starts with the
	// error code, 0 for success.
	if n < unix.NLMSG_HDRLEN+4 || binary.LittleEndian.Uint16(reply[4:]) != unix.NLMSG_ERROR {
		return "BADREPLY"
	}
	if code := int32(binary.LittleEndian.Uint32(reply[unix.NLMSG_HDRLEN:])); code != 0 {
		return unix.ErrnoName(unix.Errno(-code))
	}
	return "OK"
}
