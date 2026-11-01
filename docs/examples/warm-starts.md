# Warm starts

Some containers are slow not because the image is large but because the
process has work to do before it is useful: load weights onto a GPU,
compile kernels, warm a JIT, parse a dataset, start an interpreter and
import half of PyPI.

You only have to pay for that once. Snapshot the container when it is
ready, and start the next one from that moment.

```sh
gcr snapshot api /var/lib/gcr/snapshots/warm      # the original keeps serving

ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm \
  IMAGE api-2 true                                # already warm
```

The new container begins with the old one's memory: the model on the GPU,
the imports done, the caches full. Its command is never run, because its
processes are restored rather than started.

## A model server, measured

[`examples/vllm-snapshot`](https://github.com/makhov/gcr/tree/main/examples/vllm-snapshot)
starts vLLM's OpenAI server twice: once cold, once from a snapshot of the
warm server, each timed to its first completion.

```sh
sudo examples/vllm-snapshot/demo.sh
```

Qwen3-8B, one RTX PRO 6000 Blackwell (96 GB), Fedora 44, Linux 7.2:

| | run 1 | run 2 |
| --- | --- | --- |
| Cold start to first completion | 136.1 s | 139.5 s |
| Snapshot of the running server (28.5 GB of GPU memory) | 14.8 s, 32 GB | 17.0 s, 32 GB |
| **From the snapshot to first completion** | **8.9 s** | **9.1 s** |

The restored server is the same server: the same weights on the GPU, the
same compiled kernels and CUDA graphs, the same answer, and its log
carries on where the warm one's stopped.

What to know before trying it on your own model:

- **The snapshot is as large as the GPU memory the process reserved**,
  and the node needs that much host memory free while it is taken, because
  CRIU's CUDA plugin stages GPU memory through host memory
  ([GPUs](../gpus.md)). vLLM reserves 90% of the GPU by default, most of
  it an empty KV cache; the demo sets `--gpu-memory-utilization 0.3`.
- The demo drives containerd with `ctr` and takes the GPU through CDI, so
  it needs neither Kubernetes nor the device plugin. It needs the NVIDIA
  driver and a CDI spec (`nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`).
- Restoring onto a different GPU index is not supported yet.

## Without a GPU

Nothing above needs one. The same trick pays off wherever startup is
expensive:

```sh
# a Python process that has imported its world and built its caches
ctr run -d --runtime io.containerd.gcr.v2 IMAGE warm python3 -c 'import heavy; serve()'
gcr snapshot warm /var/lib/gcr/snapshots/warm
```

Then, for each new worker, `--annotation gcr.dev/fork-from=…`. On one
small container ten forks start together in about 0.1 s.

Two annotations matter when you start many:

- **[`gcr.dev/lazy-restore=true`](../fork.md#lazy-restore)** runs the fork
  before its memory has arrived, and with gcr's CRIU it then copies in
  only the pages the fork touches. A fork of a 1 GiB Python process runs
  with 8 MiB resident. Needs `vm.unprivileged_userfaultfd = 1`, and on
  x86 it is [not reliable yet](../fork.md#lazy-restore).
- **[`gcr.dev/memory-merge=true`](../fork.md#shared-memory)** lets the
  kernel fold the forks' identical pages into one copy. Ten forks of a
  128 MiB snapshot settle at about 210 MiB instead of 1.25 GiB. Needs KSM
  running, and merges only between containers that opt in.

## Keeping the snapshot around

A snapshot has to outlive every fork of it, so gcr counts users and
refuses to delete one that is in use (`gcr snapshot-users`,
`gcr snapshot-rm`: [snapshot lifetime](../fork.md#snapshot-lifetime)).

To warm more than one host, push the snapshot to a registry once and pull
it wherever you need it: [a warm pool across the
fleet](warm-pool.md).
