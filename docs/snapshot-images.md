# Snapshots as images

A running pod's container can be pushed to a registry as an ordinary
container image. A pod that runs the image carries on from where the
container was, memory and all: nothing to pull by hand, no annotation,
no special RuntimeClass.

```console
$ kubectl gcr snapshot pod/counter registry.example.com/snapshots/counter:v1
snapshotting counter/c on worker-1, into registry.example.com/snapshots/counter:v1
registry.example.com/snapshots/counter@sha256:5131e882dfe8c88d17f895ef8dc5b693f0e30f28e4cc9e0936943c2322c86d61
```

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: counter-copy
spec:
  replicas: 2
  selector: {matchLabels: {app: counter-copy}}
  template:
    metadata: {labels: {app: counter-copy}}
    spec:
      runtimeClassName: gcr
      containers:
        - name: c
          image: registry.example.com/snapshots/counter:v1
```

Each replica resumes the processes as they were at the snapshot, with
the files they had written. The pod that was snapshotted keeps running:
it is paused for as long as the dump takes (a few seconds for a small
process).

## Allowing it

The nodes restore only images that match a list in gcr's configuration,
which is empty by default. With the Helm chart:

```sh
helm upgrade gcr oci://ghcr.io/makhov/charts/gcr --reuse-values \
  --set 'runtime.snapshotImages={registry.example.com/snapshots/*}'
```

or `gcr setup -snapshot-images=registry.example.com/snapshots/*` on each
node, which writes the gcr handler's `SnapshotImages`
([configuration](configuration.md#snapshotimages)). An entry is an
image reference, a prefix ending in `*`, or `*` for any image.

Why a list: a normal image's code runs inside the container, within
what the pod's spec allows. A snapshot's processes are not run but
restored: CRIU, as root on the node, recreates them from the state the
image holds, before the container's limits apply. CRIU does not guard
against made-up state, so a pod author who could run any image made to
look like a snapshot could reach the node. Keep the list to the
registries your snapshots go to, which only those you trust can push
to. A cluster where everyone who creates pods is trusted with the nodes
anyway can use `*`.

Whatever the list, gcr checks each restored thread against the pod's
spec before it runs: no capability outside the container's bounding set,
`no_new_privs` where the spec sets it (`allowPrivilegeEscalation:
false`), no root IDs for a container running as another user that may
not change its IDs, and a seccomp filter where the spec has one. A pod
that drops capabilities the snapshot's processes had is refused:

```
restore: core-1.img: a thread's effective capabilities hold CAP_CHOWN, ... which the container's bounding set does not
```

What a seccomp filter allows is not compared with the pod's profile.

## What the image is

The container's own image, with two layers on top:

- its writable layer at the snapshot, as an ordinary layer;
- its state: the processes' memory and the rest of CRIU's images, in
  `/.gcr-snapshot`, which the restored container does not see.

The config is the original image's, labelled `dev.gcr.snapshot=v1`, so
it runs anywhere as an image too: on a node without gcr, or with
another runtime, the entrypoint starts afresh over the snapshot's files.

The original image's layers are copied from containerd's content store
on the node, so the registry holds the whole image, and nodes that have
the original image pull only the two new layers. A registry that already
has a layer is not sent it again.

## What carries over

- **Memory, processes, open files,** and the files the container wrote.
- **Not its network identity:** the new pod has its own IP, and TCP
  connections the snapshot had are reset.
- **Its hostname is the new pod's,** not the one it was taken in.
- **Volumes** are the new pod's, and not in the image: a process that
  had a file open on one needs that file there when it is restored.

## Requirements and limits

- The node restoring it needs the same architecture (checked), and a CPU
  with the features the processes used (CRIU checks).
- The snapshot is taken on the pod's node by a Job in `gcr-system`, which
  pushes with the node's credentials: root's Docker config there (`sudo
  docker login`). Pods pull it with their usual `imagePullSecrets`.
- `--plain-http` for a registry without TLS.
- **Not yet:** pods in gcr's own user namespace
  (`gcr.dev/host-users: "false"`); a container with a terminal; CRI-O
  and Docker (they mount the image themselves); lazy pulls.

On a node without Kubernetes, `gcr snapshot-image ID REF` does the same
for a container containerd runs ([the `gcr` command](cli.md)).
