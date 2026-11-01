---
hide:
  - navigation
---

# gcr

A containerd runtime (`io.containerd.gcr.v2`) that treats a running
container's processes as something a platform can work with: snapshot it
without stopping it, fork it any number of times, rewind it to an earlier
point, move it to another host. It runs containers in-process through
libcontainer, with no runc binary, and hardens them by default beyond
what runc does.

```sh
gcr snapshot warm /var/lib/gcr/snapshots/warm          # the parent keeps running
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm IMAGE fork-1 CMD
```

[Get started](getting-started.md){ .md-button .md-button--primary }
[Install](install.md){ .md-button }
[GitHub](https://github.com/makhov/gcr){ .md-button }

## What you can do with it

<div class="grid cards" markdown>

-   :material-undo-variant: **Give an agent undo**

    ---

    Run each command in a sandbox after a point on its timeline, and take
    any of them back: files, processes and their memory. A dropped
    database comes back with its rows.

    [:octicons-arrow-right-24: Time travel](time-travel.md)

-   :material-source-branch: **Let an agent branch**

    ---

    Fork its sandbox, running services and all, try several approaches at
    once, keep the one that worked. A fork holds only the memory it
    touches.

    [:octicons-arrow-right-24: Fork](fork.md)

-   :material-fire: **Start warm**

    ---

    Fork new containers from a snapshot of one that has already loaded
    its model or interpreter, on this node or, through a registry, on any
    other, reading only what each touches.

    [:octicons-arrow-right-24: Moving snapshots](snapshot-registry.md)

-   :material-truck-fast: **Move what cannot restart**

    ---

    Migrate a running container to another node with a downtime that does
    not grow with its memory. In Kubernetes, drains and node shutdowns
    move pods instead of restarting them.

    [:octicons-arrow-right-24: gcr-operator](operator.md)

</div>

## What it does

- **Fork.** Start new containers from a snapshot of a running one, with
  its memory (an interpreter up, a model loaded) and its files. Forks
  share the snapshot's files without copying them, and, opt-in, its
  memory.
- **Snapshots between hosts** through any OCI registry, including ones
  backed by S3.
- **Time travel.** A point on the container's timeline at every `exec`,
  or on request, incremental where the kernel tracks dirty pages, and a
  rewind to any of them in place.
- **Live migration** to another host, pre-copy where the kernel tracks
  dirty memory, or post-copy, optionally over mutual TLS. In Kubernetes,
  gcr-operator turns evictions into migrations, and moves StatefulSets'
  pods and pods on nodes that shut down through checkpoints in a
  registry.
- **GPUs.** NVIDIA GPUs through CDI, with CUDA state in checkpoints and
  forks.
- **Hardening.** User namespaces by default, a denied set of system calls
  on top of any seccomp profile, supervised mounts, a netlink policy in
  the kernel (BPF LSM), and Landlock confining each container to its
  root. Docker-in-Docker runs unprivileged.
- **A drop-in runtime** for containerd 1.7 to 2.4 and Kubernetes: the
  whole task API, CRI conformance. CRI-O runs it too, with snapshots and
  forks but without time travel or migration, and so does Docker
  (`docker run --runtime gcr`).

## Requirements

- Linux 6.12+ with cgroup v2, run as root
- containerd 1.7 or later, or CRI-O with some features missing
- libseccomp
- for checkpoints, forks, time travel and migration: CRIU, the fork at
  [makhov/criu](https://github.com/makhov/criu) for time travel and for
  Landlock across restores

[Installation](install.md) sets it up on a host or a cluster, and
[Getting started](getting-started.md) runs a first fork.
