# Node maintenance without restarts

Draining a node normally means restarting whatever runs on it. With
gcr-operator, an opted-in pod moves to another node instead, with its
processes and memory intact and a pause of a few hundred milliseconds.

The same mechanism covers the cluster autoscaler, node upgrades, and
nodes that shut down, because all of them arrive as evictions.

## Set it up

You need gcr on the cluster and at least two nodes with it
([Installation](../install.md#your-own-cluster)):

```sh
helm upgrade gcr oci://ghcr.io/makhov/charts/gcr -n gcr-system \
  --set operator.enabled=true
kubectl -n gcr-system rollout status deployment/gcr-operator
```

Then opt a workload in with one annotation:

```yaml
spec:
  template:
    metadata:
      annotations:
        gcr.dev/migrate-on-evict: "true"
    spec:
      runtimeClassName: gcr
```

## Drain the node

```console
$ kubectl drain worker-1 --ignore-daemonsets
evicting pod default/counter-dc564fd68-qctk2
error when evicting pods/"counter-dc564fd68-qctk2" -n "default" (will retry after 5s): admission webhook "evictions.gcr.dev" denied the request: pod default/counter-dc564fd68-qctk2 is moving to another node
evicting pod default/counter-dc564fd68-qctk2
node/worker-1 drained
```

That pod was counting in memory. It moved in 10 s, with 346 ms of
downtime, and carried on counting on the other node from the number it
had reached (three-node cluster, arm64).

What happened: the operator answers the eviction with 429, the way a
PodDisruptionBudget does, migrates the pod, and lets the drain's next
retry through once the source is gone. A pod that cannot move is evicted
normally, so **a drain is never held up** — worth remembering, because it
means adding the operator cannot wedge your maintenance.

For a Deployment or a Job, the moved pod is handed over to its
controller, which therefore never starts a cold replacement. A Job's run
carries on rather than starting over.

## What moves

| Pod | Drained | Its node shuts down |
| --- | --- | --- |
| A Deployment's | moved live | restored in the pod its ReplicaSet creates, with a registry |
| A Job's | moved live, the run carries on | restored in the pod the Job creates (same index), with a registry |
| A bare pod | moved live | restored from its checkpoint, with a registry |
| A StatefulSet's | moved through a registry, with one; evicted without | restored from its checkpoint, with a registry |
| A DaemonSet's | evicted | stopped |

A pod moves if it has **one container** (no sidecars), **its volumes can
follow it** (ConfigMaps, Secrets, projected and downward API volumes
always; `emptyDir` in a live move; a StatefulSet's PVCs unless they are
tied to one node), and it is **not in a Kubernetes user namespace**
(`hostUsers: false`).

Its IP changes and open TCP connections reset. Services still find it:
it keeps its labels.

## Surviving a node shutdown

A spot reclaim or a `systemctl poweroff` gives the kubelet seconds, not
minutes. For pods to come back elsewhere rather than start cold, two
extra pieces are needed:

- **a registry** for the checkpoints: `--set
  operator.registry=registry.example.com/gcr/checkpoints`. The nodes push
  and pull, as root, with the credentials in `/root/.docker/config.json`;
- **`gcr shutdown-guard` on each node**, which checkpoints the pods as
  the kubelet stops them.

Measured on two k0s VMs, `systemctl poweroff` of a node: every pod
carried on counting on the other node, with no restarts. A StatefulSet's
pod and a bare pod came back 3.3 to 4.6 s after the shutdown began, a
Deployment's two pods in 4.8 and 6.5 s, and an Indexed Job's two in 24 s
— 20 s of which is the Job controller waiting before it replaces a
failed pod. Leave the control plane some room: with 2 CPUs its API server
could not take the kubelet's status updates during the shutdown, and
nothing was restored.

A **Job** counts a pod its node stopped as failed, towards
`backoffLimit`, unless it ignores disruptions:

```yaml
spec:
  podFailurePolicy:
    rules:
      - action: Ignore
        onPodConditions: [{type: DisruptionTarget}]
```

The same rule applies to pods moved by a drain. Keep the operator itself
off the nodes that get shut down, or it goes down with them — the control plane or an on-demand node pool is the place
for it. [gcr-operator: node shutdown](../operator.md#node-shutdown) sets
it all up.

## Moving one pod by hand

No operator needed:

```sh
kubectl gcr migrate counter-dc564fd68-qctk2 --to worker-2
```

It needs a cluster admin's credentials, because it creates a pod on the
`gcr-migrate` RuntimeClass ([the plugin](../install.md#the-kubectl-plugin),
[moving a pod](../kubernetes.md#moving-a-pod)).

## What it costs

- **Downtime does not grow much with memory** on x86, where the kernel
  tracks dirty pages and gcr pre-copies: the pause is the last round plus
  the restore. On arm64 there is no dirty tracking, so a move is
  stop-and-copy and the pause scales with memory, unless you use
  post-copy ([live migration](../migration.md)).
- **Both nodes must have the same architecture** and CPU features, and
  the image must be present on the target.
- Memory, files and the token cross the network in one connection, which
  is plain HTTP unless you set up
  [`MigrationTLS`](../configuration.md#migrationtls). On an untrusted
  network, set it up.
