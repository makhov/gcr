# The gcr command

`gcr` does what containerd's API has no call for. Commands that act on a
container reach its shim over the container's control socket
(`/run/gcr-ctl/`), so they run as root on the node the container runs on.
Containers are named by their containerd ID, in the namespace `-n` gives
(default `default`; Kubernetes pods are in `k8s.io`).

```
gcr [-n namespace] timeline ID
gcr [-n namespace] rewind ID POINT
gcr [-n namespace] point ID [LABEL]
gcr [-n namespace] snapshot ID DIR
gcr snapshot-users DIR
gcr snapshot-rm DIR...
gcr snapshot-push [-plain-http] [-format tar|nydus] DIR REF
gcr snapshot-pull [-plain-http] [-store DIR] [-lazy] REF DIR
gcr [-n namespace] migrate [-token TOKEN] ID TARGET-URL
```

Errors go to standard error with `gcr:` in front, and the exit status is
1 (2 for a usage error).

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

### `gcr snapshot ID DIR`

Writes the running container's processes and writable layer to `DIR`,
and leaves it running. `DIR` must be empty or absent. Containers created
with `gcr.dev/fork-from=DIR` start from it ([fork](fork.md)).

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

### `gcr migrate [-token TOKEN] ID TARGET-URL`

Moves a running container to a target container created with
`gcr.dev/migrate-in` ([live migration](migration.md)). `TARGET-URL` is
`http://host:port`, or `https://host:port` when the handler has
`MigrationTLS`.

- `-token TOKEN`: the target's `gcr.dev/migrate-token`. Defaults to
  `$GCR_MIGRATE_TOKEN`, which keeps it out of the process list.

It prints the pre-copy rounds and the pages each sent (or that the kernel
cannot track dirty memory, and the migration was stop-and-copy), and the
downtime. If a pre-copy round fails, the migration goes on stop-and-copy
and says why.

```
$ GCR_MIGRATE_TOKEN=... gcr migrate demo https://node-b:7781
pre-copy rounds: 3 (pages [65536 812 40])
downtime: 212ms
```
