//go:build linux

package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/checkpoint-restore/go-criu/v8/stats"
)

const (
	maxPreCopyRounds = 5
	// Stop pre-copying once a round writes fewer pages than this...
	smallRoundPages = 2560 // 10 MiB of 4 KiB pages
)

// Source is the running container being migrated.
type Source interface {
	// PreDump sends dirty memory to a page server while the container runs.
	PreDump(ctx context.Context, dir, parent, host string, port int) error
	// CanPreDump is false for containers CRIU cannot pre-dump (with GPUs):
	// they migrate stop-and-copy.
	CanPreDump() bool
	// FreezeDump freezes the container and dumps it; it stays frozen.
	FreezeDump(ctx context.Context, dir, parent, host string, port int) error
	// Thaw lets a frozen container continue (the migration failed).
	Thaw() error
	// Stop ends the frozen source after the target took over.
	Stop() error
	UpperDir() (string, error)
	// IDBase is the user namespace range's first host ID, 0 if none.
	IDBase() uint32
	// Layers are the read-only layers beneath the writable layer.
	Layers() []string
}

// Result describes a finished migration.
type Result struct {
	// PreCopyPages is the number of pages each pre-copy round sent.
	PreCopyPages []uint64      `json:"preCopyPages"`
	Downtime     time.Duration `json:"downtime"`
	// MemoryTracking is false when the kernel cannot track dirty pages and
	// the migration was stop-and-copy only.
	MemoryTracking bool `json:"memoryTracking"`
	// PreCopyFailed is why a pre-copy round failed, after which the
	// migration went on stop-and-copy.
	PreCopyFailed string `json:"preCopyFailed,omitempty"`
}

// Send migrates src to the receiver at target, over TLS for an https://
// target (t says how). work holds the local round directories. If
// anything fails after the freeze, src is thawed and keeps running, and
// the target is told to abort.
func Send(ctx context.Context, src Source, target, token string, t TLS, work string) (res Result, retErr error) {
	u, err := url.Parse(target)
	if err != nil {
		return res, err
	}
	tlsConf, err := t.clientConfig(u)
	if err != nil {
		return res, err
	}
	c := &client{
		base: u, token: token, idBase: src.IDBase(), layers: src.Layers(),
		http: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConf}},
	}
	defer c.http.CloseIdleConnections()
	// Once the target runs the container, the source must never run again:
	// no abort, no thaw, whatever happens after.
	handedOver := false
	defer func() {
		if retErr != nil && !handedOver {
			_ = c.post(context.WithoutCancel(ctx), "/abort", nil)
		}
	}()

	res.MemoryTracking = memoryTracking()
	parent := ""
	restart := false
	if res.MemoryTracking && src.CanPreDump() {
		for i := 0; i < maxPreCopyRounds; i++ {
			round, port, stop, err := c.startRound(ctx, false)
			if err != nil {
				return res, err
			}
			dir := filepath.Join(work, strconv.Itoa(round))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				stop()
				return res, err
			}
			err = src.PreDump(ctx, dir, parent, loopback, port)
			stop()
			if err != nil {
				// Pre-copy only shortens the downtime: go on stop-and-copy.
				// Not from the rounds so far, whose memory may now be
				// incomplete (CRIU may have reset the written-page bits
				// without sending the pages). CRIU can fail here on a task
				// it cannot seize (one waiting on a vfork child).
				res.PreCopyFailed = err.Error()
				parent, restart = "", true
				break
			}
			pages := pagesWritten(dir)
			res.PreCopyPages = append(res.PreCopyPages, pages)
			parent = filepath.Join("..", strconv.Itoa(round))
			n := len(res.PreCopyPages)
			// ...or once rounds stop shrinking by at least 10%.
			if pages < smallRoundPages || (n > 1 && pages*10 > res.PreCopyPages[n-2]*9) {
				break
			}
		}
	}

	round, port, stop, err := c.startRound(ctx, restart)
	if err != nil {
		return res, err
	}
	defer stop()
	dir := filepath.Join(work, strconv.Itoa(round))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, err
	}
	frozen := time.Now()
	if err := src.FreezeDump(ctx, dir, parent, loopback, port); err != nil {
		return res, fmt.Errorf("final dump: %w", err)
	}
	stop()
	defer func() {
		if retErr != nil && !handedOver {
			if err := src.Thaw(); err != nil {
				retErr = fmt.Errorf("%w (and thawing the source failed: %v)", retErr, err)
			}
		}
	}()

	if err := c.putDir(ctx, "/images", dir); err != nil {
		return res, fmt.Errorf("send images: %w", err)
	}
	upper, err := src.UpperDir()
	if err != nil {
		return res, err
	}
	if err := c.putDir(ctx, "/upper", upper); err != nil {
		return res, fmt.Errorf("send writable layer: %w", err)
	}
	if err := c.post(ctx, "/restore", nil); err != nil {
		return res, fmt.Errorf("restore on target: %w", err)
	}
	handedOver = true
	res.Downtime = time.Since(frozen)
	if err := src.Stop(); err != nil {
		return res, fmt.Errorf("target is running but stopping the source failed: %w", err)
	}
	return res, nil
}

// pagesWritten reads how many pages a dump wrote from its stats-dump.
func pagesWritten(dir string) uint64 {
	d, err := os.Open(dir)
	if err != nil {
		return 0
	}
	defer d.Close()
	st, err := stats.CriuGetDumpStats(d)
	if err != nil {
		return 0
	}
	return st.GetPagesWritten()
}

type client struct {
	base   *url.URL
	token  string
	idBase uint32
	layers []string
	http   *http.Client
}

func (c *client) header(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(HeaderIDBase, strconv.Itoa(int(c.idBase)))
	if len(c.layers) > 0 {
		req.Header.Set(HeaderLayers, strings.Join(c.layers, ":"))
	}
}

func (c *client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	return c.doHeader(ctx, method, path, body, out, nil)
}

func (c *client) doHeader(ctx context.Context, method, path string, body io.Reader, out any, extra http.Header) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base.JoinPath(path).String(), body)
	if err != nil {
		return err
	}
	c.header(req)
	for k, v := range extra {
		req.Header[k] = v
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *client) post(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, out)
}

// startRound has the target start a page server for the next round, and
// returns the loopback port CRIU's page client sends that round's memory
// to, and how to stop listening there once the dump is done.
func (c *client) startRound(ctx context.Context, restart bool) (round, port int, stop func(), err error) {
	var r struct{ Round int }
	var header http.Header
	if restart {
		header = http.Header{HeaderRestart: {"1"}}
	}
	if err := c.doHeader(ctx, http.MethodPost, "/rounds", nil, &r, header); err != nil {
		return 0, 0, nil, err
	}
	port, stop, err = c.pageProxy(ctx, r.Round)
	if err != nil {
		return 0, 0, nil, err
	}
	return r.Round, port, stop, nil
}

func (c *client) putDir(ctx context.Context, path, dir string) error {
	body, wait, err := tarDir(dir)
	if err != nil {
		return err
	}
	err = c.do(ctx, http.MethodPut, path, body, nil)
	body.Close()
	if werr := wait(); err == nil && werr != nil {
		err = fmt.Errorf("tar %s: %w", dir, werr)
	}
	return err
}
