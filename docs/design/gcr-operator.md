# gcr-operator: evictions that migrate

Status: built, all four milestones below. The user-facing guide is [gcr-operator](../operator.md); this page is how it came to work the way it does.

## The goal

`kubectl drain`, the cluster autoscaler, and node upgrades evict pods. An evicted pod is deleted, and its controller starts a fresh one elsewhere: the process, its memory and its warm state are gone. For a pod that opts in, gcr-operator turns the eviction into a live migration to another node. The pod's processes carry on there after a pause of well under a second, and the eviction then completes as usual.

## Today

- **gcr migrates a container between nodes.** On a three-node k0da cluster (k0s v1.33, arm64), a busybox pod counting in memory moved from one worker to the other: `gcr migrate` from the source node, into a target pod created there with `gcr.dev/migrate-in`. It was stop-and-copy (arm64 tracks no dirty memory), with 354 ms of downtime, and the target carried on counting from where the source stopped. x86 adds pre-copy rounds ([migration](../migration.md)).
- **The target pulls the image itself.** Only the writable layer travels. gcr's check that the layers beneath it match applies to forks of a snapshot, not to image-based pods.
- **Nothing does this from Kubernetes.** A person ran `gcr migrate` on the source node, against a target pod they had created and pinned to the other node.

## Components

1. **The operator,** a Deployment. It watches pods that opt in, decides when and where they move, creates target pods, and drives each migration.
2. **An eviction webhook.** A `ValidatingWebhookConfiguration` on `CREATE` of `pods/eviction`, served by the operator. It holds an opted-in pod's eviction until the pod has moved.
3. **A node agent** on every node: the gcr installer DaemonSet, which already runs there with the host's namespaces. It runs `gcr migrate` for the operator, against the shim's control socket. Its API, over mTLS, has one call: migrate a container to an address with a token.
4. **A RuntimeClass for targets,** `gcr-migrate`: a handler whose `PodAnnotations` list `gcr.dev/migrate-in`, `gcr.dev/migrate-token` and `gcr.dev/userns-base` ([configuration](../configuration.md#podannotations)). Only the operator's service account may create pods with it, enforced by a `ValidatingAdmissionPolicy`. A tenant who could would make a node's shim listen on any address and restore what arrives.

## Opting in

The pod's template carries `gcr.dev/migrate-on-evict: "true"` and runs on a gcr RuntimeClass. Nothing changes for other pods: the webhook lets their evictions through untouched.

## The flow

1. **An eviction arrives** for an opted-in pod. The webhook records a migration (a `Migration` object, owned by the pod) and answers `429 Too Many Requests` with `Retry-After`, as a PodDisruptionBudget does. `kubectl drain` and the autoscaler retry.
2. **The operator creates the target:** the source's pod spec on the `gcr-migrate` RuntimeClass, with `migrate-in` on a free port, a fresh random token, and the source's `userns-base`. Its labels are changed so no controller selects it yet (see [ownership](#ownership)). Draining cordons the node, so the scheduler places the target elsewhere. Its start blocks until a migration arrives.
3. **The operator asks the source node's agent** to migrate each of the pod's containers to the target node's address, one container at a time. The pause container stays behind: the target has its own sandbox, with a new IP.
4. **On success,** gcr has stopped the source. The operator hands the target over to the source's controller, and the next retry of the eviction is allowed: it deletes the source pod.
5. **On failure,** gcr thaws the source, which carries on. The operator deletes the target and marks the migration failed. The eviction is then let through, so a drain is never blocked by a pod that can't move; the pod restarts cold, as it would have without gcr. Retrying, or giving up after a deadline, is a policy knob.

## Ownership

This is the hard part. When the source is deleted, its ReplicaSet sees one replica too few and starts a new, cold one. When the target appears with matching labels, the ReplicaSet sees one too many and deletes one.

The ReplicaSet's choice of which pod to delete can be steered:
- **Hold the target back.** It's created with labels its ReplicaSet doesn't select. It's still running the migration, so it isn't Ready, and the ReplicaSet would delete a not-Ready pod first.
- **After the migration,** the operator sets `controller.kubernetes.io/pod-deletion-cost` low on the source, then relabels the target to match. The ReplicaSet adopts the target, which is Ready, sees one replica too many, and deletes the source: Ready pods are ranked by deletion cost.
- **The eviction's retry** then finds the source gone or going.

Other owners:
- **StatefulSets:** a pod's name is its identity, and two pods named `db-0` can't coexist, even briefly. Moving one means deleting the source after the dump and before creating a same-named target: a pause of the target's whole start, not of a migration. Later.
- **Jobs:** adopted like ReplicaSets' pods, with two differences ([gcr-operator](../operator.md#deployments-and-jobs)). A Job deletes the more recently Ready of two pods and ignores `pod-deletion-cost`, so the target is handed over only once the source is no longer Ready. A Job counts a deleted pod as failed, so the source is marked `DisruptionTarget` before its processes go, for Jobs whose `podFailurePolicy` ignores that.
- **Bare pods:** the target simply replaces the source.

## What moves, and what doesn't

| | |
| --- | --- |
| processes, memory, threads, file descriptors | moved (CRIU) |
| the container's writable layer | moved |
| image layers | pulled by the target (same image, pinned by digest) |
| ConfigMaps, Secrets, projected volumes | mounted afresh by the target's kubelet, same contents |
| `emptyDir` | copied to the target pod's own, while the container is frozen, as the writable layer is (milestone 3) |
| PersistentVolumes | ReadWriteMany: mounted on both. ReadWriteOnce: refused at first (the volume must detach from the source first, which lengthens the pause) |
| IP address, open TCP connections | a new IP; connections reset. Services follow labels, so traffic goes to the target once it is Ready |
| GPUs | stop-and-copy only (CRIU's CUDA plugin cannot pre-dump), and the target needs the same GPU model and driver ([GPUs](../gpus.md)) |

## Security

- **Each migration has its own token,** random, known to the operator and the target pod only. It lives in the target's annotations, so who may read pods matters.
- **Migration traffic** goes over TLS with certificates from a cluster CA the operator manages (`MigrationTLS`), so a node only accepts migrations from another node.
- **The node agent** accepts calls from the operator only, over mTLS.
- **Only the operator can use the target RuntimeClass** (see [components](#components)).

## Open questions

- **The x86 lazy-restore crash.** Post-copy migration and lazy restores crash intermittently on x86 (makhov/gcr#60). Pre-copy, which restores in full, isn't affected. The operator should use pre-copy until that's fixed.
- **Choosing the target node:** leave it to the scheduler (simplest), or prefer nodes that already have the image, or are close by.
- **Spot and node shutdown.** With a shutdown notice instead of an eviction, there may be no time to migrate: milestone 4 checkpoints to a registry instead ([below](#spot-and-node-shutdown)).

## Milestones

1. **`gcr-operator migrate pod/NAME [--to NODE]`:** the operator, the node agent, the target RuntimeClass, and bare pods. No webhook yet.
2. **Evictions:** the webhook, Deployments and ReplicaSets through the ownership handover, and fallback on failure. The demo: drain a node while a stateful process keeps running on another.
3. **`emptyDir`, and Jobs.** Built.
4. **StatefulSets, and spot and node shutdown through registry checkpoints** ([below](#milestone-4-checkpoints-through-a-registry)). Built.

## Milestone 4: checkpoints through a registry

So far a migration runs the target next to the source until the switch, which is how the downtime stays near 0.3 s. A StatefulSet's pod can't have a twin: it is `web-0` by name, its DNS name and its PersistentVolumeClaims follow that name, and deleting it makes the StatefulSet create `web-0` again at once, cold. And on spot reclaim or node shutdown there may be no second pod to migrate to in time. Both are handled the same way: **checkpoint the pod into a registry, let its controller create the replacement as it would, and have the replacement restore from the checkpoint.**

### The flow, for a StatefulSet's pod

Built (step 3 below), with `-registry` on the operator:

1. **Checkpoint and stop.** A Job on the source node runs `gcr snapshot -stop ID DIR`. The container is frozen, dumped as a snapshot (the format `gcr.dev/fork-from` starts from) and killed, not thawed, so its processes can't run on, or write to its volumes, after the checkpoint. The downtime starts here.
2. **Push.** The same Job pushes the snapshot to the operator's registry (`gcr snapshot-push`), tagged with the pod's UID.
3. **Record the restore.** The operator writes a pending restore for `NAMESPACE/web-0` into the ConfigMap `gcr-system/gcr-restores`: the snapshot's directory, its reference, and a random token. Only the operator writes it.
4. **Delete the source.** The StatefulSet creates `web-0` again.
5. **Steer the new pod.** The operator's mutating webhook on pod `CREATE` (for StatefulSets' opted-in pods) sees `web-0` with a pending restore, and adds:
   - `runtimeClassName: gcr-migrate`;
   - `gcr.dev/fork-from: <the snapshot's directory>` and `gcr.dev/restore-token: <the token>`;
   - `gcr.dev/fork-wait: 100s`: the node's shim waits that long for the snapshot to arrive.
6. **Pull where it lands.** The scheduler places the pod as it would any other, so the operator never picks the node. Once it is bound, a Job there pulls the snapshot into gcr's snapshot directory, and the waiting shim starts the container from it. The process carries on where it was frozen, in a pod with the same name and DNS name, a new IP (so its TCP connections reset, as with any migration here), and the same volumes, reattached by Kubernetes on the new node.
7. **Clean up.** Once the pod is Ready, or the move has failed, the operator forgets the pending restore.

**Drain's retries.** kubectl drain evicts by name, and retries while the operator answers 429, so its retries reach the new `web-0`. The operator answers 404 for a pod it moved or restored (the pod evicted is gone), unless that pod's own node is being drained too (cordoned, or tainted by the cluster autoscaler).

### What it costs

- **Downtime is seconds,** not 0.3 s: dump, push, pull, deleting and creating the pod, and the restore. The pull overlaps nothing, since the source is stopped by then; most of the time goes to moving the memory twice. Pushing straight to the target node instead of a registry would halve that, and is the obvious follow-up.
- **Volumes:** ReadWriteMany volumes are fine. ReadWriteOnce network volumes are detached from the source node and attached to the target by Kubernetes, adding their attach time (tens of seconds on some clouds). Node-local volumes (`local`, `hostPath`) can't move: such pods are refused, as now.
- **A failure after step 1 can't fall back to the running source,** which is gone: the pod then starts cold, as it would without gcr. If the webhook is down, the new pod isn't steered. If the pull or the restore fails, the operator deletes the pod again. Before step 1, nothing has changed.

### Who may restore what

`gcr.dev/fork-from` names a host directory to restore from, so pod authors can't use it (`RestrictedAnnotations`), and `gcr-migrate`'s `PodAnnotations` allow it. But here the pod is created by the StatefulSet controller, not by the operator. So the `ValidatingAdmissionPolicy` that lets only the operator and cluster admins use `gcr-migrate` has one more case: **the StatefulSet controller may, for a pod whose `gcr.dev/restore-token` and `gcr.dev/fork-from` are a pending restore's.**

The policy reads `gcr-restores` as its parameter (`paramKind: ConfigMap`, `parameterNotFoundAction: Deny`). CEL has no JSON parser, so a restore is stored as JSON starting `{"token":"T","dir":"D",`, and the policy matches that prefix against the pod's annotations. A pod template that copies the annotations is refused, with a made-up token, the wrong directory, or a spent token, and so is a pod author with a live one. The token lives in the pod's annotations, but is good only while that restore is pending.

The operator checks the policy has the parameter when it starts. One that finds an older policy evicts StatefulSets' pods rather than moving them, since the StatefulSet's new pod would be refused.

### Spot and node shutdown

Built (step 4 below), for graceful node shutdowns: systemd's, which the kubelet handles with `shutdownGracePeriod`, and what spot reclaims on GKE are.

Spot handlers that drain the node first, such as AWS Node Termination Handler and Karpenter's interruption handling, go through evictions. The webhook already turns those into migrations. A graceful node shutdown doesn't: the kubelet stops the pods itself, and admits no new pod meanwhile, so no Job can reach the node to checkpoint them. So the checkpoint happens on the node:

1. **`gcr shutdown-guard`** (a systemd unit on each node) holds a logind delay lock, as the kubelet does. When logind signals the shutdown, it marks it, in `/run/gcr/shutdown`.
2. **The shim checkpoints as the kubelet stops.** The kubelet stops each pod's container through the CRI. If the node is marked and the pod opted in (`gcr.dev/migrate-on-evict`, which reaches the shim with the other `gcr.dev/*` pod annotations), the shim first snapshots and stops the container into `/var/lib/gcr/checkpoints/<pod UID>` (`gcr snapshot -stop`'s code). Only then does the stop go on. A kill waits for a snapshot in progress, so the kubelet's SIGTERM can't reach the process mid-dump, where the checkpoint would carry it along.
3. **The guard pushes** each checkpoint to the registry, tagged with the pod's UID, as soon as it is written. It lets the shutdown go on once the kubelet has released its own lock (it has stopped every pod) and every checkpoint is pushed or failed, or after `-wait`.
4. **The operator restores.** Its pod informer sees the kubelet's status on the pod: `Failed`, "Pod was terminated in response to imminent node shutdown.", with the condition `DisruptionTarget` (`TerminationByKubelet`).
   - **A StatefulSet's pod:** the operator records a pending restore for it, and the webhook steers the pod the StatefulSet creates again, as for an eviction. The checkpoint is pulled onto the pod's node; the pull Job retries until the guard's push has arrived.
   - **A bare pod:** the operator creates the replacement, as `kubectl gcr migrate -registry` does, and deletes the failed pod once it runs.
   - **A ReplicaSet's or a Job's pod:** its controller replaces it with a pod of another name, so the restore can't be keyed by name. The operator records an *owned* restore, keyed `NAMESPACE.TOKEN`, with the owner's UID (and, for an Indexed Job's pod, its completion index). When the controller creates a pod, the webhook claims one of its owner's unclaimed restores of the same index, recording the claim with the restore, so that one restore goes to one pod. It steers the pod as for a StatefulSet's, and labels it `gcr.dev/restore=ID`, since the pod has no name yet at admission; the operator finds it by the label, and pulls the checkpoint onto its node.
   - **Who may restore:** the admission policy lets the StatefulSet, ReplicaSet and Job controllers create a pod on `gcr-migrate` with a pending restore's token and directory, recorded under the pod's name or under its token. An operator that finds an older policy (one naming no ReplicaSet controller) restores StatefulSets' and bare pods only.

**The race with the controller.** A controller replaces the pod as soon as the kubelet marks it failed, which can be within a second of the shutdown, since the dump is quick. The informer usually tells the operator first. If it hasn't yet when the new pod is created, the webhook looks up the last the informer saw of the pod being replaced (a StatefulSet's by its name, a ReplicaSet's or a Job's among its owner's pods). If that pod was on a node that isn't Ready, the webhook waits up to 2 s for the informer to say it was stopped by a shutdown, then records the restore and steers the new pod.

**Measured** on two k0s VMs (arm64, Ubuntu 26.04, k0s v1.33, `shutdownGracePeriod: 60s`), `systemctl poweroff` of the node with a StatefulSet's pod and a bare pod, twice. Each time:
- both containers were checkpointed and pushed within the first second;
- both pods were restored on the other node 3.3 to 4.6 s after the shutdown started;
- both carried on counting from where they were stopped, with 0 restarts.

And with a Deployment's two pods and an Indexed Job's two (`podFailurePolicy` ignoring `DisruptionTarget`), on `sd1` with 4 CPUs (with 2, its API server could not take the kubelet's status updates during the shutdown, and nothing was restored):
- every pod was restored in the pod its controller created, the Job's each in the pod of its index, and carried on counting, with 0 restarts;
- the Deployment's in 4.8 and 6.5 s, the Job's in 24 s, 20 s of it the Job controller waiting before it replaces a failed pod;
- the Job finished with `succeeded: 2` and no failures counted.

**Not yet:**
- **How much fits before the node goes:** a spot notice is two minutes on most clouds, and GKE's graceful shutdown 30 s. A few GiB of memory push in that time over a fast link.

### Order of work

1. `gcr snapshot -stop`: checkpoint and stop, through the control API. Built.
2. Checkpoint-to-registry and restore-from-registry Jobs in `kubemigrate`, used by `kubectl gcr migrate -registry`, with bare pods first: the plugin creates the replacement itself. That tests the path end to end without the webhook. Built.
3. The mutating webhook, the pending restores, and the admission policy's new case: StatefulSets. Built.
4. Graceful node shutdowns: the shim, `gcr shutdown-guard`, and the operator's informer. Built, for StatefulSets' and bare pods.
