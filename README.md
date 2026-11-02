# gcr

**Containers you can rewind, copy and move.**

gcr is a container runtime for Docker, containerd and Kubernetes. Your
containers run as they always do. And while they run, you can save one,
start copies of it, take it back to an earlier moment, or move it to
another machine, without restarting it.

Documentation: **[makhov.github.io/gcr](https://makhov.github.io/gcr/)**

## Install

On a Linux machine with Docker:

```sh
curl -sSfL https://raw.githubusercontent.com/makhov/gcr/main/install.sh | sudo sh -s -- -docker
docker run --rm --runtime gcr busybox echo hello
```

On Kubernetes: the [Helm chart](docs/install.md#your-own-cluster). No
Linux machine? [A VM on your laptop](docs/install.md#a-vm-on-your-laptop).

## How to use it

1. **Install it:** one command on a host, or a Helm chart on a cluster.
2. **Run containers as usual,** with gcr as the runtime: `docker run
   --runtime gcr`, or `runtimeClassName: gcr` in a pod.
3. **Work with them while they run:**

```sh
kubectl gcr snapshot pod/app registry.example.com/app:warm   # an image that starts warm
kubectl gcr point pod/app "before the migration"             # mark this moment: #1
kubectl gcr rewind pod/app 1                                 # and go back to it
kubectl gcr migrate pod/app                                  # move it to another node
```

The [quickstart](docs/getting-started.md) takes ten minutes on one
machine.

## Why gcr

- **Start in seconds, not minutes.** Save a container once it has loaded
  its model, and start new ones from that moment: a vLLM server answers
  in 9 seconds instead of 136. [Warm starts](docs/examples/warm-starts.md)
- **Undo anything.** Take a sandbox back to before a bad command: files,
  processes and memory. [Agent sandboxes](docs/examples/agent-sandbox.md)
- **Try many things at once.** Copy a running environment and try
  several approaches side by side; copies share memory. [Fork](docs/fork.md)
- **Maintenance without restarts.** Drain a node and its pods move,
  memory and all, with a pause of a few hundred milliseconds.
  [Node maintenance](docs/examples/node-maintenance.md)
- **Nothing new to learn.** A drop-in replacement for runc: same images,
  same tools, and containers start faster.
  [Differences from runc](docs/runc-differences.md)
- **Safer by default.** Stricter isolation than runc, with user
  namespaces on. [Hardening](docs/hardening.md)

gcr shares the host's kernel, as runc does: for untrusted code from many
tenants, run it inside VMs. [What it is good for, and where it falls
short](docs/tradeoffs.md). `gcr` is a placeholder name.

## Documentation

Also as a site: [makhov.github.io/gcr](https://makhov.github.io/gcr/).

- **[Quickstart](docs/getting-started.md):** install it, fork a running
  container, undo a command
- **[Installation](docs/install.md):** one host, a VM, a throwaway
  cluster, your own cluster, the kubectl plugin, gcr-operator
- **[Examples](docs/examples/index.md):** agent sandboxes, warm starts,
  node maintenance, a warm pool across the fleet
- **[Configuration](docs/configuration.md):** what to change and why,
  with recipes
- Running containers: [Docker](docs/docker.md),
  [ctr](docs/ctr.md), [Kubernetes](docs/kubernetes.md),
  [CRI-O](docs/crio.md), [gcr-operator](docs/operator.md),
  [operations as resources](docs/operations.md)
- Features: [fork](docs/fork.md),
  [moving snapshots](docs/snapshot-registry.md),
  [snapshots as images](docs/snapshot-images.md),
  [time travel](docs/time-travel.md),
  [live migration](docs/migration.md), [GPUs](docs/gpus.md)
- Isolation: [user namespaces](docs/user-namespaces.md),
  [hardening](docs/hardening.md),
  [what the hardening breaks](docs/compatibility.md),
  [security model](docs/security.md)
- Reference: [the `gcr` command](docs/cli.md),
  [troubleshooting](docs/troubleshooting.md),
  [differences from runc](docs/runc-differences.md)
- Inside gcr: [architecture](docs/architecture.md),
  [development and tests](docs/development.md)
