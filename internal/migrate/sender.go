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
}

// Send migrates src to the receiver at target. work holds the local
// round directories. If anything fails after the freeze, src is thawed and
// keeps running, and the target is told to abort.
func Send(ctx context.Context, src Source, target, token, work string) (res Result, retErr error) {
	u, err := url.Parse(target)
	if err != nil {
		return res, err
	}
	c := &client{base: u, token: token, idBase: src.IDBase(), layers: src.Layers()}
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
	if res.MemoryTracking && src.CanPreDump() {
		for i := 0; i < maxPreCopyRounds; i++ {
			round, port, err := c.startRound(ctx)
			if err != nil {
				return res, err
			}
			dir := filepath.Join(work, strconv.Itoa(round))
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return res, err
			}
			if err := src.PreDump(ctx, dir, parent, u.Hostname(), port); err != nil {
				return res, fmt.Errorf("pre-copy round %d: %w", round, err)
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

	round, port, err := c.startRound(ctx)
	if err != nil {
		return res, err
	}
	dir := filepath.Join(work, strconv.Itoa(round))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, err
	}
	frozen := time.Now()
	if err := src.FreezeDump(ctx, dir, parent, u.Hostname(), port); err != nil {
		return res, fmt.Errorf("final dump: %w", err)
	}
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
}

func (c *client) do(ctx context.Context, method, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.base.JoinPath(path).String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set(HeaderIDBase, strconv.Itoa(int(c.idBase)))
	if len(c.layers) > 0 {
		req.Header.Set(HeaderLayers, strings.Join(c.layers, ":"))
	}
	resp, err := http.DefaultClient.Do(req)
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

func (c *client) startRound(ctx context.Context) (round, port int, err error) {
	var r struct{ Round, Port int }
	if err := c.post(ctx, "/rounds", &r); err != nil {
		return 0, 0, err
	}
	return r.Round, r.Port, nil
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
