# gcr

A containerd runtime (`io.containerd.gcr.v2`) with checkpoint, restore and fork
built in. Containers run in-process through libcontainer (from a small runc
fork, `makhov/runc`, which adds an init hook and a way to wrap the criu it
runs); no runc binary.
`gcr` is a placeholder name.

## Status

- containerd task API: create, start, exec, terminals, attach/stdin close,
  resize, state, kill, wait, delete, pids, pause, resume, stats, resource
  updates, checkpoint, restore. Passes the CRI conformance suite (critest).
- User namespaces by default (below).
- Time travel, live migration and fork (below).
- Hardening: a denied set of system calls on top of any profile, supervised
  mounts, a netlink policy (BPF LSM) and Landlock confining containers to
  their root (below).
- NVIDIA GPUs through CDI, with CUDA state in checkpoints and forks
  (below).
- Not yet: the NVIDIA ioctl proxy, restoring onto another GPU index.

What this is good for, what it costs, and what has not been shown yet:
[docs/tradeoffs.md](docs/tradeoffs.md). Every setting and annotation:
[docs/configuration.md](docs/configuration.md). What the hardening breaks,
and the way around each: [docs/compatibility.md](docs/compatibility.md).

### Compared with runc

Same host (arm64, Linux 7.0), containerd 2.4.2, runc v1.5.2 (the release
gcr's libcontainer comes from), both runtimes on their defaults (gcr's
hardening and Landlock on):

| | gcr | runc |
| --- | --- | --- |
| critest | 123 passed, 0 failed | 113 passed, 0 failed |
| critest NRI tests | 11 of 11 | 11 of 11 |
| StartContainer, median / p90 | 16.1 / 19.5 ms | 21.9 / 26.7 ms |
| StopContainer, median / p90 | 16.5 / 21.3 ms | 30.7 / 34.1 ms |
| CreatePod / StopPod, median | 52.2 / 53.4 ms | 54.8 / 65.2 ms |
| Shim memory per pod (PSS, 10 pods) | ~6 MiB | ~4 MiB |

runc skips the user namespace tests, which gcr passes; the rest of critest's
skips (NRI without a socket, benchmarks, an image volume field containerd
does not report) are the same for both. Latencies are critest's
benchmarks, 50 sequential runs: gcr runs libcontainer in the shim instead of
executing a runc binary per operation. gcr's shim is the bigger binary
(more of it resident, shared between shims).

## Requirements

- Linux 6.12+ with cgroup v2, run as root
- containerd 1.7 or later (tested: 1.7.29, 2.0.7, 2.2.1, 2.4.2), or
  CRI-O with some features missing (below)
- libseccomp (runtime and, to build, headers)
- CRIU for checkpoint/restore; time travel needs the fork with
  `--seccomp-notify-socket` (see below), and keeping Landlock across a
  restore needs its `--landlock-fs`; GPU checkpoints need its CUDA plugin

## Install

Build on the Linux host (cgo is required) and install both binaries:

```sh
make install          # containerd-shim-gcr-v2 and gcr into /usr/local/bin
make criu             # optional: build and install the CRIU fork from ../criu
```

containerd finds the shim by name on `PATH`: the runtime type
`io.containerd.gcr.v2` maps to `containerd-shim-gcr-v2`.

## Running containers

### With ctr

No configuration is needed; name the runtime type directly:

```sh
ctr image pull docker.io/library/busybox:latest
ctr run -d --runtime io.containerd.gcr.v2 docker.io/library/busybox:latest demo sleep 3600
ctr task ls
ctr task kill -s KILL demo && ctr task rm demo && ctr container rm demo
```

Checkpoint and restore go through containerd as usual:

```sh
ctr container checkpoint --task --rw demo localhost/demo-ckpt:1
ctr task kill -s KILL demo && ctr task rm demo && ctr container rm demo
ctr container restore --live --rw demo localhost/demo-ckpt:1
```

### With Kubernetes (CRI)

Register the runtime with containerd's CRI plugin in
`/etc/containerd/config.toml` and restart containerd:

```toml
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]
  runtime_type = 'io.containerd.gcr.v2'
  # Pass gcr's pod annotations (e.g. time travel) through to the runtime.
  pod_annotations = ['gcr.dev/*']
```

Restart containerd after installing a new gcr: it reads the runtime's
features (user namespaces, idmapped mounts) at startup.

Then add a RuntimeClass and select it from pods:

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gcr
handler: gcr
---
apiVersion: v1
kind: Pod
metadata:
  name: demo
  annotations:
    gcr.dev/time-travel: execve   # optional, see Time travel
spec:
  runtimeClassName: gcr
  containers:
    - name: demo
      image: busybox
      command: ["sleep", "3600"]
```

If the kubelet uses the systemd cgroup driver, gcr follows it (CRI sends
`slice:prefix:name` cgroup paths).

containerd 1.7 takes config version 2, where the table is
`[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.gcr]`. Listing a
runtime there replaces the default `runc` entry, so list that too.

gcr is built on containerd 2.4's shim library. containerd 2.3 and later
start it with their shim bootstrap protocol and use the task API's version
3. For older releases (and CRI-O) gcr answers the way shims did before
2.3 and serves the task API's version 2 as well, whose messages are the
same.

### With CRI-O

CRI-O runs containerd shims as runtimes of type `vm` (the way it runs
Kata Containers):

```toml
# /etc/crio/crio.conf.d/10-gcr.conf
[crio.runtime.runtimes.gcr]
runtime_path = "/usr/local/bin/containerd-shim-gcr-v2"
runtime_type = "vm"
runtime_root = "/run/gcr-crio"
# runtime_config_path = "/etc/gcr/gcr.toml"   # gcr's options (docs/configuration.md)
```

The RuntimeClass is the same as above. Pods run with gcr's hardening and
Landlock; exec, logs, resource updates, stats and OOM kills work, and
CRI conformance passes except for port forwarding, which CRI-O does not
implement for `vm` runtimes. Not available under CRI-O:

- **gcr's own user namespaces** (`gcr.dev/host-users=false`,
  `UserNamespaces = 'always'`). CRI-O mounts the container's rootfs
  itself, and gcr idmaps the layers containerd hands it. Pods with
  `hostUsers: false` get CRI-O's user namespace, as with any runtime.
- **Snapshots, forks, time travel and live migration.** They need gcr to
  mount the overlay rootfs (to read and replace its writable layer, and
  to stack snapshot layers under it). gcr refuses them with an error
  that says so.
- **Checkpoint and restore through the CRI**, which CRI-O does not
  implement for `vm` runtimes.

## User namespaces

Containers run in a user namespace by default: root inside is an
unprivileged range of 65536 host IDs, one range per pod (all containers of a
pod share the sandbox's user namespace, so they can share volumes and
PID/IPC namespaces).

Kubernetes pods follow Kubernetes: `hostUsers` defaults to true there, and
kubelet sends it like an explicit true, so a runtime cannot change it. Use
`hostUsers: false` (containerd's own user namespaces, supported by gcr and
the better path: the pod's network namespace is created inside the user
namespace), enforced cluster-wide by policy if wanted, or opt a pod into
gcr's with the annotation `gcr.dev/host-users: "false"`.

To enforce user namespaces regardless of Kubernetes, set the policy on a
runtime handler (here a second handler, so a RuntimeClass picks it):

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-userns]
  runtime_type = 'io.containerd.gcr.v2'
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-userns.options]
    UserNamespaces = 'always'   # 'auto' (default) | 'always' | 'never'
```

`always` ignores `hostUsers` and the opt-out annotation; a container that
cannot run in a user namespace (privileged, host network/PID/IPC,
`CAP_NET_ADMIN` in a pod network) is refused rather than run as host root.
`never` turns gcr's user namespaces off. The options can also live in a
file: `ConfigPath = '/etc/gcr/gcr.toml'`.

For gcr's own user namespaces:

- Image layers are mounted idmapped, so files look root-owned inside; files
  the container creates are stored as its mapped IDs.
- Bind mounts on filesystems that support it (ext4, xfs, btrfs, tmpfs, f2fs,
  vfat) are idmapped, so volumes keep their on-disk owners.
- In a pod network namespace gcr sets `ip_unprivileged_port_start=0` and an
  open `ping_group_range` (root can bind port 80 and ping), applies the
  pod's `net.*` sysctls itself, and bind-mounts `/sys` read-only.
- Raw sockets do not work in a pod network namespace (it belongs to the
  host's user namespace); containers adding `CAP_NET_ADMIN` there get host
  users.
- Skipped for privileged containers, containers sharing the host's network,
  PID or IPC namespace, and specs that bring their own user namespace
  (Kubernetes `hostUsers: false`). Opt out with `gcr.dev/host-users=true`.
- A restore claims the range the checkpoint was taken with. A migration
  target must be created with the source's range,
  `gcr.dev/userns-base=<first host ID>`; the source refuses otherwise.

## Hardening

Whatever seccomp profile a workload brings (or none, as Kubernetes pods
default to), gcr refuses calls behind most container escapes and kernel
exploits reachable from containers:

- with EPERM: `io_uring_*`, `bpf`, `perf_event_open`, `userfaultfd`,
  module loading and kexec, `open_by_handle_at`;
- with ENOSYS, as if the kernel lacked it: the keyring (`add_key`,
  `keyctl`, `request_key`), which is not namespaced (runc and other callers
  carry on without a session keyring then), and the new mount API
  (`fsopen`, `fsconfig`, `fsmount`, `fspick`, `open_tree`, `move_mount`,
  `mount_setattr`): `mount(8)` from util-linux 2.40 on falls back to
  `mount(2)`, which the supervisor checks, only on ENOSYS;
- when the container has no profile of its own, also: `AF_PACKET`,
  `AF_VSOCK`, `AF_KEY` and `AF_ALG` sockets, and raw IP sockets (`ping` uses
  ICMP datagram sockets, which gcr opens); `clone3` returns ENOSYS so libc
  falls back to `clone`, whose flags the filter can see. User namespaces
  the container makes itself are guarded (below).

A container that can mount (it has `CAP_SYS_ADMIN` without being
privileged; in a user namespace that capability only reaches the namespace)
has `mount(2)` handed to gcr's supervisor. The supervisor allows `proc`,
`sysfs`, `tmpfs`, `devpts`, `mqueue`, `cgroup2` and `overlay`, and bind
mounts, remounts and propagation changes; other filesystems the kernel would
allow in a user namespace (`ramfs`, `fuse`, ...) are refused. It performs the
mount itself, from the arguments it copied and checked (so another thread
cannot swap them after the check), in the caller's namespaces and with its
IDs and capabilities, so the kernel's own checks are the caller's. In a
user namespace the supervisor also sees `setns(2)`: joining the network
namespace a process is already in is a no-op that gcr answers itself
(dockerd does it, and the kernel would want rights over a pod network
namespace's owner); all other calls run as made. The filter is inherited,
so all of this holds for nested containers too, whatever profile they ask
for.

Privileged means `CAP_SYS_ADMIN` and access to all devices (what
`--privileged` gives); `CAP_SYS_ADMIN` alone is not.

**Nested user namespaces.** Chrome's sandbox (Puppeteer, Playwright),
bubblewrap and rootless podman/buildah make user namespaces of their own.
Root in one can reach kernel code an unprivileged process cannot, so in
gcr's own profile they are guarded rather than refused
(`NestedUserNamespaces = 'guarded'`, the default; tools that mount in
them, such as bubblewrap and rootless podman, also need `Landlock = 'off'`,
see [compatibility](docs/compatibility.md)):

- mounts go through the supervisor's filesystem allowlist, also in nested
  namespaces;
- netfilter (`NETLINK_NETFILTER` sockets) is refused to containers without
  `CAP_NET_ADMIN`; nf_tables is where most exploits reached this way go;
- the rest of the filter (raw and packet sockets, `io_uring`, `bpf`, the
  keyring) holds inside as well, and Landlock still refuses a confined
  container any mount.

What root in a new network namespace can ask the kernel to create there
is limited by the netlink policy (next section).
`NestedUserNamespaces = 'deny'` refuses `CLONE_NEWUSER` instead. A workload's own seccomp profile decides for
itself; containerd's `RuntimeDefault` refuses it, on runc as well. Ubuntu
hosts restrict unprivileged user namespaces with AppArmor
(`kernel.apparmor_restrict_unprivileged_userns`), which takes the
capabilities away inside unless a profile allows it, as with runc.

Privileged containers are exempt. Kubernetes pods with a profile
(`RuntimeDefault`, `Localhost`) get the denied set merged in. Pods that run
`Unconfined` stay unconfined by default, as CRI conformance requires
(`Seccomp: 0`). That is Kubernetes' own default, which a runtime cannot
tell from an explicit choice, so on nodes running gcr set the kubelet's
`seccompDefault: true` (GA since Kubernetes 1.27):

```yaml
# KubeletConfiguration
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
seccompDefault: true
```

Every pod without a profile then runs `RuntimeDefault`, and gets gcr's
denied set with it; only pods that ask for `Unconfined` stay unconfined.
Set the policy per runtime handler, in the runtime's options:
`Hardening = 'standard'` (default), `'denied'` (also gives unconfined pods
the denied set), `'always'` (gives them gcr's whole profile), or `'off'`.
`denied` and `always` fail CRI conformance's two tests of unconfined
seccomp. A workload cannot weaken any of them.
`test/e2e/hardening` probes every refused call.

### Netlink policy

Root in a network namespace, including one a container makes for itself,
configures it over netlink, and each kind of link, qdisc or filter it may
create is kernel code of its own (bonding, macvlan, VXLAN, tunnels, the
teql scheduler, ...). seccomp sees the socket's protocol, not the kind in
the message, so gcr decides in the kernel: a BPF LSM program on
`netlink_send` checks every `NETLINK_ROUTE` request from a gcr container
and allows

- link kinds `veth`, `dummy`, `bridge` and `vlan` (what container
  networking inside a container needs: Docker-in-Docker makes veth pairs
  and a bridge);
- qdisc, class and filter kinds `pfifo_fast`, `pfifo`, `bfifo`, `fq_codel`,
  `fq`, `codel`, `sfq`, `tbf`, `htb`, `prio`, `mq`, `cake`, `netem`,
  `ingress`, `clsact`, `u32`, `flower`, `matchall`, `basic`, `fw`.

Other kinds are refused with EPERM; messages that name no kind (addresses,
routes, neighbours, bringing links up) pass, as do other netlink protocols
(netfilter is seccomp's business, above). It covers the cgroups of hardened
containers, nested containers included, and nothing else on the node; the
program and its maps are pinned under `/sys/fs/bpf/gcr` and shared by all
shims. Set `NetlinkPolicy = 'off'` per runtime handler to skip it.

**The node needs the BPF LSM enabled:** `bpf` in the kernel's `lsm=` list
(`CONFIG_BPF_LSM=y` is common, the boot parameter is not; see
`hack/lima.yaml`). Without it the shim logs once and containers run with
seccomp alone. A GitHub-hosted runner cannot change its kernel command
line, so the tests that check the policy's refusals run on a self-hosted
runner instead (`hack/runner-setup.sh`, the `bpf-lsm` workflow); they skip
elsewhere, unless `GCR_E2E_REQUIRE_BPF` is set, which turns the skip into
a failure.

### Landlock

gcr confines each container's processes with Landlock to the container's
root filesystem: full access beneath its `/`, nothing outside it. Inside,
this changes nothing; the container's mounts and permissions decide. What
it stops is reaching the host's files through anything that leads outside:
a leaked host file descriptor or working directory (runc's CVE-2024-21626),
a `/proc/<pid>/root` link into another mount namespace, or whatever a
mount namespace bug would expose. Pipes, sockets and namespace files live
on internal filesystems Landlock does not restrict.

- **Applied in the container's init,** through an init hook in gcr's runc
  fork. That happens once the root filesystem is final and before
  capabilities drop, so no `no_new_privs` is needed and setuid binaries
  keep working. Everything the init starts inherits it.
- **Not for `exec`.** Landlock only lets a process trace or inspect
  (`/proc/<pid>/fd`, `environ`, `root`) processes in its own or a nested
  domain, and an exec would get a domain of its own. Debuggers, `strace -p`,
  `jcmd` and the like would stop working on the container.
- **Not for containers that can mount** (`CAP_SYS_ADMIN`, Docker-in-Docker):
  Landlock forbids mount changes. Privileged containers are exempt too.
- **Kept across checkpoint/restore** with the CRIU fork's `--landlock-fs`.
  The kernel cannot report a process's Landlock domain, so CRIU cannot dump
  it; gcr asks for the same confinement again. With a CRIU without it, gcr
  logs that restored containers are not confined.
- **Not for containers that join another container's PID namespace**:
  ephemeral debug containers (`kubectl debug --target`) and
  `shareProcessNamespace` pods. They join it to trace and inspect the
  processes there, which a sibling Landlock domain could not. The container
  they join stays confined.

Set it per runtime handler: `Landlock = 'standard'` (default) or `'off'`.
Kernels without Landlock run containers unconfined.
`test/e2e/landlock` passes a host directory descriptor into a container
and checks it leads nowhere, before and after a restore.

### Docker-in-Docker

dockerd runs in a gcr pod with a user namespace without being privileged:

- **User namespace from Kubernetes** (`hostUsers: false`, or CRI's
  `userns_options`). containerd then creates the pod's network namespace
  inside it, and dockerd can configure it. A pod in gcr's own user
  namespace (`gcr.dev/host-users: "false"`, `UserNamespaces = 'always'`)
  has a network namespace the host owns, which dockerd cannot manage.
- **Capabilities `SYS_ADMIN` and `NET_ADMIN`.** In the user namespace they
  reach only the pod's own namespaces. Mounts go through the supervisor
  (above), and `pivot_root`, which default profiles leave out, is allowed.
- **A delegated cgroup.** gcr gives a container in a user namespace with
  `CAP_SYS_ADMIN` its cgroup (the directory, `cgroup.procs`,
  `cgroup.threads`, `cgroup.subtree_control`) and a writable
  `/sys/fs/cgroup`; its cgroup namespace keeps it inside that subtree.
- **seccomp `RuntimeDefault`.** The hardening and supervision above are
  merged into it.
- **AppArmor `Unconfined` on AppArmor hosts**, as for privileged DinD. Under
  the default CRI profile the nested runtime's processes end up with labels
  that profile does not let signal each other.
- **dockerd flags `--iptables=false --bridge=none`.** Nested containers
  share the pod's network (`--network host`).

Nested `docker run` and `docker build` work. Nested `--privileged` does not:
the pod's `/sys` is read-only, and the kernel refuses a new read-write
sysfs under it. `test/e2e/dind` runs it all through CRI.

## GPUs

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

## Time travel

A container started with `gcr.dev/time-travel=execve` gets a point on its
timeline just after each `execve` (at most one per `gcr.dev/time-travel-interval`,
default `1s`; the last `gcr.dev/time-travel-keep`, default 20, are kept). A
point is the processes (CRIU) plus the writable layer, taken with the new
program stopped before its first instruction.

```sh
ctr run -d --runtime io.containerd.gcr.v2 --annotation gcr.dev/time-travel=execve IMAGE ID CMD...
gcr timeline ID
gcr rewind ID 3
```

Rewind replaces the processes and the writable layer in place; containerd
keeps seeing one running task. It needs CRIU with `--seccomp-notify-socket`
([makhov/criu#1](https://github.com/makhov/criu/pull/1)) installed as `criu`.

## Fork

A snapshot of a running container is a starting point for any number of
new ones. The parent keeps running:

```sh
gcr snapshot my-container /var/lib/gcr/snapshots/warm
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm \
  docker.io/library/python:3.13 fork-1 python3 -c 'pass'
```

Each fork restores the snapshot's processes, so it starts with whatever
the parent had in memory (an interpreter up, a model loaded) instead of
from its image's entrypoint, and gets the snapshot's files with its own
writable layer on top: what one fork writes, no other fork and not the
snapshot ever sees. The image the container is created with supplies the
lower layers and must be the one the snapshot was taken from.

Forks resume with identical memory, so whatever a program seeded or
generated before the snapshot (random number generators, IDs, tokens) is
the same in each. A fork can tell that it is one, and which:
`/.gcr/fork.json` holds its container ID, the snapshot and when it was
created, and with `gcr.dev/fork-signal=SIGUSR2` (or another signal) gcr
signals its init once it is restored, with the file in place, which is
the moment to re-seed. The signal is opt-in: a process that does not
handle SIGUSR2 dies of it.

In Kubernetes, `gcr.dev/fork-from` is for handlers that allow it: a pod
author could otherwise point it at any host directory. The handler lists
it in `PodAnnotations`, and its `SnapshotRoots` say where snapshots may be
(docs/configuration.md); `gcr.dev/migrate-in` is restricted the same way.

Forks share the parent's user namespace range, because the snapshot's
files carry host IDs from it: they are isolated from other containers,
not from each other at the host-ID level, which suits fanning one
workload out and not running two tenants.

Nothing is copied: the snapshot's writable layer is mounted read-only
underneath each fork's own, empty one, so a fork costs the same whether
the snapshot changed a file or a gigabyte. gcr assembles that overlay
itself, idmapping the image's layers and leaving the snapshot's alone (its
files already carry the range's IDs); containerd's idmapped overlay would
map it twice. A snapshot of a fork holds only what the fork changed, and
records the snapshots beneath it (`gcr-layers`), so forks of forks stack
layers rather than copy them. The price is that a snapshot must outlive
the forks of it, and the snapshots taken of those (Snapshot lifetime,
below). Checkpoints of a fork
record the same layers, and a fork migrates only to a fork of the same
snapshot (shared storage at the same path); the target refuses anything
else before the first page is sent.

### Snapshot lifetime

A snapshot has to outlive what mounts its layer: the forks of it, the
snapshots taken of those, and their forks. gcr keeps count, so the
platform can ask before it deletes one:

```sh
$ gcr snapshot-users /var/lib/gcr/snapshots/warm
KIND       USER                                  SINCE
container  default/fork-1                        2026-10-29 14:02:11
snapshot   /var/lib/gcr/snapshots/warm-step-2    2026-10-29 14:05:40
$ gcr snapshot-rm /var/lib/gcr/snapshots/warm
gcr: snapshot /var/lib/gcr/snapshots/warm is in use by container default/fork-1, snapshot /var/lib/gcr/snapshots/warm-step-2: failed precondition
```

- A container holds a reference (`gcr-refs/` in the snapshot) from create
  to delete, with a lock on it, so a shim that dies leaves nothing
  behind: a reference nobody holds is cleared when next read.
- A snapshot of a fork holds one on the snapshots beneath it for as long
  as it exists.
- `gcr snapshot-rm` refuses a snapshot in use, and a directory that does
  not look like a snapshot. A fork of a removed snapshot fails to start
  rather than run with files missing, and `gcr snapshot` refuses to write
  over a directory that is not empty.
- A snapshot holds its container's user namespace range until it is
  removed, so no other container gets those host IDs while a fork could
  still map them. A fork after a reboot, when the range is no longer
  held, claims it again, and is refused if another container has it by
  then. A snapshot deleted by hand rather than with `gcr snapshot-rm`
  lets go of its range when gcr next allocates one.

References are kept on the host the containers run on. With snapshots on
shared storage, check each host before deleting one. Checkpoints of a
fork (containerd's, not snapshots) hold no reference.

### Moving snapshots between hosts

Snapshots travel through any OCI registry: one backed by S3, ECR, GHCR,
Harbor, Zot.

```sh
# where the snapshot was taken
gcr snapshot-push /var/lib/gcr/snapshots/warm registry.example.com/snapshots/warm:v1
# anywhere else
gcr snapshot-pull registry.example.com/snapshots/warm:v1 /var/lib/gcr/snapshots/warm
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm IMAGE fork-1 CMD...
```

- **What gets pushed.** A snapshot is one artifact: its process images,
  and the writable layers a fork mounts (its own, and those of the
  snapshots it was forked from).
  - Each part is a zstd-compressed tar, packed deterministically, so a
    layer that several snapshots share is stored once in the registry
    and uploaded once.
  - The tars keep numeric owners, xattrs and overlay's whiteouts.
- **Where a pull puts things.**
  - The snapshot goes to the directory given.
  - The layers beneath it go to a store (`-store`, default
    `/var/lib/gcr/layers`), shared by every snapshot pulled onto the host
    that has them.
  - Both are counted like snapshots taken there (`gcr snapshot-users`,
    `gcr snapshot-rm`).
- **Credentials** come from Docker's config (`docker login`). Registries
  on loopback are spoken to over plain HTTP; `-plain-http` forces it for
  others.
- **User namespace ranges.** A snapshot's files and process images
  carry host IDs from the range its container mapped, and its forks map
  that range.
  - A pull keeps the range if it is free on the target.
  - If another container has it there, the pull moves the snapshot into
    a free range: file owners and the IDs in capability and ACL xattrs
    are shifted, and the user namespace maps in the CRIU images
    (`userns-*.img`) are rewritten. CRIU records every other ID as the
    container sees it. `gcr snapshot-pull` says when it did this.
  - Snapshots taken from the same container in the same range share one
    range on the target, as forks of one snapshot do, so their layers
    in the store are shared too.
  - A move costs a pass over the snapshot's files; a moved snapshot's
    layers are a copy of their own in the store, and pushing it again
    packs new blobs.
- **What else the target needs.** The image the snapshot was taken
  from, the same architecture (checked), and a CPU with the features
  the processes were using (CRIU checks).

A registry storing in S3, for example the distribution registry's own:

```yaml
# config.yml for docker.io/library/registry:3
version: 0.1
storage:
  s3:
    region: eu-west-1
    bucket: gcr-snapshots
http:
  addr: :5000
```

### Lazy restore

A restore or fork loads the snapshot's memory before the container runs,
which costs the time to read it. With `gcr.dev/lazy-restore=true` the
container runs as soon as its processes are in place, and a
`criu lazy-pages` daemon sends each page the moment it is touched:

```sh
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm \
  --annotation gcr.dev/lazy-restore=true \
  docker.io/library/python:3.13 fork-1 python3 -c 'pass'
```

A fork of a snapshot holding 384 MiB of touched memory takes 175 ms to
start this way against 459 ms eagerly, and the gap grows with the
snapshot. What it costs: the daemon has to outlive the restore, and a
container whose daemon dies before it has touched every page gets SIGBUS
on the next one (gcr logs that the daemon stopped early). Each page is
still a fork's own copy once it arrives; see below for sharing them.

**The node has to allow it.** The restored tasks open the userfaultfd
themselves, from inside the container's user namespace, and the kernel
allows that unrestricted only to `CAP_SYS_PTRACE` in the initial user
namespace or when `vm.unprivileged_userfaultfd` is 1. The restricted form
(`UFFD_USER_MODE_ONLY`) is not enough, because the kernel also faults a
restored process's pages on its behalf, during a read into one for
instance, and those faults would reach nobody. Without the setting gcr
refuses the container and says so, rather than leaving a CRIU log to read.

Setting it to 1 lets any unprivileged process on the node use userfaultfd
to handle faults from kernel mode, which is a step in several kernel
exploits; it is why gcr's own filter denies `userfaultfd` to containers,
and that still holds with the setting on, since only the restore (before
the filter is in place) uses it. So the exposure is to whatever else runs
on the node, which argues for turning it on where gcr containers are the
only unprivileged workload and leaving it alone elsewhere.

### Shared memory

Forks of one snapshot start with identical copies of its memory. With
`gcr.dev/memory-merge=true` the kernel's same-page merging (KSM) folds
those copies into one, copy-on-write: a fork pays again only for the pages
it writes. Ten forks of a snapshot holding 128 MiB come to about 210 MiB a
second after they start, against 1.25 GiB without it.

How: Linux 6.4's `PR_SET_MEMORY_MERGE` marks all of a process's memory
mergeable, and its children inherit it. gcr runs the criu that restores a
container through the shim, which sets it before exec'ing criu, so every
task criu restores has it; a container started from its image gets it from
its init, just before the exec. KSM must be running
(`/sys/kernel/mm/ksm/run` = 1); without it gcr refuses the annotation.

What it costs, and why it is opt-in:

- Merging happens after the fact. ksmd scans memory in the background
  (`pages_to_scan`, `sleep_millisecs` set how fast, and what CPU it
  takes), so forks briefly hold their full size and a scheduler has to
  admit them at that size.
- KSM merges across every process that allows it, not per group. Two
  containers that both opt in can tell, from how long a write takes,
  whether the other holds a page with given contents: a known side
  channel. Use it among containers that trust each other, such as the forks
  of one snapshot (which share a user namespace range anyway), and not
  across tenants.

The other routes considered: restoring memory as private mappings of the
image files, shared through the page cache, is immediate but changes what
the process sees. An `MADV_DONTNEED` on such a mapping reads the file back
instead of giving zeros, and allocators rely on the zeros. Restoring once
and fork(2)ing whole containers does not exist in the kernel. KSM keeps
anonymous memory anonymous, so nothing in the container can tell.

`test/e2e/fork` snapshots a counter and runs ten forks of it at once, and
measures ten forks' memory with merging on.

## Live migration

The target is an ordinary gcr container with the same image and command,
created with `gcr.dev/migrate-in` (address to listen on) and
`gcr.dev/migrate-token`. It does not start its process; its `Start` waits for
the source and restores what it receives.

```sh
# target host
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/migrate-in=0.0.0.0:7781 --annotation gcr.dev/migrate-token=$TOKEN \
  IMAGE ID CMD...
# source host
gcr migrate -token $TOKEN ID http://TARGET-HOST:7781      # https:// with MigrationTLS (below)
```

With user namespaces, create the target with the source's range
(`--annotation gcr.dev/userns-base=N`; a refused migration prints N).

When the kernel tracks dirty memory (x86, s390, powerpc; not arm64), the
source pre-copies memory in rounds to CRIU page servers on the target while
it keeps running. It then freezes, sends the remaining memory, the rest of
the checkpoint and the writable layer, and the target restores. The source
stays frozen until the target runs and is thawed if anything fails before;
afterwards it is killed. Open TCP connections reset.

Everything goes over the one address the target listens on. Its page
servers listen on loopback only, and each round's memory is carried to
them over the migration's connection (an HTTP Upgrade), after the token
is checked. Without TLS that connection is in clear text, memory
included. To encrypt it and have each side verify the other, give both
runtime handlers certificates from one CA, and migrate to `https://`:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr.options.MigrationTLS]
  Cert = '/etc/gcr/tls/node.crt'   # valid for the address sources connect to
  Key = '/etc/gcr/tls/node.key'
  CA = '/etc/gcr/tls/ca.crt'       # the CA both sides trust
```

The target then serves only TLS 1.3, and asks every source for a
certificate signed by the CA. A source with `MigrationTLS` set refuses
`http://` targets. The token still has to match: the certificate says
the source is a node of the cluster, and the token that it is the one
meant to send this container.

## Tests

| | What | Needs |
| --- | --- | --- |
| `make test` | unit tests | Linux, libseccomp |
| `make test-e2e` | end-to-end tests in `test/e2e`, below | root (via sudo), containerd, CNI, the CRIU fork, network |
| `critest --runtime-handler gcr` | CRI conformance | root, containerd, CNI |
| `make test-k8s` | Kubernetes (stock k0s via k0da) with a gcr RuntimeClass | docker or podman, k0da >= v0.4.0 |

The end-to-end tests are Go tests behind the `e2e` build tag, one directory
per test, sharing a harness in `test/e2e/framework` (containerd's client,
CRI, checkpoint/restore, static helper programs to run in containers):

| `test/e2e/` | What |
| --- | --- |
| `checkpoint` | a counter carries on after checkpoint/restore, in the same user namespace |
| `timetravel` | a timeline, a rewind; the counter, the filesystem and the recording carry on from the point |
| `gpu` | checkpoint and restore of CUDA state, and forks of a model in GPU memory (needs a GPU node; skipped elsewhere); a GPU gcr cannot describe is refused checkpoints |
| `fork` | ten containers from one snapshot: each resumes its memory and mounts the snapshot's layer beneath its own; forks of forks; a fork's identity and signal; lazy restore, and what it saves; merged memory; a snapshot of a fork through a registry, forked after everything local is gone |
| `workloads` | real programs forked: Python, Node.js and JVM servers holding a model in memory, and Postgres with a table; each fork serves, keeps its state and diverges; a re-seed on the fork signal |
| `migrate` | live migration between containerd namespaces (pre-copy on x86); a refused one leaves the source running |
| `hardening` | refused system calls with hardening on and off; supervised mounts, by exact return value; nested user namespaces; the netlink policy (BPF LSM) |
| `landlock` | a leaked host descriptor, with Landlock on and off, across a restore |
| `dind` | Docker-in-Docker, unprivileged, in a user namespace |
| `cri` | a pod through CRI: exec, exit codes, logs, updates, stats, user namespace |
| `crio` | a pod under CRI-O (runtime type `vm`): exec, exit codes, logs, the container is gcr's, stop and remove end the shim; skipped without CRI-O (`hack/crio-setup.sh`) |

`sudo hack/clean-e2e.sh` removes what a failed run left behind (a leaked
counter container keeps writing, which slows the next run down). Runs
take a lock (`/tmp/gcr-e2e.lock`), so a run started while another is in
progress waits rather than competing with it for the machine.
`make test-e2e` runs them all, each package's test binary as root, one
package at a time; `make test-e2e E2E='-run Landlock ./test/e2e/landlock'`
picks some. `sudo hack/ci-setup.sh` prepares a host for them (containerd,
CNI, cri-tools, the CRIU fork). CI runs everything on every pull request,
except the netlink policy's own tests: they need the BPF LSM in the
kernel's `lsm=` list, which a GitHub-hosted runner cannot have, so the
`bpf-lsm` workflow runs them on a self-hosted runner (on main, on request,
and on a pull request labelled `run-bpf`). To set one up:

```sh
REPO=makhov/gcr TOKEN=$(gh api -X POST \
  repos/makhov/gcr/actions/runners/registration-token -q .token) \
  sudo -E ./hack/runner-setup.sh      # adds bpf to lsm=, registers a service
```

It is a host the repository's workflows execute code on, so use one you
are willing to give everyone with write access.

## Develop

Everything builds and runs in a Linux VM (kernel 6.12+, containerd 2.4, CRIU):

```sh
limactl start --name=gcr hack/lima.yaml   # once
limactl restart gcr                       # once: boots with the BPF LSM (netlink policy)
make vm T=criu                            # once: build and install the CRIU fork from ../criu
make vm T=test-e2e                        # build, install, run the end-to-end tests
```

For the CRI conformance suite:
`critest --runtime-handler gcr` (needs CNI plugins and the `gcr` runtime in
containerd's CRI config).
