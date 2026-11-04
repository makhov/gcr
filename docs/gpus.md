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
- **Same GPU count.** Which GPUs is the driver's call: it restores onto
  GPUs of the same chip with enough memory, and refuses others (another
  architecture, a different chip of the same one). gcr does not compare
  model names, so two cards on one chip may take each other's
  snapshots; this has been tried only between identical cards.
- **Any GPU index.** A restore onto other GPUs of the node, or of
  another node, gives the container those GPUs: the plugin maps the
  snapshot's GPUs to the container's in bus order
  (`cuda_plugin.device-map=auto`, restores only), and gcr puts the new
  GPUs' `/dev/nvidia<minor>` into the restored container before CUDA's
  state goes back, since libcuda opens a GPU by that name. In a user
  namespace, where a container's device nodes are bind mounts, the
  snapshot's are bound to the new GPUs; there, GPUs cannot trade minors
  (0 and 1 for 1 and 0).

A mismatch fails the restore with the reason. A container whose GPUs gcr
cannot describe (no driver information) is refused checkpoints rather than
checkpointed into something no node could restore. CRIU's CUDA plugin
cannot pre-dump: GPU containers migrate stop-and-copy, and time travel
refuses them (every point would copy GPU memory).

Forks work the same way: each fork of a snapshot gets its own copy of
the device memory, restored onto the GPU it is given, which it shares
with the parent and other forks there as separate CUDA contexts.

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
same driver version and GPUs of the same chip (above); which GPU the
node hands out does not matter. Keep GPU node pools on one driver version, and pin
restored pods to those nodes with a `nodeSelector`.

## When GPU memory is larger than RAM

A checkpoint moves the GPU memory a process uses into its host memory
before CRIU writes anything to disk, all at once: the driver's checkpoint
API has no way to stream it out. That copy is the process's own memory,
so it counts against the container's memory limit, and a snapshot of a
pod using 20 GiB of GPU memory needs 20 GiB of room under its limit on
top of what it already uses. A restore needs the same room: the GPU
memory comes back through the restored process's memory.

Without that room the kernel kills the container in the middle of the
checkpoint, and the workload is lost:

```console
Memory cgroup out of memory: Killed process 189619 (python3) ...
```

There are two ways to make the room:

1. **A memory limit that fits.** Set the pod's memory limit to its host
   memory plus the GPU memory it uses, on nodes with that much RAM free.
   Fastest, and the only way with no swap.
2. **Swap.** The copy is ordinary memory, so it can go to swap, and a
   node can snapshot more GPU memory than it has RAM. It is slow: with
   20 GiB of GPU memory under an 8 GiB limit, a checkpoint that takes
   4 s in memory took 49–240 s, and a restore that takes 2 s took over 7
   minutes (below).
   The restore is the slower half: the GPU memory comes back through
   swap under the same limit, paged in and out again. Swap keeps a
   workload alive rather than making snapshots fast. The node needs:

    - **A swap device** at least as large as the GPU memory being
      snapshotted, on a fast local disk.
    - **`vm.swappiness` above 0.** Many cloud images set it to 0, and
      then a container's memory never swaps, however much swap there
      is (`sysctl vm.swappiness=60`).
    - **The container allowed to swap.** Under Kubernetes that is
      [NodeSwap](https://kubernetes.io/docs/concepts/cluster-administration/swap-memory-management/)
      with `swapBehavior: LimitedSwap`, which gives swap only to
      Burstable pods whose memory request is below their limit, in
      proportion to that request: size the request, or the swap, so the
      pod's share covers its GPU memory.

Measured on the node below, with a process holding 20 GiB on the GPU in a
container limited to 8 GiB (64 GiB in the last row), through the
driver's checkpoint API:

| | Checkpoint | Restore | Afterwards |
| --- | --- | --- | --- |
| No swap | killed | — | the workload is gone |
| 64 GiB swap, `vm.swappiness=0` | killed | — | nothing was swapped |
| 64 GiB swap, `vm.swappiness=60` | 49–240 s | over 7 minutes | 8 GiB in RAM, 20.9 GiB in swap at the checkpoint; restored with its data intact |
| 64 GiB limit: fits in memory | 4.2 s | 2.3 s | 21 GiB in RAM at the checkpoint; restored with its data intact |

The swap was a file on the virtual machine's disk, a Ceph RBD volume:
network storage, whose speed varied between runs (49 s and 240 s for the
same checkpoint). Swap on a local disk has not been measured.

## Tested

Run on four NVIDIA RTX PRO 6000 Blackwell (96 GB each), driver
580.178.04, Linux 7.0, x86: `test/e2e/gpu` runs where there is a GPU
(`/dev/nvidiactl`, the CDI spec, the CUDA plugin) and is skipped
elsewhere.

- `TestCUDA` keeps a counter in GPU memory with PyTorch and checks that
  it carries on after a checkpoint and restore.
- `TestCUDAFork` loads 4 GiB of weights into GPU memory, snapshots the
  running container (2.9 s), and starts two forks (1.7 s each). Each
  one carries on with the weights intact, as does the parent.
- `TestCUDAForkOtherGPU`, on a node with more than one GPU, snapshots
  the same model on the first GPU and forks it onto the last (1.8–2.4 s),
  in a user namespace and without one: the fork runs on the last GPU,
  with the weights intact.
- `TestCUDASnapshotFiles` snapshots a GPU container that creates and
  deletes files every few milliseconds, three times, and forks each
  snapshot: its writable layer has to match the dump, although CRIU
  lets the processes run while it seizes them (CUDA has to, to be
  locked).

`GCR_E2E_CUDA_IMAGE` picks the image. The default is PyTorch built for
CUDA 12.8, the first with kernels for Blackwell (sm_120).
