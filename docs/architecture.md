# Architecture

gcr is two binaries:

- `containerd-shim-gcr-v2`, the runtime. containerd starts one per pod
  (or per container started outside Kubernetes) and talks to it over
  the shim task API. It runs the containers itself, through
  libcontainer linked into it: there is no runc binary and no process
  per operation.
- `gcr`, a command line for what containerd's API has no call for
  (snapshots, time travel, migration, moving snapshots between hosts).
  It talks to a container's shim over that container's control socket.

```
            containerd (or CRI-O)
                 │ task API (ttrpc)
                 ▼
   ┌──────── containerd-shim-gcr-v2 ────────────────────────────────┐
   │ task service ── container (libcontainer) ── init, exec         │
   │      │               │                                         │
   │      │               ├─ supervisor: seccomp notifications      │
   │      │               │    (mount, setns, execve) ─ nsexec      │
   │      │               ├─ CRIU (checkpoint, restore, fork,       │
   │      │               │        time travel, migration)          │
   │      │               └─ OOM watcher (memory.events)            │
   │      └─ control socket (/run/gcr-ctl/<hash>.sock) ◄── gcr CLI  │
   │      └─ migration receiver (gcr.dev/migrate-in) ◄── other node │
   └────────────────────────────────────────────────────────────────┘
        BPF LSM netlink policy (pinned in /sys/fs/bpf/gcr, all shims)
```

## The shim

`cmd/containerd-shim-gcr-v2` is built on containerd's shim library. It
serves the task API's version 3 to containerd 2.3 and later, and
version 2 (same messages) to older containerd and to CRI-O, which runs
it as a runtime of type `vm`.

- **`internal/manager`** starts and stops shim processes (containerd's
  shim bootstrap protocol).
- **`internal/task`** implements the task API: create, start, exec,
  kill, wait, delete, checkpoint, restore and the rest. On create it
  reads the handler's options (`internal/config`), decides what applies
  to this container (user namespace, hardening, Landlock, the netlink
  policy, time travel, fork, migration) and refuses what the handler
  does not allow.
- **`internal/container`** runs one OCI container through libcontainer:
  its rootfs mounts, init process, stdio, exit status, cgroup and
  OOM kills. Checkpoint, restore, snapshots and forks are here, as
  calls into CRIU.

libcontainer re-executes the shim binary as the container's init (the
`init` argument). gcr's runc fork, `makhov/runc`, adds two things:
an init hook, which runs once the root filesystem is final and before
capabilities drop (gcr applies Landlock and memory merging there), and
a way to run CRIU through a wrapper, so the shim can set up CRIU's
process before it starts (memory merging again).

## Process state: CRIU

Every feature that moves process state goes through CRIU, which
libcontainer drives over RPC. Options the RPC has no field for go in a
CRIU configuration file the container rewrites before each dump and
restore. gcr's CRIU, the `gcr` branch of
[makhov/criu](https://github.com/makhov/criu) (upstream CRIU and gcr's
changes, released as tags like `v4.2.1+gcr.0`; `hack/criu-version` pins
the one gcr builds), adds:

- `--seccomp-notify-socket`: a restored container's seccomp listener is
  handed to the new shim's supervisor, so time travel and supervised
  mounts keep working after a restore;
- `--landlock-fs`: the restored processes are confined again (the kernel
  cannot report a Landlock domain, so CRIU cannot dump one);
- `--lazy-pages-on-demand`: a fork's lazy-pages daemon copies only the
  pages it touches, and copies the rest on SIGUSR1 before a dump;
- `STATIC=libs` and `STATIC=1` builds, which releases use.

What each feature adds on top:

| Feature | What it is in CRIU terms |
| --- | --- |
| Checkpoint / restore | containerd's own: a dump to a checkpoint image, a restore into a new task |
| Snapshot (`gcr snapshot`) | a dump that leaves the container running (`leave-running`), plus a copy of the writable layer; with `-stop`, the container is killed once it is written |
| Fork (`gcr.dev/fork-from`) | a restore of a snapshot into a new container, whose rootfs is the image, then the snapshot's layers read-only, then its own empty writable layer |
| Time travel | a dump at each `execve`, held by the supervisor; incremental where the kernel tracks dirty pages (parent images); a rewind is a restore in place |
| Live migration | pre-dumps streamed to page servers on the target, then a final dump and the writable layer; the target restores |
| Lazy restore | a restore with `lazy-pages`, pages served on fault by a `criu lazy-pages` daemon |

`internal/timetravel` keeps the timeline, `internal/migrate` the sender
and receiver, `internal/snapshot` who uses a snapshot, and
`internal/snapshot/registry` pushes and pulls snapshots as OCI
artifacts.

## The supervisor

Some system calls cannot be decided by a seccomp filter alone, because
the answer depends on memory the filter cannot read (a path, a mount's
flags) or needs something done on the caller's behalf. gcr's filter
hands those to user space (`SECCOMP_RET_USER_NOTIF`), and
`internal/supervisor` answers them, one supervisor per container:

- **`mount(2)`**, for containers in a user namespace that can mount:
  the arguments are copied out of the caller, checked against the
  filesystem allowlist, and the mount is done by gcr.
- **`setns(2)`**, in a user namespace: joining the network namespace
  the caller is already in is answered by gcr; everything else runs as
  made.
- **`execve(2)`**, for time travel: the call is let through, the new
  program is stopped before its first instruction, and a point is
  taken.

A mount has to happen in the caller's namespaces and as the caller.
The Go runtime cannot change namespaces safely, so the shim re-executes
itself as a small helper (`internal/nsexec`): a C constructor that runs
before the Go runtime starts, joins the caller's namespaces, takes its
credentials and AppArmor profile, drops its own file descriptors and
makes the call.

## Kernel-side policy

- **seccomp** (`internal/hardening`): the denied set merged into any
  profile, gcr's own profile for containers without one, and the
  notifications above.
- **Landlock** (`internal/landlock`): applied in the init hook, confines
  the container to its root filesystem.
- **The netlink policy** (`internal/netpolicy`): a BPF LSM program on
  `netlink_send` (`internal/netpolicy/bpf/netlink.bpf.c`), loaded by the
  first shim and pinned under `/sys/fs/bpf/gcr` for all of them. Each
  shim adds its hardened containers' cgroups to the program's map. A
  newer gcr replaces an older program in place.
- **User namespaces** (`internal/userns`): a range of 65536 host IDs per
  pod, allocated from files under `/run/gcr/userns`; layers are mounted
  idmapped.

## Where state lives

| Path | What |
| --- | --- |
| `/run/gcr/<namespace>/` | libcontainer state, per container |
| `/run/gcr/userns/` | user namespace range allocations |
| `/run/gcr/netlink.lock`, `/run/gcr/netlink.version` | loading and upgrading the netlink policy |
| `/run/gcr-ctl/<hash>.sock` | control sockets (root only), one per container |
| `/sys/fs/bpf/gcr/` | the netlink policy's program, link and maps |
| `/var/lib/gcr/<namespace>/<id>/` | a container's timeline and migration rounds |
| `/var/lib/gcr/layers/` | layers of pulled snapshots (`gcr snapshot-pull -store`) |
| the bundle (containerd's) | CRIU logs of failed dumps and restores (`criu-dump-*`, `criu-restore-*`), the `oom` marker |
| snapshot directories | wherever `gcr snapshot` writes them; `images/` (CRIU), `upper/` (writable layer), `gcr-refs/` (who uses it) |

## Source layout

| Path | What |
| --- | --- |
| `cmd/containerd-shim-gcr-v2` | the shim |
| `cmd/gcr` | the command line |
| `internal/` | packages above, plus `control` (control socket), `gpu` (GPU checks for checkpoints), `imagetar` (extracting CRIU images from elsewhere), `ocispec` (spec predicates) |
| `test/e2e` | end-to-end tests, one directory per feature ([development](development.md)) |
| `hack/` | VM, CI, CRI-O and runner setup |
