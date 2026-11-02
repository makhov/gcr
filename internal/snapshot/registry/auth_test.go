//go:build linux

package registry

import (
	"context"
	"testing"
)

func TestParseAuth(t *testing.T) {
	cfg := `{"auths": {
		"registry.example.com": {"auth": "dXNlcjpwYXNz"},
		"https://index.docker.io/v1/": {"username": "hub", "password": "secret"},
		"ghcr.io": {"identitytoken": "tok"}
	}}`
	store, err := parseAuth([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for host, want := range map[string][2]string{
		"registry.example.com": {"user", "pass"},
		"registry-1.docker.io": {"hub", "secret"},
		"docker.io":            {"hub", "secret"},
		"elsewhere.example":    {"", ""},
	} {
		c, err := store.Get(ctx, host)
		if err != nil {
			t.Fatal(err)
		}
		if c.Username != want[0] || c.Password != want[1] {
			t.Errorf("%s: %s:%s, want %s:%s", host, c.Username, c.Password, want[0], want[1])
		}
	}
	if c, _ := store.Get(ctx, "ghcr.io"); c.RefreshToken != "tok" {
		t.Errorf("ghcr.io: token %q", c.RefreshToken)
	}
	for _, bad := range []string{`{`, `{"auths": {"r": {"auth": "!!"}}}`, `{"auths": {"r": {"auth": "bm9jb2xvbg=="}}}`} {
		if _, err := parseAuth([]byte(bad)); err == nil {
			t.Errorf("%s: no error", bad)
		}
	}
}
