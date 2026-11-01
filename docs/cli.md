# The gcr command

`gcr` does what containerd's API has no call for. Commands that act on a
container reach its shim over the container's control socket
(`/run/gcr-ctl/`), so they run as root on the node the container runs on.
Containers are named by their containerd ID, in the namespace `-n` gives
(default `default`; Kubernetes pods are in `k8s.io`).

```
gcr setup [-docker] [-no-restart] [-bin-dir DIR] [-containerd-config FILE] [-containerd-unit UNIT]
gcr doctor
gcr kube-label NODE KEY=VALUE|KEY-
gcr version
gcr [-n namespace] timeline ID
gcr [-n namespace] rewind ID POINT
gcr [-n namespace] point ID [LABEL]
gcr [-n namespace] snapshot [-stop] ID DIR
gcr snapshot-users DIR
gcr snapshot-rm DIR...
gcr snapshot-push [-plain-http] [-format tar|nydus] DIR REF
gcr snapshot-pull [-plain-http] [-store DIR] [-lazy] REF DIR
gcr [-n namespace] migrate [-token TOKEN] [-post-copy] ID TARGET-URL
```

Errors go to standard error with `gcr:` in front, and the exit status is
1 (2 for a usage error).

## Setting up a node

### `gcr setup`

Installs gcr on the node and registers the runtime
([install](install.md#on-a-host)): `gcr` and the shim into
`-bin-dir` (`/usr/local/bin`), and, from a release, gcr's CRIU and its CUDA
plugin into `/var/lib/gcr`; a containerd drop-in in `conf.d` beside
`-containerd-config`, in the configuration's own format version, which it
makes containerd import; with `-docker`, runtime `gcr` in
`/etc/docker/daemon.json`. It restarts what it reconfigured (`-containerd-unit`,
default `containerd`) unless `-no-restart`, and if containerd does not
come back with the new configuration, puts the old one back. On k0s it
writes `/etc/k0s/containerd.d/gcr.toml` instead, which k0s applies itself.
It ends with `gcr doctor`. Running it again is safe, and with a newer `gcr`
upgrades.

### `gcr doctor`

Checks the node: the kernel, cgroup v2, the shim and its version,
containerd's and Docker's registration, CRIU (and whether it is gcr's),
Landlock, the netlink policy (BPF LSM), lazy restore, dirty memory
tracking, memory merging, nydus, GPUs. Each line is `ok`, `off` (a feature
this node does not have) or `MISSING` (gcr does not work as it should),
with what to do about it. It exits non-zero if anything is missing.

### `gcr kube-label NODE KEY=VALUE|KEY-`

Sets or removes (`KEY-`) a label on a node, as the pod's service account:
the installer DaemonSet marks its node `gcr.dev/runtime=ready` with it.

### `gcr version`

Prints gcr's version and, for a release, what it carries.

## Time travel

### `gcr timeline ID`

Lists the points on a container's timeline, newest first, with the call
each was taken before, or its label. The container must have been
started with `gcr.dev/time-travel` ([time travel](time-travel.md)).

```
$ gcr timeline demo
POINT  TIME          BEFORE
#3     14:02:11.403  execve("/usr/bin/python3", ["python3", "train.py"])
#2     14:02:09.118  execve("/bin/sh", ["sh", "-c", "pip install ..."])
#1     14:02:07.950  execve("/bin/sh", ["sh"])
```

### `gcr point ID [LABEL]`

Takes a point on the container's timeline now, with the container frozen,
and prints it. `LABEL` says what it comes before (`gcr timeline` shows
it). The container must have time travel on; with
`gcr.dev/time-travel=manual`, this is the only way points are taken
([points on request](time-travel.md#points-on-request)).

### `gcr rewind ID POINT`

Replaces the container's processes and writable layer with those of a
point (`3` or `#3`), and continues the program that point stopped
before its first instruction (or, for a point taken on request, every
process from where it was). The container stays the same task to
containerd. The timeline keeps its points, later ones included, so a
rewind can be undone by rewinding forward; new points are added after
them.

## Snapshots and forks

### `gcr snapshot [-stop] ID DIR`

Writes the running container's processes and writable layer to `DIR`,
and leaves it running. `DIR` must be empty or absent. Containers created
with `gcr.dev/fork-from=DIR` start from it ([fork](fork.md)).

With `-stop`, the container stops once the snapshot is written: its
processes are killed while still frozen, so nothing they do comes after
what the snapshot holds, and a container started from it carries on from
there. It's a checkpoint to move a container by, on this node or, pushed
with `gcr snapshot-push`, on another. If the snapshot fails, the container
runs on. A Kubernetes pod's container stopped this way isn't restarted on
this node by its kubelet: it goes on elsewhere.

### `gcr snapshot-users DIR`

Lists what uses a snapshot: the containers forked from it, and the
snapshots taken of those.

### `gcr snapshot-rm DIR...`

Removes snapshots, and lets go of the user namespace ranges they held.
Refuses a snapshot something still uses, and a directory that does not
look like a snapshot.

### `gcr snapshot-push [-plain-http] [-format tar|nydus] DIR REF`

Pushes a snapshot to an OCI registry as one artifact, and prints its
digest ([moving snapshots](snapshot-registry.md)). `REF` is like
`registry.example.com/snapshots/warm:v1`; the tag defaults to `latest`.
Credentials come from Docker's config (`docker login`).

- `-plain-http`: speak HTTP to the registry. Registries on loopback
  always get HTTP.
- `-format`: `tar` (the default) or `nydus`
  ([formats](snapshot-registry.md#formats)).

### `gcr snapshot-pull [-plain-http] [-store DIR] [-lazy] REF DIR`

Pulls a snapshot to `DIR`, and the layers beneath it to the store, in
whichever format it was pushed. If the
snapshot's user namespace range is in use on this node, it is moved to a
free one and `gcr` says so.

- `-store DIR`: where the layers beneath pulled snapshots go, shared by
  every snapshot pulled onto the node. Default `/var/lib/gcr/layers`.
  Include it in the handler's `SnapshotRoots` if pods fork from pulled
  snapshots.
- `-plain-http`: as for `snapshot-push`.
- `-lazy`: for a snapshot pushed with `-format nydus`, mount its parts
  instead of downloading them ([lazy pulls](snapshot-registry.md#lazy-pulls)).

## Live migration

### `gcr migrate [-token TOKEN] [-post-copy] ID TARGET-URL`

Moves a running container to a target container created with
`gcr.dev/migrate-in` ([live migration](migration.md)). `TARGET-URL` is
`http://host:port`, or `https://host:port` when the handler has
`MigrationTLS`.

- `-token TOKEN`: the target's `gcr.dev/migrate-token`. Defaults to
  `$GCR_MIGRATE_TOKEN`, which keeps it out of the process list.
- `-post-copy`: restore the target before its memory arrives; it fetches
  the memory from the source as it runs ([post-copy](migration.md#post-copy)).
  Prints the downtime and when the target had all its memory.

It prints the pre-copy rounds and the pages each sent (or that the kernel
cannot track dirty memory, and the migration was stop-and-copy), and the
downtime. If a pre-copy round fails, the migration goes on stop-and-copy
and says why.

```
$ GCR_MIGRATE_TOKEN=... gcr migrate demo https://node-b:7781
pre-copy rounds: 3 (pages [65536 812 40])
downtime: 212ms
```

### `gcr shutdown-guard [-plain-http] [-wait DURATION] REPOSITORY`

Runs on a Kubernetes node, as the systemd unit
`deploy/gcr-shutdown-guard.service`. When the node shuts down, the pods
that opted in to moving (`gcr.dev/migrate-on-evict: "true"`) are
checkpointed as the kubelet stops them. Each checkpoint is pushed to
`REPOSITORY`, tagged with its pod's UID, for gcr-operator to restore
elsewhere ([node shutdown](operator.md#node-shutdown)).

It holds a systemd-logind delay lock on shutdowns. When a shutdown starts,
it marks the node as shutting down (`/run/gcr/shutdown`), and pushes each
checkpoint as the shim writes it. It releases the lock once the kubelet has
released its own (it has stopped every pod) and every checkpoint is pushed
or has failed.

- `-wait DURATION`: the longest it holds a shutdown (80 s). logind's
  `InhibitDelayMaxSec` bounds it too, and must be at least as long.
- `-plain-http`: speak HTTP to the registry.
