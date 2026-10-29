# GPUs

NVIDIA GPUs get into gcr containers through CDI: the NVIDIA device plugin
(CDI strategy) and the spec nvidia-ctk generates
(`/etc/cdi/nvidia.yaml`). containerd puts the device nodes, driver
libraries and hooks into the OCI spec, so they are part of what a restore
rebuilds; there is no toolkit hook adding mounts behind the runtime's back.

Checkpoints of GPU containers go through CRIU's CUDA plugin
(`/usr/lib/criu/cuda_plugin.so`, built by `make criu`): before the dump it
locks CUDA and moves device memory and state into each process's host
memory (driver API on r580+, `cuda-checkpoint` before), and after the
restore it moves them back. gcr records the container's GPUs with each
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
