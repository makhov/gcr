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
	task, err := c.C.NewTask(c.ns.Ctx, c.output())
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

// Remove kills and deletes the container, if it is still there. After a
// failed test it first logs what the container's processes are doing.
func (c *Container) Remove() {
	ctx := c.ns.Ctx
	if c.Task != nil && c.ns.t.Failed() {
		c.diagnose()
	}
	if c.ns.t.Failed() {
		// A lazy restore's daemon log: what it copied and zero-filled.
		logs, _ := filepath.Glob(filepath.Join("/run/containerd/io.containerd.runtime.v2.task", c.ns.Name, c.ID, "criu-restore-*", "lazy-pages.log"))
		for _, l := range logs {
			b, _ := os.ReadFile(l)
			var notable []string
			from := map[string]int{}
			for _, line := range strings.Split(string(b), "\n") {
				switch {
				case strings.Contains(line, "Read page from"):
					// "from self", "from parent": which image of a chain.
					f := strings.Fields(line[strings.Index(line, "Read page from")+len("Read page from"):])
					if len(f) > 0 {
						from[f[0]]++
					}
				case strings.Contains(line, "transferred"), strings.Contains(line, "zero page"),
					strings.Contains(line, "REMOVE"), strings.Contains(line, "UNMAP"), strings.Contains(line, "REMAP"),
					strings.Contains(line, "FORK"), strings.Contains(line, "Error"), strings.Contains(line, "Warn"),
					strings.Contains(line, "pages to be handled"), strings.Contains(line, "mcopy_rc"):
					notable = append(notable, line)
				}
			}
			c.ns.t.Logf("%s: page reads by image %v; notable lines:\n%s", l, from, strings.Join(notable[:min(len(notable), 60)], "\n"))
			// khugepaged's collapses so far, against a passing run's.
			if vm, err := os.ReadFile("/proc/vmstat"); err == nil {
				var thp []string
				for _, line := range strings.Split(string(vm), "\n") {
					if strings.HasPrefix(line, "thp_collapse") || strings.HasPrefix(line, "thp_fault") {
						thp = append(thp, line)
					}
				}
				c.ns.t.Logf("/proc/vmstat: %s", strings.Join(thp, ", "))
			}
			// What the daemon did with the pages a crash names, and with
			// those it found already there (EEXIST): something other
			// than the daemon filled them, and the process kept that.
			out, _ := os.ReadFile(c.logPath())
			rl, _ := os.ReadFile(filepath.Join(filepath.Dir(l), "restore.log"))
			for _, pages := range []struct {
				why   string
				pages []string
			}{{"crash", crashPages(string(out))}, {"EEXIST", eexistPages(string(b))}} {
				for _, page := range pages.pages {
					var hits, restored []string
					for _, line := range strings.Split(string(b), "\n") {
						if strings.Contains(line, page) {
							hits = append(hits, line)
						}
					}
					for _, line := range strings.Split(string(rl), "\n") {
						if strings.Contains(line, page) {
							restored = append(restored, line)
						}
					}
					// The mapping the restore put it in, whether it left
					// that page to the daemon, and what else it says of it.
					addr, _ := strconv.ParseUint(page+"000", 16, 64)
					c.ns.t.Logf("%s page %s000: %d lines in the daemon's log\n%s\nits mapping in restore.log:\n%s\nrestore.log lines naming it (%d):\n%s",
						pages.why, page, len(hits), strings.Join(hits[:min(len(hits), 12)], "\n"),
						strings.Join(restoreLines(string(rl), addr), "\n"),
						len(restored), strings.Join(restored[:min(len(restored), 12)], "\n"))
				}
			}
		}
	}
	if c.ns.t.Failed() {
		if b, err := os.ReadFile(c.logPath()); err == nil && len(b) > 0 {
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			c.ns.t.Logf("output of %s (%s), last lines:\n%s", c.ID, c.ns.Name, strings.Join(lines[max(0, len(lines)-30):], "\n"))
		}
	}
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

// ReadInt reads a number from a file in the container's root filesystem,
// waiting briefly for one: the container may be between writes.
func (c *Container) ReadInt(path string) int {
	c.ns.t.Helper()
	var last string
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := c.ReadFile(path)
		if err == nil {
			if n, convErr := strconv.Atoi(s); convErr == nil {
				return n
			}
			last = s
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			c.ns.t.Fatalf("%s: no number after 5s (last %q)", path, last)
		}
		time.Sleep(50 * time.Millisecond)
	}
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

// Checkpoint checkpoints the running container with its read-write
// layer, as ctr container checkpoint --task --rw does, and leaves it
// running. The checkpoint image is removed when the test ends.
func (c *Container) Checkpoint() (containerd.Image, error) {
	ctx := c.ns.Ctx
	ref := "localhost/gcr-e2e-checkpoint/" + c.ID + ":latest"
	img, err := c.C.Checkpoint(ctx, ref,
		containerd.WithCheckpointRuntime, containerd.WithCheckpointImage,
		containerd.WithCheckpointRW, containerd.WithCheckpointTask)
	if err != nil {
		return nil, err
	}
	c.ns.t.Cleanup(func() { _ = c.ns.Client.ImageService().Delete(context.WithoutCancel(ctx), ref) })
	return img, nil
}

// CheckpointRestore checkpoints the running container (with its
// read-write layer), kills it, and restores it from the checkpoint, as
// ctr container checkpoint --task --rw and restore --live --rw do.
func (c *Container) CheckpointRestore() {
	t, ctx := c.ns.t, c.ns.Ctx
	t.Helper()
	img, err := c.Checkpoint()
	if err != nil {
		t.Fatalf("checkpoint %s: %v", c.ID, err)
	}
	c.Remove()
	restored, err := c.ns.Client.Restore(ctx, c.ID, img,
		containerd.WithRestoreImage, containerd.WithRestoreSpec,
		containerd.WithRestoreRuntime, containerd.WithRestoreRW)
	if err != nil {
		t.Fatalf("restore %s: %v", c.ID, err)
	}
	c.C = restored
	task, err := restored.NewTask(ctx, c.output(), containerd.WithTaskCheckpoint(img))
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

// RequireBPFLSM skips a test that needs the kernel's BPF LSM, or fails it
// when GCR_E2E_REQUIRE_BPF is set: a runner meant to exercise the netlink
// policy must not pass by skipping it.
func RequireBPFLSM(t testing.TB, available bool) {
	t.Helper()
	if available {
		return
	}
	const env = "GCR_E2E_REQUIRE_BPF"
	if os.Getenv(env) != "" {
		t.Fatalf("%s is set but the kernel's LSM list has no bpf (lsm=...,bpf on the kernel command line)", env)
	}
	t.Skip("the kernel's LSM list has no bpf")
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

// output sends the container's stdout and stderr to a file, which a
// failed test shows: what a process that crashed said.
func (c *Container) output() cio.Creator {
	_ = os.MkdirAll(filepath.Dir(c.logPath()), 0o755)
	f, err := os.OpenFile(c.logPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return cio.NullIO
	}
	c.ns.t.Cleanup(func() { f.Close() })
	return cio.NewCreator(cio.WithStreams(nil, f, f))
}

func (c *Container) logPath() string {
	return filepath.Join(os.TempDir(), "gcr-e2e-output", c.ns.Name+"-"+c.ID+".log")
}

// diagnose logs, for each process in the container's cgroup, each
// thread's state, what it waits in, and its kernel stack, and whether the
// cgroup is frozen: what a container that stopped making progress is
// stuck on.
func (c *Container) diagnose() {
	t := c.ns.t
	// Not Task.Pid: that is the pid when the task was created, and a
	// container restored at start (a migration's target) has another.
	if st, err := c.Task.Status(c.ns.Ctx); err == nil {
		t.Logf("diagnose %s: task %s, exit status %d", c.ID, st.Status, st.ExitStatus)
	}
	pids, err := c.Task.Pids(c.ns.Ctx)
	if err != nil || len(pids) == 0 {
		t.Logf("diagnose %s: no processes (%v)", c.ID, err)
		return
	}
	cg, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pids[0].Pid))
	if err != nil {
		t.Logf("diagnose %s: %v", c.ID, err)
		return
	}
	dir := filepath.Join("/sys/fs/cgroup", strings.TrimPrefix(strings.TrimSpace(string(cg)), "0::"))
	events, _ := os.ReadFile(filepath.Join(dir, "cgroup.events"))
	t.Logf("diagnose %s: cgroup %s: %s", c.ID, dir, strings.Join(strings.Fields(string(events)), " "))
	procs, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	for _, p := range strings.Fields(string(procs)) {
		tids, _ := os.ReadDir(filepath.Join("/proc", p, "task"))
		for _, tid := range tids {
			base := filepath.Join("/proc", p, "task", tid.Name())
			stat, _ := os.ReadFile(filepath.Join(base, "stat"))
			state := "?"
			if i := strings.LastIndexByte(string(stat), ')'); i > 0 && i+2 < len(stat) {
				state = string(stat[i+2])
			}
			wchan, _ := os.ReadFile(filepath.Join(base, "wchan"))
			stack, _ := os.ReadFile(filepath.Join(base, "stack"))
			lines := strings.Split(strings.TrimSpace(string(stack)), "\n")
			if len(lines) > 8 {
				lines = lines[:8]
			}
			t.Logf("diagnose %s: pid %s tid %s state %s wchan %s stack: %s", c.ID, p, tid.Name(), state, wchan, strings.Join(lines, " <- "))
		}
	}
}

// crashPages are the pages (as hex without the last three digits, as
// CRIU logs addresses) of the addresses a Go crash names: its arguments,
// frame and stack pointers.
func crashPages(output string) []string {
	seen := map[string]bool{}
	var pages []string
	for _, f := range strings.FieldsFunc(output, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r == 'x')
	}) {
		hex, ok := strings.CutPrefix(f, "0x")
		if !ok || len(hex) < 9 || seen[hex[:len(hex)-3]] {
			continue
		}
		seen[hex[:len(hex)-3]] = true
		pages = append(pages, hex[:len(hex)-3])
	}
	if len(pages) > 8 {
		pages = pages[:8]
	}
	return pages
}

// eexistPages are the pages (as crashPages gives them) a lazy-pages
// daemon's copies found already filled: the address of the "uffd_copy:"
// line before each "mcopy_rc:-17".
func eexistPages(log string) []string {
	seen := map[string]bool{}
	var pages []string
	last := ""
	for _, line := range strings.Split(log, "\n") {
		if _, a, ok := strings.Cut(line, "uffd_copy: 0x"); ok {
			last, _, _ = strings.Cut(a, "/")
			continue
		}
		if strings.Contains(line, "mcopy_rc:-17") && len(last) > 3 && !seen[last[:len(last)-3]] {
			seen[last[:len(last)-3]] = true
			pages = append(pages, last[:len(last)-3])
		}
	}
	if len(pages) > 8 {
		pages = pages[:8]
	}
	return pages
}

// restoreLines are the lines of a CRIU restore log whose range covers
// addr: the mapping that holds it ("vma S E", "premap S-E -> P"), the
// reads that filled pages there ("Read A N pages", from any image of a
// chain) and the pages it left to the lazy-pages daemon ("Lazy restore
// skips N pages at A"). A read covering a page it left lazy is a page
// filled twice.
func restoreLines(log string, addr uint64) []string {
	hex := func(f string) (uint64, bool) {
		n, err := strconv.ParseUint(strings.TrimPrefix(f, "0x"), 16, 64)
		return n, err == nil
	}
	covers := func(start, n uint64) bool { return addr >= start && addr < start+n }
	var out []string
	for _, line := range strings.Split(log, "\n") {
		// "(00.036577)      1: ...": drop the timestamp.
		body := line
		if i := strings.Index(line, ") "); strings.HasPrefix(line, "(") && i > 0 {
			body = line[i+2:]
		}
		f := strings.Fields(body)
		for i := range f {
			switch {
			case f[i] == "vma" && i+2 < len(f):
				a, ok1 := hex(f[i+1])
				b, ok2 := hex(f[i+2])
				if ok1 && ok2 && covers(a, b-a) {
					out = append(out, line)
				}
			case f[i] == "premap" && i+1 < len(f):
				r := strings.SplitN(f[i+1], "-", 2)
				if len(r) == 2 {
					a, ok1 := hex(r[0])
					b, ok2 := hex(r[1])
					if ok1 && ok2 && covers(a, b-a) {
						out = append(out, line)
					}
				}
			case f[i] == "Read" && i+3 < len(f) && f[i+3] == "pages":
				a, ok1 := hex(f[i+1])
				n, err := strconv.ParseUint(f[i+2], 10, 64)
				if ok1 && err == nil && covers(a, n*4096) {
					out = append(out, line)
				}
			case f[i] == "skips" && i+4 < len(f) && f[i+3] == "at":
				n, err := strconv.ParseUint(f[i+1], 10, 64)
				a, ok := hex(f[i+4])
				if ok && err == nil && covers(a, n*4096) {
					out = append(out, line)
				}
			}
		}
	}
	if len(out) > 16 {
		out = out[:16]
	}
	return out
}
