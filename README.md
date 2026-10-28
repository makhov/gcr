# gcr

A containerd runtime (`io.containerd.gcr.v2`) with checkpoint, restore and fork
built in. Containers run in-process through libcontainer; no runc binary.
`gcr` is a placeholder name.

Design: https://claude.ai/artifact/MKbmAZNzEyYc9QFnoeoNbA

## Status

- Task API: create, start, state, kill, wait, delete, pids, pause, resume, checkpoint, restore.
- Not yet: exec, terminals, stats, update, user namespaces by default, GPUs.

## Develop

Everything builds and runs in a Linux VM (kernel 6.12+, containerd 2.4, CRIU):

```sh
limactl start --name=gcr hack/lima.yaml   # once
make vm T=test-e2e                        # build, install, run the checkpoint/restore test
```

`hack/e2e.sh` runs a counter, checkpoints it with `ctr`, restores it, and checks
the counter continues instead of restarting.
