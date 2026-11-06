# Live migration

Moving a running container to another host, without restarting it. The
downtime is a few hundred milliseconds, and on x86 it does not grow much
with the container's memory, because gcr pre-copies while the container
keeps running.

Use it for work that cannot restart: a long computation, a session with
state, a server holding caches it would take minutes to rebuild. In
Kubernetes, gcr-operator does this for you whenever a node is drained
([node maintenance](examples/node-maintenance.md)); what follows is the
mechanism underneath, and how to drive it by hand.

The target is an ordinary gcr container with the same image and command,
created with `gcr.dev/migrate-in` (address to listen on) and
`gcr.dev/migrate-token`. It does not start its process; its `Start` waits for
the source and restores what it receives.

```sh
# target host
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/migrate-in=0.0.0.0:7781 --annotation gcr.dev/migrate-token=$TOKEN \
  IMAGE ID CMD...
# source host
gcr migrate -token $TOKEN ID http://TARGET-HOST:7781      # https:// with MigrationTLS (below)
```

With user namespaces, create the target with the source's range
(`--annotation gcr.dev/userns-base=N`; a refused migration prints N).

When the kernel tracks dirty memory (x86, s390, powerpc; not arm64), the
source pre-copies memory in rounds to CRIU page servers on the target while
it keeps running. It then freezes, sends the remaining memory, the rest of
the checkpoint and the writable layer, and the target restores. The source
stays frozen until the target runs and is thawed if anything fails before;
afterwards it is killed. Open TCP connections reset (see below).

What happens to the processes' open files and sockets:

| | on the target |
| --- | --- |
| files in the container's filesystem | reopened at the same offset: the writable layer moves, and the image is the same |
| files on volumes | reopened by path. ConfigMaps, Secrets and projected volumes have the same contents there; an `emptyDir`'s files go with the migration, on disk or in memory; a ReadWriteMany claim's (NFS, CephFS, a cloud's file share) is mounted by both pods at once, and the source's writes are flushed to it after the freeze; other volumes (`hostPath`, ReadWriteOnce claims) are refused in Kubernetes ([what can move](#between-kubernetes-nodes)) |
| files deleted while open or mapped | kept: CRIU stores small ones and links back mapped ones |
| pipes, sockets between the container's processes, eventfd, epoll, signalfd, timerfd, memfd, shared memory | restored with their contents |
| stdin, stdout, stderr | the target's: its log carries on |
| listening sockets, UDP sockets | recreated, on the same ports |
| **established TCP connections** | **closed**: the peer sees them reset. The address changes on another node, so they cannot follow (CRIU's `tcp-close`) |
| **connections not accepted yet** | **dropped**, the same way: the peer sees them reset (CRIU's `skip-in-flight`) |
| file locks | moved with their files (CRIU's `file-locks`) |

A migration is refused, and the source carries on, for a Unix socket connected to something outside the container (a host daemon's), io_uring (CRIU does not support it), and inotify or fanotify watches whose files the target cannot find by handle.

## Pre-copy

Each round sends the pages written since the round before, while the
container runs: the first, all its memory. Rounds stop after five, or once
one sends less than 10 MiB, or shrinks by less than a tenth from the one
before (the container writes as fast as rounds send). The freeze then
sends only what was written since the last round. `gcr migrate` prints the
pages each round sent.

The target then starts from the images without loading their memory: a
CRIU lazy-pages daemon there loads each page from the images when the
container touches it, and all the others in the background, from its own
disk. The downtime is the freeze, the last round, and a restore of
processes without their memory; it no longer grows with the memory. Unlike
post-copy, the source is not needed once the target runs.

The target restores in full instead (and `gcr migrate` does not say "the
target loads its memory...") where it cannot restore lazily: a container in
a user namespace on a node with `vm.unprivileged_userfaultfd` = 0, or one
with GPUs.

Where the kernel does not track dirty memory, there are no rounds: the
freeze sends all the memory (stop-and-copy), and the target still starts
without loading it. Post-copy (below) is the way to keep that downtime
short.

Everything goes over the one address the target listens on. Its page
servers listen on loopback only, and each round's memory is carried to
them over the migration's connection (an HTTP Upgrade), after the token
is checked. Without TLS that connection is in clear text, memory
included. To encrypt it and have each side verify the other, give both
runtime handlers certificates from one CA, and migrate to `https://`:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr.options.MigrationTLS]
  Cert = '/etc/gcr/tls/node.crt'   # valid for the address sources connect to
  Key = '/etc/gcr/tls/node.key'
  CA = '/etc/gcr/tls/ca.crt'       # the CA both sides trust
```

The target then serves only TLS 1.3, and asks every source for a
certificate signed by the CA. A source with `MigrationTLS` set refuses
`http://` targets. The token still has to match: the certificate says
the source is a node of the cluster, and the token that it is the one
meant to send this container.

## Post-copy

`gcr migrate -post-copy` restores the target before its memory arrives.
The source freezes and sends everything but its memory: CRIU's process
images and the writable layer. The target restores at once, and a CRIU
lazy-pages daemon there fetches each page from the source the moment the
container touches it, and all the others in the background, over the
migration's connection like the rest. Once the target has every page,
CRIU ends the source's processes.

The downtime then no longer grows with the memory, which matters most
where the kernel cannot track dirty memory (arm64), and every migration
is otherwise stop-and-copy. A container counting through its memory,
migrated between two containerd namespaces on one host (arm64 VM):

| memory | stop-and-copy downtime | post-copy downtime | post-copy, memory all at the target |
| --- | --- | --- | --- |
| 256 MiB | 0.49 to 0.54 s | 0.16 to 0.17 s | 0.77 to 0.79 s |
| 1 GiB | 1.36 s | 0.20 s | 1.44 s |

What it costs:

- **Two hosts at once.** Until the target has all its memory, the
  container runs on the target with part of its memory on the source. If
  the source, or the connection, goes before then, the target's
  processes get SIGBUS on the next page they lack: the container is lost
  on both. A failure before the target runs leaves the source running,
  as with pre-copy.
- **Lazy restore on the target.** A container in a user namespace is
  restored lazily only where `vm.unprivileged_userfaultfd` is 1
  ([fork](fork.md#lazy-restore) has why), and nothing is restored lazily
  on a node that is itself a rootless container (a k0da node under
  rootless Podman), which can't open a userfaultfd. Such a target refuses
  a post-copy migration before the source freezes, and restores a
  pre-copy one in full.
- **No GPUs.** CRIU's CUDA plugin cannot serve GPU memory lazily.
- **No pre-copy rounds.** Post-copy starts from a full dump; pages the
  target touches early wait on the network instead of on the freeze.
  Pre-copy (above) doesn't need the source afterwards, but its downtime
  is still longer where the kernel tracks dirty memory: 256 MiB on x86
  CI paused 350 to 380 ms with pre-copy, 205 to 210 ms with post-copy.

In Kubernetes, a [Migration](operations.md#migration) with
`mode: PostCopy` (`kubectl gcr migrate --post-copy`) moves this way. A
drain's migrations stay pre-copy: a node being drained is no node to
depend on until the memory has moved.


## Between Kubernetes nodes

A [Migration](operations.md#migration) moves a running pod to another node; [gcr-operator](operator.md) carries it out. `kubectl gcr migrate`, gcr's kubectl plugin (`cmd/kubectl-gcr`), creates one and waits:

```console
$ kubectl gcr migrate pod/counter
migration/counter-tp8vv created; waiting for gcr-operator
pod/counter moved to km-worker-1 as pod/counter-b853d1 (downtime 331ms)
```

```console
$ kubectl get migrations
NAME            POD       FROM          TO            DOWNTIME   PHASE       AGE
counter-tp8vv   counter   km-worker-0   km-worker-1   331ms      Succeeded   15s
```

That run was on a three-node k0da cluster (k0s v1.37, arm64), where a pod counting in memory carried on counting on the other node. The migration took 15 s, most of it two Jobs starting; the pod's processes paused for 331 ms.

How it works:
1. **A Job on the pod's node** runs `gcr info` for its user namespace range.
2. **It creates the target:** the pod's spec, named after it, on the `gcr-migrate` runtime class, with a random port and token (`gcr.dev/migrate-in`, `gcr.dev/migrate-token`). The scheduler places it on another node, or `--to NODE` does.
3. **A second Job on the pod's node** runs `gcr migrate` into it. The token reaches the Job through a Secret, not its spec.
4. **On success, the pod is deleted** and the target carries on. On failure, the target is deleted and the pod carries on.

gcr-operator waits through the API server's trouble (restarting, overloaded, a dropped connection) rather than failing the migration: a Job that ran on is still followed to its end, and the steps after the processes moved are tried again. If a migration still goes wrong after the processes left the pod, gcr refuses to start them again in it, since they ran elsewhere ("migrated to another node and does not start here again"). gcr-operator then deletes the pod, unless a migration of it is under way, so that its controller starts a new one, cold. A bare pod is left for you to delete.

The Jobs run gcr's installer image, privileged, in the node's namespaces. gcr-operator creates the target: only it and `system:masters` may create pods on `gcr-migrate` (`gcr.yaml`'s `ValidatingAdmissionPolicy`), since its pods make a node's shim listen for a migration. Creating a Migration needs the `gcr-operations` role in the pod's namespace ([who may do what](operations.md#who-may-do-what)).

What can move, for now:
- **Bare pods, a ReplicaSet's (a Deployment's) and a Job's:** the target is handed over to its controller once it is Ready ([gcr-operator](operator.md#deployments-and-jobs)). Pods of other controllers are refused, since those would replace the pod with a cold one. StatefulSets' pods move [through a registry](#through-a-registry) instead.
- **One container moves.** In a pod with several, sidecars included, the annotation `gcr.dev/migrate-container: NAME` (or a Migration's `container`) names the one that moves, and the others start anew in the new pod, as in any pod: a log shipper, a metrics agent or a proxy beside the workload. Without one named, such a pod is refused. On a three-node k0da cluster, a pod with two containers and a sidecar moved twice, 107 and 137 ms, the named container carrying on and the others restarting. What the moving container shares with the others:
  - **TCP over localhost to them** (a proxy): reset, as any connection; it reconnects to the new ones.
  - **A Unix socket to them**: refused, as any socket to outside the container.
  - **An `emptyDir`**: its files are copied in the freeze, while the new pod's other containers may already be writing there.
  - **A shared PID namespace** (`shareProcessNamespace`): refused.

  Through a registry, pods with one container only.
- **Not pinned to its node** by a `nodeSelector` on `kubernetes.io/hostname` (unless `--to` names the node it pins), since the target is made from the pod's spec and could run nowhere else.
- **Volumes the target mounts afresh:** ConfigMaps, Secrets, projected and downward API volumes.
- **`emptyDir` volumes,** on disk or in memory: their files are copied to the target pod's own while the container is frozen, after the writable layer, so they add to the downtime as it does: on the arm64 dev VM, 256 MiB added 0.35 s on disk and 0.2 s in memory, to about 0.3 s without. Files the container has open there are reopened at the same offset.
- **ReadWriteMany claims** (NFS, CephFS, a cloud's file share), not tied to a node: the target mounts the volume while the source still has it, and once the source freezes, its writes still in the node's page cache are flushed (`syncfs`, well under a millisecond on NFS). Files open there reopen at the same offset, and `flock` locks move with them. A file there deleted while still open is refused before the freeze: NFS keeps it as `.nfsXXXX` on the source's node only, and the target can't link it back. On a three-node k0da cluster with an NFS server, a counter writing through one open file moved four times, pre-copy and post-copy, pausing 90 to 207 ms, and its file came out unbroken.
- **No other volumes:** `hostPath` and ReadWriteOnce claims (a cloud disk, Ceph RBD) are refused: only one node can have such a volume at a time, so it would have to be detached and attached again while the pod is frozen. Only a StatefulSet's pod, moving [through a registry](#through-a-registry), takes those along.
- **No user namespace.** A pod with `hostUsers: false` is refused: the kubelet picks its range, and would give the target another one. Pods in gcr's own user namespace (`gcr.dev/host-users: "false"`) can't be checkpointed in Kubernetes yet either. CRIU checks every namespace belongs to the container's user namespace, and the pod's network namespace, made by CRI, belongs to the host's ([#70](https://github.com/makhov/gcr/issues/70)).

The pod gets a new name and IP, and its TCP connections reset. Services select it by its labels, which it keeps.

What the processes see of the move:

- **Clocks carry on.** `CLOCK_MONOTONIC` and `CLOCK_BOOTTIME` continue from where they were, whatever the new node's uptime: CRIU restores the processes into a time namespace offset to their dumped values. Timers and timeouts don't jump; they don't count the pause either. `CLOCK_REALTIME` isn't namespaced: it moves forward by the pause, and by any difference between the nodes' clocks.
- **The new pod's name as the hostname.** `hostname`, `/etc/hostname` and downward API files give the new pod's name. What a process copied at its start keeps the old one: `$HOSTNAME` in its environment, and the pod's name or IP in variables from the downward API.

### Through a registry

`throughRegistry` moves a pod through a checkpoint in gcr-operator's registry (its `-registry`, chart `operator.registry`) instead of from node to node:

```sh
kubectl gcr migrate --through-registry pod/web
```

1. A replacement pod is created and placed by the scheduler. Its container is to start from the checkpoint (`gcr.dev/fork-from`), and its node's shim waits up to 100 s for it to arrive (`gcr.dev/fork-wait`).
2. On the source node, `gcr snapshot -stop` checkpoints the container and stops it, and `gcr snapshot-push` pushes the checkpoint, tagged with the pod's UID.
3. On the replacement's node, `gcr snapshot-pull` fetches it into `/var/lib/gcr/checkpoints`, and the container starts from it, carrying on where the source stopped.
4. The source pod is deleted.

A StatefulSet's pod can't have a replacement made first, since the replacement has the same name. It is checkpointed and stopped, then deleted, and its StatefulSet creates it again. gcr-operator ([StatefulSets](operator.md#statefulsets)) steers that new pod to restore from the checkpoint. The scheduler places it, and the checkpoint is pulled to its node.

The downtime is seconds rather than a fraction of one: 5.5 to 6.6 s on a three-node k0da cluster (arm64, a small process, a registry on the cluster's network), about half of it the two Jobs starting. But nothing on the source node is needed once the checkpoint is pushed. That's what a node about to go away needs, and a pod that can't run a twin next to itself, such as a StatefulSet's ([gcr-operator](operator.md#statefulsets)).

- **The nodes must reach the registry,** with the credentials in root's Docker config on each (`gcr snapshot-push`). `-plain-http` talks to it without TLS.
- **Bare pods and StatefulSets' pods,** and no `emptyDir` volumes: a checkpoint holds the writable layer, not volumes.
- **A StatefulSet's PersistentVolumeClaims follow the pod,** reattached by Kubernetes on the new node, unless their volumes are tied to a node (`local`, `hostPath`, or a node affinity on `kubernetes.io/hostname`, as local-path's have), which is refused. A bare pod's claims are refused: its replacement waits next to it, and couldn't attach a ReadWriteOnce volume the source holds.
- **No way back once the source has stopped.** If the snapshot fails, the source runs on and the replacement is deleted. After the stop, a bare pod's replacement is left waiting for the checkpoint, and the Migration's message says where it is. A StatefulSet's pod is deleted again, so that its StatefulSet starts it cold, as without gcr.
- **The checkpoint stays in the registry,** and on the replacement's node, where the container's writable layer sits on it for as long as it runs.

## Draining nodes, and node shutdowns

[gcr-operator](operator.md) moves pods that opt in when their node is
drained, or shuts down, rather than restarting them: live, as
`kubectl gcr migrate` does, or through a registry for StatefulSets and
node shutdowns.
