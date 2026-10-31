package registry

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// TestRegistryThatNeverAnswers: a registry that takes requests and never
// answers them (one blocked writing its log, say) fails a push's first
// request, rather than holding it forever.
func TestRegistryThatNeverAnswers(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(io.Discard, c) }() // reads, never writes
		}
	}()
	saved := client
	client = newClient(200 * time.Millisecond)
	defer func() { client = saved }()

	repo, err := Repository(ln.Addr().String()+"/snapshots/x:v1", false)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	_, err = repo.Exists(ctx, ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageLayer,
		Digest:    "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Size:      1,
	})
	if err == nil {
		t.Fatal("a registry that never answers: no error")
	}
	if ctx.Err() != nil {
		t.Fatalf("still waiting after %s: %v", time.Since(start).Round(time.Second), err)
	}
	t.Logf("failed after %s: %v", time.Since(start).Round(time.Millisecond), err)
}
