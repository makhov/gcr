//go:build e2e && linux

// leak runs inside a container: it receives a directory descriptor over
// the unix socket in its argument, as a leaked host descriptor would reach
// a container, and reads "secret" through it. It prints "OK <secret>" or
// the errno's name.
package main

import (
	"errors"
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

func main() {
	c, err := net.Dial("unix", os.Args[1])
	if err != nil {
		fmt.Println("NOFD", err)
		return
	}
	buf, oob := make([]byte, 1), make([]byte, unix.CmsgSpace(4))
	_, oobn, _, _, err := c.(*net.UnixConn).ReadMsgUnix(buf, oob)
	if err != nil {
		fmt.Println("NOFD", err)
		return
	}
	msgs, _ := unix.ParseSocketControlMessage(oob[:oobn])
	if len(msgs) != 1 {
		fmt.Println("NOFD")
		return
	}
	fds, err := unix.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) != 1 {
		fmt.Println("NOFD", err)
		return
	}
	fd, err := unix.Openat(fds[0], "secret", unix.O_RDONLY, 0)
	if err != nil {
		var errno unix.Errno
		errors.As(err, &errno)
		fmt.Println(unix.ErrnoName(errno))
		return
	}
	b := make([]byte, 64)
	n, _ := unix.Read(fd, b)
	fmt.Printf("OK %s", b[:max(n, 0)])
}
