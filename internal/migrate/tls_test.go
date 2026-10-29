//go:build linux

package migrate

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"testing"

	"github.com/containerd/errdefs"

	"github.com/makhov/gcr/internal/migrate/testcerts"
)

func newTLSReceiver(t *testing.T, f testcerts.Files) *Receiver {
	t.Helper()
	r, err := NewReceiver(context.Background(), "127.0.0.1:0", testToken,
		TLS{Cert: f.ServerCert, Key: f.ServerKey, CA: f.CA}, t.TempDir(),
		func() (string, error) { return t.TempDir(), nil },
		func() uint32 { return 0 },
		func() []string { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func newClient(t *testing.T, target string, tl TLS) (*client, error) {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	conf, err := tl.clientConfig(u)
	if err != nil {
		return nil, err
	}
	c := &client{base: u, token: testToken, http: &http.Client{Transport: &http.Transport{TLSClientConfig: conf}}}
	t.Cleanup(c.http.CloseIdleConnections)
	return c, nil
}

func TestTLS(t *testing.T) {
	f := testcerts.Write(t, t.TempDir())
	r := newTLSReceiver(t, f)
	target := "https://" + r.Addr()
	ctx := context.Background()

	for name, tc := range map[string]struct {
		target string
		tls    TLS
		ok     bool
	}{
		"mutual":                {target, TLS{Cert: f.ClientCert, Key: f.ClientKey, CA: f.CA}, true},
		"no client certificate": {target, TLS{CA: f.CA}, false},
		"target not trusted":    {target, TLS{Cert: f.ClientCert, Key: f.ClientKey, CA: f.OtherCA}, false},
		"plain HTTP":            {"http://" + r.Addr(), TLS{}, false},
	} {
		c, err := newClient(t, tc.target, tc.tls)
		if err != nil {
			t.Fatal(err)
		}
		err = c.post(ctx, "/abort", nil)
		if (err == nil) != tc.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestTLSRefusesPlainTargets(t *testing.T) {
	if _, err := newClient(t, "http://127.0.0.1:1", TLS{CA: "/ca.crt"}); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("http:// with MigrationTLS set: %v", err)
	}
	if _, err := newClient(t, "ftp://127.0.0.1:1", TLS{}); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("ftp://: %v", err)
	}
	if err := (TLS{Cert: "/c"}).Validate(); !errdefs.IsInvalidArgument(err) {
		t.Fatalf("Cert without Key: %v", err)
	}
}

// The page stream is carried over the migration's connection, after the
// token is checked, to a page server on loopback (an echo server here).
func TestPageTunnel(t *testing.T) {
	f := testcerts.Write(t, t.TempDir())
	r := newTLSReceiver(t, f)
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		conn, err := echo.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(conn, conn)
		conn.Close()
	}()
	r.mu.Lock()
	r.round, r.ps = 1, &pageServer{Port: echo.Addr().(*net.TCPAddr).Port}
	r.mu.Unlock()
	t.Cleanup(func() { r.ps = nil }) // no process for Close to kill

	c, err := newClient(t, "https://"+r.Addr(), TLS{Cert: f.ClientCert, Key: f.ClientKey, CA: f.CA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.openPages(context.Background(), 2); err == nil {
		t.Fatal("a page stream for a round that has no page server")
	}
	bad := *c
	bad.token = "wrong"
	if _, err := bad.openPages(context.Background(), 1); err == nil {
		t.Fatal("a page stream without the token")
	}

	port, stop, err := c.pageProxy(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	conn, err := net.Dial("tcp", net.JoinHostPort(loopback, strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	msg := []byte("pages for round 1")
	if _, err := conn.Write(msg); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != string(msg) {
		t.Fatalf("through the tunnel: %q, %v", got, err)
	}
}
