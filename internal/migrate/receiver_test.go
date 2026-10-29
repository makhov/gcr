//go:build linux

package migrate

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

const testToken = "secret-0123456789abcdef"

func newTestReceiver(t *testing.T, idBase uint32, layers ...string) (*Receiver, string) {
	t.Helper()
	r, err := NewReceiver(context.Background(), "127.0.0.1:0", testToken, TLS{}, t.TempDir(),
		func() (string, error) { return t.TempDir(), nil },
		func() uint32 { return idBase },
		func() []string { return layers })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, "http://" + r.Addr()
}

func post(t *testing.T, url, token string, idBase string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if idBase != "" {
		req.Header.Set(HeaderIDBase, idBase)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestReceiverNeedsToken(t *testing.T) {
	for _, token := range []string{"", "short"} {
		if _, err := NewReceiver(context.Background(), "127.0.0.1:0", token, TLS{}, t.TempDir(), nil, nil, nil); err == nil {
			t.Fatalf("receiver with token %q accepted", token)
		}
	}
	_, url := newTestReceiver(t, 0)
	for _, token := range []string{"", "wrong"} {
		if resp := post(t, url+"/rounds", token, "0"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", token, resp.StatusCode)
		}
	}
}

func TestReceiverRefusesOtherUserNamespaceRange(t *testing.T) {
	_, url := newTestReceiver(t, 1073741824)
	resp := post(t, url+"/rounds", testToken, "1073807360")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409", resp.StatusCode)
	}
	b := new(strings.Builder)
	_, _ = io.Copy(b, resp.Body)
	if !strings.Contains(b.String(), "gcr.dev/userns-base=1073807360") {
		t.Fatalf("error does not say how to create the target: %q", b)
	}
}

func TestReceiverRefusesOtherLayers(t *testing.T) {
	// A fork's writable layer means nothing above other layers: a target
	// that is not a fork of the same snapshot is refused before any round.
	_, url := newTestReceiver(t, 0, "/snap/b/upper", "/snap/a/upper")
	for _, h := range []string{"", "/snap/a/upper", "/snap/b/upper"} {
		req, _ := http.NewRequest(http.MethodPost, url+"/rounds", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set(HeaderIDBase, "0")
		if h != "" {
			req.Header.Set(HeaderLayers, h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("layers %q: status %d, want 409", h, resp.StatusCode)
		}
	}
}

func TestReceiverAbort(t *testing.T) {
	r, url := newTestReceiver(t, 0)
	if resp := post(t, url+"/abort", testToken, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("abort: status %d", resp.StatusCode)
	}
	if _, err := r.Wait(context.Background()); err == nil {
		t.Fatal("Wait did not fail after an abort")
	}
}

// A source whose pre-copy round failed restarts: the round it gave up on
// is stopped, not waited for (CRIU may never have connected to it), and
// the next is a full dump, since the memory the rounds before hold may be
// incomplete.
func TestReceiverRestart(t *testing.T) {
	if _, err := exec.LookPath("criu"); err != nil || os.Geteuid() != 0 {
		t.Skip("needs criu, as root")
	}
	r, url := newTestReceiver(t, 0)
	round := func(restart bool) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, url+"/rounds", nil)
		req.Header.Set("Authorization", "Bearer "+testToken)
		req.Header.Set(HeaderIDBase, "0")
		if restart {
			req.Header.Set(HeaderRestart, "1")
		}
		done := make(chan *http.Response, 1)
		go func() {
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
			}
			done <- resp
		}()
		select {
		case resp := <-done:
			defer resp.Body.Close()
			var body struct{ Round int }
			if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&body) != nil {
				t.Fatalf("POST /rounds (restart %v): status %d", restart, resp.StatusCode)
			}
			return body.Round
		case <-time.After(10 * time.Second):
			t.Fatalf("POST /rounds (restart %v) hung on the round before", restart)
			return 0
		}
	}
	if n := round(false); n != 1 {
		t.Fatalf("first round %d", n)
	}
	// Nobody connects to round 1's page server; a restart must not wait.
	if n := round(true); n != 2 {
		t.Fatalf("restarted round %d, want 2", n)
	}
	r.mu.Lock()
	parent := r.ps.Parent
	r.mu.Unlock()
	if parent != "" {
		t.Fatalf("the restarted round is incremental on %q, want a full dump", parent)
	}
}
