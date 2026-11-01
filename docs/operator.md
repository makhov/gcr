# gcr-operator

gcr-operator keeps pods running when the node under them goes away.
Pods opt in with one annotation, and then:

- **Draining a node** (`kubectl drain`, the cluster autoscaler, node
  upgrades) moves them to another node instead of evicting them. Their
  processes carry on there, memory and all, after a pause of a few hundred
  milliseconds.
- **StatefulSets' pods,** which can't have a twin running, move through a
  checkpoint in a registry, in seconds, and keep their name, DNS name and
  volumes.
- **A node shutting down** (`systemctl poweroff`, a spot reclaim through
  the kubelet's graceful node shutdown) checkpoints them as the kubelet
  stops them, and the operator restores them on another node.

A pod that can't move is evicted, as it would be without gcr: a drain is
never held up by the operator.

## Install

It needs gcr on the cluster ([`gcr.yaml`](install.md#on-kubernetes)), and
two or more nodes with gcr.

```sh
kubectl apply -f https://github.com/makhov/gcr/releases/latest/download/gcr-operator.yaml
kubectl -n gcr-system rollout status deployment/gcr-operator
kubectl -n gcr-system logs deployment/gcr-operator
```

```
serving evictions on :8443; migrations' Jobs run ghcr.io/makhov/gcr:v0.1.6
```

The manifest creates, in `gcr-system`:
- the operator's Deployment and Service;
- its RBAC;
- a `ValidatingWebhookConfiguration` on evictions;
- a `MutatingWebhookConfiguration` on pod creation, for StatefulSets'
  pods.

Both webhooks fail open: with the operator down, evictions go through and
pods start as they would without gcr.

### With a registry

StatefulSets and node shutdowns move pods through checkpoints in an OCI
registry. Give the operator a repository there:

```sh
kubectl -n gcr-system patch deployment gcr-operator --type=json -p '[
  {"op": "add", "path": "/spec/template/spec/containers/0/args",
   "value": ["-registry", "registry.example.com/gcr/checkpoints"]}]'
kubectl -n gcr-system logs deployment/gcr-operator | head -1
```

```
StatefulSets' pods move through registry.example.com/gcr/checkpoints, and pods on nodes shutting down are restored from it
```

- **The nodes push and pull the checkpoints,** not the operator, as root,
  with the credentials in root's Docker config on each node
  (`/root/.docker/config.json`, as `docker login` writes it). Add
  `-plain-http` for a registry without TLS.
- **Checkpoints are tagged with the pod's UID,** and stay in the registry;
  clean the repository up as you would any other.
- If the log says instead `StatefulSets' pods are evicted, not moved`, the
  cluster's `gcr.yaml` is older than the operator: apply the release's
  `gcr.yaml` again.

### Where it runs

The operator moves pods off the node it runs on too, and holds its own
eviction until they have moved. But a node that shuts down takes the
operator with it. If you use node shutdowns, keep it on nodes that aren't
shut down that way (on-demand nodes, or the control plane):

```sh
kubectl -n gcr-system patch deployment gcr-operator -p \
  '{"spec":{"template":{"spec":{"nodeSelector":{"node.kubernetes.io/lifecycle":"on-demand"}}}}}'
```

(Use whatever label your nodes have.)

## Opting pods in

Annotate the pod, or the pod template of a Deployment, StatefulSet or
Job:

```yaml
spec:
  template:
    metadata:
      annotations:
        gcr.dev/migrate-on-evict: "true"
    spec:
      runtimeClassName: gcr
```

What then happens to each kind of pod:

| Pod | When evicted (drain, autoscaler) | When its node shuts down |
| --- | --- | --- |
| A Deployment's (ReplicaSet's) | moved live, handed over to the ReplicaSet | starts cold elsewhere (not yet restored) |
| A Job's | moved live, handed over to the Job; the run carries on | starts cold elsewhere (not yet restored) |
| A bare pod | moved live | restored from its checkpoint, with a registry |
| A StatefulSet's | moved through the registry, with one; evicted without | restored from its checkpoint, with a registry |
| Others (DaemonSets) | evicted | stopped |

A pod moves if:
- **it has one container,** no sidecars;
- **its volumes can follow it:**
  - ConfigMaps, Secrets, projected and downward API volumes, which the
    new node mounts afresh;
  - `emptyDir` volumes, copied over in a live move, but not through a
    registry;
  - a StatefulSet's PersistentVolumeClaims, reattached on the new node,
    unless their volumes are tied to one node (`local`, `hostPath`,
    local-path).
- **it isn't in a Kubernetes user namespace** (`hostUsers: false`).

[Live migration](migration.md#between-kubernetes-nodes) has the details.
The pod's IP changes, and its open TCP connections reset. Services find
it by its labels, which it keeps.

## Draining a node

```console
$ kubectl drain ns-worker-1 --ignore-daemonsets
evicting pod default/counter-dc564fd68-qctk2
error when evicting pods/"counter-dc564fd68-qctk2" -n "default" (will retry after 5s): admission webhook "evictions.gcr.dev" denied the request: pod default/counter-dc564fd68-qctk2 is moving to another node
evicting pod default/counter-dc564fd68-qctk2
node/ns-worker-1 drained
```

That Deployment's pod moved to another node in 10 s, with 346 ms of
downtime, and carried on counting there (three-node k0da cluster, arm64).

The operator answers an opted-in pod's eviction 429 (Too Many Requests),
as a PodDisruptionBudget does, and moves the pod as
[`kubectl gcr migrate`](kubernetes.md#moving-a-pod) does. The drain
retries; once the pod has moved, its source is gone and the drain goes
on. If the move fails, the next retry is let through and the pod
restarts elsewhere, as without gcr.

### Deployments and Jobs

The moved pod (the target) is handed over to the pod's controller, so
that the controller never starts a cold replacement:

- **A Deployment's pod:**
  - the target is created without the labels its ReplicaSet selects by,
    so the ReplicaSet doesn't see it while it waits;
  - once it is Ready, the source is marked cheapest to delete, and the
    target gets the labels and the ReplicaSet as its owner;
  - the ReplicaSet then has one pod too many, and deletes the source.
- **A Job's pod:** the target is hidden the same way, and gets the Job's
  labels, owner and tracking finalizer once the source has stopped. The
  run carries on rather than starting over. The source is marked
  `DisruptionTarget`, as an eviction would mark it, so each move counts as
  a failed pod towards `backoffLimit`, unless the Job ignores disruptions:

  ```yaml
  spec:
    podFailurePolicy:
      rules:
        - action: Ignore
          onPodConditions: [{type: DisruptionTarget}]
  ```

  With that rule, a Job moved in a drain finishes with `succeeded: 1` and
  no failures.

Once a pod has moved, its source can't run again: the shim refuses to
start its container on the old node, so a kubelet restarting it before
the pod is deleted can't run the workload twice.

## StatefulSets

With a [registry](#with-a-registry), a StatefulSet's opted-in pod moves
like this when evicted:

1. Its container is checkpointed and stopped on its node, and the
   checkpoint is pushed to the registry.
2. The operator records a **pending restore** for the pod: the
   checkpoint, and a random token.
3. It deletes the pod, and the StatefulSet creates it again by the same
   name.
4. The operator's mutating webhook steers the new pod to restore from the
   checkpoint: on the `gcr-migrate` RuntimeClass, with the token.
5. The scheduler places it. The checkpoint is pulled to that node, and
   the pod carries on where it was stopped, with the same name, DNS name
   and volumes.

On a three-node k0da cluster, a drain moved a StatefulSet's pod in 10 s,
with 6.3 to 6.8 s of downtime: more than a live move, since memory goes
through the registry, and the pod can't run on both nodes at once.

Only the StatefulSet controller may create a pod on `gcr-migrate`, and
only with a pending restore's token and checkpoint (`gcr.yaml`'s
admission policy). A pod template that asks for `gcr-migrate` itself is
refused.

If the move fails after the pod was stopped, the pod is deleted again,
and the StatefulSet starts it cold, as without gcr.

## Node shutdown

When a node shuts down gracefully, the kubelet stops its pods itself, and
admits no new pods meanwhile. So the checkpoints are taken on the node:
`gcr shutdown-guard` holds the shutdown, the shim checkpoints each
opted-in container as the kubelet stops it, and the guard pushes the
checkpoints to the registry. The operator then restores the pods on
another node: a StatefulSet's in the pod the StatefulSet creates again,
a bare pod in a replacement it creates.

On two k0s VMs, both kinds of pod carried on counting on the other node
3.3 to 4.6 s after `systemctl poweroff`, with no restarts.

### Setting it up

The operator needs a [registry](#with-a-registry). Then, on each node:

1. **Turn on the kubelet's graceful node shutdown,** in its
   configuration (on k0s, in a worker profile):

    ```yaml
    apiVersion: kubelet.config.k8s.io/v1beta1
    kind: KubeletConfiguration
    shutdownGracePeriod: 60s
    shutdownGracePeriodCriticalPods: 10s
    ```

2. **Let logind hold shutdowns long enough** for the kubelet and the
   pushes, in a drop-in that sorts after the others (Ubuntu's
   `unattended-upgrades-logind-maxdelay.conf` otherwise caps it at 30 s,
   and the kubelet then doesn't handle shutdowns at all):

    ```sh
    printf '[Login]\nInhibitDelayMaxSec=90\n' | sudo tee /etc/systemd/logind.conf.d/zz-gcr.conf
    sudo systemctl restart systemd-logind
    ```

    Then restart the kubelet.

3. **Install the guard,** pointing at the operator's registry, with the
   registry's credentials in root's Docker config:

    ```sh
    sudo mkdir -p /etc/gcr
    printf 'GCR_SHUTDOWN_REGISTRY=registry.example.com/gcr/checkpoints\nGCR_SHUTDOWN_ARGS=-wait 80s\n' \
      | sudo tee /etc/gcr/shutdown-guard.env
    sudo curl -fLo /etc/systemd/system/gcr-shutdown-guard.service \
      https://github.com/makhov/gcr/releases/latest/download/gcr-shutdown-guard.service
    sudo systemctl daemon-reload
    sudo systemctl enable --now gcr-shutdown-guard
    ```

    Add `-plain-http` to `GCR_SHUTDOWN_ARGS` for a registry without TLS.

4. **Check** that both the kubelet and the guard hold a shutdown lock:

    ```
    $ systemd-inhibit --list
    WHO      UID USER PID  COMM    WHAT     WHY                                         MODE
    gcr      0   root 1127 gcr     shutdown Checkpointing pods that move to other nodes delay
    kubelet  0   root 1380 kubelet shutdown Kubelet needs time to handle node shutdown  delay
    ```

    If there's no kubelet line, its graceful shutdown is off: its log
    says why (`Failed to start node shutdown manager`).

`journalctl -u gcr-shutdown-guard` shows what the guard did at the last
shutdown (`journalctl -b -1` after the reboot): each checkpoint pushed,
or why not.

Deployments' and Jobs' pods aren't restored after a node shutdown yet:
their replacements start cold, as without gcr.

## Watching it

The operator marks the pod it is moving with the annotation
`gcr.dev/migration`:

- `migrating TIME by OPERATOR-POD`: on its way. Evictions are answered 429
  meanwhile.
- `failed: WHY`: it couldn't move. The next eviction goes through, and the
  annotation says why.

```sh
kubectl get pod NAME -o jsonpath='{.metadata.annotations.gcr\.dev/migration}'
kubectl -n gcr-system logs deployment/gcr-operator
```

The operator's log has a line for each step of each move, and how long
the pod was paused:

```
default/counter-dc564fd68-qctk2 moved to ns-worker-0 as counter-dc564fd68-qctk2-4b1e2a (downtime 346ms)
default/web-0 moved to rel-worker-1 as web-0 (downtime 6.333s (from the stop to Ready))
default/lone restored on lima-sd1 as lone-5f9a39 (downtime 4.538s (from the shutdown seen to Ready))
```

The moves' Jobs run in `gcr-system`, named `gcr-info-…`, `gcr-migrate-…`,
`gcr-checkpoint-…` and `gcr-pull-…`, and are deleted when done.

## When a move fails

Every failure falls back to what Kubernetes does without gcr:

| What fails | What happens |
| --- | --- |
| The operator is down | Evictions go through, and pods start cold elsewhere. |
| The pod can't move (a volume, two containers…) | It is evicted. `gcr.dev/migration` says why. |
| A live move fails | The target is deleted, the pod keeps running, and the next eviction goes through. |
| A StatefulSet's checkpoint fails before the stop | The pod keeps running, and the next eviction goes through. |
| …after the stop, or its restore fails | The pod is deleted again, and starts cold. |
| A shutdown's checkpoint never reaches the registry | The StatefulSet's pod starts cold after 3 minutes; the bare pod stays failed. |

If the operator itself is drained in the middle of moves, it finishes
them first, for up to 100 s. A move left behind by an operator that went
away is taken over by the next one.

## Troubleshooting

**Pods are evicted, not moved.** Read the pod's `gcr.dev/migration`
annotation (above). Common reasons: the pod has more than one container,
a `hostPath` or node-local volume, or `hostUsers: false`; or no other
node has gcr ready (`kubectl get nodes -l gcr.dev/runtime=ready`).

**StatefulSets' pods are evicted.** The operator has no `-registry`, or
its log's first line says the cluster's `gcr.yaml` is too old for it.

**A StatefulSet's pod started cold.** The operator's log says why. If
the pod was created without the annotation `gcr.dev/restore-token`, the
mutating webhook didn't reach the operator.

**Pods weren't restored after a node shutdown.** On that node, after its
reboot:
- `journalctl -b -1 -u gcr-shutdown-guard`: whether it saw the shutdown,
  and pushed each checkpoint. A push error usually means missing
  credentials in root's Docker config.
- The kubelet's log, for `Failed to start node shutdown manager`: its
  graceful shutdown is off, or logind's `InhibitDelayMaxSec` is shorter
  than `shutdownGracePeriod`.

Check also that the operator wasn't on that node.

## Flags

Set in the Deployment's `args`:

| Flag | Default | |
| --- | --- | --- |
| `-registry REPO` | | Registry repository (`host[:port]/path`) for StatefulSets and node shutdowns. Without it, StatefulSets' pods are evicted. |
| `-plain-http` | off | Talk to the registry without TLS. |
| `-image IMAGE` | the installer's image, or `busybox:1.37` | The image the moves' Jobs run: anything with `nsenter`. |
| `-gcr PATH` | `/usr/local/bin/gcr` | gcr on the nodes. |
| `-namespace NS` | its own | Where the Jobs run, and the Service. |
| `-service NAME` | `gcr-operator` | The webhooks' Service, which its certificate names. |
| `-webhook NAME` | `gcr-operator` | The webhook configurations it gives its certificate's CA. |
| `-listen ADDR` | `:8443` | Where the webhooks listen. |

## Security

- **The operator runs unprivileged.** What touches the nodes is the Jobs
  it starts, privileged, in `gcr-system` only, and the `gcr-migrate`
  RuntimeClass its targets run on.
- **Only cluster admins and the operator may run pods on `gcr-migrate`,**
  plus the StatefulSet controller with a live restore token. Its pods can
  make a node's shim listen for a migration, or restore from a checkpoint
  on the node.
- **Its RBAC:**
  - pods: read, create, patch and delete, to move them;
  - jobs and secrets: in `gcr-system` only;
  - ReplicaSets, Jobs, nodes and volumes: read only;
  - its own webhook configurations: to give them its CA.
- **Checkpoints hold the pod's memory,** secrets included: keep the
  registry as private as the workloads.

## Uninstalling

```sh
kubectl delete -f https://github.com/makhov/gcr/releases/latest/download/gcr-operator.yaml
```

Evictions then go through as without gcr. On nodes with the shutdown
guard, `sudo systemctl disable --now gcr-shutdown-guard`.
