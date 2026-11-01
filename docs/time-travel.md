# Time travel

A container on gcr can keep points on a timeline and be put back on any
of them: its processes, what they held in memory, and its writable
filesystem layer. The container never appears to stop, so containerd,
Docker or the kubelet keep seeing one running task.

This is undo for a sandbox. [Agent sandboxes](examples/agent-sandbox.md)
is the use case it was built for.

## Points on request

`gcr.dev/time-travel=manual` takes no points by itself. You ask for one
before anything you might want to take back, and get nothing in between,
which is exactly what an agent's sandbox wants.

```sh
ctr run -d --runtime io.containerd.gcr.v2 --annotation gcr.dev/time-travel=manual IMAGE ID CMD...

gcr point ID "rm -rf /work"      # prints the point, e.g. #4
ctr task exec --exec-id x ID rm -rf /work
gcr rewind ID 4                  # /work is back, and the processes are as they were
```

The container is frozen for the point, not stopped at an instruction, so
a rewind lets every process carry on from where it was.

Two things to know:

- **Take a point with no command running inside.** A command that `exec`
  started is the shim's child, not the container's, and CRIU cannot dump
  it.
- This mode needs no CRIU of gcr's own, unlike `execve` below.

## Points at every exec

`gcr.dev/time-travel=execve` puts a point just after every `execve`, with
the new program stopped before its first instruction, which gives you a
timeline of a shell session or a build without asking for anything:

```sh
ctr run -d --runtime io.containerd.gcr.v2 --annotation gcr.dev/time-travel=execve IMAGE ID CMD...
gcr timeline ID
gcr rewind ID 3
```

- At most one point per `gcr.dev/time-travel-interval` (default `1s`), and
  the last `gcr.dev/time-travel-keep` (default 20) are kept. A handler can
  bound both ([`TimeTravel`](configuration.md#timetravel)).
- An `execve` of a file that does not exist, which is what a shell's
  search of `PATH` produces, is not a point.
- This mode needs gcr's CRIU (`--seccomp-notify-socket`), which a
  release's `gcr setup` installs ([installation](install.md#one-host)).

## What it costs

| | |
| --- | --- |
| Every `exec` in `execve` mode | 45–85 µs more, through gcr's supervisor |
| A full point | the container frozen while its memory is dumped and its writable layer copied: about 90 ms at 64 MiB, 260–390 ms at 1 GiB |
| Disk | that much per point, unless points are incremental (below) |
| The first point and rewind in a container | seconds rather than milliseconds: nothing is cached yet. Later ones settle at about 0.1 s for a small container |

[The numbers in context](tradeoffs.md#checkpoint-restore-and-time-travel).

**Incremental points.** Where the kernel tracks which pages a process
wrote (soft-dirty bits: x86, not arm64), each point dumps only the memory
written since the last one, and a restore reads the rest from the points
before it. A point of a 256 MiB container that wrote little since the
last is a few hundred KiB against 257 MiB for a full one.

Every eighth point is full again, so old points can be deleted a chain at
a time; between `keep` and `keep` + 7 are kept. A full point also follows
any other dump of the container (a snapshot, a checkpoint, a migration)
and any rewind, which leave nothing to count from.

## Where it does not work

- **CRI-O**, which knows a container by its init's pid and start time: a
  rewind brings in another process, so CRI-O would take the container for
  gone ([CRI-O](crio.md)).
- **Docker containers on a bridge network.** A rewind restores the
  processes' network as it was, which works only where Docker set none
  up: `--network none` or `--network host`
  ([Docker networking](docker.md#networking)).
