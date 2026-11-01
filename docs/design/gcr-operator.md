# gcr-operator: evictions that migrate

Status: milestones 1 (`kubectl gcr migrate`) and 2 (evictions, and Deployments' pods handed over) are built, and `emptyDir` volumes of 3; see [migration](../migration.md#draining-nodes-gcr-operator). The rest is a proposal.

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
- **Jobs:** they count pods by labels, too, so similar adoption should work; not in the first version.
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
- **Spot and node shutdown.** With a shutdown notice instead of an eviction, there may be no time to migrate. Checkpointing to a registry and restoring the replacement from it is a related feature, sharing the agent and the ownership handover.

## Milestones

1. **`gcr-operator migrate pod/NAME [--to NODE]`:** the operator, the node agent, the target RuntimeClass, and bare pods. No webhook yet.
2. **Evictions:** the webhook, Deployments and ReplicaSets through the ownership handover, and fallback on failure. The demo: drain a node while a stateful process keeps running on another.
3. **`emptyDir`, and Jobs.** `emptyDir` is built.
4. **StatefulSets, and spot and node shutdown through registry checkpoints.**
