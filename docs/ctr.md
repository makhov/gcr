# ctr and containerd

`ctr` ships with containerd and needs no configuration to use gcr: name
the runtime type and that is it. This is the shortest path for trying
things out, and what gcr's own end-to-end tests use.

```sh
ctr image pull docker.io/library/busybox:latest
ctr run -d --runtime io.containerd.gcr.v2 docker.io/library/busybox:latest demo sleep 3600
ctr task ls
ctr task kill -s KILL demo && ctr task rm demo && ctr container rm demo
```

The runtime type (`io.containerd.gcr.v2`) is the shim itself. The named
handlers (`gcr`, `gcr-migrate`) that `gcr setup` registers are for the
CRI, which is what Kubernetes uses; `ctr` goes straight to the shim.

## Snapshots and forks

The `gcr` command takes containerd IDs in the namespace `-n` names
(`default` unless you say otherwise):

```sh
ctr run -d --runtime io.containerd.gcr.v2 docker.io/library/python:3.13-alpine warm \
  python3 -c 'import time; data = list(range(5_000_000)); print("ready", flush=True); time.sleep(1e9)'

gcr snapshot warm /var/lib/gcr/snapshots/warm        # the parent keeps running

ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm \
  docker.io/library/python:3.13-alpine fork-1 true
```

[Fork](fork.md) explains what a fork shares with its snapshot, and
[time travel](time-travel.md) adds `gcr.dev/time-travel` for undo.

## Checkpoint and restore

containerd's own checkpoint and restore work as they do with runc, and
put the result in an image:

```sh
ctr container checkpoint --task --rw demo localhost/demo-ckpt:1
ctr task kill -s KILL demo && ctr task rm demo && ctr container rm demo
ctr container restore --live --rw demo localhost/demo-ckpt:1
```

A checkpoint is a container you can move or keep; a
[snapshot](fork.md) is one you fork from while the original runs. Use
`gcr snapshot-push` to put a snapshot in a registry instead
([moving snapshots](snapshot-registry.md)).

## Handler options per container

Options that a CRI handler would set in containerd's configuration go in
a file you pass per container:

```sh
ctr run --runtime io.containerd.gcr.v2 --runtime-config-path /etc/gcr/gcr.toml \
  IMAGE ID CMD...
```

```toml
# /etc/gcr/gcr.toml
Hardening = 'always'
UserNamespaces = 'always'
```

[Configuration](configuration.md) lists every option. Containers created
through `ctr` are not subject to the restrictions that apply to pod
annotations: whoever can reach containerd's socket is already root.
