//go:build linux

package migrate

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func newTestReceiver(t *testing.T, idBase uint32, layers ...string) (*Receiver, string) {
	t.Helper()
	r, err := NewReceiver(context.Background(), "127.0.0.1:0", "secret", TLS{}, t.TempDir(),
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
	if _, err := NewReceiver(context.Background(), "127.0.0.1:0", "", TLS{}, t.TempDir(), nil, nil, nil); err == nil {
		t.Fatal("receiver without a token accepted")
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
	resp := post(t, url+"/rounds", "secret", "1073807360")
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
		req.Header.Set("Authorization", "Bearer secret")
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
	if resp := post(t, url+"/abort", "secret", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("abort: status %d", resp.StatusCode)
	}
	if _, err := r.Wait(context.Background()); err == nil {
		t.Fatal("Wait did not fail after an abort")
	}
}
