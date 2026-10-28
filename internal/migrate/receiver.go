//go:build linux

package migrate

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"
)

const (
	// AnnotationIn makes a container wait for an incoming migration instead
	// of starting its process; the value is the host:port to listen on.
	AnnotationIn = "gcr.dev/migrate-in"
	// AnnotationToken is the shared secret the source must present.
	AnnotationToken = "gcr.dev/migrate-token"
)

// Receiver is the target side of a migration, run by the target container's
// shim. Its Start waits on Wait, then restores from the returned directory.
//
//	POST /rounds   start a page server for the next round -> {"round","port"}
//	PUT  /images   the final dump's images (tar), after its page server is done
//	PUT  /upper    the writable layer (tar)
//	POST /restore  restore and start; answers once the container runs
//	POST /abort    give up; Start fails
type Receiver struct {
	ctx    context.Context
	host   string
	token  string
	root   string
	upper  func() (string, error)
	idBase func() uint32

	srv *http.Server
	ln  net.Listener

	mu    sync.Mutex
	round int
	ps    *pageServer

	ready   chan string
	result  chan error
	aborted chan struct{}
	once    sync.Once
}

// NewReceiver listens on addr. root holds the round directories; upper
// returns the target container's writable layer and idBase its user
// namespace range (0 if none).
func NewReceiver(ctx context.Context, addr, token, root string, upper func() (string, error), idBase func() uint32) (*Receiver, error) {
	if token == "" {
		return nil, fmt.Errorf("%s needs %s: %w", AnnotationIn, AnnotationToken, errdefs.ErrInvalidArgument)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("%s=%q: %w", AnnotationIn, addr, errdefs.ErrInvalidArgument)
	}
	if host == "" {
		host = "0.0.0.0"
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	r := &Receiver{
		ctx:  ctx,
		host: host, token: token, root: root, upper: upper, idBase: idBase,
		ready: make(chan string, 1), result: make(chan error, 1), aborted: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rounds", r.auth(r.startRound))
	mux.HandleFunc("PUT /images", r.auth(r.putImages))
	mux.HandleFunc("PUT /upper", r.auth(r.putUpper))
	mux.HandleFunc("POST /restore", r.auth(r.restore))
	mux.HandleFunc("POST /abort", r.auth(r.abort))
	r.srv = &http.Server{Handler: mux}
	r.ln = ln
	go func() { _ = r.srv.Serve(ln) }()
	log.G(ctx).WithField("addr", ln.Addr()).Info("migration receiver listening")
	return r, nil
}

// Addr is the address the receiver listens on.
func (r *Receiver) Addr() string { return r.ln.Addr().String() }

// Wait blocks until the source asks for the restore and returns the images
// directory to restore from.
func (r *Receiver) Wait(ctx context.Context) (string, error) {
	select {
	case dir := <-r.ready:
		return dir, nil
	case <-r.aborted:
		return "", errors.New("migration aborted by the source")
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Done reports the restore's outcome to the source.
func (r *Receiver) Done(err error) {
	r.result <- err
}

func (r *Receiver) Close() {
	_ = r.srv.Close()
	r.mu.Lock()
	if r.ps != nil {
		r.ps.Kill()
	}
	r.mu.Unlock()
}

func (r *Receiver) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		want := "Bearer " + r.token
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, req)
	}
}

func (r *Receiver) dir(round int) string {
	return filepath.Join(r.root, strconv.Itoa(round))
}

// finishRound waits for the current page server; callers hold r.mu.
func (r *Receiver) finishRound() error {
	if r.ps == nil {
		return nil
	}
	err := r.ps.Wait()
	r.ps = nil
	return err
}

// HeaderIDBase carries the source's user namespace range (0: none). CRIU
// restores processes into the mapping they were dumped with, so the target
// must have been created with the same range.
const HeaderIDBase = "Gcr-Userns-Base"

func (r *Receiver) startRound(w http.ResponseWriter, req *http.Request) {
	src, _ := strconv.ParseUint(req.Header.Get(HeaderIDBase), 10, 32)
	if dst := r.idBase(); uint32(src) != dst {
		hint := fmt.Sprintf("create the target with %s=%d", "gcr.dev/userns-base", src)
		if src == 0 {
			hint = "create the target with gcr.dev/host-users=true"
		}
		http.Error(w, fmt.Sprintf("user namespace ranges differ (source %d, target %d): %s", src, dst, hint), http.StatusConflict)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.finishRound(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	round := r.round + 1
	dir := r.dir(round)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	parent := ""
	if round > 1 {
		parent = filepath.Join("..", strconv.Itoa(round-1))
	}
	// Not the request's context: the page server outlives this request.
	ps, err := startPageServer(r.ctx, r.host, dir, parent)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.round, r.ps = round, ps
	_ = json.NewEncoder(w).Encode(map[string]int{"round": round, "port": ps.Port})
}

func (r *Receiver) putImages(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.round == 0 {
		http.Error(w, "no round started", http.StatusConflict)
		return
	}
	if err := r.finishRound(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := untarDir(req.Body, r.dir(r.round)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (r *Receiver) putUpper(w http.ResponseWriter, req *http.Request) {
	upper, err := r.upper()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The target's layer is fresh from the image; the source's replaces it.
	entries, err := os.ReadDir(upper)
	if err == nil {
		for _, e := range entries {
			if err = os.RemoveAll(filepath.Join(upper, e.Name())); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = untarDir(req.Body, upper)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (r *Receiver) restore(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	round := r.round
	r.mu.Unlock()
	select {
	case r.ready <- r.dir(round):
	default:
		http.Error(w, "restore already requested", http.StatusConflict)
		return
	}
	select {
	case err := <-r.result:
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	case <-req.Context().Done():
	}
}

func (r *Receiver) abort(w http.ResponseWriter, req *http.Request) {
	r.once.Do(func() { close(r.aborted) })
}
