//go:build linux

package migrate

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/containerd/log"
)

// Memory does not travel to an open port. CRIU's page servers listen on
// loopback, and its page clients connect to loopback; each page stream
// is carried between them over the migration's own connection, where
// the token is checked and, with MigrationTLS, everything is encrypted:
//
//	criu dump -> source proxy -> POST /rounds/N/pages (Upgrade) -> criu page-server
const (
	loopback   = "127.0.0.1"
	pagesProto = "gcr-pages"
)

// pages takes over the request's connection and joins it to the current
// round's page server.
func (r *Receiver) pages(w http.ResponseWriter, req *http.Request) {
	round, _ := strconv.Atoi(req.PathValue("round"))
	r.mu.Lock()
	ps := r.ps
	current := ps != nil && round == r.round
	r.mu.Unlock()
	if !current {
		http.Error(w, fmt.Sprintf("round %d has no page server", round), http.StatusConflict)
		return
	}
	if !strings.EqualFold(req.Header.Get("Upgrade"), pagesProto) {
		http.Error(w, "want Upgrade: "+pagesProto, http.StatusBadRequest)
		return
	}
	local, err := net.Dial("tcp", net.JoinHostPort(loopback, strconv.Itoa(ps.Port)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		local.Close()
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: " + pagesProto + "\r\n\r\n")
	if err := brw.Flush(); err != nil {
		conn.Close()
		local.Close()
		return
	}
	go splice(bufferedConn{brw.Reader, conn}, local)
}

// bufferedConn reads what the server had already buffered first.
type bufferedConn struct {
	r *bufio.Reader
	net.Conn
}

func (c bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// pageProxy listens on loopback for CRIU's page client and carries each
// connection to round's page server through the target. stop closes the
// listener; streams under way finish on their own.
func (c *client) pageProxy(ctx context.Context, round int) (port int, stop func(), err error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(loopback, "0"))
	if err != nil {
		return 0, nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				remote, err := c.openPages(ctx, round)
				if err != nil {
					// CRIU sees the connection drop and fails the dump.
					log.G(ctx).WithError(err).Warn("migration: page stream")
					conn.Close()
					return
				}
				splice(conn, remote)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, func() { ln.Close() }, nil
}

func (c *client) openPages(ctx context.Context, round int) (io.ReadWriteCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.base.JoinPath("rounds", strconv.Itoa(round), "pages").String(), nil)
	if err != nil {
		return nil, err
	}
	c.header(req)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", pagesProto)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("page stream for round %d: %s: %s", round, resp.Status, strings.TrimSpace(string(b)))
	}
	rw, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, fmt.Errorf("page stream for round %d: connection not handed over", round)
	}
	return rw, nil
}

// splice copies both ways until either side is done, then closes both.
// CRIU's page client waits for the server's answer to its last request
// before it closes, so nothing is in flight by then.
func splice(a, b io.ReadWriteCloser) {
	var once sync.Once
	done := make(chan struct{}, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, _ = io.Copy(dst, src)
		once.Do(func() {
			a.Close()
			b.Close()
		})
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
}
