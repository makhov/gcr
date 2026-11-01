//go:build linux

package migrate

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/containerd/log"

	"github.com/makhov/gcr/internal/imagetar"
)

const (
	// AnnotationIn makes a container wait for an incoming migration instead
	// of starting its process; the value is the host:port to listen on.
	AnnotationIn = "gcr.dev/migrate-in"
	// AnnotationToken is the shared secret the source must present.
	AnnotationToken = "gcr.dev/migrate-token"
	// MinTokenLength is the shortest token accepted.
	MinTokenLength = 16
)

// Receiver is the target side of a migration, run by the target container's
// shim. Its Start waits on Wait, then restores from the returned directory.
//
//	POST /rounds   start a page server for the next round -> {"round"}
//	               (post-copy: a round without one, its memory to come later)
//	POST /rounds/N/pages  Upgrade: the round's page stream (tunnel.go)
//	POST /lazy-pages      Upgrade: post-copy, the source's page server, for
//	               the restore's lazy-pages daemon (tunnel.go)
//	PUT  /images   the final dump's images (tar), after its page server is done
//	PUT  /upper    the writable layer (tar)
//	PUT  /volume?dest=PATH  an emptyDir volume's files (tar), by where the
//	               container sees it
//	POST /restore  restore and start; answers once the container runs
//	POST /abort    give up; Start fails
type Receiver struct {
	ctx    context.Context
	token  string
	root   string
	upper  func() (string, error)
	idBase func() uint32
	layers func() []string

	srv *http.Server
	ln  net.Listener

	mu    sync.Mutex
	round int
	// chain is how many rounds the current one is incremental on, plus
	// one; 0 before the first round and after a restart.
	chain int
	ps    *pageServer
	// What has been received: each part is taken once, and nothing
	// after the restore was asked for.
	imagesRound int
	upperDone   bool
	volumesDone map[string]bool
	restoring   bool

	// postCopy: the last round's memory is fetched from the source as
	// the restored container touches it; lazy carries the streams to the
	// source's page server, and lazyLn is where the restore connects.
	postCopy bool
	lazy     chan io.ReadWriteCloser
	lazyLn   net.Listener

	// PostCopyCheck, if set, says whether this node can restore lazily;
	// a post-copy migration is refused before the source freezes.
	PostCopyCheck func() error
	// EmptyDirs, if set, are the target container's emptyDir volumes, by
	// where it sees them (Container.EmptyDirs): the source's files are
	// put in each. Without it, any volume sent is refused.
	EmptyDirs func() map[string]string

	ready   chan Incoming
	result  chan error
	aborted chan struct{}
	once    sync.Once
}

// NewReceiver listens on addr, with TLS if t has a certificate. root
// holds the round directories; upper returns the target container's
// writable layer, idBase its user namespace range (0 if none) and layers
// the read-only layers beneath its writable layer.
func NewReceiver(ctx context.Context, addr, token string, t TLS, root string, upper func() (string, error), idBase func() uint32, layers func() []string) (*Receiver, error) {
	if len(token) < MinTokenLength {
		return nil, fmt.Errorf("%s needs %s of at least %d characters: %w", AnnotationIn, AnnotationToken, MinTokenLength, errdefs.ErrInvalidArgument)
	}
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return nil, fmt.Errorf("%s=%q: %w", AnnotationIn, addr, errdefs.ErrInvalidArgument)
	}
	tlsConf, err := t.serverConfig()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if tlsConf != nil {
		ln = tls.NewListener(ln, tlsConf)
	}
	r := &Receiver{
		ctx:   ctx,
		token: token, root: root, upper: upper, idBase: idBase, layers: layers, volumesDone: map[string]bool{},
		ready: make(chan Incoming, 1), result: make(chan error, 1), aborted: make(chan struct{}),
		lazy: make(chan io.ReadWriteCloser, 4),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rounds", r.auth(r.startRound))
	mux.HandleFunc("POST /rounds/{round}/pages", r.auth(r.pages))
	mux.HandleFunc("POST /lazy-pages", r.auth(r.lazyPages))
	mux.HandleFunc("PUT /images", r.auth(r.putImages))
	mux.HandleFunc("PUT /upper", r.auth(r.putUpper))
	mux.HandleFunc("PUT /volume", r.auth(r.putVolume))
	mux.HandleFunc("POST /restore", r.auth(r.restore))
	mux.HandleFunc("POST /abort", r.auth(r.abort))
	// Root listens here: no connection may sit on it for nothing.
	r.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: time.Minute}
	r.ln = ln
	go func() { _ = r.srv.Serve(ln) }()
	log.G(ctx).WithField("addr", ln.Addr()).WithField("tls", tlsConf != nil).Info("migration receiver listening")
	return r, nil
}

// Addr is the address the receiver listens on.
func (r *Receiver) Addr() string { return r.ln.Addr().String() }

// Incoming is what to restore: the images, and for a post-copy migration
// the page server (on loopback, carried to the source) serving the memory.
type Incoming struct {
	Dir        string
	PageServer string
}

// Wait blocks until the source asks for the restore and returns what to
// restore from.
func (r *Receiver) Wait(ctx context.Context) (Incoming, error) {
	select {
	case in := <-r.ready:
		return in, nil
	case <-r.aborted:
		return Incoming{}, errors.New("migration aborted by the source")
	case <-ctx.Done():
		return Incoming{}, ctx.Err()
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

// CloseLazyPages stops taking post-copy page streams, once the restore
// has all its memory. The streams under way are not cut.
func (r *Receiver) CloseLazyPages() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lazyLn != nil {
		r.lazyLn.Close()
	}
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

// abandonRound stops the current round's page server, which the source
// may never have connected to; callers hold r.mu.
func (r *Receiver) abandonRound() {
	if r.ps == nil {
		return
	}
	r.ps.Kill()
	_ = r.ps.Wait()
	r.ps = nil
}

// HeaderRestart, on POST /rounds, says the source gave up on the rounds
// before (a pre-dump failed, after which the memory they hold may be
// incomplete): the new round is a full dump, with no parent.
const HeaderRestart = "Gcr-Restart"

// HeaderIDBase carries the source's user namespace range (0: none). CRIU
// restores processes into the mapping they were dumped with, so the target
// must have been created with the same range.
const HeaderIDBase = "Gcr-Userns-Base"

// HeaderPostCopy, on POST /rounds and POST /restore, makes the migration
// post-copy: the round has no page server, and the restore fetches the
// memory from the source.
const HeaderPostCopy = "Gcr-Post-Copy"

// HeaderLayers carries the read-only layers beneath the source's writable
// layer, ':'-separated (overlay's own separator, so no layer has one). The
// writable layer sent is only what changed above them, so the target must
// have been created with the same ones: a fork of the same snapshot, on
// storage both hosts see at the same path.
const HeaderLayers = "Gcr-Layers"

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
	var srcLayers []string
	if h := req.Header.Get(HeaderLayers); h != "" {
		srcLayers = strings.Split(h, ":")
	}
	if dst := r.layers(); !slices.Equal(srcLayers, dst) {
		http.Error(w, fmt.Sprintf("layers beneath the writable layer differ (source %q, target %q): "+
			"create the target with the source's %s", srcLayers, dst, "gcr.dev/fork-from"), http.StatusConflict)
		return
	}
	postCopy := req.Header.Get(HeaderPostCopy) != ""
	if postCopy && r.PostCopyCheck != nil {
		if err := r.PostCopyCheck(); err != nil {
			http.Error(w, "post-copy: "+err.Error(), http.StatusConflict)
			return
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.restoring {
		http.Error(w, "restore already requested", http.StatusConflict)
		return
	}
	restart := req.Header.Get(HeaderRestart) != ""
	if restart {
		// The source gave up on the rounds so far: their memory may be
		// incomplete. This round is a full dump.
		r.abandonRound()
		r.chain = 0
	} else if err := r.finishRound(); err != nil {
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
	if r.chain > 0 {
		parent = filepath.Join("..", strconv.Itoa(round-1))
	}
	r.chain++
	if postCopy {
		// Its memory comes later, from the source's page server.
		r.round, r.postCopy = round, true
		_ = json.NewEncoder(w).Encode(map[string]int{"round": round})
		return
	}
	// Not the request's context: the page server outlives this request.
	ps, err := startPageServer(r.ctx, loopback, dir, parent)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.round, r.ps = round, ps
	_ = json.NewEncoder(w).Encode(map[string]int{"round": round})
}

func (r *Receiver) putImages(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.round == 0:
		http.Error(w, "no round started", http.StatusConflict)
		return
	case r.restoring, r.imagesRound == r.round:
		http.Error(w, "images already received", http.StatusConflict)
		return
	}
	if err := r.finishRound(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	r.imagesRound = r.round
	// Not GNU tar: CRIU images are regular files and their parent link,
	// and nothing else is extracted, nor anything through a link.
	if err := imagetar.Extract(req.Body, r.dir(r.round)); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}

func (r *Receiver) putUpper(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	taken := r.upperDone || r.restoring
	r.upperDone = true
	r.mu.Unlock()
	if taken {
		http.Error(w, "writable layer already received", http.StatusConflict)
		return
	}
	upper, err := r.upper()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// The target's layer is fresh from the image; the source's replaces it.
	err = clearDir(upper)
	if os.IsNotExist(err) {
		err = nil
	}
	if err == nil {
		err = untarDir(req.Body, upper)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// putVolume replaces the files of the target's emptyDir volume at the
// container path dest with the source's, before the restore: what the
// source's processes have open there must be there for them again.
func (r *Receiver) putVolume(w http.ResponseWriter, req *http.Request) {
	dest := req.URL.Query().Get("dest")
	var dir string
	if r.EmptyDirs != nil {
		dir = r.EmptyDirs()[dest]
	}
	if dir == "" {
		http.Error(w, fmt.Sprintf("the target has no emptyDir volume at %q: create it with the source's volumes", dest), http.StatusConflict)
		return
	}
	r.mu.Lock()
	taken := r.volumesDone[dest] || r.restoring
	r.volumesDone[dest] = true
	r.mu.Unlock()
	if taken {
		http.Error(w, fmt.Sprintf("volume %q already received", dest), http.StatusConflict)
		return
	}
	err := clearDir(dir)
	if err == nil {
		err = untarDir(req.Body, dir)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// clearDir removes what is in dir, not dir itself: an emptyDir's directory
// is the kubelet's (and, for medium: Memory, a tmpfs mount).
func clearDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func (r *Receiver) restore(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	round, already := r.round, r.restoring
	r.restoring = true
	r.mu.Unlock()
	if already {
		http.Error(w, "restore already requested", http.StatusConflict)
		return
	}
	in := Incoming{Dir: r.dir(round)}
	if r.postCopy {
		ln, err := net.Listen("tcp", net.JoinHostPort(loopback, "0"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		r.mu.Lock()
		r.lazyLn = ln
		r.mu.Unlock()
		go r.serveLazyPages(ln)
		in.PageServer = ln.Addr().String()
	}
	select {
	case r.ready <- in:
	default:
		http.Error(w, "restore already requested", http.StatusConflict)
		return
	}
	select {
	case err := <-r.result:
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		// The migration is over either way: stop listening once this
		// answer is out.
		go func() {
			time.Sleep(time.Second)
			r.Close()
		}()
	case <-req.Context().Done():
	}
}

func (r *Receiver) abort(w http.ResponseWriter, req *http.Request) {
	r.once.Do(func() { close(r.aborted) })
}
