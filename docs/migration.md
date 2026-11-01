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
| files on volumes | reopened by path. ConfigMaps, Secrets and projected volumes have the same contents there; an `emptyDir`'s files go with the migration, on disk or in memory; other volumes holding a node's own files (`hostPath`) would not, and `kubectl gcr migrate` refuses such pods |
| files deleted while open or mapped | kept: CRIU stores small ones and links back mapped ones |
| pipes, sockets between the container's processes, eventfd, epoll, signalfd, timerfd, memfd, shared memory | restored with their contents |
| stdin, stdout, stderr | the target's: its log carries on |
| listening sockets, UDP sockets | recreated, on the same ports |
| **established TCP connections** | **closed**: the peer sees them reset. The address changes on another node, so they cannot follow (CRIU's `tcp-close`) |
| file locks | moved with their files (CRIU's `file-locks`) |

A migration is refused, and the source carries on, for a Unix socket connected to something outside the container (a host daemon's), io_uring (CRIU does not support it), and inotify or fanotify watches whose files the target cannot find by handle.

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

- **Unreliable on x86 for now.** Post-copy restores the target lazily,
  which on some x86 hosts occasionally leaves it running on wrong memory
  ([lazy restore](fork.md#lazy-restore),
  [#83](https://github.com/makhov/gcr/issues/83)). Until that's fixed, use
  pre-copy (the default) on x86.
- **Two hosts at once.** Until the target has all its memory, the
  container runs on the target with part of its memory on the source. If
  the source, or the connection, goes before then, the target's
  processes get SIGBUS on the next page they lack: the container is lost
  on both. A failure before the target runs leaves the source running,
  as with pre-copy.
- **Lazy restore on the target.** A container in a user namespace is
  restored lazily only where `vm.unprivileged_userfaultfd` is 1
  ([fork](fork.md#lazy-restore) has why); a target without it refuses a
  post-copy migration before the source freezes.
- **No GPUs.** CRIU's CUDA plugin cannot serve GPU memory lazily.
- **No pre-copy rounds yet.** Post-copy starts from a full dump; pages
  the target touches early wait on the network instead of on the freeze.


## Between Kubernetes nodes

`kubectl gcr migrate`, gcr's kubectl plugin (`cmd/kubectl-gcr`), moves a running pod to another node:

```console
$ kubectl gcr migrate pod/counter
asking km-worker-0 for counter's user namespace
target counter-b853d1 created; waiting for it to be scheduled
migrating counter from km-worker-0 to km-worker-1 (http://172.20.0.4:17394)
deleting counter
pod/counter moved to km-worker-1 as pod/counter-b853d1 (downtime 331ms)
```

That run was on a three-node k0da cluster (k0s v1.37, arm64), where a pod counting in memory carried on counting on the other node. The command took 15 s, most of it two Jobs starting; the pod's processes paused for 331 ms.

How it works:
1. **A Job on the pod's node** runs `gcr info` for its user namespace range.
2. **It creates the target:** the pod's spec, named after it, on the `gcr-migrate` runtime class, with a random port and token (`gcr.dev/migrate-in`, `gcr.dev/migrate-token`). The scheduler places it on another node, or `--to NODE` does.
3. **A second Job on the pod's node** runs `gcr migrate` into it. The token reaches the Job through a Secret, not its spec.
4. **On success, the pod is deleted** and the target carries on. On failure, the target is deleted and the pod carries on.

The Jobs run gcr's installer image, privileged, in the node's namespaces; the plugin finds the image from the installer's DaemonSet, or takes `-image`. Running the plugin needs a cluster admin: only `system:masters` and gcr's operator may create pods on `gcr-migrate` (`gcr.yaml`'s `ValidatingAdmissionPolicy`), since its pods make a node's shim listen for a migration.

What can move, for now:
- **Bare pods, a ReplicaSet's (a Deployment's) and a Job's:** the target is handed over to its controller once it is Ready ([gcr-operator](operator.md#deployments-and-jobs)). Pods of other controllers are refused, since those would replace the pod with a cold one. StatefulSets' pods move [through a registry](#through-a-registry) instead.
- **One container** (no sidecars).
- **Volumes the target mounts afresh:** ConfigMaps, Secrets, projected and downward API volumes.
- **`emptyDir` volumes,** on disk or in memory: their files are copied to the target pod's own while the container is frozen, after the writable layer, so they add to the downtime as it does: on the arm64 dev VM, 256 MiB added 0.35 s on disk and 0.2 s in memory, to about 0.3 s without. Files the container has open there are reopened at the same offset.
- **Not other volumes holding files on the node** (`hostPath`, local PersistentVolumes): refused.
- **No user namespace.** A pod with `hostUsers: false` is refused: the kubelet picks its range, and would give the target another one. Pods in gcr's own user namespace (`gcr.dev/host-users: "false"`) can't be checkpointed in Kubernetes yet either. CRIU checks every namespace belongs to the container's user namespace, and the pod's network namespace, made by CRI, belongs to the host's ([#70](https://github.com/makhov/gcr/issues/70)).

The pod gets a new name and IP, and its TCP connections reset. Services select it by its labels, which it keeps.

### Through a registry

`-registry` moves a pod through a checkpoint in a registry instead of from node to node:

```sh
kubectl gcr migrate -registry registry.example:5000/gcr-checkpoints pod/web
```

1. A replacement pod is created and placed by the scheduler. Its container is to start from the checkpoint (`gcr.dev/fork-from`), and its node's shim waits up to 100 s for it to arrive (`gcr.dev/fork-wait`).
2. On the source node, `gcr snapshot -stop` checkpoints the container and stops it, and `gcr snapshot-push` pushes the checkpoint, tagged with the pod's UID.
3. On the replacement's node, `gcr snapshot-pull` fetches it into `/var/lib/gcr/checkpoints`, and the container starts from it, carrying on where the source stopped.
4. The source pod is deleted.

A StatefulSet's pod can't have a replacement made first, since the replacement has the same name. It is checkpointed and stopped, then deleted, and its StatefulSet creates it again. gcr-operator, started with the same `-registry` ([StatefulSets](operator.md#statefulsets)), steers that new pod to restore from the checkpoint. The scheduler places it, and the checkpoint is pulled to its node.

The downtime is seconds rather than a fraction of one: 5.5 to 6.6 s on a three-node k0da cluster (arm64, a small process, a registry on the cluster's network), about half of it the two Jobs starting. But nothing on the source node is needed once the checkpoint is pushed. That's what a node about to go away needs, and a pod that can't run a twin next to itself, such as a StatefulSet's ([gcr-operator](operator.md#statefulsets)).

- **The nodes must reach the registry,** with the credentials in root's Docker config on each (`gcr snapshot-push`). `-plain-http` talks to it without TLS.
- **Bare pods and StatefulSets' pods,** and no `emptyDir` volumes: a checkpoint holds the writable layer, not volumes.
- **A StatefulSet's PersistentVolumeClaims follow the pod,** reattached by Kubernetes on the new node, unless their volumes are tied to a node (`local`, `hostPath`, or a node affinity on `kubernetes.io/hostname`, as local-path's have), which is refused. A bare pod's claims are refused: its replacement waits next to it, and couldn't attach a ReadWriteOnce volume the source holds.
- **No way back once the source has stopped.** If the snapshot fails, the source runs on and the replacement is deleted. After the stop, a bare pod's replacement is left waiting for the checkpoint, and `kubectl gcr` says where it is. A StatefulSet's pod is deleted again, so that its StatefulSet starts it cold, as without gcr.
- **The checkpoint stays in the registry,** and on the replacement's node, where the container's writable layer sits on it for as long as it runs.

## Draining nodes, and node shutdowns

[gcr-operator](operator.md) moves pods that opt in when their node is
drained, or shuts down, rather than restarting them: live, as
`kubectl gcr migrate` does, or through a registry for StatefulSets and
node shutdowns.
