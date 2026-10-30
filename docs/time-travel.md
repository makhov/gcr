# Time travel

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
keeps seeing one running task.

What it costs ([tradeoffs](tradeoffs.md#checkpoint-restore-and-time-travel) has the numbers): every exec goes
through gcr's supervisor (45–85 µs more), and a full point freezes the
container while its memory is dumped and its writable layer copied
(about 90 ms at 64 MiB of memory, 260–390 ms at 1 GiB), and takes that
much disk.

Where the kernel tracks which pages a process wrote (soft-dirty bits: x86,
not arm64), points are incremental: each dumps only the memory written
since the last, and a restore reads the rest from the points before it
(CRIU's parent images). Every eighth point is full again, so old points
can be deleted a chain at a time; between `keep` and `keep` + 7 are kept.
A full point follows any other dump of the container (a snapshot, a
checkpoint, a migration) and any rewind, which leave nothing to count
from. A point of a 256 MiB container that wrote little since the last is
a few hundred KiB, against 257 MiB for a full one.

It needs CRIU with `--seccomp-notify-socket`
([makhov/criu#1](https://github.com/makhov/criu/pull/1)) installed as `criu`.
