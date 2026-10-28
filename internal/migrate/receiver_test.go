//go:build linux

package migrate

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func newTestReceiver(t *testing.T, idBase uint32) (*Receiver, string) {
	t.Helper()
	r, err := NewReceiver(context.Background(), "127.0.0.1:0", "secret", t.TempDir(),
		func() (string, error) { return t.TempDir(), nil },
		func() uint32 { return idBase })
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
	if _, err := NewReceiver(context.Background(), "127.0.0.1:0", "", t.TempDir(), nil, nil); err == nil {
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

func TestReceiverAbort(t *testing.T) {
	r, url := newTestReceiver(t, 0)
	if resp := post(t, url+"/abort", "secret", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("abort: status %d", resp.StatusCode)
	}
	if _, err := r.Wait(context.Background()); err == nil {
		t.Fatal("Wait did not fail after an abort")
	}
}
