//go:build e2e && linux

// Package framework is the harness for gcr's end-to-end tests: containers
// and tasks through containerd's client, pods through CRI, and static
// helper programs to run inside containers.
//
// The tests need root, containerd with the gcr runtime handler, CNI, the
// CRIU fork and network access for images. Run them with
//
//	go test -exec sudo -p 1 -tags e2e ./test/e2e/...
//
// (make e2e), one package at a time: some share host resources.
package framework

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/containerd/api/types/runtimeoptions/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

const (
	Runtime = "io.containerd.gcr.v2"
	Busybox = "docker.io/library/busybox:latest"
	Socket  = "/run/containerd/containerd.sock"
)

// Main runs a test package's tests if it runs as root, and skips them
// otherwise: call it from TestMain.
func Main(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Println("e2e tests need root (go test -exec sudo); skipping")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var (
	clientOnce sync.Once
	client     *containerd.Client
	clientErr  error
)

// Client is the shared containerd client.
func Client(t testing.TB) *containerd.Client {
	t.Helper()
	clientOnce.Do(func() { client, clientErr = containerd.New(Socket) })
	if clientErr != nil {
		t.Fatalf("containerd: %v", clientErr)
	}
	return client
}

// Namespace is a containerd namespace for a test.
type Namespace struct {
	t      testing.TB
	Name   string
	Ctx    context.Context
	Client *containerd.Client
}

// NewNamespace returns containerd namespace name for t.
func NewNamespace(t testing.TB, name string) *Namespace {
	t.Helper()
	ctx, cancel := context.WithCancel(namespaces.WithNamespace(context.Background(), name))
	t.Cleanup(cancel)
	return &Namespace{t: t, Name: name, Ctx: ctx, Client: Client(t)}
}

// Pull pulls and unpacks ref, unless it is there already.
func (n *Namespace) Pull(ref string) containerd.Image {
	n.t.Helper()
	if img, err := n.Client.GetImage(n.Ctx, ref); err == nil {
		if ok, _ := img.IsUnpacked(n.Ctx, ""); ok {
			return img
		}
	}
	img, err := n.Client.Pull(n.Ctx, ref, containerd.WithPullUnpack)
	if err != nil {
		n.t.Fatalf("pull %s: %v", ref, err)
	}
	return img
}

// Spec describes a container to run.
type Spec struct {
	Image       string // Busybox if empty
	Args        []string
	Annotations map[string]string
	Mounts      []specs.Mount
	// RuntimeConfig is the runtime handler's options as TOML
	// (Hardening, Landlock, ...); empty for the defaults.
	RuntimeConfig string
	SpecOpts      []oci.SpecOpts
}

// Container is a container with a task, removed when the test ends.
type Container struct {
	ns   *Namespace
	ID   string
	C    containerd.Container
	Task containerd.Task
}

// UniqueID is a container ID unique to this test run.
func UniqueID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), time.Now().UnixNano()%100000)
}

func (n *Namespace) create(id string, s Spec) containerd.Container {
	n.t.Helper()
	ref := s.Image
	if ref == "" {
		ref = Busybox
	}
	img := n.Pull(ref)
	specOpts := []oci.SpecOpts{oci.WithImageConfig(img)}
	if len(s.Args) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(s.Args...))
	}
	if len(s.Annotations) > 0 {
		specOpts = append(specOpts, oci.WithAnnotations(s.Annotations))
	}
	if len(s.Mounts) > 0 {
		specOpts = append(specOpts, oci.WithMounts(s.Mounts))
	}
	specOpts = append(specOpts, s.SpecOpts...)
	var runtimeOpts any
	if s.RuntimeConfig != "" {
		runtimeOpts = &runtimeoptions.Options{ConfigBody: []byte(s.RuntimeConfig)}
	}
	c, err := n.Client.NewContainer(n.Ctx, id,
		containerd.WithNewSnapshot(id, img),
		containerd.WithNewSpec(specOpts...),
		containerd.WithRuntime(Runtime, runtimeOpts))
	if err != nil {
		n.t.Fatalf("create %s: %v", id, err)
	}
	return c
}

// Create creates a container without starting its task.
func (n *Namespace) Create(id string, s Spec) *Container {
	n.t.Helper()
	c := &Container{ns: n, ID: id, C: n.create(id, s)}
	n.t.Cleanup(c.Remove)
	return c
}

// Start creates and starts the container's task. It does not fail the
// test, so it can run in another goroutine (an incoming migration's task
// starts only once the checkpoint arrives).
func (c *Container) Start() error {
	task, err := c.C.NewTask(c.ns.Ctx, cio.NullIO)
	if err != nil {
		return fmt.Errorf("task %s: %w", c.ID, err)
	}
	c.Task = task
	if err := task.Start(c.ns.Ctx); err != nil {
		return fmt.Errorf("start %s: %w", c.ID, err)
	}
	return nil
}

// Run starts a container in the background.
func (n *Namespace) Run(id string, s Spec) *Container {
	n.t.Helper()
	c := n.Create(id, s)
	if err := c.Start(); err != nil {
		n.t.Fatal(err)
	}
	return c
}

// Output runs a container to completion and returns its output (stdout
// and stderr) and exit code.
func (n *Namespace) Output(id string, s Spec) (string, int) {
	n.t.Helper()
	c := &Container{ns: n, ID: id, C: n.create(id, s)}
	defer c.Remove()
	var out syncBuffer
	task, err := c.C.NewTask(n.Ctx, cio.NewCreator(cio.WithStreams(nil, &out, &out)))
	if err != nil {
		n.t.Fatalf("task %s: %v", id, err)
	}
	c.Task = task
	exit, err := task.Wait(n.Ctx)
	if err != nil {
		n.t.Fatal(err)
	}
	if err := task.Start(n.Ctx); err != nil {
		n.t.Fatalf("start %s: %v", id, err)
	}
	st := <-exit
	task.IO().Wait()
	return out.String(), int(st.ExitCode())
}

// Remove kills and deletes the container, if it is still there.
func (c *Container) Remove() {
	ctx := c.ns.Ctx
	if c.Task != nil {
		_ = c.Task.Kill(ctx, syscall.SIGKILL)
		if exit, err := c.Task.Wait(ctx); err == nil {
			select {
			case <-exit:
			case <-time.After(5 * time.Second):
			}
		}
		_, _ = c.Task.Delete(ctx, containerd.WithProcessKill)
		c.Task = nil
	}
	if c.C != nil {
		if err := c.C.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
			c.ns.t.Logf("remove %s: %v", c.ID, err)
		}
		c.C = nil
	}
}

// Rootfs is path inside the running container's root filesystem, as the
// host sees it.
func (c *Container) Rootfs(path string) string {
	return filepath.Join("/run/containerd/io.containerd.runtime.v2.task", c.ns.Name, c.ID, "rootfs", path)
}

// ReadFile reads a file in the container's root filesystem, trimmed.
func (c *Container) ReadFile(path string) (string, error) {
	b, err := os.ReadFile(c.Rootfs(path))
	return strings.TrimSpace(string(b)), err
}

// ReadInt reads a number from a file in the container's root filesystem.
func (c *Container) ReadInt(path string) int {
	c.ns.t.Helper()
	s, err := c.ReadFile(path)
	if err != nil {
		c.ns.t.Fatal(err)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		c.ns.t.Fatalf("%s: %q is not a number", path, s)
	}
	return n
}

// Status is the task's status.
func (c *Container) Status() containerd.ProcessStatus {
	st, err := c.Task.Status(c.ns.Ctx)
	if err != nil {
		return containerd.Unknown
	}
	return st.Status
}

var execSeq struct {
	sync.Mutex
	n int
}

// Exec runs args in the container and returns its output and exit code.
func (c *Container) Exec(args ...string) (string, int) {
	c.ns.t.Helper()
	spec, err := c.C.Spec(c.ns.Ctx)
	if err != nil {
		c.ns.t.Fatal(err)
	}
	pspec := *spec.Process
	pspec.Args = args
	pspec.Terminal = false
	execSeq.Lock()
	execSeq.n++
	id := fmt.Sprintf("exec-%d", execSeq.n)
	execSeq.Unlock()
	var out syncBuffer
	p, err := c.Task.Exec(c.ns.Ctx, id, &pspec, cio.NewCreator(cio.WithStreams(nil, &out, &out)))
	if err != nil {
		c.ns.t.Fatalf("exec %v: %v", args, err)
	}
	defer p.Delete(c.ns.Ctx)
	exit, err := p.Wait(c.ns.Ctx)
	if err != nil {
		c.ns.t.Fatal(err)
	}
	if err := p.Start(c.ns.Ctx); err != nil {
		c.ns.t.Fatalf("exec %v: %v", args, err)
	}
	st := <-exit
	p.IO().Wait()
	return out.String(), int(st.ExitCode())
}

// CheckpointRestore checkpoints the running container (with its
// read-write layer), kills it, and restores it from the checkpoint, as
// ctr container checkpoint --task --rw and restore --live --rw do.
func (c *Container) CheckpointRestore() {
	t, ctx := c.ns.t, c.ns.Ctx
	t.Helper()
	ref := "localhost/gcr-e2e-checkpoint/" + c.ID + ":latest"
	img, err := c.C.Checkpoint(ctx, ref,
		containerd.WithCheckpointRuntime, containerd.WithCheckpointImage,
		containerd.WithCheckpointRW, containerd.WithCheckpointTask)
	if err != nil {
		t.Fatalf("checkpoint %s: %v", c.ID, err)
	}
	t.Cleanup(func() { _ = c.ns.Client.ImageService().Delete(context.WithoutCancel(ctx), ref) })
	c.Remove()
	restored, err := c.ns.Client.Restore(ctx, c.ID, img,
		containerd.WithRestoreImage, containerd.WithRestoreSpec,
		containerd.WithRestoreRuntime, containerd.WithRestoreRW)
	if err != nil {
		t.Fatalf("restore %s: %v", c.ID, err)
	}
	c.C = restored
	task, err := restored.NewTask(ctx, cio.NullIO, containerd.WithTaskCheckpoint(img))
	if err != nil {
		t.Fatalf("restore task %s: %v", c.ID, err)
	}
	c.Task = task
	if err := task.Start(ctx); err != nil {
		t.Fatalf("start restored %s: %v%s", c.ID, err, criuLog(err))
	}
}

// criuLog is the tail of the CRIU log a restore error names ("criu log in
// DIR"), which goes away with the container.
func criuLog(err error) string {
	_, rest, ok := strings.Cut(err.Error(), "criu log in ")
	if !ok {
		return ""
	}
	dir, _, _ := strings.Cut(rest, ")")
	b, rerr := os.ReadFile(filepath.Join(dir, "restore.log"))
	if rerr != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return "\nrestore.log:\n" + strings.Join(lines[max(0, len(lines)-25):], "\n")
}

// Eventually polls cond every 100ms until it holds or timeout passes.
func Eventually(t testing.TB, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// BuildStatic builds the Go program in dir (relative to the calling
// test's directory) as a static binary to run inside containers, and
// returns its path.
func BuildStatic(t testing.TB, dir string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(1)
	src := filepath.Join(filepath.Dir(file), dir)
	out := filepath.Join(TempDir(t), filepath.Base(dir))
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-tags", "e2e", "-o", out, ".")
	cmd.Dir = src
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", dir, err, b)
	}
	return out
}

// TempDir is a temporary directory to bind-mount into containers. Unlike
// t.TempDir's, its path is open to every user: CRIU restores bind mounts
// from inside the container's user namespace.
func TempDir(t testing.TB) string {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{filepath.Dir(dir), dir} {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// BindRO is a read-only bind mount of a host file into a container.
func BindRO(src, dst string) specs.Mount {
	return specs.Mount{Type: "bind", Source: src, Destination: dst, Options: []string{"rbind", "ro"}}
}

// syncBuffer is a bytes.Buffer safe for the concurrent writes of stdout
// and stderr.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
