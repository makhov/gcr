package migrate

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// frozenSource is a Source whose final dump writes nothing, and which
// counts its thaws.
type frozenSource struct {
	mu     sync.Mutex
	thawed int
	upper  string
}

func (s *frozenSource) PreDump(context.Context, string, string, string, int) error { return nil }
func (s *frozenSource) CanPreDump() bool                                           { return false }
func (s *frozenSource) FreezeDump(context.Context, string, string, string, int) error {
	return nil
}
func (s *frozenSource) LazyDump(context.Context, string, string, int) (func() error, func() error, error) {
	return nil, nil, nil
}
func (s *frozenSource) Thaw() error                  { s.mu.Lock(); s.thawed++; s.mu.Unlock(); return nil }
func (s *frozenSource) Stop() error                  { return nil }
func (s *frozenSource) UpperDir() (string, error)    { return s.upper, nil }
func (s *frozenSource) EmptyDirs() map[string]string { return nil }
func (s *frozenSource) IDBase() uint32               { return 0 }
func (s *frozenSource) Layers() []string             { return nil }

// A /restore that gets no answer may have restored the container on the
// target: the source must stay frozen, and the target not be aborted. One
// the target refuses leaves the source to run again.
func TestRestoreWithoutAnswer(t *testing.T) {
	for name, answer := range map[string]func(http.ResponseWriter){
		"connection lost": func(w http.ResponseWriter) {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
		},
		"refused": func(w http.ResponseWriter) { http.Error(w, "restore failed", http.StatusInternalServerError) },
	} {
		t.Run(name, func(t *testing.T) {
			aborted := false
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				switch r.URL.Path {
				case "/rounds":
					_, _ = w.Write([]byte(`{"Round": 1}`))
				case "/restore":
					answer(w)
				case "/abort":
					aborted = true
				}
			}))
			defer target.Close()
			src := &frozenSource{upper: t.TempDir()}
			_, err := Send(context.Background(), src, target.URL, "token", TLS{}, t.TempDir(), Options{})
			if err == nil {
				t.Fatal("no error")
			}
			unknown := name == "connection lost"
			if unknown != strings.Contains(err.Error(), "stays frozen") || unknown == (src.thawed > 0) || unknown == aborted {
				t.Errorf("%v; thawed %d times, aborted %v", err, src.thawed, aborted)
			}
		})
	}
}
