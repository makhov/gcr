//go:build linux

// Package control is the shim's per-container control socket for features
// outside containerd's task API (time travel, migration), and its client.
package control

import (
	"bytes"
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

	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/timetravel"
)

// socketDir holds the control sockets; a variable for tests.
var socketDir = "/run/gcr-ctl"

// SocketPath is the control socket of container id in a containerd
// namespace. It is hashed to stay within sun_path's 108 bytes.
func SocketPath(namespace, id string) string {
	h := sha256.Sum256([]byte(namespace + "/" + id))
	return filepath.Join(socketDir, hex.EncodeToString(h[:8])+".sock")
}

type Timeline interface {
	Points() []timetravel.Point
	Rewind(ctx context.Context, point int) error
	Take(ctx context.Context, label string) (timetravel.Point, error)
}

// Handlers are what one container supports; nil ones answer 404.
type Handlers struct {
	Timeline Timeline
	Migrate  func(ctx context.Context, target, token string, opts migrate.Options) (migrate.Result, error)
	// Snapshot writes the running container's processes and writable
	// layer to dir, and leaves it running; containers can then be forked
	// from it.
	Snapshot func(ctx context.Context, dir string) error
	// Info describes the container to a migration's caller.
	Info func() Info
}

// Info is what a target for a migration of the container must match.
type Info struct {
	// IDBase is the first host ID of the container's user namespace
	// range, 0 if it has none: the target is created with it
	// (gcr.dev/userns-base).
	IDBase uint32 `json:"idBase"`
	// Layers are the read-only layers beneath its writable one.
	Layers []string `json:"layers,omitempty"`
}

type pointRequest struct {
	Label string `json:"label"`
}

type snapshotRequest struct {
	Dir string `json:"dir"`
}

type migrateRequest struct {
	Target   string `json:"target"`
	Token    string `json:"token"`
	PostCopy bool   `json:"postCopy,omitempty"`
}

type Server struct {
	path string
	srv  *http.Server
}

func Serve(ctx context.Context, namespace, id string, h Handlers) (*Server, error) {
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
	noTimeline := fmt.Sprintf("time travel is off (start the container with %s)", timetravel.AnnotationTrigger)
	mux.HandleFunc("GET /timeline", func(w http.ResponseWriter, r *http.Request) {
		if h.Timeline == nil {
			http.Error(w, noTimeline, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(h.Timeline.Points())
	})
	mux.HandleFunc("POST /rewind/{point}", func(w http.ResponseWriter, r *http.Request) {
		if h.Timeline == nil {
			http.Error(w, noTimeline, http.StatusNotFound)
			return
		}
		point, err := strconv.Atoi(r.PathValue("point"))
		if err != nil {
			http.Error(w, "bad point id", http.StatusBadRequest)
			return
		}
		if err := h.Timeline.Rewind(context.WithoutCancel(ctx), point); err != nil {
			httpError(w, err)
		}
	})
	mux.HandleFunc("POST /points", func(w http.ResponseWriter, r *http.Request) {
		if h.Timeline == nil {
			http.Error(w, noTimeline, http.StatusNotFound)
			return
		}
		var req pointRequest
		if r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
		}
		p, err := h.Timeline.Take(context.WithoutCancel(ctx), req.Label)
		if err != nil {
			httpError(w, err)
			return
		}
		_ = json.NewEncoder(w).Encode(p)
	})
	mux.HandleFunc("POST /snapshot", func(w http.ResponseWriter, r *http.Request) {
		if h.Snapshot == nil {
			http.Error(w, "snapshots are not supported for this container", http.StatusNotFound)
			return
		}
		var req snapshotRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Dir == "" {
			http.Error(w, "need dir", http.StatusBadRequest)
			return
		}
		if !filepath.IsAbs(req.Dir) {
			http.Error(w, "dir must be absolute", http.StatusBadRequest)
			return
		}
		if err := h.Snapshot(context.WithoutCancel(ctx), req.Dir); err != nil {
			httpError(w, err)
		}
	})
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		if h.Info == nil {
			http.Error(w, "no information for this container", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(h.Info())
	})
	mux.HandleFunc("POST /migrate", func(w http.ResponseWriter, r *http.Request) {
		if h.Migrate == nil {
			http.Error(w, "migration is not supported for this container", http.StatusNotFound)
			return
		}
		var req migrateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
			http.Error(w, "need target", http.StatusBadRequest)
			return
		}
		// Not tied to the client's connection: a half-done migration must
		// run its own cleanup.
		res, err := h.Migrate(context.WithoutCancel(ctx), req.Target, req.Token, migrate.Options{PostCopy: req.PostCopy})
		if err != nil {
			httpError(w, err)
			return
		}
		_ = json.NewEncoder(w).Encode(res)
	})
	s := &Server{path: path, srv: &http.Server{Handler: mux}}
	go func() { _ = s.srv.Serve(ln) }()
	return s, nil
}

func httpError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errdefs.IsNotFound(err):
		code = http.StatusNotFound
	case errdefs.IsFailedPrecondition(err), errdefs.IsInvalidArgument(err):
		code = http.StatusConflict
	}
	http.Error(w, err.Error(), code)
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
	err := c.do(ctx, http.MethodGet, "/timeline", nil, &points)
	return points, err
}

func (c *Client) Rewind(ctx context.Context, point int) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/rewind/%d", point), nil, nil)
}

// TakePoint takes a point on the container's timeline now; label says what
// it comes before.
func (c *Client) TakePoint(ctx context.Context, label string) (timetravel.Point, error) {
	var p timetravel.Point
	err := c.do(ctx, http.MethodPost, "/points", pointRequest{Label: label}, &p)
	return p, err
}

// Snapshot asks the container to write a snapshot to dir and keep running.
func (c *Client) Snapshot(ctx context.Context, dir string) error {
	return c.do(ctx, http.MethodPost, "/snapshot", snapshotRequest{Dir: dir}, nil)
}

func (c *Client) Migrate(ctx context.Context, target, token string, opts migrate.Options) (migrate.Result, error) {
	var res migrate.Result
	err := c.do(ctx, http.MethodPost, "/migrate", migrateRequest{Target: target, Token: token, PostCopy: opts.PostCopy}, &res)
	return res, err
}

func (c *Client) Info(ctx context.Context) (Info, error) {
	var info Info
	err := c.do(ctx, http.MethodGet, "/info", nil, &info)
	return info, err
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://gcr"+path, body)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return fmt.Errorf("no control socket for this container (is it running under gcr?): %w", err)
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
