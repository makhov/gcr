# A warm pool across the fleet

Build the warm state once, put it in a registry, and let every node fork
from it. Nothing has to be loaded, compiled or imported twice.

## Build it

On one host, run the container, wait until it is ready, snapshot it, and
push:

```sh
ctr run -d --runtime io.containerd.gcr.v2 IMAGE warm CMD...
# ... wait until it is warm ...

gcr snapshot warm /var/lib/gcr/snapshots/warm
gcr snapshot-push /var/lib/gcr/snapshots/warm registry.example.com/snapshots/warm:v1
```

A snapshot is one OCI artifact: the process images plus the writable
layers a fork mounts. Any registry takes it, including one backed by S3,
ECR, GHCR, Harbor or Zot. Layers shared between snapshots are stored and
uploaded once.

## Use it everywhere

On each node:

```sh
gcr snapshot-pull registry.example.com/snapshots/warm:v1 /var/lib/gcr/snapshots/warm

ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm \
  IMAGE worker-1 true
```

The node needs the image the snapshot came from, the same architecture
(gcr checks), and a CPU with the features those processes were using
(CRIU checks). Credentials come from `docker login`.

For Kubernetes, pods ask for a fork with the same annotation, and the
handler has to allow it — otherwise any pod author could point gcr at any
directory on the node:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-forks]
  runtime_type = 'io.containerd.gcr.v2'
  pod_annotations = ['gcr.dev/*']
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-forks.options]
    PodAnnotations = ['gcr.dev/fork-from']
    SnapshotRoots = ['/var/lib/gcr/snapshots', '/var/lib/gcr/layers']
```

Give that handler a RuntimeClass and the pods pick it with
`runtimeClassName: gcr-forks` ([configuration](../configuration.md#podannotations),
[Kubernetes](../kubernetes.md)).

## Pull it before you need it

A pull reads the whole snapshot, which is the slow part. Two ways to get
it off the critical path:

- **Pull it when the node joins**, from a DaemonSet or a node startup
  script, so the first fork is local already.
- **`gcr snapshot-pull -lazy`** fetches the metadata and reads the rest
  as the forks touch it, where the registry supports it
  ([formats](../snapshot-registry.md#formats)).

## What to watch

- **User namespace ranges travel with the snapshot.** Its files carry
  host IDs from the range its container had. A pull keeps that range if
  it is free on the target, and otherwise moves the snapshot into a free
  one, rewriting owners, xattrs and the maps in the CRIU images. It says
  when it did, and that costs a pass over the files.
- **The layer store is shared.** Layers under a pulled snapshot go to
  `/var/lib/gcr/layers` (`-store`), counted exactly like local snapshots,
  so `gcr snapshot-rm` refuses one still in use.
- **References are per host.** With snapshots on shared storage, check
  each host before deleting one.

[Moving snapshots between hosts](../snapshot-registry.md) is the
reference, with the formats and an S3-backed registry's configuration.
