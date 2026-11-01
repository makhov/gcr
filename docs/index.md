---
hide:
  - navigation
---

# gcr

**gcr runs containers like any other runtime, and then lets you do four
things runc cannot: snapshot a container while it keeps running, start
copies of it, rewind it to an earlier moment, and move it to another
machine without restarting it.**

It is a containerd runtime (`io.containerd.gcr.v2`), so `ctr`, Docker,
Kubernetes and CRI-O drive it with the commands you already use. Pick the
runtime and the rest stays the same.

## Try it

On a Linux box with Docker, this installs gcr and runs a container on it:

```sh
arch=$(uname -m | sed -e s/x86_64/amd64/ -e s/aarch64/arm64/)
v=$(curl -fsSLo /dev/null -w '%{url_effective}' https://github.com/makhov/gcr/releases/latest | sed 's#.*/##')
curl -fLo gcr "https://github.com/makhov/gcr/releases/download/$v/gcr-$v-linux-$arch" && chmod +x gcr

sudo ./gcr setup -docker        # installs gcr, registers it with containerd and Docker
docker run --rm --runtime gcr busybox echo hello
```

No Linux box? [A VM on your laptop](install.md#a-vm-on-your-laptop) or a
throwaway cluster works too.

[Quickstart: first fork in 10 minutes](getting-started.md){ .md-button .md-button--primary }
[Examples](examples/index.md){ .md-button }
[GitHub](https://github.com/makhov/gcr){ .md-button }

## Why you might want this

<div class="grid cards" markdown>

-   :material-undo-variant: **Undo for agent sandboxes**

    ---

    Mark a point before a command, let the agent run it, and take it back
    if it went wrong: files, processes, and what they held in memory. A
    dropped database comes back with its rows.

    [:octicons-arrow-right-24: Agent sandboxes](examples/agent-sandbox.md)

-   :material-source-branch: **Several attempts at once**

    ---

    Fork a running sandbox, services and all, try three fixes in
    parallel, keep the one that passed. Each fork pays only for the
    memory it touches.

    [:octicons-arrow-right-24: Branching](examples/agent-sandbox.md)

-   :material-fire: **Start warm instead of cold**

    ---

    Snapshot a container once its model is on the GPU or its interpreter
    is up, then start new ones from that moment. vLLM answers in 9
    seconds instead of 136.

    [:octicons-arrow-right-24: Warm starts](examples/warm-starts.md)

-   :material-truck-fast: **Drain a node without restarting work**

    ---

    Move a running container to another machine with downtime measured in
    hundreds of milliseconds, whatever it holds in memory. In Kubernetes,
    `kubectl drain` becomes a migration.

    [:octicons-arrow-right-24: Node maintenance](examples/node-maintenance.md)

</div>

## What it does

| | |
| --- | --- |
| **[Fork](fork.md)** | Start containers from a snapshot of a running one, with its memory and files. Forks share the snapshot's files, and optionally its memory, so ten forks cost far less than ten containers. |
| **[Time travel](time-travel.md)** | Points on a container's timeline, taken on request or after every `exec`, and a rewind to any of them in place. |
| **[Live migration](migration.md)** | Move a running container to another host, over mutual TLS if you want it. |
| **[Snapshots between hosts](snapshot-registry.md)** | Push a snapshot to any OCI registry, including S3-backed ones, and fork from it on another node. |
| **[GPUs](gpus.md)** | NVIDIA GPUs through CDI, with CUDA state inside snapshots and forks. |
| **[Hardening](hardening.md)** | Tighter defaults than runc: user namespaces, a denied set of syscalls on top of any seccomp profile, supervised mounts, a netlink policy in the kernel, and Landlock. Docker-in-Docker still runs, unprivileged. |
| **[Drop-in runtime](runc-differences.md)** | containerd 1.7 to 2.4, Kubernetes (CRI conformance passes), Docker, and CRI-O with some features missing. |

Containers start in 16 ms against runc's 22, because gcr runs
libcontainer inside the shim instead of executing a binary per operation.
[The full comparison](tradeoffs.md#compared-with-runc).

## What it is not

gcr is a container runtime, not a virtual machine: its containers share
the host's kernel. For hostile code from many tenants on one host, use
gVisor, Kata or Firecracker, or run gcr inside a VM per tenant.
[What gcr is good for, and where it falls short](tradeoffs.md) is the
honest version, with numbers.

## Requirements

- Linux 6.12 or later, cgroup v2, root
- containerd 1.7+, or CRI-O with [some features missing](crio.md)
- libseccomp
- CRIU for snapshots, forks, time travel and migration. `gcr setup`
  installs the one gcr needs; you do not have to.

Not every host can do everything, so `gcr doctor` tells you what yours
can. [Installation](install.md) covers one host, a throwaway cluster and
your own cluster.
