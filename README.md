# gcr

A containerd runtime that lets you do four things runc cannot: snapshot a
container while it keeps running, start copies of it, rewind it to an
earlier moment, and move it to another machine without restarting it.

It runs containers in-process through libcontainer, with no runc binary,
and hardens them by default beyond what runc does. `gcr` is a placeholder
name.

Documentation: **[makhov.github.io/gcr](https://makhov.github.io/gcr/)**

## Try it

On a Linux box with Docker:

```sh
arch=$(uname -m | sed -e s/x86_64/amd64/ -e s/aarch64/arm64/)
v=$(curl -fsSLo /dev/null -w '%{url_effective}' https://github.com/makhov/gcr/releases/latest | sed 's#.*/##')
curl -fLo gcr "https://github.com/makhov/gcr/releases/download/$v/gcr-$v-linux-$arch" && chmod +x gcr

sudo ./gcr setup -docker          # installs gcr, registers it, reports on the host
docker run --rm --runtime gcr busybox echo hello
```

Then snapshot a running container and start two more from it:

```sh
gcr snapshot warm /var/lib/gcr/snapshots/warm          # the parent keeps running
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm IMAGE fork-1 CMD
```

[Quickstart](docs/getting-started.md) does this end to end, including an
undo, in about ten minutes. No Linux box?
[A VM on your laptop](docs/install.md#a-vm-on-your-laptop).

## What you can do with it

- **[Give an agent undo.](docs/examples/agent-sandbox.md)** Mark a point
  before each command and take any of them back: files, processes and
  their memory. A dropped database comes back with its rows.
- **[Let an agent branch.](docs/examples/agent-sandbox.md)** Fork its
  sandbox, running services and all, try several approaches at once, keep
  the one that worked. A fork holds only the memory it touches.
- **[Start warm.](docs/examples/warm-starts.md)** Fork new containers
  from a snapshot of one that has already loaded its model: vLLM answers
  in 9 s instead of 136. Through a registry, on
  [any node](docs/examples/warm-pool.md).
- **[Move what cannot restart.](docs/examples/node-maintenance.md)**
  Migrate a running container to another node, or let `kubectl drain`
  move pods instead of restarting them.

## What it does

- **Fork.** Start new containers from a snapshot of a running one, with
  its memory (an interpreter up, a model loaded) and its files. Forks
  share the snapshot's files without copying them, and, opt-in, its
  memory. [Fork](docs/fork.md)
- **Snapshots between hosts** through any OCI registry, including ones
  backed by S3. [Moving snapshots](docs/snapshot-registry.md)
- **Time travel.** A point on the container's timeline at every `exec`,
  or on request, incremental where the kernel tracks dirty pages, and a
  rewind to any of them in place. [Time travel](docs/time-travel.md)
- **Live migration** to another host, pre-copy where the kernel tracks
  dirty memory, or post-copy, optionally over mutual TLS.
  [Live migration](docs/migration.md)
- **GPUs.** NVIDIA GPUs through CDI, with CUDA state in checkpoints and
  forks. [GPUs](docs/gpus.md)
- **Hardening.** User namespaces by default, a denied set of system calls
  on top of any seccomp profile, supervised mounts, a netlink policy in
  the kernel (BPF LSM), and Landlock confining each container to its
  root. Docker-in-Docker runs unprivileged.
  [User namespaces](docs/user-namespaces.md), [hardening](docs/hardening.md)
- **A drop-in runtime** for containerd 1.7 to 2.4 and Kubernetes: the
  whole task API, CRI conformance. CRI-O runs it too, with snapshots and
  forks but without time travel or migration, and so does Docker
  (`docker run --runtime gcr`). [Docker](docs/docker.md)
- **Kubernetes migrations.** gcr-operator turns evictions (`kubectl
  drain`, the autoscaler) into live migrations, and moves StatefulSets'
  pods, and pods on nodes that shut down, through checkpoints in a
  registry. [gcr-operator](docs/operator.md)

Containers start in 16 ms against runc's 22 ms, and stop in 17 against
31, because libcontainer runs in the shim rather than as a binary per
operation. [The full comparison](docs/tradeoffs.md#compared-with-runc).

Not yet: the NVIDIA ioctl proxy, restoring onto another GPU index,
SELinux labels for supervised mounts.
[What gcr is good for, and where it falls short](docs/tradeoffs.md) is
the honest account.

## Requirements

- Linux 6.12+ with cgroup v2, run as root
- containerd 1.7 or later, or CRI-O with some features missing
- libseccomp
- for checkpoints, forks, time travel and migration: CRIU, which
  `gcr setup` installs (the fork at
  [makhov/criu](https://github.com/makhov/criu), for time travel and for
  Landlock across restores)

`gcr doctor` reports what a host can do.

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
  [CRI-O](docs/crio.md), [gcr-operator](docs/operator.md)
- Features: [fork](docs/fork.md),
  [moving snapshots](docs/snapshot-registry.md),
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
