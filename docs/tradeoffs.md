# What gcr is good for, and where it falls short

gcr is a container runtime, not a virtual machine. Its containers share the
host's kernel, as runc's do; what it adds is process state as something a
platform can work with (checkpoint, restore, fork, rewind, migrate) and a
container that is harder to break out of than runc's by default. This page
says what that buys, what it costs, and what has not been shown yet. The
README has the details of each feature; numbers here come from it and from
the end-to-end tests (arm64 VM, Linux 7.0, unless said otherwise).

## In one table

| Want | gcr |
| --- | --- |
| Start many sandboxes from a warm state (interpreter up, model loaded) | Yes: fork, in tens of milliseconds, sharing files and (opt-in) memory |
| Run code from people you do not trust, next to each other | Only with care: a shared kernel, hardened; see Isolation |
| Drop-in for runc under containerd or Kubernetes | Yes: CRI conformance passes on containerd 1.7 to 2.4; some hardening is off under Kubernetes defaults. Under CRI-O, without gcr's user namespaces and process-state features |
| Move a running sandbox to another node | Yes, with pre-copy on x86; stop-and-copy on arm64; TCP connections reset |
| Undo what a sandbox did | Yes: time travel, for its processes and writable layer, not its volumes |
| GPUs | Yes, NVIDIA through CDI: checkpoint, restore and fork with GPU memory, tested on one Blackwell GPU |
| Strong isolation for hostile multi-tenant code | No: use a VM-based runtime |

## Advantages

**Process state is a first-class object.** A running container can be
snapshotted without stopping it, forked any number of times, rewound to a
point on its timeline, or moved to another host. These are built into the
runtime, behind containerd's ordinary task API and a few annotations, not a
separate tool driving CRIU from outside.

**Forks are cheap.** Ten forks of a small container start together in
about 0.1 s. A fork does not copy the snapshot's files: the snapshot's
writable layer is mounted read-only beneath each fork's own, so a fork
costs an empty layer whatever the snapshot holds. Its memory loads in
about 0.3 s for 384 MiB, or 0.18 s with lazy restore, which runs the fork
before its memory arrives. With memory merging on, ten forks of a 128 MiB
snapshot settle at about 210 MiB instead of 1.25 GiB.

**Safer defaults than runc, without breaking workloads.**

- User namespaces by default: root in the container is an unprivileged
  range of host IDs.
- A denied set of system calls on top of whatever profile the workload
  brings (`io_uring`, `bpf`, the new mount API, the keyring, raw and packet
  sockets, ...).
- Mounts handed to a supervisor that allows only known filesystems.
- A BPF LSM policy on which kinds of network devices and queues a
  container may create.
- Landlock confining each container to its root filesystem, which closes
  the class of escapes through leaked host file descriptors
  (CVE-2024-21626).

Some tools that need more than a default container keep working:
Docker-in-Docker runs unprivileged, and nested user namespaces (Chrome's
sandbox) are guarded rather than refused. Tools that mount inside a
nested namespace (bubblewrap, rootless podman) need a handler with
Landlock off. [Compatibility](compatibility.md) lists what the hardening
breaks and the way around each; [configuration](configuration.md) has the
knobs.

**Fast and compatible.** gcr runs libcontainer inside the shim instead of
executing a runc binary per operation: container start is 16 ms median
against runc's 22 ms, stop 17 against 31. It passes CRI conformance
(critest), including the user namespace tests runc skips.

**Every feature is tested end to end.** Each one has a test that runs real
containers through containerd and checks the outcome, and the hardening
tests check that each refused call is refused, by return value.

## Limitations

### Isolation

- **One kernel for everything.** A kernel bug reachable from a container
  is a way out. gcr removes the system calls behind most known escapes and
  confines what remains, but it cannot remove the kernel itself. gVisor
  answers system calls in a kernel of its own, and Kata and Firecracker
  give each sandbox its own kernel under a hypervisor; both are stronger
  boundaries, at a cost in compatibility or overhead. For hostile code
  from many tenants on one host, use one of them, or gcr inside a VM per
  tenant.
- **Under Kubernetes defaults, much of the hardening is off.** Kubernetes
  sends `Unconfined` seccomp and `hostUsers: true` by default, and a
  runtime cannot tell that from an explicit choice. CRI conformance
  requires an `Unconfined` container to have no seccomp filter, so gcr
  leaves it be: a default pod gets no denied set and no user namespace,
  only Landlock. The kubelet's `seccompDefault: true` fixes the first for
  every pod that does not ask for `Unconfined`, and keeps conformance.
  Beyond that, `Hardening = 'denied'` or `'always'` and
  `UserNamespaces = 'always'` on a runtime handler, or pod settings
  (`hostUsers: false`), at the cost of those conformance tests.
- **Some workloads need the hardening relaxed.** io_uring programs,
  in-container eBPF and profilers, packet capture, kernel WireGuard and
  VXLAN, FUSE mounts, and sandboxing tools that mount (bubblewrap,
  rootless podman) are refused under the defaults. Each needs a handler
  configured for it ([compatibility](compatibility.md)). There is no
  per-container opt-out.
- **Landlock has gaps.** It does not apply to processes started with
  `exec` (it would break debuggers), to containers that can mount
  (Docker-in-Docker), to privileged containers, or to containers that join
  another's PID namespace.
- **The netlink policy needs the BPF LSM** in the kernel's boot
  parameters. Most distributions build it but do not enable it, and
  without it gcr falls back to seccomp alone.
- **No independent security review yet.** The tests show each defence
  does what it claims, not that nothing was missed.

### Fork

- **Forks are not separate tenants.** They share the parent's user
  namespace range, because the snapshot's files carry host IDs from it.
  Fork within one workload, not across customers.
- **Forks are identical, including what should be unique.** Each one
  resumes with the parent's memory: the same seeds in user-space random
  number generators, the same IDs and tokens generated before the
  snapshot. The kernel's random source is unaffected,
  but programs that seeded their own before the snapshot will produce the
  same "random" values in every fork. This is the same problem as cloning
  a VM. gcr tells a fork it is one (`/.gcr/fork.json`, and
  `gcr.dev/fork-signal` once it is restored), but the program has to
  listen and re-seed: gcr cannot do it for it.
- **A snapshot has to outlive its forks,** and any snapshots taken of
  them, since their layers are mounted from it. gcr counts who uses each
  one and refuses to remove one in use (`gcr snapshot-users`,
  `gcr snapshot-rm`), but only on its own host: with snapshots on shared
  storage, the platform has to ask every host. Deciding when a snapshot
  is no longer wanted is the platform's job.
- **A snapshot holds its user namespace range** until it is removed, so
  a snapshot kept around keeps one of the host's 16,384 ranges.
- **Snapshots are plain directories on the host,** holding the full memory
  of the container, secrets included. Protect them like the workload's
  data, and the registries they are pushed to as well: anyone who can
  pull one can read that memory. gcr moves them between hosts
  (`gcr snapshot-push`, `gcr snapshot-pull`); naming and expiring them is
  up to the platform.
- **A pulled snapshot may be moved to another user namespace range.** Its
  files and processes carry host IDs from the range it was taken in.
  Where that range is taken, a pull shifts the snapshot into a free one,
  which costs a pass over its files and a copy of its layers. This
  relies on CRIU recording every ID but the namespace's maps as the
  container sees it: a kind of state that CRIU records otherwise would
  come back with the wrong owner.
- **A fork must use the image the snapshot was taken from.** Nothing
  outside the container's writable layer is captured: volumes and bind
  mounts must be there, in the same state, when a fork starts.
- **Tested with a handful of real programs.** These fork in the tests,
  each keeping its listening socket and in-memory state and diverging from
  the others afterwards:
  - a Python, a Node.js and a JVM HTTP server, each holding a 48 MiB
    model in memory (forks start in 0.2–0.3 s);
  - Postgres with a table: several processes sharing memory, with their
    data in the writable layer.

  Connections open at the time of the snapshot are not restored (CRIU
  would need `--tcp-established` and the same addresses). Nor are kernel
  objects CRIU does not support.

### Lazy restore

- **The node must set `vm.unprivileged_userfaultfd = 1`,** which lets
  every unprivileged process on it use userfaultfd from kernel mode, a
  common step in kernel exploits. gcr's containers still cannot use it.
  The exposure is to the rest of the node.
- **The container depends on a daemon** until it has touched every page.
  If the daemon dies first, the next page the container touches kills it
  (SIGBUS). gcr logs it when that happens.

### Shared memory (memory merging)

- **Merging lags.** The kernel scans memory in the background, so forks
  hold their full size for a moment (about a second in the tests, with
  the scanner set to run fast). Admission control has to count the full
  size. The scanner's speed costs CPU, and the node operator sets it.
- **KSM is a side channel.** It merges across every process that allows
  it, not per group. Two containers that both opt in can learn whether
  the other holds a page with given contents by timing a write. Use it
  among containers that trust each other, such as forks of one snapshot.
- **Needs Linux 6.4 or later, with KSM running.**

### Checkpoint, restore and time travel

- **CRIU decides what can be saved.** Most of what ordinary programs use
  restores, but not everything, and a process holding something CRIU
  cannot save cannot be checkpointed, forked, rewound or migrated. Systems
  that implement their own kernel (gVisor) or snapshot a whole VM
  (Firecracker) save state their own way and have limits of their own,
  but not CRIU's.
- **gcr depends on forks of runc and CRIU.** Time travel needs CRIU with
  seccomp notification support, keeping Landlock across a restore needs
  another change, and both are in `makhov/criu`, not upstream CRIU. The
  runc fork adds an init hook and a way to wrap the criu it runs.
- **Time travel costs every exec, and each point freezes the container.**
  Measured (`test/e2e/timetravel` `TestCost`, `GCR_E2E_BENCH=1`; an arm64
  VM and an x86 VM, points on their virtual disks):
  - An exec takes 45–85 µs longer (16–30% for a tiny program), whether or
    not it takes a point: each one goes through gcr's supervisor.
  - A point is a full dump of memory plus a copy of the writable layer,
    with the container frozen meanwhile: about 90–125 ms at 64 MiB of
    memory, 140–160 ms at 256 MiB, 260–390 ms at 1 GiB, and about 30 ms
    more for 200 MiB written. These are writes to the page cache; a node
    short of memory or disk bandwidth freezes longer.
  - Each point takes as much disk as the container's memory plus its
    writable layer: with the default of 20 points kept, a 1 GiB container
    keeps about 20 GiB. Points are full, not incremental.

  Points are taken at most once per interval (1 s by default), so the
  freeze is a fraction of that at most; a container that does not exec
  takes none.
- **Rewind covers processes and the writable layer only,** not volumes,
  nor anything outside the container (databases it talked to, files on a
  shared volume).
- **Time travel is refused for containers with GPUs:** every point would
  copy GPU memory.

### Live migration

- **Open TCP connections reset.** Clients have to reconnect.
- **Stop-and-copy on arm64.** Pre-copy, which keeps downtime short for
  large memory, needs the kernel to track dirty pages, which arm64 does
  not.
- **The target must match the source:** the same image and command, the
  same user namespace range, and for a fork the same snapshot at the same
  path on shared storage. A mismatch is refused before any memory is
  sent.
- **Encrypted only with `MigrationTLS`.** Without it, the migration's
  one connection (memory, files and the token) is plain HTTP. Setting it
  takes a certificate for each node, from a CA the platform runs.

### GPUs

- **Tested on one GPU model.** The GPU tests ran on a single NVIDIA RTX
  PRO 6000 Blackwell (driver 580): checkpoint and restore, and forks of a
  4 GiB model in GPU memory (snapshot 1.9 s, each fork 1.6–1.8 s). Not
  yet on several GPUs in one container, other generations, or under
  Kubernetes. CI has no GPU, so these tests run only by hand on a GPU
  node.
- **A GPU fork copies device memory.** Unlike forks' host memory, which
  memory merging can share, each fork has its own copy of the
  snapshot's GPU memory, and the forks have to fit on the GPU together.
- **A restore needs the same GPU index,** the same driver version, and
  the same GPU models and count.
- **GPU containers migrate stop-and-copy.** CRIU's CUDA plugin cannot
  pre-dump.
- **Not yet built:** the NVIDIA ioctl proxy, and restoring onto another
  GPU index.

### Platform and maturity

- **Linux 6.12+ with cgroup v2, run as root;** containerd 1.7 or later.
  Under CRI-O, gcr runs pods with its hardening, but without its user
  namespaces or any of its process-state features (snapshots, forks,
  time travel, migration): CRI-O mounts the rootfs itself. No rootless
  mode.
- **Developed on arm64 (Linux 7.0); CI runs on x86 (Ubuntu 24.04).** The
  whole end-to-end suite also passes on an x86 GPU node (Ubuntu 24.04
  with its 7.0 HWE kernel). Other kernels and distributions have not been
  tried.
- **Each shim uses about 6 MiB, against runc's 4 MiB,** because the shim
  carries libcontainer instead of calling a binary.
- **Young.** The name is a placeholder, interfaces (annotations, the
  snapshot layout) may change, and it has not run in production.
