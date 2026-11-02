# Fork

A snapshot of a running container is a starting point for any number of
new ones. The original keeps running.

```sh
gcr snapshot my-container /var/lib/gcr/snapshots/warm
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm \
  docker.io/library/python:3.13 fork-1 python3 -c 'pass'
```

Used for [warm starts](examples/warm-starts.md) and for
[letting an agent branch](examples/agent-sandbox.md).

## What a fork gets

- **The snapshot's processes**, restored, so it starts with whatever the
  parent had in memory (an interpreter up, a model loaded) instead of
  from the image's entrypoint. The command you pass is never run.
- **The snapshot's files**, with its own writable layer on top: what one
  fork writes, no other fork and not the snapshot ever sees.
- **Nothing copied.** The snapshot's writable layer is mounted read-only
  beneath each fork's own empty one, so a fork costs the same whether the
  snapshot changed one file or a gigabyte.

The container must be created from the image the snapshot was taken from:
that image supplies the layers underneath.

## What to watch out for

**Forks are identical, including what should be unique.** They resume
with the same memory, so anything a program seeded or generated before
the snapshot (random number generators, IDs, tokens) is the same in every
fork. A fork can tell that it is one: `/.gcr/fork.json` holds its
container ID, the snapshot, and when it was created. With
`gcr.dev/fork-signal=SIGUSR2` (or another signal) gcr signals its init
once it is restored and that file is in place, which is the moment to
re-seed. The signal is opt-in, because a process that does not handle
SIGUSR2 dies of it.

**Forks are not separate tenants.** They share the parent's user
namespace range, because the snapshot's files carry host IDs from it.
They are isolated from other containers, not from each other at the
host-ID level, which suits fanning one workload out and not running two
customers.

**A snapshot must outlive its forks**, and the snapshots taken of them
([below](#snapshot-lifetime)).

**On Kubernetes, forking is a privilege.** `gcr.dev/fork-from` names a
path on the node, so a pod author who could set it freely would get more
than their own container. The handler has to list the annotation in
`PodAnnotations`, and its `SnapshotRoots` say where snapshots may live
([configuration](configuration.md#podannotations)); `gcr.dev/migrate-in`
is restricted the same way.

## How the layers stack

gcr assembles the fork's overlay itself, idmapping the image's layers and
leaving the snapshot's alone, since its files already carry the range's
IDs; containerd's idmapped overlay would map them twice.

A snapshot of a fork holds only what that fork changed, and records the
snapshots beneath it (`gcr-layers`), so forks of forks stack layers
rather than copy them. The price is the lifetime rule above. Checkpoints
of a fork record the same layers, and a fork migrates only to a fork of
the same snapshot (shared storage, same path); the target refuses
anything else before the first page is sent.

## Snapshot lifetime

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


## Lazy restore

> **Unreliable on x86 for now** ([#83](https://github.com/makhov/gcr/issues/83)).
> On some x86 hosts a lazy restore occasionally gives the restored process
> pages that were filled before the lazy-pages daemon could fill them, so it
> runs on wrong memory and usually crashes soon after. It shows up on
> GitHub's x86 runners (Linux 7.0) a few times in a hundred lazy restores. It
> hasn't shown up on arm64, or in hundreds of runs on an x86 host with
> Linux 7.2, but until the cause is found, use eager forks for anything that
> matters on x86.

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
snapshot.

**A fork holds only what it touches.** With gcr's CRIU
([makhov/criu](https://github.com/makhov/criu), `--lazy-pages-on-demand`),
the daemon of a fork copies in only the pages the fork touches, for as
long as it runs, and gcr stops it when the fork is deleted. A fork of a 1
GiB Python process runs with 8 MiB resident, and still does seconds later;
an eager fork, or a lazy one with upstream CRIU (whose daemon copies the
rest in the background), holds the whole gigabyte. Ten forks of one
snapshot then cost ten times what each touched, not ten times the
snapshot. Restores from a checkpoint keep the background copy: their
images may not outlive the restore, while a fork holds its snapshot.

- **Dumps fill the memory first.** A snapshot, a checkpoint, a time
  travel point or a migration of a container whose memory is still being
  filled first has its daemon copy in the rest, and waits: CRIU would
  take a page nobody copied in for one never written, and leave it out.
  So the first snapshot of a lazy fork costs reading the rest of its
  memory.
- **The daemon must outlive the container's use of its pages.** A
  container whose daemon dies gets SIGBUS on the next page it had not
  touched (gcr logs that the daemon stopped early). With on-demand
  pages, that is for the container's lifetime.

Each page is still a fork's own copy once it arrives; see below for
sharing them.

**The node has to allow it.** The restored tasks open the userfaultfd
themselves, from inside the container's user namespace, and the kernel
allows that unrestricted only to `CAP_SYS_PTRACE` in the initial user
namespace or when `vm.unprivileged_userfaultfd` is 1. The restricted form
(`UFFD_USER_MODE_ONLY`) is not enough, because the kernel also faults a
restored process's pages on its behalf, during a read into one for
instance, and those faults would reach nobody. Without the setting gcr
refuses the container and says so, rather than leaving a CRIU log to read.
Containers without a user namespace (`gcr.dev/host-users=true`) need no
setting: their restored tasks are host root.

Setting it to 1 lets any unprivileged process on the node use userfaultfd
to handle faults from kernel mode, which is a step in several kernel
exploits; it is why gcr's own filter denies `userfaultfd` to containers,
and that still holds with the setting on, since only the restore (before
the filter is in place) uses it. So the exposure is to whatever else runs
on the node, which argues for turning it on where gcr containers are the
only unprivileged workload and leaving it alone elsewhere.

## Shared memory

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
