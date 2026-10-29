//go:build linux

// Package migrate moves a running container to another gcr container,
// usually on another host. The source shim pushes memory in rounds to CRIU
// page servers run by the target shim, then freezes, sends the rest of the
// checkpoint and the writable layer, and the target restores it.
package migrate

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
)

// pageServer is a CRIU page server receiving one dump into dir.
type pageServer struct {
	Port int
	// Parent is the round directory this one is incremental on, "" if
	// it is a full dump.
	Parent string
	cmd    *exec.Cmd
	done   chan error
}

// startPageServer runs `criu page-server` on host:<free port> writing into
// dir, chained to the previous round's directory when parent is set. It
// returns once the server accepts connections.
func startPageServer(ctx context.Context, host, dir, parent string) (*pageServer, error) {
	port, err := freePort(host)
	if err != nil {
		return nil, err
	}
	ready, readyW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer ready.Close()

	args := []string{"page-server", "--images-dir", dir, "--address", host,
		"--port", strconv.Itoa(port), "--status-fd", "3", "-o", "page-server.log", "-v4"}
	if parent != "" {
		args = append(args, "--prev-images-dir", parent)
	}
	cmd := exec.CommandContext(ctx, "criu", args...)
	cmd.ExtraFiles = []*os.File{readyW}
	if err := cmd.Start(); err != nil {
		readyW.Close()
		return nil, fmt.Errorf("start page server: %w", err)
	}
	readyW.Close()
	ps := &pageServer{Port: port, Parent: parent, cmd: cmd, done: make(chan error, 1)}
	go func() { ps.done <- cmd.Wait() }()

	// CRIU writes \0 and closes the fd once it listens; EOF alone means it died.
	b := make([]byte, 1)
	if n, _ := io.ReadFull(ready, b); n != 1 {
		err := <-ps.done
		return nil, fmt.Errorf("page server did not start (log in %s/page-server.log): %v", dir, err)
	}
	return ps, nil
}

// Wait returns when the dump connected to this server has finished.
func (p *pageServer) Wait() error {
	if err := <-p.done; err != nil {
		return fmt.Errorf("page server: %w", err)
	}
	return nil
}

func (p *pageServer) Kill() {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}

func freePort(host string) (int, error) {
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// memoryTracking reports whether the kernel can track dirty pages, which
// pre-copy rounds need (soft-dirty bits; not available on arm64).
func memoryTracking() bool {
	return exec.Command("criu", "check", "--feature", "mem_dirty_track").Run() == nil
}

// tarDir streams dir as a tar archive, keeping xattrs, owners and device
// nodes (overlay whiteouts and opaque dirs must survive).
func tarDir(dir string) (io.ReadCloser, func() error, error) {
	cmd := exec.Command("tar", "--xattrs", "--xattrs-include=*", "--numeric-owner", "-C", dir, "-cf", "-", ".")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd.Wait, nil
}

// untarDir unpacks a tarDir stream into dir.
func untarDir(r io.Reader, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cmd := exec.Command("tar", "--xattrs", "--xattrs-include=*", "--numeric-owner", "-C", dir, "-xpf", "-")
	cmd.Stdin = r
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("untar into %s: %w: %s", dir, err, out)
	}
	return nil
}
