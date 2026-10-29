//go:build e2e && linux

// Package migrate checks live migration on one host, between containerd
// namespaces: a counter migrates and carries on from where it was, and a
// migration the target refuses leaves the source running.
package migrate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"

	"github.com/makhov/gcr/internal/control"
	"github.com/makhov/gcr/internal/migrate"
	"github.com/makhov/gcr/internal/migrate/testcerts"
	"github.com/makhov/gcr/internal/userns"
	"github.com/makhov/gcr/test/e2e/framework"
)

func TestMain(m *testing.M) { framework.Main(m) }

const addr = "127.0.0.1:7781"

var counter = []string{"sh", "-c", "i=0; while true; do i=$((i+1)); echo $i > /tmp/count.new && mv /tmp/count.new /tmp/count; sleep 0.1; done"}

// pair starts the same container in a source and a target namespace; the
// target waits for a migration. annotations go to both; each side's
// runtime handler has its own config.
func pair(t *testing.T, annotations map[string]string, srcConfig, dstConfig string) (src, dst *framework.Container, token string, started <-chan error) {
	id := framework.UniqueID("mig")
	token = fmt.Sprintf("secret-%d", time.Now().UnixNano())
	src = framework.NewNamespace(t, "gcr-e2e-src").Run(id, framework.Spec{Args: counter, Annotations: annotations, RuntimeConfig: srcConfig})
	in := map[string]string{migrate.AnnotationIn: addr, migrate.AnnotationToken: token}
	for k, v := range annotations {
		in[k] = v
	}
	dst = framework.NewNamespace(t, "gcr-e2e-dst").Create(id, framework.Spec{Args: counter, Annotations: in, RuntimeConfig: dstConfig})
	errc := make(chan error, 1)
	go func() { errc <- dst.Start() }()
	time.Sleep(2 * time.Second)
	return src, dst, token, errc
}

func TestMigrate(t *testing.T) { testMigrate(t, "http", "", "") }

// TestMigrateTLS migrates with mutual TLS: memory and files go over the
// one connection, which each side verifies against the CA.
func TestMigrateTLS(t *testing.T) {
	f := testcerts.Write(t, t.TempDir())
	testMigrate(t, "https", tlsConfig(f.ClientCert, f.ClientKey, f.CA), tlsConfig(f.ServerCert, f.ServerKey, f.CA))
}

func tlsConfig(cert, key, ca string) string {
	return fmt.Sprintf("[MigrationTLS]\nCert = %q\nKey = %q\nCA = %q\n", cert, key, ca)
}

func testMigrate(t *testing.T, scheme, srcConfig, dstConfig string) {
	// The source holds its user namespace range until it is killed, so on
	// one host the target cannot take it; across hosts it would be created
	// with gcr.dev/userns-base.
	src, dst, token, started := pair(t, map[string]string{userns.AnnotationHostUsers: "true"}, srcConfig, dstConfig)
	if _, err := control.NewClient("gcr-e2e-src", src.ID).Migrate(t.Context(), scheme+"://"+addr, token); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("target: %v", err)
	}
	last := src.ReadInt("/tmp/count")
	time.Sleep(time.Second)
	if st := dst.Status(); st != containerd.Running {
		t.Fatalf("target is %s", st)
	}
	if st := src.Status(); st != containerd.Stopped {
		t.Fatalf("source is %s", st)
	}
	moved := dst.ReadInt("/tmp/count")
	t.Logf("source stopped at %d; target at %d one second later", last, moved)
	// A fresh start would be at ~10; carrying on means past the source's.
	if moved <= last {
		t.Fatalf("target restarted instead of resuming (%d -> %d)", last, moved)
	}
}

func TestMigrateRefused(t *testing.T) {
	// Each side in its own user namespace range: the target refuses.
	src, _, token, _ := pair(t, nil, "", "")
	_, err := control.NewClient("gcr-e2e-src", src.ID).Migrate(t.Context(), "http://"+addr, token)
	if err == nil {
		t.Fatal("migration between different user namespace ranges succeeded")
	}
	if !strings.Contains(err.Error(), "user namespace ranges differ") {
		t.Fatalf("unexpected error: %v", err)
	}
	a := src.ReadInt("/tmp/count")
	time.Sleep(500 * time.Millisecond)
	if b := src.ReadInt("/tmp/count"); b <= a {
		t.Fatalf("source stopped after a refused migration (%d -> %d)", a, b)
	}
	t.Logf("refused: %v", err)
}

// TestMigrateTLSRefused: a source without a certificate the target's CA
// signed gets nowhere, and keeps running.
func TestMigrateTLSRefused(t *testing.T) {
	f := testcerts.Write(t, t.TempDir())
	src, _, token, _ := pair(t, map[string]string{userns.AnnotationHostUsers: "true"},
		tlsConfig("", "", f.CA), tlsConfig(f.ServerCert, f.ServerKey, f.CA))
	c := control.NewClient("gcr-e2e-src", src.ID)
	if _, err := c.Migrate(t.Context(), "https://"+addr, token); err == nil {
		t.Fatal("migrated without a client certificate")
	} else {
		t.Logf("refused: %v", err)
	}
	// And a source with MigrationTLS set will not send in the clear.
	if _, err := c.Migrate(t.Context(), "http://"+addr, token); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("plain HTTP with MigrationTLS set: %v", err)
	}
	a := src.ReadInt("/tmp/count")
	time.Sleep(500 * time.Millisecond)
	if b := src.ReadInt("/tmp/count"); b <= a {
		t.Fatalf("source stopped after a refused migration (%d -> %d)", a, b)
	}
}
