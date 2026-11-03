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

`kubectl gcr snapshot` creates a [Snapshot](operations.md#snapshot),
which [gcr-operator](operator.md) carries out. Without the plugin, create
it yourself, from a pipeline for instance:

```yaml
apiVersion: gcr.dev/v1alpha1
kind: Snapshot
metadata:
  name: counter-v1
spec:
  pod: counter
  image: registry.example.com/snapshots/counter:v1
  pushSecret: registry-push   # a docker-registry Secret; without, the node's credentials
```

```sh
kubectl apply -f snapshot.yaml
kubectl wait --for=jsonpath='{.status.phase}'=Succeeded snapshot/counter-v1
kubectl get snapshot counter-v1 -o jsonpath='{.status.image}'   # the image, by digest
```

**Or let the cluster decide where snapshots go.** Configured with a
registry for them
([a registry for snapshots](operator.md#a-registry-for-snapshots)),
gcr-operator names the image itself — `REGISTRY/NAMESPACE/POD`, tagged
with the Snapshot's name — and a Snapshot is a pod's name and nothing
else:

```yaml
apiVersion: gcr.dev/v1alpha1
kind: Snapshot
metadata:
  name: counter-v1
spec:
  pod: counter
```

`kubectl gcr snapshot pod/counter` does the same without the file, with
the API server naming the Snapshot, and so the tag: write the resource
yourself for a tag you choose. Read `status.image` for what was pushed,
by digest, whichever way you name it; that digest is what a pod should
run.

## Building one in CI

`gcr snapshot-build` makes the same image on any host with containerd
and gcr, outside Kubernetes: it runs the image as a pod's container
would run, waits until it is ready, snapshots it and pushes it.

```sh
sudo gcr snapshot-build -ready 'wget -qO- localhost:8000/health' \
  registry.example.com/app:v1 registry.example.com/snapshots/app:v1
```

Pods then run `registry.example.com/snapshots/app:v1` like any image,
and start warm.

- **`-ready CMD`** runs in the container, through `/bin/sh -c`, every
  second until it exits 0 (`-timeout`, 10 minutes by default). `-wait`
  waits longer after that. Without either, the snapshot is taken as soon
  as it starts.
- **`-- COMMAND...`** after the two references replaces the image's
  command, as a pod's `command` does.
- **The container is set up like a pod's,** so that it restores into
  one: its network, IPC and hostname belong to a sandbox it joins, and
  `/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf`, `/dev/shm`,
  `/dev/termination-log` and a service account's token are mounted from
  outside, as the kubelet mounts them. The restored processes get their
  pod's.
- **It has no network but its loopback** while it warms up: download
  what it needs into the image beforehand. (Not the host's: CRIU would
  take that for its own and leave a restored pod in the node's network.
  A restore refuses a snapshot taken in the host's network.)
- **It must be run with the pod's mounts.** A restore compares the
  snapshot's bind mounts with the pod's and refuses a mismatch, naming
  them:

  ```
  restore: this container mounts /data, and the snapshot's did not: a snapshot restores only into a container with the same mounts
  ```

  So say what the pod mounts: `-volume PATH` for each volume mount,
  `-no-service-account` for `automountServiceAccountToken: false`. A
  volume starts empty, in the build as in the pod.
- **And with its privileges:** `-user UID[:GID]` for `runAsUser`, and
  `-no-new-privileges` for `allowPrivilegeEscalation: false`, which a
  restore checks ([allowing it](#allowing-it)).
- **`-device nvidia.com/gpu=0`** gives it a GPU through CDI, as the
  device plugin gives a pod one: a snapshot of a container using a GPU
  has to be taken with the GPU, and the host needs a CDI spec
  (`nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`). Repeat it
  for more than one. What the nodes restoring it need, and the index it
  has to land on, are in [GPUs](gpus.md#in-kubernetes).
- `-env KEY=VALUE` adds to the image's environment; the restored
  processes keep the environment they had, not the pod's.
- `-address` is containerd's socket (`/run/containerd/containerd.sock`).
  Credentials, to pull the image and push the snapshot, are root's
  Docker config's; `-plain-http` for registries without TLS.

The host needs what gcr needs (Linux 6.12 or later, cgroup v2), which
GitHub's hosted Ubuntu runners do not have yet: use a self-hosted
runner, or a VM, with a newer kernel. And the nodes that run the image
need a CPU with the features the processes used on the CI host (CRIU
checks): build on the same kind of machine the pods run on.

In a GitHub Actions workflow, on such a runner:

```yaml
jobs:
  warm-image:
    runs-on: [self-hosted, linux, x64]
    steps:
      - name: Install gcr
        run: |
          curl -sSfL https://raw.githubusercontent.com/makhov/gcr/main/install.sh | sh
          sudo gcr setup
      - name: Log in
        run: echo "${{ secrets.GITHUB_TOKEN }}" | sudo docker login ghcr.io -u "${{ github.actor }}" --password-stdin
      - name: Snapshot
        run: |
          sudo gcr snapshot-build -ready 'wget -qO- localhost:8000/health' \
            ghcr.io/${{ github.repository }}:${{ github.sha }} \
            ghcr.io/${{ github.repository }}/warm:${{ github.sha }}
```

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
not change its IDs. A pod that drops capabilities the snapshot's
processes had is refused:

```
restore: core-1.img: a thread's effective capabilities hold CAP_CHOWN, ... which the container's bounding set does not
```

Seccomp filters are not checked: the snapshot's own are restored.

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
- gcr-operator takes the snapshot on the pod's node, in a Job in
  `gcr-system`. It pushes with the Snapshot's `pushSecret`
  (`--push-secret`), or else the node's credentials: root's Docker config
  there (`sudo docker login`). Pods pull it with their usual
  `imagePullSecrets`.
- `plainHTTP` (`--plain-http`) for a registry without TLS.
- **Not yet:** pods in gcr's own user namespace
  (`gcr.dev/host-users: "false"`); a container with a terminal; CRI-O
  and Docker (they mount the image themselves); lazy pulls.

On a node without Kubernetes, `gcr snapshot-image ID REF` does the same
for a container containerd runs ([the `gcr` command](cli.md)).
