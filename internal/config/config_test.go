//go:build linux

package config

import (
	"os"
	"path/filepath"
	"testing"

	runcoptions "github.com/containerd/containerd/api/types/runc/options"
	"github.com/containerd/containerd/api/types/runtimeoptions/v1"
	"github.com/containerd/typeurl/v2"

	"github.com/makhov/gcr/internal/userns"
)

func opts(t *testing.T, o *runtimeoptions.Options) *Config {
	t.Helper()
	a, err := typeurl.MarshalAnyToProto(o)
	if err != nil {
		t.Fatal(err)
	}
	c, err := FromOptions(a)
	if err != nil {
		t.Fatal(err)
	}
	return &c
}

func TestFromOptions(t *testing.T) {
	if c, err := FromOptions(nil); err != nil || c.UserNamespaces != userns.Auto {
		t.Fatalf("no options: %+v, %v", c, err)
	}
	if c := opts(t, &runtimeoptions.Options{ConfigBody: []byte("UserNamespaces = 'always'\n")}); c.UserNamespaces != userns.Always {
		t.Fatalf("body: %+v", c)
	}
	path := filepath.Join(t.TempDir(), "gcr.toml")
	_ = os.WriteFile(path, []byte("UserNamespaces = 'never'\n"), 0o600)
	if c := opts(t, &runtimeoptions.Options{ConfigPath: path}); c.UserNamespaces != userns.Never {
		t.Fatalf("path: %+v", c)
	}
	a, _ := typeurl.MarshalAnyToProto(&runcoptions.Options{IoUid: 1000, IoGid: 1000})
	if c, err := FromOptions(a); err != nil || c.IOUID != 1000 || c.UserNamespaces != userns.Auto {
		t.Fatalf("runc options: %+v, %v", c, err)
	}

	a, _ = typeurl.MarshalAnyToProto(&runtimeoptions.Options{ConfigBody: []byte("UserNamespaces = 'maybe'\n")})
	if _, err := FromOptions(a); err == nil {
		t.Fatal("bad mode accepted")
	}
}
