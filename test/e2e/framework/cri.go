//go:build e2e && linux

package framework

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	cri "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// RuntimeHandler is the CRI runtime handler that runs gcr.
const RuntimeHandler = "gcr"

var (
	criMu    sync.Mutex
	criConns = map[string]*grpc.ClientConn{}
)

// CRI talks to a CRI service: containerd's, or CRI-O's (NewCRIAt).
type CRI struct {
	t       testing.TB
	Ctx     context.Context
	Runtime cri.RuntimeServiceClient
	Images  cri.ImageServiceClient
}

// NewCRI talks to containerd's CRI service.
func NewCRI(t testing.TB) *CRI {
	t.Helper()
	return NewCRIAt(t, Socket)
}

// NewCRIAt talks to the CRI service listening on socket.
func NewCRIAt(t testing.TB, socket string) *CRI {
	t.Helper()
	criMu.Lock()
	conn, ok := criConns[socket]
	if !ok {
		var err error
		if conn, err = grpc.NewClient("unix://"+socket, grpc.WithTransportCredentials(insecure.NewCredentials())); err != nil {
			criMu.Unlock()
			t.Fatalf("CRI at %s: %v", socket, err)
		}
		criConns[socket] = conn
	}
	criMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &CRI{t: t, Ctx: ctx, Runtime: cri.NewRuntimeServiceClient(conn), Images: cri.NewImageServiceClient(conn)}
}

// Pull pulls an image through CRI.
func (c *CRI) Pull(ref string) {
	c.t.Helper()
	if _, err := c.Images.PullImage(c.Ctx, &cri.PullImageRequest{Image: &cri.ImageSpec{Image: ref}}); err != nil {
		c.t.Fatalf("pull %s: %v", ref, err)
	}
}

// Pod is a pod sandbox on the gcr runtime handler, removed when the test
// ends. Its log directory is a temporary directory.
type Pod struct {
	cri    *CRI
	ID     string
	Config *cri.PodSandboxConfig
}

// RunPod runs a pod with gcr's runtime handler. config's metadata gets a
// unique UID and its log directory a temporary one.
func (c *CRI) RunPod(config *cri.PodSandboxConfig) *Pod {
	c.t.Helper()
	return c.RunPodWith(RuntimeHandler, config)
}

// RunPodWith runs a pod with another runtime handler, as RunPod.
func (c *CRI) RunPodWith(handler string, config *cri.PodSandboxConfig) *Pod {
	c.t.Helper()
	if config.Metadata == nil {
		config.Metadata = &cri.PodSandboxMetadata{}
	}
	if config.Metadata.Namespace == "" {
		config.Metadata.Namespace = "default"
	}
	config.Metadata.Uid = UniqueID(config.Metadata.Name)
	config.LogDirectory = c.t.TempDir()
	resp, err := c.Runtime.RunPodSandbox(c.Ctx, &cri.RunPodSandboxRequest{Config: config, RuntimeHandler: handler})
	if err != nil {
		c.t.Fatalf("run pod: %v", err)
	}
	p := &Pod{cri: c, ID: resp.PodSandboxId, Config: config}
	c.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, _ = c.Runtime.StopPodSandbox(ctx, &cri.StopPodSandboxRequest{PodSandboxId: p.ID})
		_, _ = c.Runtime.RemovePodSandbox(ctx, &cri.RemovePodSandboxRequest{PodSandboxId: p.ID})
	})
	return p
}

// Start creates and starts a container in the pod; its image is pulled
// first.
func (p *Pod) Start(config *cri.ContainerConfig) string {
	c := p.cri
	c.t.Helper()
	c.Pull(config.Image.Image)
	resp, err := c.Runtime.CreateContainer(c.Ctx, &cri.CreateContainerRequest{
		PodSandboxId: p.ID, Config: config, SandboxConfig: p.Config})
	if err != nil {
		c.t.Fatalf("create container: %v", err)
	}
	if _, err := c.Runtime.StartContainer(c.Ctx, &cri.StartContainerRequest{ContainerId: resp.ContainerId}); err != nil {
		c.t.Fatalf("start container: %v", err)
	}
	return resp.ContainerId
}

// Exec runs cmd in a container and returns its stdout, followed by its
// stderr if it failed, and its exit code.
func (c *CRI) Exec(id string, timeout time.Duration, cmd ...string) (string, int) {
	c.t.Helper()
	resp, err := c.Runtime.ExecSync(c.Ctx, &cri.ExecSyncRequest{ContainerId: id, Cmd: cmd, Timeout: int64(timeout.Seconds())})
	if err != nil {
		c.t.Fatalf("exec %v: %v", cmd, err)
	}
	out := string(resp.Stdout)
	if resp.ExitCode != 0 {
		out += string(resp.Stderr)
	}
	return out, int(resp.ExitCode)
}

// Log is a container's log file.
func (p *Pod) Log(config *cri.ContainerConfig) string {
	b, _ := os.ReadFile(filepath.Join(p.Config.LogDirectory, config.LogPath))
	return string(b)
}
