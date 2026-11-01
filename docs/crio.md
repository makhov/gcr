# CRI-O

CRI-O runs gcr as a runtime of type `vm`, the way it runs Kata
Containers. Snapshots and forks work; time travel and migration do not.

```toml
# /etc/crio/crio.conf.d/10-gcr.conf
[crio.runtime.runtimes.gcr]
runtime_path = "/usr/local/bin/containerd-shim-gcr-v2"
runtime_type = "vm"
runtime_root = "/run/gcr-crio"
# runtime_config_path = "/etc/gcr/gcr.toml"   # gcr's options (configuration.md)
```

Pods then ask for it with `runtimeClassName: gcr`, as under containerd
([Kubernetes](kubernetes.md)).

## What works

Pods run with gcr's [hardening](hardening.md) and
[Landlock](security.md). Exec, logs, resource updates, stats and OOM
kills all work, and CRI conformance passes except for port forwarding,
which CRI-O does not implement for `vm` runtimes at all.

**Snapshots and forks work**: `gcr snapshot`, `gcr.dev/fork-from`, and
moving snapshots through a registry. CRI-O mounts the container's overlay
rootfs itself; gcr reads the writable layer from that mount, and for a
fork mounts the same overlay again with the snapshot's layers underneath.
A fork is restored when CRI-O creates it, because CRI-O wants its process
then, and held frozen until CRI-O starts it.

As under containerd, pods may only fork from a handler that allows it
([`PodAnnotations`](configuration.md#podannotations)):

```toml
[crio.runtime.runtimes.gcr-forks]
runtime_path = "/usr/local/bin/containerd-shim-gcr-v2"
runtime_type = "vm"
runtime_root = "/run/gcr-crio"
runtime_config_path = "/etc/gcr/forks.toml"   # PodAnnotations, SnapshotRoots
```

## What does not

Each of these is refused with an error saying why, rather than failing
quietly.

| Not available | Why |
| --- | --- |
| gcr's own [user namespaces](user-namespaces.md) (`gcr.dev/host-users=false`, `UserNamespaces = 'always'`) | gcr idmaps the layers containerd hands it, and CRI-O mounts them unmapped. A pod with `hostUsers: false` gets CRI-O's user namespace instead, as with any runtime; a fork of it must then map the same IDs as its snapshot. |
| [Time travel](time-travel.md) | CRI-O identifies a container by its init's pid and start time. A rewind brings in another process, so CRI-O would take the container for gone. |
| [Migration](migration.md) into a CRI-O container | CRI-O wants the container's process at create time, before the source has sent one. Migrating out of one has not been tried. |
| Checkpoint and restore through the CRI | CRI-O does not implement them for `vm` runtimes. |
| Restarting CRI-O with gcr pods running | CRI-O does not reconnect to the shims of its `vm` runtimes, and stopping such a pod afterwards crashes it (CRI-O 1.37). Drain the node first. |

For everything on this list, containerd is the way to get it
([Installation](install.md)).
