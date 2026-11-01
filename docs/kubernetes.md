# Kubernetes

gcr runs Kubernetes pods through containerd's CRI, like runc: it passes
CRI conformance (critest), on containerd 1.7 to 2.4. This page covers
running pods on it once it is [installed](install.md#on-kubernetes), and
what gcr adds there.

## Running a pod on gcr

Choose the RuntimeClass `gcr`:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: demo
spec:
  runtimeClassName: gcr
  containers:
    - name: demo
      image: busybox
      command: [sleep, "3600"]
```

The RuntimeClass schedules the pod onto nodes gcr is ready on
(`gcr.dev/runtime=ready`). In a Deployment, a StatefulSet or a Job, set
`runtimeClassName` in the pod template.

To make gcr the runtime of every pod that names none, set it as
containerd's default runtime (`default_runtime_name = 'gcr'` in the CRI
plugin's `containerd` table), as k0da does. Pods that need runc then ask
for a RuntimeClass of it.

What to expect compared with runc:
- **Hardening is on by default:** a denied set of system calls, supervised
  mounts and Landlock. Most workloads don't notice;
  [what the hardening breaks](compatibility.md) lists what does, and the
  way around each.
- **Node agents** (CNI plugins, eBPF tools, VPNs) are better off on runc,
  or on a handler with hardening off
  ([recipes](configuration.md#recipes)).
- **The kubelet's cgroup driver,** cgroupfs or systemd, is followed: CRI
  says which in the cgroup paths it sends.
- [Differences from runc](runc-differences.md) has the rest.

## Pod annotations

gcr's features are asked for with `gcr.dev/` annotations on the pod (in a
template, on the template's metadata). The ones a pod may set:

| Annotation | What it does |
| --- | --- |
| `gcr.dev/time-travel: execve` | A point on the container's timeline after each exec, to rewind to ([time travel](time-travel.md)). `manual` takes points only when asked. |
| `gcr.dev/host-users: "false"` | Run the pod in gcr's own user namespace ([user namespaces](user-namespaces.md)). |
| `gcr.dev/memory-merge: "true"` | Let the kernel merge identical memory with other pods that opt in. |
| `gcr.dev/migrate-on-evict: "true"` | Move the pod to another node instead of evicting it ([gcr-operator](operator.md)). |

[Configuration](configuration.md#annotations) lists them all. Some are
restricted, because they name host paths or make the node listen:
`gcr.dev/fork-from`, `gcr.dev/migrate-in`. A pod may use them only on a
handler that allows them (`PodAnnotations`), such as `gcr-migrate`, which
only cluster admins and gcr-operator may use.

For user namespaces, `hostUsers: false` (Kubernetes' own) is the better
path for pods that don't need to move or be checkpointed. Pods that
should move can't use it yet: the kubelet picks their range, and a
target would get another one.

## Time travel and snapshots on a node

gcr's command runs on the node, against the pod's container by its
containerd ID, in containerd's `k8s.io` namespace:

```sh
id=$(kubectl get pod demo -o jsonpath='{.status.containerStatuses[0].containerID}' | sed 's#containerd://##')
# on the pod's node:
sudo gcr -n k8s.io timeline $id
sudo gcr -n k8s.io rewind $id 3
sudo gcr -n k8s.io snapshot $id /var/lib/gcr/snapshots/warm
```

[The `gcr` command](cli.md) has the rest.

## Moving a pod

`kubectl gcr migrate` ([install](install.md#the-kubectl-plugin)) moves a
running pod to another node: its processes carry on there, memory and
all, after a pause well under a second.

```console
$ kubectl gcr migrate pod/counter
asking km-worker-0 for counter's user namespace
target counter-b853d1 created; waiting for it to be scheduled
migrating counter from km-worker-0 to km-worker-1 (http://172.20.0.4:17394)
deleting counter
pod/counter moved to km-worker-1 as pod/counter-b853d1 (downtime 331ms)
```

- `--to NODE` picks the node; by default the scheduler does, not the
  pod's own node.
- `-registry REPO` moves it through a checkpoint in a registry instead:
  seconds of downtime, but nothing is needed from the source once it is
  pushed. A StatefulSet's pod moves only this way.
- The pod gets a new name and IP (a StatefulSet's keeps its name), and
  its TCP connections reset. Services select it by its labels, which it
  keeps.

[Live migration](migration.md#between-kubernetes-nodes) says how it works
and what can move. To move pods when nodes are drained, or shut down,
run [gcr-operator](operator.md).
