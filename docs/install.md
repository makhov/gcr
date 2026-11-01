# Installation

This page installs gcr:

- [on one host](#on-a-host), for `ctr`, Docker or a CRI;
- [on a Kubernetes cluster](#on-kubernetes), with its installer DaemonSet;
- the [`kubectl gcr` plugin](#the-kubectl-plugin);
- [gcr-operator](#gcr-operator), which moves pods instead of restarting
  them;
- the [node shutdown guard](#node-shutdown), which moves pods when their
  node shuts down.

It ends with [upgrading](#upgrading) and [uninstalling](#uninstalling).
[Getting started](getting-started.md) then runs a first container and a
first fork.

## Requirements

Each node needs:

- Linux 6.12 or later, with cgroup v2;
- containerd 1.7 or later (tested: 1.7.29, 2.0.7, 2.2.1, 2.4.2), or
  CRI-O, without time travel and migration;
- libseccomp.

gcr brings its own CRIU ([makhov/criu](https://github.com/makhov/criu)),
which checkpoints, forks, time travel and migration need. It needs glibc
2.35 or later (Ubuntu 22.04, Debian 12, RHEL 10, and later); where the
host has none, as on Alpine, gcr installs a static CRIU, which does
everything but GPU checkpoints.

Some features need more of the kernel, and `gcr doctor` says which a node
lacks ([below](#checking-a-node)).

## On a host

A release is one file per architecture, `gcr`, which carries the rest:

```sh
v=$(curl -fsSLo /dev/null -w '%{url_effective}' https://github.com/makhov/gcr/releases/latest | sed 's#.*/##')
arch=amd64                 # or arm64
curl -fLo gcr https://github.com/makhov/gcr/releases/download/$v/gcr-$v-linux-$arch
curl -fsSL https://github.com/makhov/gcr/releases/download/$v/SHA256SUMS | grep " gcr-$v-linux-$arch\$" | sed "s# gcr-.*# gcr#" | sha256sum -c
chmod +x gcr
sudo ./gcr setup           # -docker to register with dockerd too
```

`gcr setup`:

- installs `gcr` and the shim (`containerd-shim-gcr-v2`) into
  `/usr/local/bin`;
- installs gcr's CRIU and its CUDA plugin into `/var/lib/gcr`, where gcr
  uses them ahead of any `criu` the host has, which it leaves alone;
- registers two runtime handlers with containerd, `gcr` and
  `gcr-migrate` (the targets of migrations), in a drop-in:
  `/etc/containerd/conf.d/gcr.toml`, or `/etc/k0s/containerd.d/gcr.toml`
  on k0s. It makes `config.toml` import `conf.d` if it doesn't, keeping
  the old one as `config.toml.gcr-backup`;
- with `-docker`, registers the runtime with dockerd
  ([Docker](docker.md));
- restarts containerd: running containers carry on. `-no-restart` leaves
  that to you;
- ends with `gcr doctor`.

For distributions that run containerd their own way, such as k3s and
RKE2, `-containerd-config FILE` and `-containerd-unit UNIT` say where its
configuration is and which unit to restart.

Running `gcr setup` again with a newer `gcr` upgrades the host. Running
containers keep the shim they started with.

### Checking a node

`gcr doctor` says what the host has for gcr's features, and what to do
about what it lacks:

```
$ sudo gcr doctor
ok   kernel          7.0.0-38-generic
ok   cgroup v2       unified hierarchy
ok   shim            /usr/local/bin/containerd-shim-gcr-v2 v0.1.6
ok   containerd      v2.4.2, runtime gcr registered
ok   CRIU            /var/lib/gcr/bin/criu 4.2.1 (v4.2.1+gcr.1)
ok   Landlock        ABI 8
ok   netlink policy  BPF LSM
ok   lazy restore    vm.unprivileged_userfaultfd = 1
off  dirty memory    not tracked (arm64): migration is stop-and-copy or post-copy, time travel points are full
off  memory merge    KSM is not running (/sys/kernel/mm/ksm/run is 0, not 1): no gcr.dev/memory-merge
                     -> echo 1 > /sys/kernel/mm/ksm/run
ok   nydus           snapshots in the nydus format, lazy pulls
```

`off` lines are features the node can't use yet; everything else works
without them.

### By hand

To register gcr with containerd yourself, add the runtime to its CRI
plugin and restart containerd:

```toml
# /etc/containerd/config.toml, or a drop-in it imports
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]
  runtime_type = 'io.containerd.gcr.v2'
  # Pass gcr's pod annotations (time travel, forks) through to the runtime.
  pod_annotations = ['gcr.dev/*']
```

containerd 1.7 reads configuration version 2, where the table is
`[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.gcr]`. Listing a
runtime there replaces the default `runc` entry, so list that too. The
`gcr-migrate` handler and its options are in
[Configuration](configuration.md#podannotations). containerd finds the
shim by name on `PATH`: the runtime type `io.containerd.gcr.v2` is
`containerd-shim-gcr-v2`. Restart containerd after installing a new gcr:
it reads the runtime's features at startup.

gcr is built on containerd 2.4's shim library. containerd 2.3 and later
start it with their shim bootstrap protocol and use the task API's version
3. For older releases (and CRI-O) gcr answers the way shims did before
2.3, and serves the task API's version 2 as well.

CRI-O is set up in [Getting started](getting-started.md#with-cri-o). To
build gcr from source, see [Development](development.md).

## On Kubernetes

On a cluster whose nodes run containerd, one manifest installs gcr on
every node:

```sh
kubectl apply -f https://github.com/makhov/gcr/releases/latest/download/gcr.yaml
```

It creates, in the namespace `gcr-system`:

- **The installer DaemonSet** (`gcr-installer`). On each node, a
  privileged pod runs the release's `gcr setup` on the host (as
  [above](#on-a-host)), then labels the node `gcr.dev/runtime=ready`. A
  node missing something gcr needs keeps its pod crash-looping, with
  `gcr doctor`'s report in the pod's log.
- **The RuntimeClass `gcr`,** which pods choose with
  `runtimeClassName: gcr`. It schedules them onto nodes labelled ready.
- **The RuntimeClass `gcr-migrate`,** for the targets of migrations.
  Only cluster admins and gcr-operator may run pods on it (a
  `ValidatingAdmissionPolicy`), plus StatefulSets restoring a pod the
  operator moves. Its pods make a node's shim listen for a migration, or
  restore from a checkpoint on the node.
- **The ConfigMap `gcr-restores`,** which the operator writes and the
  policy reads ([gcr-operator](operator.md#statefulsets)).

Check it:

```sh
kubectl -n gcr-system rollout status daemonset/gcr-installer
kubectl get nodes -l gcr.dev/runtime=ready
kubectl run demo --image=busybox --restart=Never \
  --overrides='{"spec":{"runtimeClassName":"gcr"}}' -- sleep 3600
```

- **k0s** applies `/etc/k0s/containerd.d/gcr.toml` itself, without a
  containerd restart. **k3s and RKE2** need `GCR_SETUP_ARGS` in the
  DaemonSet set to `-containerd-config` and `-containerd-unit` for their
  containerd.
- **Nodes without glibc** (Alpine, k0s's image) get a static CRIU and GNU
  tar: everything but GPU checkpoints works there.
- **[k0da](https://github.com/makhov/k0da)** (k0s in containers) installs
  gcr on its nodes itself with `--gcr`, makes it the default runtime, and
  adds a `runc` RuntimeClass for pods that want runc.

[Kubernetes](kubernetes.md) covers running pods on gcr.

## The kubectl plugin

`kubectl gcr migrate` moves a running pod to another node, by hand
([Kubernetes](kubernetes.md#moving-a-pod)). Install it on your PATH, where
kubectl finds plugins:

```sh
v=$(curl -fsSLo /dev/null -w '%{url_effective}' https://github.com/makhov/gcr/releases/latest | sed 's#.*/##')
os=darwin arch=arm64       # or linux, amd64
curl -fLo kubectl-gcr https://github.com/makhov/gcr/releases/download/$v/kubectl-gcr-$v-$os-$arch
chmod +x kubectl-gcr && sudo mv kubectl-gcr /usr/local/bin/
kubectl gcr migrate -h
```

It needs a cluster admin's credentials: it creates pods on `gcr-migrate`.

## gcr-operator

gcr-operator turns evictions (`kubectl drain`, the cluster autoscaler,
node upgrades) of pods that opt in into live migrations. With a registry,
it also moves StatefulSets' pods, and restores pods whose node shuts
down. It needs `gcr.yaml` installed, and two or more nodes with gcr.

```sh
kubectl apply -f https://github.com/makhov/gcr/releases/latest/download/gcr-operator.yaml
kubectl -n gcr-system rollout status deployment/gcr-operator
```

[gcr-operator](operator.md) covers opting pods in, the registry, what
moves and how, and what to do when something doesn't.

## Node shutdown

For pods to move when their node shuts down (`systemctl poweroff`, a spot
reclaim through the kubelet's graceful node shutdown), each node also
runs `gcr shutdown-guard`, and gcr-operator runs with a registry.
[gcr-operator: node shutdown](operator.md#node-shutdown) sets it up.

## Upgrading

- **One host:** run the new release's `gcr setup`.
- **Kubernetes:** apply the new release's `gcr.yaml`, and
  `gcr-operator.yaml` if you run it. The installer DaemonSet upgrades one
  node at a time. Running pods keep the shim they started with; new pods
  get the new one.

## Uninstalling

On Kubernetes:

```sh
kubectl delete -f https://github.com/makhov/gcr/releases/latest/download/gcr-operator.yaml
kubectl delete -f https://github.com/makhov/gcr/releases/latest/download/gcr.yaml
```

Deleting the DaemonSet takes the `gcr.dev/runtime=ready` label off the
nodes and leaves gcr installed on them. Pods on `gcr` keep running.

On a host, after the containers on gcr are gone:

```sh
sudo rm /etc/containerd/conf.d/gcr.toml        # or /etc/k0s/containerd.d/gcr.toml
sudo systemctl restart containerd
sudo rm -rf /usr/local/bin/gcr /usr/local/bin/containerd-shim-gcr-v2 /var/lib/gcr
```

`/var/lib/gcr` also holds snapshots and checkpoints; keep what you still
want to fork from.
