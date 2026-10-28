//go:build linux

// Package control is the shim's per-container control socket for features
// outside containerd's task API (the time-travel timeline), and its client.
package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/containerd/errdefs"

	"github.com/makhov/gcr/internal/timetravel"
)

const socketDir = "/run/gcr-ctl"

// SocketPath is the control socket of container id in a containerd
// namespace. It is hashed to stay within sun_path's 108 bytes.
func SocketPath(namespace, id string) string {
	h := sha256.Sum256([]byte(namespace + "/" + id))
	return filepath.Join(socketDir, hex.EncodeToString(h[:8])+".sock")
}

type Timeline interface {
	Points() []timetravel.Point
	Rewind(ctx context.Context, point int) error
}

type Server struct {
	path string
	srv  *http.Server
}

func Serve(ctx context.Context, namespace, id string, tl Timeline) (*Server, error) {
	path := SocketPath(namespace, id)
	if err := os.MkdirAll(socketDir, 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /timeline", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(tl.Points())
	})
	mux.HandleFunc("POST /rewind/{point}", func(w http.ResponseWriter, r *http.Request) {
		point, err := strconv.Atoi(r.PathValue("point"))
		if err != nil {
			http.Error(w, "bad point id", http.StatusBadRequest)
			return
		}
		if err := tl.Rewind(context.WithoutCancel(ctx), point); err != nil {
			code := http.StatusInternalServerError
			if errdefs.IsNotFound(err) {
				code = http.StatusNotFound
			}
			http.Error(w, err.Error(), code)
		}
	})
	s := &Server{path: path, srv: &http.Server{Handler: mux}}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

func (s *Server) Close() {
	_ = s.srv.Close()
	_ = os.Remove(s.path)
}

// Client talks to one container's control socket.
type Client struct {
	http *http.Client
}

func NewClient(namespace, id string) *Client {
	path := SocketPath(namespace, id)
	return &Client{http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		},
	}}}
}

func (c *Client) Timeline(ctx context.Context) ([]timetravel.Point, error) {
	var points []timetravel.Point
	err := c.do(ctx, http.MethodGet, "/timeline", &points)
	return points, err
}

func (c *Client) Rewind(ctx context.Context, point int) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/rewind/%d", point), nil)
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://gcr"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return fmt.Errorf("no time-travel control socket (is the container running with %s?): %w",
				timetravel.AnnotationTrigger, err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", b)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
