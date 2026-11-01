//go:build e2e && linux

// Package migrate checks live migration on one host, between containerd
// namespaces: a counter migrates and carries on from where it was, and a
// migration the target refuses leaves the source running.
package migrate

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	specs "github.com/opencontainers/runtime-spec/specs-go"

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

func TestMigrate(t *testing.T) { testMigrate(t, "http", "", "", migrate.Options{}) }

// TestMigrateTLS migrates with mutual TLS: memory and files go over the
// one connection, which each side verifies against the CA.
func TestMigrateTLS(t *testing.T) {
	f := testcerts.Write(t, t.TempDir())
	testMigrate(t, "https", tlsConfig(f.ClientCert, f.ClientKey, f.CA), tlsConfig(f.ServerCert, f.ServerKey, f.CA), migrate.Options{})
}

// TestMigratePostCopyTLS: post-copy's page stream goes over the TLS
// connection too.
func TestMigratePostCopyTLS(t *testing.T) {
	f := testcerts.Write(t, t.TempDir())
	testMigrate(t, "https", tlsConfig(f.ClientCert, f.ClientKey, f.CA), tlsConfig(f.ServerCert, f.ServerKey, f.CA), migrate.Options{PostCopy: true})
}

func tlsConfig(cert, key, ca string) string {
	return fmt.Sprintf("[MigrationTLS]\nCert = %q\nKey = %q\nCA = %q\n", cert, key, ca)
}

func testMigrate(t *testing.T, scheme, srcConfig, dstConfig string, opts migrate.Options) {
	// The source holds its user namespace range until it is killed, so on
	// one host the target cannot take it; across hosts it would be created
	// with gcr.dev/userns-base.
	src, dst, token, started := pair(t, map[string]string{userns.AnnotationHostUsers: "true"}, srcConfig, dstConfig)
	if _, err := control.NewClient("gcr-e2e-src", src.ID).Migrate(t.Context(), scheme+"://"+addr, token, opts); err != nil {
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
	_, err := control.NewClient("gcr-e2e-src", src.ID).Migrate(t.Context(), "http://"+addr, token, migrate.Options{})
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
	if _, err := c.Migrate(t.Context(), "https://"+addr, token, migrate.Options{}); err == nil {
		t.Fatal("migrated without a client certificate")
	} else {
		t.Logf("refused: %v", err)
	}
	// And a source with MigrationTLS set will not send in the clear.
	if _, err := c.Migrate(t.Context(), "http://"+addr, token, migrate.Options{}); err == nil || !strings.Contains(err.Error(), "https://") {
		t.Fatalf("plain HTTP with MigrationTLS set: %v", err)
	}
	a := src.ReadInt("/tmp/count")
	time.Sleep(500 * time.Millisecond)
	if b := src.ReadInt("/tmp/count"); b <= a {
		t.Fatalf("source stopped after a refused migration (%d -> %d)", a, b)
	}
}

// TestMigratePostCopy migrates a container holding 256 MiB post-copy: the
// target runs before its memory arrives and fetches it from the source,
// as it touches pages and in the background, through the migration's
// connection. The memory checks itself (hog reads its pattern back from
// pages all over), the target carries on from the source's count, and the
// source is gone once the target has everything.
func TestMigratePostCopy(t *testing.T) { testMigrateMemory(t, true) }

// TestMigrateMemory is TestMigratePostCopy's workload migrated as before,
// stop-and-copy where the kernel cannot track dirty pages (arm64): what
// post-copy saves.
func TestMigrateMemory(t *testing.T) { testMigrateMemory(t, false) }

func testMigrateMemory(t *testing.T, postCopy bool) {
	const mib = "256"
	hog := framework.BuildStatic(t, "../fork/hog")
	spec := framework.Spec{
		Args:   []string{"/hog", mib},
		Mounts: []specs.Mount{framework.BindRO(hog, "/hog")},
		// See testMigrate: one host, one range.
		Annotations: map[string]string{userns.AnnotationHostUsers: "true"},
	}
	id := framework.UniqueID("mig-post")
	token := fmt.Sprintf("secret-%d", time.Now().UnixNano())
	src := framework.NewNamespace(t, "gcr-e2e-src").Run(id, spec)
	framework.Eventually(t, 2*time.Minute, "the source to fill its memory", func() bool {
		s, _ := src.ReadFile("/tmp/ready")
		return s == mib
	})
	in := spec
	in.Annotations = map[string]string{migrate.AnnotationIn: addr, migrate.AnnotationToken: token, userns.AnnotationHostUsers: "true"}
	dst := framework.NewNamespace(t, "gcr-e2e-dst").Create(id, in)
	started := make(chan error, 1)
	go func() { started <- dst.Start() }()
	time.Sleep(2 * time.Second)

	res, err := control.NewClient("gcr-e2e-src", src.ID).Migrate(t.Context(), "http://"+addr, token, migrate.Options{PostCopy: postCopy})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("target: %v", err)
	}
	if postCopy {
		if !res.PostCopy || res.Downtime <= 0 || res.PostCopyTook < res.Downtime {
			t.Fatalf("result %+v", res)
		}
		t.Logf("post-copy of %s MiB: downtime %s, memory fetched by %s", mib,
			res.Downtime.Round(time.Millisecond), res.PostCopyTook.Round(time.Millisecond))
	} else {
		t.Logf("%s MiB, pre-copy rounds %v: downtime %s %v, lazy restore %v", mib, res.PreCopyPages, res.Downtime.Round(time.Millisecond), res.Phases, res.LazyRestore)
		if !res.LazyRestore {
			t.Errorf("the target restored its memory before running (gcr.dev/host-users: no user namespace, no GPU)")
		}
	}
	// Stopped as the migration returns, which a status read can just miss.
	framework.Eventually(t, 5*time.Second, "the source to stop once the target has its memory", func() bool {
		return src.Status() == containerd.Stopped
	})
	last := dst.ReadInt("/tmp/count")
	framework.Eventually(t, time.Minute, "the target to read 25 more pages back", func() bool {
		s, _ := dst.ReadFile("/tmp/count")
		if strings.HasPrefix(s, "corrupt") {
			t.Fatalf("the target read back wrong memory: %s", s)
		}
		n, err := strconv.Atoi(s)
		return err == nil && n >= last+25
	})
}

// TestMigratePostCopyFails: a target that fails to restore, after the
// source froze and sent it everything but its memory, leaves the source
// running with its memory intact. The target here lacks a mount the
// source has, which CRIU's restore refuses.
func TestMigratePostCopyFails(t *testing.T) {
	hog := framework.BuildStatic(t, "../fork/hog")
	hostUsers := map[string]string{userns.AnnotationHostUsers: "true"}
	id := framework.UniqueID("mig-post-fail")
	token := fmt.Sprintf("secret-%d", time.Now().UnixNano())
	src := framework.NewNamespace(t, "gcr-e2e-src").Run(id, framework.Spec{
		Args: []string{"/hog", "64"}, Mounts: []specs.Mount{framework.BindRO(hog, "/hog")}, Annotations: hostUsers,
	})
	framework.Eventually(t, time.Minute, "the source to fill its memory", func() bool {
		s, _ := src.ReadFile("/tmp/ready")
		return s == "64"
	})
	dst := framework.NewNamespace(t, "gcr-e2e-dst").Create(id, framework.Spec{
		Args: counter,
		Annotations: map[string]string{migrate.AnnotationIn: addr, migrate.AnnotationToken: token,
			userns.AnnotationHostUsers: "true"},
	})
	started := make(chan error, 1)
	go func() { started <- dst.Start() }()
	time.Sleep(2 * time.Second)

	_, err := control.NewClient("gcr-e2e-src", src.ID).Migrate(t.Context(), "http://"+addr, token, migrate.Options{PostCopy: true})
	if err == nil {
		t.Fatal("migrated to a target that cannot restore it")
	}
	t.Logf("failed as it should: %v", err)
	if err := <-started; err == nil {
		t.Fatal("the target started")
	}
	if st := src.Status(); st != containerd.Running {
		t.Fatalf("source is %s after a failed migration", st)
	}
	last := src.ReadInt("/tmp/count")
	framework.Eventually(t, time.Minute, "the source to count on", func() bool {
		s, _ := src.ReadFile("/tmp/count")
		if strings.HasPrefix(s, "corrupt") {
			t.Fatalf("the source read back wrong memory: %s", s)
		}
		n, err := strconv.Atoi(s)
		return err == nil && n >= last+10
	})
}

// TestMigrateOpenConnectionAndLock: a container holding an established
// TCP connection and a file lock moves; the connection is closed on the
// target (its address changes across nodes), the lock moves with its
// file, and the processes carry on. Without tcp-close and file-locks,
// CRIU refused to dump it at all.
func TestMigrateOpenConnectionAndLock(t *testing.T) {
	script := []string{"sh", "-c", `
		sleep 100000 | nc -l -p 9000 >/dev/null &
		sleep 0.5
		(while :; do echo x; sleep 1; done) | nc 127.0.0.1 9000 &
		flock /tmp/lock -c "sleep 100000" &
		i=0; while :; do i=$((i+1)); echo $i > /tmp/count; sleep 0.1; done`}
	id := framework.UniqueID("mig-tcp")
	token := fmt.Sprintf("secret-%d", time.Now().UnixNano())
	hostUsers := map[string]string{userns.AnnotationHostUsers: "true"}
	src := framework.NewNamespace(t, "gcr-e2e-src").Run(id, framework.Spec{Args: script, Annotations: hostUsers})
	in := map[string]string{migrate.AnnotationIn: addr, migrate.AnnotationToken: token, userns.AnnotationHostUsers: "true"}
	dst := framework.NewNamespace(t, "gcr-e2e-dst").Create(id, framework.Spec{Args: script, Annotations: in})
	started := make(chan error, 1)
	go func() { started <- dst.Start() }()
	framework.Eventually(t, 30*time.Second, "the source's connection to be established", func() bool {
		out, _ := src.Exec("netstat", "-tn")
		return strings.Contains(out, "ESTABLISHED")
	})
	if _, err := control.NewClient("gcr-e2e-src", src.ID).Migrate(t.Context(), "http://"+addr, token, migrate.Options{}); err != nil {
		t.Fatalf("migrate with an open connection and a lock: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("target: %v", err)
	}
	last := src.ReadInt("/tmp/count")
	time.Sleep(time.Second)
	if moved := dst.ReadInt("/tmp/count"); moved <= last {
		t.Fatalf("target restarted instead of resuming (%d -> %d)", last, moved)
	}
	if out, _ := dst.Exec("netstat", "-tn"); strings.Contains(out, "ESTABLISHED") {
		t.Errorf("the connection is still established on the target:\n%s", out)
	}
}
