//go:build linux

package container

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestIDMapRootfs(t *testing.T) {
	// A non-empty upper layer is left as it is (no chown, so no root needed).
	upper := t.TempDir()
	_ = os.WriteFile(filepath.Join(upper, "f"), nil, 0o600)
	in := []mount.Mount{{Type: "overlay", Source: "overlay", Options: []string{"lowerdir=/l", "upperdir=" + upper, "workdir=/w"}}}
	orig := slices.Clone(in[0].Options)

	out, err := idmapRootfs(in, "0:1073741824:65536", os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(out[0].Options, "uidmap=0:1073741824:65536") || !slices.Contains(out[0].Options, "gidmap=0:1073741824:65536") {
		t.Fatalf("options %v", out[0].Options)
	}
	if !slices.Equal(in[0].Options, orig) {
		t.Fatalf("input options changed: %v", in[0].Options)
	}

	if _, err := idmapRootfs([]mount.Mount{{Type: "bind"}}, "0:1:1", 0); err == nil {
		t.Fatal("non-overlay rootfs accepted")
	}
}

func TestUpperDir(t *testing.T) {
	c := &Container{rootfs: []mount.Mount{{Type: "overlay", Options: []string{"lowerdir=/a", "upperdir=/u", "workdir=/w"}}}}
	if u, err := c.UpperDir(); err != nil || u != "/u" {
		t.Fatalf("UpperDir = %q, %v", u, err)
	}
	if _, err := (&Container{rootfs: []mount.Mount{{Type: "bind"}}}).UpperDir(); err == nil {
		t.Fatal("UpperDir of a bind rootfs succeeded")
	}
}

func TestSystemdCgroupPath(t *testing.T) {
	for path, want := range map[string]bool{
		"system.slice:cri-containerd:abc": true,
		"/k8s.io/abc":                     false,
		"":                                false,
	} {
		s := &specs.Spec{Linux: &specs.Linux{CgroupsPath: path}}
		if got := systemdCgroupPath(s); got != want {
			t.Errorf("systemdCgroupPath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCRIUErrors(t *testing.T) {
	log := filepath.Join(t.TempDir(), "dump.log")
	_ = os.WriteFile(log, []byte("(0.1) fine\n(0.2) Error (criu/a.c:1): first\n(0.3) Error (criu/b.c:2): second\n"), 0o600)
	got := criuErrors(log)
	if got != ": (0.2) Error (criu/a.c:1): first; (0.3) Error (criu/b.c:2): second" {
		t.Fatalf("criuErrors = %q", got)
	}
	if got := criuErrors(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("missing log: %q", got)
	}
}

func TestCRIUConfig(t *testing.T) {
	cc := CRIUConfig{Path: filepath.Join(t.TempDir(), "criu.conf"), Options: []string{"a 1"}, RestoreOptions: []string{"b 2"}}
	if err := cc.write(false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cc.Path); string(b) != "a 1\n" {
		t.Fatalf("checkpoint config %q", b)
	}
	if err := cc.write(true); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(cc.Path); string(b) != "a 1\nb 2\n" {
		t.Fatalf("restore config %q", b)
	}
	if err := (CRIUConfig{}).write(true); err != nil {
		t.Fatal("writing without a path:", err)
	}
}
