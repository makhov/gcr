# Operations as resources

Everything gcr does to a running pod on Kubernetes is a resource in the
`gcr.dev/v1alpha1` API, which [gcr-operator](operator.md) carries out:

| Kind | What it does |
|------|--------------|
| [`Snapshot`](#snapshot) | Pushes a pod's container to a registry as an image that pods run, carrying on from where it was ([snapshots as images](snapshot-images.md)). |
| [`Migration`](#migration) | Moves a pod to another node, its processes running on ([live migration](migration.md#between-kubernetes-nodes)). |
| [`TimePoint`](#timepoint) | Takes a point on a container's timeline ([time travel](time-travel.md)). |
| [`Rewind`](#rewind) | Takes a container back to a point on its timeline. |

Create one with `kubectl apply`, from a pipeline, a controller or
GitOps, and read the result in its status. [`kubectl gcr`](#kubectl-gcr)
creates them for you and waits.

```yaml
apiVersion: gcr.dev/v1alpha1
kind: Snapshot
metadata:
  name: counter-v1
spec:
  pod: counter
  image: registry.example.com/snapshots/counter:v1
```

```console
$ kubectl apply -f snapshot.yaml
snapshot.gcr.dev/counter-v1 created
$ kubectl wait --for=jsonpath='{.status.phase}'=Succeeded snapshot/counter-v1
snapshot.gcr.dev/counter-v1 condition met
$ kubectl get snapshots
NAME         POD       IMAGE                                                         PHASE       AGE
counter-v1   counter   registry.example.com/snapshots/counter@sha256:5131e882dfe8…   Succeeded   6s
```

`kubectl get gcr` lists all four kinds.

## How they run

Each operation is in the namespace of the pod it is on, which
`spec.pod` names, and is carried out once:

- **New:** no `status.phase`.
- **`Running`:** gcr-operator has picked it up. `status.node` is the
  pod's node, and `status.startTime` when it started.
- **`Succeeded` or `Failed`,** with `status.completionTime`. A failed
  one says why in `status.message`.

The spec can't be changed once created: to do it again, create another.
Deleting one doesn't undo it, and deleting a running one doesn't stop it.
Finished operations stay until deleted.

If gcr-operator restarts while one runs, the operation is marked `Failed`
("gcr-operator restarted while it ran") rather than run again: it may
have been done, or half done. Check the pod, then create another if needed.
An answer from the API server lost as gcr-operator marks one `Running`
isn't taken for that: it reads the operation back and goes ahead.

gcr-operator does the work on the pod's node, in a short-lived
privileged Job in `gcr-system`, running `gcr` there (`gcr snapshot-image`,
`gcr migrate`, `gcr point`, `gcr rewind`), as the plugin did before these
resources.

## Snapshot

```yaml
apiVersion: gcr.dev/v1alpha1
kind: Snapshot
metadata:
  name: counter-v1
spec:
  pod: counter
  container: c            # may be left out in a pod with one container
  image: registry.example.com/snapshots/counter:v1   # optional, see below
  registry: registry.example.com/snapshots           # optional, see below
  pushSecret: registry-push   # optional, see below
  plainHTTP: false        # true for a registry without TLS
status:
  phase: Succeeded
  node: worker-1
  image: registry.example.com/snapshots/counter@sha256:5131e882…
```

`status.image` is the pushed image by digest; pods run it on the `gcr`
RuntimeClass ([snapshots as images](snapshot-images.md)). The pod keeps
running: it is paused while it is dumped, typically a few seconds.

**Where the image goes.** A cluster usually has one registry for
snapshots, configured on the operator
([a registry for snapshots](operator.md#a-registry-for-snapshots)), and
then a Snapshot names nothing but its pod:

| The spec says | The image is |
| --- | --- |
| `image` | that, exactly. |
| `registry`, no `image` | `REGISTRY/NAMESPACE/POD`, tagged with the Snapshot's name. |
| neither | the same, in the operator's registry (`-snapshot-registry`). |

The tag is the Snapshot's own name, so a Snapshot you write has the tag
you give it; `kubectl gcr` lets the API server generate the name
(`counter-mz7qd`), and the tag is that.

With neither, and no registry on the operator, the Snapshot fails saying
so — nothing is pushed anywhere by accident. `status.image` is the digest
of what was pushed in every case, so read the image from there rather
than rebuilding the name.

**Credentials.** `pushSecret` names a `kubernetes.io/dockerconfigjson`
Secret in the Snapshot's namespace:

```sh
kubectl create secret docker-registry registry-push \
  --docker-server=registry.example.com --docker-username=… --docker-password=…
```

gcr-operator passes the Secret to gcr on the node for this push only
(through a Secret in `gcr-system`, deleted afterwards). Without
`pushSecret`, the push uses the node's own credentials (root's
`docker login` there). Tenants should push with their own credentials:
give `pushSecret`, and keep the nodes' credentials for registries only
admins push to.

## Migration

```yaml
apiVersion: gcr.dev/v1alpha1
kind: Migration
metadata:
  name: counter-to-worker-2
spec:
  pod: counter
  node: worker-2            # optional: by default, the scheduler picks one other than its own
  mode: PreCopy             # optional: PreCopy (the default) or PostCopy
  container: app            # optional: of a pod with several, the one that moves (the others start anew)
  throughRegistry: false    # optional
status:
  phase: Succeeded
  node: worker-1
  targetNode: worker-2
  target: counter-b853d1
  downtime: 331ms
  memoryMoved: 903ms        # PostCopy only: when the last of its memory arrived
```

The pod moves node to node, with a pause well under a second, and gets
a new name: `status.target`. `throughRegistry` moves it through a
checkpoint in gcr-operator's registry (its `-registry`, chart
`operator.registry`) instead: seconds of downtime, but nothing is needed
from the source node once the checkpoint is pushed. A StatefulSet's pod
always moves this way, and keeps its name. What can move:
[live migration](migration.md#between-kubernetes-nodes).

`mode` is when the memory moves:

- **`PreCopy`** copies it while the pod runs, then freezes the pod for
  what changed meanwhile. If anything fails before the target runs, the
  pod carries on where it was. Where the kernel can't track which pages
  changed (arm64), all of it moves during the freeze.
- **`PostCopy`** runs the target first, and its memory follows, each page
  as it's touched and the rest in the background
  ([post-copy](migration.md#post-copy)). The pause is the shortest, and
  doesn't grow with the memory. But until `memoryMoved`, the pod needs
  both nodes and the connection between them, and is lost if one goes. Not
  with `throughRegistry`, GPUs, or a StatefulSet's pod.

A pod being moved by a Migration isn't moved again by a drain meanwhile:
the drain waits, as for the moves gcr-operator starts itself.

## TimePoint

On a pod whose container has time travel on (annotation
`gcr.dev/time-travel: manual` or `execve`, [time travel](time-travel.md)):

```yaml
apiVersion: gcr.dev/v1alpha1
kind: TimePoint
metadata:
  name: before-migration
spec:
  pod: sandbox
  container: c        # may be left out in a pod with one container
  label: before the schema migration
status:
  phase: Succeeded
  point: 4
```

`status.point` is the point's number, for a Rewind.

## Rewind

```yaml
apiVersion: gcr.dev/v1alpha1
kind: Rewind
metadata:
  name: undo-migration
spec:
  pod: sandbox
  point: 4
```

The container's processes and writable layer are put back as they were
at the point, in place: the pod and its IP stay. Points gcr took itself (`execve`) count as well: list them
with `gcr timeline` on the node.

## kubectl gcr

The plugin ([install](install.md#the-kubectl-plugin)) creates one
operation, named after the pod, waits for it and prints the result:

| Command | Creates |
|---------|---------|
| `kubectl gcr snapshot [-c C] [--registry REPO] [--push-secret S] [--plain-http] pod/P [IMAGE]` | a Snapshot; prints the image by digest. Without IMAGE it goes to the operator's registry for snapshots |
| `kubectl gcr migrate [--to NODE] [--through-registry] pod/P` | a Migration |
| `kubectl gcr point [-c C] pod/P [LABEL]` | a TimePoint; prints `#N` |
| `kubectl gcr rewind [-c C] pod/P N` | a Rewind |

```console
$ kubectl gcr point pod/sandbox "before rm -rf"
timepoint/sandbox-zc5qh created; waiting for gcr-operator
#1
$ kubectl gcr rewind pod/sandbox 1
rewind/sandbox-lftpr created; waiting for gcr-operator
pod/sandbox rewound to #1
```

`-n` is the namespace, and `--timeout` how long to wait (10 minutes).
If the wait times out, the operation keeps going: `kubectl get` it.

## Who may do what

Creating an operation acts on a pod's processes: it replaces them
(Rewind), moves them (Migration), or copies their memory out of the
cluster (Snapshot). So creating them is not part of Kubernetes' `edit`
or `admin` roles. Grant the ClusterRole `gcr-operations`, per namespace:

```sh
kubectl create rolebinding gcr-operations --clusterrole=gcr-operations \
  --group=team-a -n team-a
```

Reading them is part of `view`, `edit` and `admin`, through the
aggregated ClusterRole `gcr-operations-view`.

An operation only acts on pods in its own namespace. A Snapshot without
a `pushSecret` pushes with the nodes' credentials to whatever image it
names, which could overwrite another team's snapshot in the same
registry. Where that matters, have tenants use `pushSecret` with
credentials scoped to their own repositories, and give the nodes none.

## Installing

The CRDs and RBAC come with gcr-operator: the chart's
`operator.enabled=true`, or `gcr-operator.yaml` ([gcr-operator](operator.md#install)).
gcr-operator logs `carrying out gcr.dev's operations` when it starts.
