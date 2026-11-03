# GPUs

Snapshots, forks and checkpoints include what is on the GPU, so a model
server can be snapshotted once it has loaded and started again from that
moment ([warm starts](examples/warm-starts.md) measures it: 9 s against
136 s cold).

NVIDIA GPUs get into gcr containers through CDI: the NVIDIA device plugin
(CDI strategy) and the spec nvidia-ctk generates
(`/etc/cdi/nvidia.yaml`). containerd puts the device nodes, driver
libraries and hooks into the OCI spec, so they are part of what a restore
rebuilds; there is no toolkit hook adding mounts behind the runtime's back.

Checkpoints of GPU containers go through CRIU's CUDA plugin, which
`gcr setup` installs with gcr's CRIU: before the dump it locks CUDA and
moves device memory and state into each process's host memory, through
the driver's checkpoint API, and after the restore it moves them back. gcr records the container's GPUs with each
checkpoint and checks them before a restore:

- **Same NVIDIA driver version.** The restored processes have its
  libraries mapped by path and content.
- **Same GPU models and count.**
- **Same GPU device minors, for now.** libcuda opens `/dev/nvidia<minor>`
  for the minor the driver reports, and the container's `/dev` is part of
  what CRIU rebuilds exactly, so a container cannot move to another GPU
  index yet. Another GPU at the same index (another node) works: GPU UUIDs
  are mapped by ordinal (`cuda_plugin.device-map=auto`, restores only).

A mismatch fails the restore with the reason. A container whose GPUs gcr
cannot describe (no driver information) is refused checkpoints rather than
checkpointed into something no node could restore. CRIU's CUDA plugin
cannot pre-dump: GPU containers migrate stop-and-copy, and time travel
refuses them (every point would copy GPU memory).

Forks work the same way: each fork of a snapshot gets its own copy of
the device memory, restored onto the same GPU, which the forks and the
parent then share as separate CUDA contexts.

## What a node needs

- **NVIDIA driver r580 or newer,** installed on the host or by the GPU
  Operator's driver container. Older drivers run GPU containers on gcr,
  but gcr refuses their checkpoints: r580 is where the driver's
  checkpoint API arrived. Before it, CRIU's CUDA plugin falls back to
  NVIDIA's `cuda-checkpoint` tool, which gcr neither installs nor is
  tested with. On r580 and newer there is nothing to install for it.
- **A CDI spec for the GPUs,** which is how they get into gcr containers:
  `nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml` from the NVIDIA
  Container Toolkit, or the GPU Operator's.
- **gcr's CRIU with its CUDA plugin,** which `gcr setup` installs, except
  on hosts without glibc (Alpine, k0s's image), which get a static CRIU
  that loads no plugins.

CRIU's CUDA plugin loads `libcuda.so.1`. gcr hands it the one the
container itself was given (its CDI mount), so the plugin always uses the
driver the container's processes use, wherever it is installed: the
GPU Operator's driver container keeps it under `/run/nvidia/driver`,
where no host program would find it.

`gcr doctor` checks the first and last:

```console
ok   GPUs            NVIDIA driver 580.65.06, CRIU's CUDA plugin installed
off  GPUs            NVIDIA driver 570.86.15: no checkpoints of GPU containers, which need r580 or newer
                     -> upgrade the NVIDIA driver to r580 or newer (docs/gpus.md)
```

## In Kubernetes

The chart's installer sets up GPU nodes as any other; what is left is
getting GPUs into gcr's pods. The NVIDIA device plugin normally hands
GPUs to the `nvidia` runtime, which pods on the `gcr` RuntimeClass do not
use, so it has to name them through CDI instead, for containerd to add
them to the container's spec:

1. **containerd with CDI on.** containerd 2.x has it on. On 1.7, set
   `enable_cdi = true` and
   `cdi_spec_dirs = ["/etc/cdi", "/var/run/cdi"]` in the
   `[plugins."io.containerd.grpc.v1.cri"]` table, and restart it.
2. **The device plugin in CDI mode.** With the GPU Operator, CDI is on by
   default from v25.10; on older versions:

    ```sh
    helm upgrade --install gpu-operator nvidia/gpu-operator -n gpu-operator \
      --reuse-values --set cdi.enabled=true
    ```

    With the device plugin on its own (drivers and the toolkit on the
    host), generate each node's CDI spec with `nvidia-ctk cdi generate`
    and run the plugin with `--set deviceListStrategy=cdi-cri`.

3. **Pods ask for both:**

    ```yaml
    spec:
      runtimeClassName: gcr
      containers:
      - name: cuda
        image: nvcr.io/nvidia/cuda:12.8.1-base-ubuntu24.04
        command: [sleep, infinity]
        resources:
          limits:
            nvidia.com/gpu: 1
    ```

Check from inside the pod that the GPU is there (`nvidia-smi`), then on
its node that gcr can checkpoint it: `gcr doctor`'s `GPUs` line, or the
installer pod's log, which has the same report.

Snapshots and migrations of GPU pods restore only onto nodes with the
same driver version and GPU models, at the same GPU indexes (above). Keep
GPU node pools on one driver version, and pin restored pods to those
nodes with a `nodeSelector`.

## Tested

Run on one NVIDIA RTX PRO 6000 Blackwell (96 GB), driver 580.178.04,
Linux 7.0, x86: `test/e2e/gpu` runs where there is a GPU
(`/dev/nvidiactl`, the CDI spec, the CUDA plugin) and is skipped
elsewhere.

- `TestCUDA` keeps a counter in GPU memory with PyTorch and checks that
  it carries on after a checkpoint and restore.
- `TestCUDAFork` loads 4 GiB of weights into GPU memory, snapshots the
  running container (1.9 s), and starts two forks (1.6–1.8 s each). Each
  one carries on with the weights intact, as does the parent.

`GCR_E2E_CUDA_IMAGE` picks the image. The default is PyTorch built for
CUDA 12.8, the first with kernels for Blackwell (sm_120).
