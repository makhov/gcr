# Serving many models on few GPUs

A model server is slow to start and expensive to leave running. Snapshot
it once it is warm, give the GPU back, and start it again from that
moment when a request arrives.

The numbers that make it worth doing, on one RTX PRO 6000 Blackwell
(96 GB), Qwen3-8B under vLLM ([warm starts](warm-starts.md) has the
run):

| | cold | from a snapshot |
| --- | --- | --- |
| To the first completion | 136.1 s | **8.9 s** |

What follows is what to get right so it stays that fast, and what it
costs.

---

## The shape of it

Three things happen at different times, and it is worth keeping them
apart:

1. **Once per model**, something builds a warm snapshot: run the server,
   wait until it answers, snapshot it, store it.
2. **Whenever the model is idle**, its container is gone and the GPU is
   free for another model.
3. **When a request arrives**, a container starts from the snapshot.

gcr does 1 and 3. Holding the request while 3 happens is an activator's
job, and there are good ones already — [KEDA's HTTP
add-on](https://github.com/kedacore/http-add-on) or [Knative
Serving](https://knative.dev/docs/serving/) scale a Deployment from zero
and proxy the first request into it. gcr's part is that the scale from
zero takes seconds rather than minutes.

---

## Build the warm snapshot once

On a host with the GPU, `gcr snapshot-build` runs the image the way a
pod runs it, waits for `-ready`, and pushes the result as an image pods
start warm from:

```sh
gcr snapshot-build -device nvidia.com/gpu=0 -timeout 30m \
  -ready 'curl -sf localhost:8000/health' \
  docker.io/vllm/vllm-openai:v0.32.0 \
  registry.example.com/snapshots/qwen3-8b:v1 \
  -- --model Qwen/Qwen3-8B --max-model-len 8192
```

- **`-device` is required for a GPU model**, and takes a CDI name, so
  the host needs a CDI spec
  (`nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`).
- **Raise `-timeout`.** It is 10 minutes by default, and loading a large
  model, compiling it and capturing CUDA graphs can take longer.
- **Download the weights into the image, or into a volume the pod also
  has.** The build has no network but its loopback, so a server that
  fetches from Hugging Face at startup will not get there
  ([building one in CI](../snapshot-images.md#building-one-in-ci)).

In a cluster, the other way round is `kubectl gcr snapshot pod/NAME`,
which snapshots a server that is already running and serving
([snapshots as images](../snapshot-images.md)). Either way the nodes
have to be told which snapshot images they may restore, which is empty
by default ([allowing it](../snapshot-images.md#allowing-it)).

---

## What it costs to store

A server reserves the GPU's memory up front — vLLM takes 90% of it by
default — and most of that is a KV cache it has not filled yet. Those
pages are zeroes, and a checkpoint compresses them to almost nothing, so
**the snapshot costs what the model wrote, not what it reserved.**

Measured through `gcr snapshot` on a container holding 20 GiB of GPU
memory with 2 GiB of it written:

| | snapshot | taken in | restored in |
| --- | --- | --- | --- |
| compressed | **2.1 GB** | 6.5 s | 5.3 s |
| uncompressed | 21 GB | 10.4 s | |

This is on by default ([`Compression`](../configuration.md#compression)).
Two things follow from it:

- **Leave `--gpu-memory-utilization` where serving wants it.** It used to
  be worth turning down because the reservation was the snapshot; it is
  not any more.
- **The node still needs host memory free for the whole reservation
  while the snapshot is taken**, because CRIU's CUDA plugin stages GPU
  memory through host memory ([GPUs](../gpus.md)). It is the stored size
  that follows what was written, not the peak. Budget host RAM for the
  GPU, not for the snapshot.

---

## Keep the snapshot where it is needed

Activation reads the whole snapshot, so where it lives is the difference
between seconds and minutes.

- **A registry is for distribution, not for the hot path.** Pulling
  gigabytes when the request is already waiting undoes the point. Nodes
  cache a snapshot image's layers after the first pull, so the first
  activation on a node is the slow one — pull it when the node joins,
  from a DaemonSet or a startup script.
- **`gcr snapshot-pull -lazy` helps the rest of the container, not the
  GPU.** Lazy pulls and `gcr.dev/lazy-restore` serve *host* memory as it
  is touched; device memory is put back in full before the processes
  run. The sub-second figures in [a warm pool across the
  fleet](warm-pool.md) are from CPU-only containers; do not expect them
  for a GPU model.
- **Many servers of one model on one node** should fork from a snapshot
  directory rather than each running its own image, and may share the
  identical pages ([`gcr.dev/memory-merge`](../fork.md#shared-memory)).
  One model per GPU does not need this.

---

## Name snapshots by what a restore checks

A GPU snapshot does not restore just anywhere. It needs the same NVIDIA
driver version, the same GPU model, and — for now — the same GPU index
([GPUs](../gpus.md#in-kubernetes)). A restore refuses rather than
restoring something wrong:

```
restore: checkpoint was taken on NVIDIA driver 580.65.06, this node has 595.91.07: failed precondition
```

So whatever picks a snapshot for `{image, model}` has to key on more
than the model's name. Put in the tag what a restore compares:

```
registry.example.com/snapshots/qwen3-8b:vllm0.32.0-len8192-rtxpro6000-d595
```

Then a node upgraded to a new driver simply misses, builds its own, and
nothing tries to restore a snapshot it cannot take. The alternative —
keying on the model alone — fails at activation, which is the worst time
to find out.

---

## What to watch

- **One model per GPU index, today.** Restoring onto a different index
  than the snapshot was taken on is refused, so a node with several GPUs
  handing them out freely will fail restores. A node per GPU, or
  index-pinned scheduling, works now.
- **A snapshot has to outlive everything started from it.** gcr counts
  users and refuses to delete one in use (`gcr snapshot-users`,
  [snapshot lifetime](../fork.md#snapshot-lifetime)).
- **Time travel is not available for GPU containers**, and GPU
  containers migrate stop-and-copy rather than with pre-copy rounds
  ([GPUs](../gpus.md)).
- **Measure against a tuned cold start, not a cold one.** 136 s is vLLM
  with nothing cached. With a warm `torch.compile` cache on disk a cold
  start is a good deal quicker, and the snapshot's honest claim is that
  it skips compilation, graph capture and warm-up altogether — not that
  it is fifteen times faster than vLLM.

---

Next: [warm starts](warm-starts.md) for the measured vLLM run and the
single-host commands, [snapshots as images](../snapshot-images.md) for
the Kubernetes path, [GPUs](../gpus.md) for what checkpointing one
requires, and [`Compression`](../configuration.md#compression) for the
option behind the sizes above.
