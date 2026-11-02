# vLLM from a snapshot

In the documentation: [Warm starts](../../docs/examples/warm-starts.md).

`demo.sh` starts vLLM's OpenAI server twice:
- **from scratch:** weights read from disk onto the GPU, the model compiled, kernels autotuned, CUDA graphs captured;
- **from a gcr snapshot** of the warm server: its processes and their memory, GPU memory included.

Each one is timed to its first completion.

Qwen3-8B on vLLM v0.32.0, one NVIDIA RTX PRO 6000 Blackwell (96 GB), driver 595.91.07, Fedora 44, Linux 7.2, k0s v1.37.1. Two runs:

| | run 1 | run 2 |
| --- | --- | --- |
| cold start to first completion | 136.1 s | 139.5 s |
| snapshot of the running server (28.5 GB of GPU memory) | 14.8 s, 32 GB | 17.0 s, 32 GB |
| **restore from the snapshot to first completion** | **8.9 s** | **9.1 s** |

The restored server carries on from the snapshot: the same model on the GPU, the same compiled kernels and CUDA graphs. It gives the same answer, and its log carries on from the warm server's.

## Running it

On a node with:
- containerd, with gcr registered (`gcr setup`, or the DaemonSet: [installation](../../docs/install.md));
- the NVIDIA driver;
- its CDI spec: `nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`, from the NVIDIA Container Toolkit.

Then, as root:

```sh
examples/vllm-snapshot/demo.sh
```

It pulls the image and downloads the model once (into `/var/lib/gcr-demo/models`), then prints the comparison.

| Variable | Default | |
| --- | --- | --- |
| `MODEL` | `Qwen/Qwen3-8B` | a Hugging Face model |
| `IMAGE` | `docker.io/vllm/vllm-openai:v0.32.0` | |
| `GPU` | `nvidia.com/gpu=0` | the CDI device |
| `VLLM_ARGS` | `--max-model-len 8192 --gpu-memory-utilization 0.3` | for `vllm serve` |
| `REGISTRY` | | `host:port`: also push the snapshot there, as tar and nydus, and restore from each |
| `CTR` | `k0s ctr` if k0s is installed, else `ctr` | |

## How it works

The demo drives containerd with `ctr`, so it needs neither Kubernetes nor NVIDIA's device plugin. The GPU comes through CDI (`--device nvidia.com/gpu=0`). The restore is an ordinary container with `gcr.dev/fork-from` naming the snapshot ([fork](../../docs/fork.md)).

CRIU's CUDA plugin moves the GPU's memory into the processes' host memory for the dump, and back after the restore ([GPUs](../../docs/gpus.md)). So:
- **The snapshot is as large as the GPU memory vLLM reserved,** and the node needs that much host memory free while it is taken. vLLM reserves 90% of the GPU by default (`--gpu-memory-utilization 0.9`), most of it for a KV cache that is empty at snapshot time. The demo reserves 30%.
- **The servers use the host's network** (`--net-host`), one at a time on `PORT`. The cold server is stopped once the snapshot is taken.
- **vLLM writes its log to `/var/lib/gcr-demo/logs`**, a mounted directory. gcr's shim does not take containerd's log files (`ctr --log-uri`).

## What it needed from gcr

Two fixes, found by running this:
- **Files deleted while still mapped.** Python's multiprocessing leaves a POSIX semaphore mapped in `/dev/shm` after deleting its file. CRIU dumps such files only when told to link them back (`--link-remap`), which gcr's dumps now do.
- **Bind mounts whose source is a symlink.** `ctr --net-host` bind-mounts the host's `/etc/resolv.conf`, which is a symlink under systemd-resolved (Fedora, Ubuntu). A restore handed CRIU the symlink, which it could not mount over. gcr now resolves the sources.
