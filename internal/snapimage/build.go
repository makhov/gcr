//go:build linux

package snapimage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/core/remotes/docker/config"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/cdi"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"oras.land/oras-go/v2/registry/remote/credentials"
)

// RuntimeType is gcr's containerd runtime.
const RuntimeType = "io.containerd.gcr.v2"

// BuildOptions says how to run the container Build snapshots.
type BuildOptions struct {
	// Address is containerd's socket.
	Address string
	// Namespace is the containerd namespace it runs in.
	Namespace string
	// ID names the container.
	ID string
	// Image is the image to run, pulled if containerd has not got it.
	Image string
	// PlainHTTP pulls it over HTTP.
	PlainHTTP bool
	// Args, if set, replace the image's command, as a pod's command does.
	Args []string
	// Env is added to the image's.
	Env []string
	// UID and GID run the process, if UID is not -1 (runAsUser).
	UID, GID int
	// NoNewPrivileges is the pod's allowPrivilegeEscalation: false.
	NoNewPrivileges bool
	// Volumes are where the pod mounts volumes: each gets an empty
	// directory.
	Volumes []string
	// Devices are CDI devices the pod is given (nvidia.com/gpu=0), as the
	// device plugin gives them: a snapshot of a container using a GPU has
	// to be taken with the GPU.
	Devices []string
	// ServiceAccount mounts a service account's token where Kubernetes
	// does: false for pods with automountServiceAccountToken: false.
	ServiceAccount bool
	// Ready, if set, runs in the container until it exits 0; Timeout
	// bounds that.
	Ready   []string
	Timeout time.Duration
	// Settle is how long to wait after it is ready.
	Settle time.Duration
	// Log, if set, gets the container's output.
	Log *os.File
	// Say reports each step.
	Say func(format string, args ...any)
}

// Build runs Image the way a Kubernetes pod's container runs on gcr, so
// that a snapshot of it restores as one: in network, IPC and UTS
// namespaces it joins rather than owns, as a pod's container joins its
// sandbox's, with /etc/hosts, /etc/hostname, /etc/resolv.conf, /dev/shm,
// /dev/termination-log, the service account's token and each volume
// mounted from outside, where the kubelet mounts them. CRIU leaves those
// out of the snapshot and the restored container gets its pod's. Its
// network has only a loopback. It returns once the container is ready;
// stop removes it and what it used.
func Build(ctx context.Context, o BuildOptions) (stop func(), retErr error) {
	ctx = namespaces.WithNamespace(ctx, o.Namespace)
	var undo []func()
	cleanup := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	defer func() {
		if retErr != nil {
			cleanup()
		}
	}()
	client, err := containerd.New(o.Address)
	if err != nil {
		return nil, fmt.Errorf("containerd at %s: %w", o.Address, err)
	}
	undo = append(undo, func() { client.Close() })

	img, err := client.GetImage(ctx, o.Image)
	if err != nil {
		o.Say("pulling %s", o.Image)
		if img, err = client.Pull(ctx, o.Image, containerd.WithPullUnpack, containerd.WithResolver(resolver(o.PlainHTTP))); err != nil {
			return nil, fmt.Errorf("pull %s: %w", o.Image, err)
		}
	} else if ok, err := img.IsUnpacked(ctx, defaults.DefaultSnapshotter); err == nil && !ok {
		if err := img.Unpack(ctx, defaults.DefaultSnapshotter); err != nil {
			return nil, err
		}
	}

	// What the kubelet and the CRI put in place for a pod.
	dir, err := os.MkdirTemp("", "gcr-build-")
	if err != nil {
		return nil, err
	}
	undo = append(undo, func() { os.RemoveAll(dir) })
	hostname := o.ID
	resolv, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		resolv = nil
	}
	files := map[string][]byte{
		"hosts":             []byte("127.0.0.1\tlocalhost\n::1\tlocalhost ip6-localhost ip6-loopback\n"),
		"hostname":          []byte(hostname + "\n"),
		"resolv.conf":       resolv,
		"termination-log":   nil,
		"sa/ca.crt":         nil,
		"sa/namespace":      []byte("default"),
		"sa/token":          nil,
		"shm/.keep-mounted": nil,
	}
	for name, b := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return nil, err
		}
	}
	shm := filepath.Join(dir, "shm")
	if err := os.Remove(filepath.Join(shm, ".keep-mounted")); err != nil {
		return nil, err
	}
	if err := unix.Mount("shm", shm, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, "mode=1777,size=65536k"); err != nil {
		return nil, fmt.Errorf("mount /dev/shm's tmpfs: %w", err)
	}
	undo = append(undo, func() { _ = unix.Unmount(shm, unix.MNT_DETACH) })
	bind := func(dst, src string, ro bool) specs.Mount {
		opts := []string{"rbind", "rprivate", "rw"}
		if ro {
			opts[2] = "ro"
		}
		return specs.Mount{Destination: dst, Type: "bind", Source: src, Options: opts}
	}
	mounts := []specs.Mount{
		bind("/etc/hosts", filepath.Join(dir, "hosts"), false),
		bind("/dev/termination-log", filepath.Join(dir, "termination-log"), false),
		bind("/etc/hostname", filepath.Join(dir, "hostname"), false),
		bind("/etc/resolv.conf", filepath.Join(dir, "resolv.conf"), false),
		bind("/dev/shm", shm, false),
	}
	if o.ServiceAccount {
		mounts = append(mounts, bind("/var/run/secrets/kubernetes.io/serviceaccount", filepath.Join(dir, "sa"), true))
	}
	for i, v := range o.Volumes {
		src := filepath.Join(dir, fmt.Sprintf("volume-%d", i))
		if err := os.Mkdir(src, 0o777); err != nil {
			return nil, err
		}
		mounts = append(mounts, bind(v, src, false))
	}

	// The sandbox: a process holding the network, IPC and UTS namespaces
	// the container joins, as a pod's pause container does. The network
	// has only a loopback: not the host's, which CRIU would take for its
	// own and leave a restored pod in.
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	holder := exec.Command(self, HolderCommand, hostname)
	holder.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWUTS | unix.CLONE_NEWIPC | unix.CLONE_NEWNET, Pdeathsig: unix.SIGKILL}
	if err := holder.Start(); err != nil {
		return nil, fmt.Errorf("the sandbox's namespaces: %w", err)
	}
	undo = append(undo, func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	nsPath := func(ns string) string { return fmt.Sprintf("/proc/%d/ns/%s", holder.Process.Pid, ns) }

	specOpts := []oci.SpecOpts{
		oci.WithImageConfig(img),
		oci.WithEnv(o.Env),
		// The CRI's mounts, not containerd's defaults (which add /run and
		// a /dev/shm of the container's own).
		func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
			s.Mounts = append(criMounts(), mounts...)
			return nil
		},
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: nsPath("net")}),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.IPCNamespace, Path: nsPath("ipc")}),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.UTSNamespace, Path: nsPath("uts")}),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.CgroupNamespace}),
		// gcr runs it as a pod's container: no user namespace of its own,
		// and the hardening a pod gets.
		oci.WithAnnotations(map[string]string{
			"io.kubernetes.cri.container-type": "container",
			"io.kubernetes.cri.sandbox-id":     o.ID,
		}),
		func(_ context.Context, _ oci.Client, _ *containers.Container, s *specs.Spec) error {
			s.Process.NoNewPrivileges = o.NoNewPrivileges
			s.Hostname = ""
			return nil
		},
	}
	if len(o.Args) > 0 {
		specOpts = append(specOpts, oci.WithProcessArgs(o.Args...))
	}
	if o.UID >= 0 {
		specOpts = append(specOpts, oci.WithUIDGID(uint32(o.UID), uint32(o.GID)))
	}
	if len(o.Devices) > 0 {
		// Last: injecting a device adds mounts, environment, hooks and
		// device nodes, and no option after it may reset those fields.
		// The mounts above do.
		specOpts = append(specOpts, cdi.WithCDIDevices(o.Devices...))
	}
	o.Say("starting %s as %s", o.Image, o.ID)
	c, err := client.NewContainer(ctx, o.ID,
		containerd.WithImage(img),
		containerd.WithNewSnapshot(o.ID, img),
		containerd.WithRuntime(RuntimeType, nil),
		containerd.WithNewSpec(specOpts...))
	if err != nil {
		return nil, fmt.Errorf("create %s: %w", o.ID, err)
	}
	undo = append(undo, func() {
		_ = c.Delete(context.WithoutCancel(ctx), containerd.WithSnapshotCleanup)
	})
	creator := cio.NullIO
	if o.Log != nil {
		creator = cio.NewCreator(cio.WithStreams(nil, o.Log, o.Log))
	}
	task, err := c.NewTask(ctx, creator)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", o.ID, err)
	}
	undo = append(undo, func() {
		ctx := context.WithoutCancel(ctx)
		_, _ = task.Delete(ctx, containerd.WithProcessKill)
	})
	exited, err := task.Wait(ctx)
	if err != nil {
		return nil, err
	}
	if err := task.Start(ctx); err != nil {
		return nil, fmt.Errorf("start %s: %w", o.ID, err)
	}

	if len(o.Ready) > 0 {
		o.Say("waiting for %q to succeed in it", strings.Join(o.Ready, " "))
		deadline := time.Now().Add(o.Timeout)
		for i := 0; ; i++ {
			select {
			case st := <-exited:
				return nil, fmt.Errorf("%s exited (%d) before it was ready", o.ID, st.ExitCode())
			default:
			}
			code, err := execIn(ctx, task, fmt.Sprintf("ready-%d", i), o.Ready)
			if err != nil {
				return nil, fmt.Errorf("ready check: %w", err)
			}
			if code == 0 {
				break
			}
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("%s not ready after %s (%q exits %d)", o.ID, o.Timeout, strings.Join(o.Ready, " "), code)
			}
			time.Sleep(time.Second)
		}
	}
	if o.Settle > 0 {
		time.Sleep(o.Settle)
	}
	select {
	case st := <-exited:
		return nil, fmt.Errorf("%s exited (%d)", o.ID, st.ExitCode())
	default:
	}
	return cleanup, nil
}

// criMounts are the mounts containerd's CRI gives every container,
// before the kubelet's.
func criMounts() []specs.Mount {
	return []specs.Mount{
		{Destination: "/proc", Type: "proc", Source: "proc", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k"}},
		{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620", "gid=5"}},
		{Destination: "/dev/mqueue", Type: "mqueue", Source: "mqueue", Options: []string{"nosuid", "noexec", "nodev"}},
		{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
		{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev", "relatime", "ro"}},
	}
}

// execIn runs args in task's container and returns its exit code.
func execIn(ctx context.Context, task containerd.Task, id string, args []string) (int, error) {
	spec, err := task.Spec(ctx)
	if err != nil {
		return 0, err
	}
	pspec := *spec.Process
	pspec.Args, pspec.Terminal = args, false
	p, err := task.Exec(ctx, id, &pspec, cio.NullIO)
	if err != nil {
		return 0, err
	}
	defer p.Delete(context.WithoutCancel(ctx))
	st, err := p.Wait(ctx)
	if err != nil {
		return 0, err
	}
	if err := p.Start(ctx); err != nil {
		return 0, err
	}
	s := <-st
	return int(s.ExitCode()), s.Error()
}

// HolderCommand is the gcr command that holds a build's sandbox
// namespaces: Holder.
const HolderCommand = "build-sandbox"

// Holder sets the hostname of the UTS namespace it was started in and
// waits to be killed.
func Holder(hostname string) error {
	if err := unix.Sethostname([]byte(hostname)); err != nil {
		return err
	}
	if err := loopbackUp(); err != nil {
		return fmt.Errorf("loopback: %w", err)
	}
	select {}
}

// loopbackUp brings up the loopback of the network namespace it runs in.
func loopbackUp() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	ifr, err := unix.NewIfreq("lo")
	if err != nil {
		return err
	}
	if err := unix.IoctlIfreq(fd, unix.SIOCGIFFLAGS, ifr); err != nil {
		return err
	}
	ifr.SetUint16(ifr.Uint16() | unix.IFF_UP)
	return unix.IoctlIfreq(fd, unix.SIOCSIFFLAGS, ifr)
}

// resolver resolves images with the credentials in Docker's config.
func resolver(plainHTTP bool) remotes.Resolver {
	store, err := credentials.NewStoreFromDocker(credentials.StoreOptions{})
	creds := func(host string) (string, string, error) {
		if err != nil {
			return "", "", nil
		}
		key := host
		if host == "registry-1.docker.io" {
			key = "https://index.docker.io/v1/"
		}
		c, err := store.Get(context.Background(), key)
		if err != nil {
			return "", "", nil
		}
		if c.RefreshToken != "" {
			return "", c.RefreshToken, nil
		}
		return c.Username, c.Password, nil
	}
	opts := config.HostOptions{Credentials: creds}
	if plainHTTP {
		opts.DefaultScheme = "http"
	}
	return docker.NewResolver(docker.ResolverOptions{Hosts: config.ConfigureHosts(context.Background(), opts)})
}
