# gcr

A containerd runtime (`io.containerd.gcr.v2`) with checkpoint, restore and fork
built in. Containers run in-process through libcontainer; no runc binary.
`gcr` is a placeholder name.

Design: https://claude.ai/artifact/MKbmAZNzEyYc9QFnoeoNbA

## Status

- Task API: create, start, state, kill, wait, delete, pids, pause, resume, checkpoint, restore.
- Time travel (below).
- Not yet: exec, terminals, stats, update, user namespaces by default, GPUs.

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

## Develop

Everything builds and runs in a Linux VM (kernel 6.12+, containerd 2.4, CRIU):

```sh
limactl start --name=gcr hack/lima.yaml   # once
make vm T=criu                            # once: build and install the CRIU fork from ../criu
make vm T=test-e2e                        # build, install, run the end-to-end tests
```

`hack/e2e.sh` checkpoints and restores a counter with `ctr`;
`hack/e2e-timetravel.sh` records a timeline, rewinds it, and checks the
counter, the filesystem and the recording all carry on from the point.
