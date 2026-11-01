# Installation

## Pick a path

| You have | Do this | Takes |
| --- | --- | --- |
| A Linux machine | [One host](#one-host) | 2 minutes |
| A Mac, or no spare machine | [A VM on your laptop](#a-vm-on-your-laptop) | 5 minutes |
| Nothing, and you want Kubernetes | [A throwaway cluster](#a-throwaway-cluster) | 5 minutes |
| A Kubernetes cluster already | [Your own cluster](#your-own-cluster) | 2 minutes |

Then [Quickstart](getting-started.md) runs a container, forks it, and
undoes a command inside one.

Every node needs:

- Linux 6.12 or later, with cgroup v2;
- containerd 1.7 or later (tested on 1.7.29, 2.0.7, 2.2.1 and 2.4.2), or
  [CRI-O](crio.md), which cannot do time travel or migration;
- libseccomp.

Snapshots, forks, time travel and migration need CRIU. gcr brings its own
([makhov/criu](https://github.com/makhov/criu)) and installs it where only
gcr uses it, leaving any `criu` on the host alone. It wants glibc 2.35 or
later (Ubuntu 22.04, Debian 12, RHEL 10 and newer); where there is none,
as on Alpine, gcr installs a static build that does everything except GPU
checkpoints.

Some features need more from the kernel than others, and `gcr doctor`
says what a node has ([below](#checking-a-node)).

## One host

A release is one file per architecture that carries the shim, gcr's CRIU
and its CUDA plugin inside it:

```sh
arch=$(uname -m | sed -e s/x86_64/amd64/ -e s/aarch64/arm64/)
v=$(curl -fsSLo /dev/null -w '%{url_effective}' https://github.com/makhov/gcr/releases/latest | sed 's#.*/##')
curl -fLo gcr "https://github.com/makhov/gcr/releases/download/$v/gcr-$v-linux-$arch"
curl -fsSL "https://github.com/makhov/gcr/releases/download/$v/SHA256SUMS" | grep " gcr-$v-linux-$arch\$" | sed "s# gcr-.*# gcr#" | sha256sum -c
chmod +x gcr

sudo ./gcr setup            # add -docker to register with Docker too
```

That one command:

- installs `gcr` and `containerd-shim-gcr-v2` into `/usr/local/bin`;
- unpacks gcr's CRIU and CUDA plugin into `/var/lib/gcr`;
- registers two runtime handlers with containerd, `gcr` and
  `gcr-migrate` (the one migrations land on), in the drop-in
  `/etc/containerd/conf.d/gcr.toml`. If `config.toml` does not import
  `conf.d`, gcr makes it, keeping the old file as `config.toml.gcr-backup`;
- with `-docker`, adds runtime `gcr` to `/etc/docker/daemon.json`
  ([Docker](docker.md));
- restarts containerd, and Docker if it changed it. Running containers
  carry on. `-no-restart` leaves the restart to you;
- runs `gcr doctor`.

Containers keep using runc unless you ask for gcr, so installing it
changes nothing that is already running.

**Distributions that run containerd their own way** (k3s, RKE2) keep its
configuration elsewhere: `-containerd-config FILE` and `-containerd-unit
UNIT` say where, and which unit to restart. On **k0s** gcr writes
`/etc/k0s/containerd.d/gcr.toml`, which k0s applies itself.

To upgrade, run a newer release's `gcr setup` again. Running containers
keep the shim they started with.

### Registering it by hand

If you would rather not let `gcr setup` edit containerd's configuration,
add the handler yourself and restart containerd:

```toml
# /etc/containerd/config.toml, or a drop-in it imports
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]
  runtime_type = 'io.containerd.gcr.v2'
  # Let gcr's pod annotations (time travel, forks) through to the runtime.
  pod_annotations = ['gcr.dev/*']
```

- **containerd 1.7** reads configuration version 2, where the table is
  `[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.gcr]`.
  Naming any runtime there replaces the default `runc` entry, so list
  `runc` as well.
- containerd finds the shim on `PATH` by name: runtime type
  `io.containerd.gcr.v2` means `containerd-shim-gcr-v2`.
- Restart containerd after installing a new gcr. It reads the runtime's
  features at startup.
- The `gcr-migrate` handler and the handler options live in
  [Configuration](configuration.md).

## A VM on your laptop

On a Mac (or anywhere with [Lima](https://lima-vm.io)), this brings up
Ubuntu 25.10 with containerd and Docker, which is all gcr needs:

```sh
brew install lima
limactl start --name=gcr https://raw.githubusercontent.com/makhov/gcr/main/hack/try-vm.yaml
limactl shell gcr
```

From a checkout, `limactl start --name=gcr hack/try-vm.yaml` does the
same. Inside the VM, carry on from
[Install gcr](getting-started.md#1-install-gcr), putting `sudo` in front
of the `docker` commands as well as the `gcr` ones: the VM's user is not
in the `docker` group, and gcr's control sockets are root's either way.

`limactl stop gcr` when you are done, `limactl delete gcr` to get rid of
it.

To build gcr rather than run a release, `hack/lima.yaml` is the
development VM instead ([Development](development.md#a-development-vm)).

## A throwaway cluster

[k0da](https://github.com/makhov/k0da) runs k0s in containers, like kind,
and installs gcr itself:

```sh
k0da create --gcr                  # --gcr-operator to add gcr-operator
kubectl get runtimeclass
```

Every node gets gcr as its default runtime, with a `runc` RuntimeClass
for pods that want runc, and k0s installs gcr's Helm chart into
`gcr-system`. [Kubernetes](kubernetes.md) runs the first pod on it.

The containers need a host kernel that meets gcr's requirements, so on a
Mac run k0da inside a Linux VM rather than against Docker Desktop.

## Your own cluster

On a cluster whose nodes run containerd, the Helm chart installs gcr on
every node:

```sh
helm install gcr oci://ghcr.io/makhov/charts/gcr \
  --namespace gcr-system --create-namespace
  # --set operator.enabled=true --set operator.registry=REPO   for gcr-operator
```

Without Helm, the same as plain manifests:

```sh
kubectl apply -f https://github.com/makhov/gcr/releases/latest/download/gcr.yaml
```

Either way you get, in `gcr-system`:

- **the installer DaemonSet** (`gcr-installer`): a privileged pod per node
  that runs the release's `gcr setup` on the host, then labels the node
  `gcr.dev/runtime=ready`. A node missing something keeps its pod
  crash-looping with `gcr doctor`'s report in the log, which is where to
  look when a node never goes ready;
- **the RuntimeClass `gcr`**, which pods ask for with `runtimeClassName:
  gcr`. It schedules them onto labelled nodes only;
- **the RuntimeClass `gcr-migrate`**, where migrations land. A
  `ValidatingAdmissionPolicy` keeps it to cluster admins, gcr-operator,
  and StatefulSets restoring a pod the operator moved;
- **the ConfigMap `gcr-restores`**, which the operator writes and that
  policy reads ([gcr-operator](operator.md#statefulsets)).

Check it:

```sh
kubectl -n gcr-system rollout status daemonset/gcr-installer
kubectl get nodes -l gcr.dev/runtime=ready
kubectl run demo --image=busybox --restart=Never \
  --overrides='{"spec":{"runtimeClassName":"gcr"}}' -- sleep 3600
```

The [chart's values](https://github.com/makhov/gcr/tree/main/charts/gcr)
cover the operator and its registry, which nodes to install on, and
`gcr setup`'s options. A chart release installs the gcr release that was
current when it was made; `helm upgrade` to a newer chart upgrades gcr a
node at a time.

Distribution notes:

- **k3s and RKE2** need `gcr setup` pointed at their containerd:
  `--set runtime.installer.setupArgs='-containerd-config /var/lib/rancher/k3s/agent/etc/containerd/config.toml -containerd-unit k3s'`,
  or `GCR_SETUP_ARGS` in the DaemonSet.
- **k0s** applies its drop-in itself, with no containerd restart.
- **Nodes without glibc** (Alpine, k0s's image) get the static CRIU:
  everything but GPU checkpoints.
- **k0da** installs gcr itself ([above](#a-throwaway-cluster)), so set
  `runtime.installer.enabled=false` if you add the chart by hand.

## Checking a node

`gcr doctor` is the first thing to run when something does not work. It
reports what the host can do, and what to do about what it cannot:

```console
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

| | Means |
| --- | --- |
| `ok` | Works. |
| `off` | This node cannot do that one feature. Everything else works, and the line says what you lose; where there is a fix, it is the `->` line. |
| `MISSING` | gcr will not work properly here. Fix it before running containers. |

It exits non-zero if anything is `MISSING`, which is what makes it useful
in a node's startup checks.

## The kubectl plugin

`kubectl gcr migrate` moves a running pod to another node by hand
([Kubernetes](kubernetes.md#moving-a-pod)). Put it anywhere on your
`PATH`:

```sh
v=$(curl -fsSLo /dev/null -w '%{url_effective}' https://github.com/makhov/gcr/releases/latest | sed 's#.*/##')
os=darwin arch=arm64       # or linux, amd64
curl -fLo kubectl-gcr "https://github.com/makhov/gcr/releases/download/$v/kubectl-gcr-$v-$os-$arch"
chmod +x kubectl-gcr && sudo mv kubectl-gcr /usr/local/bin/
kubectl gcr migrate -h
```

It creates pods on `gcr-migrate`, so it needs a cluster admin's
credentials.

## gcr-operator

gcr-operator turns evictions of pods that opt in (a `kubectl drain`, the
cluster autoscaler, a node upgrade) into live migrations. With a registry
it also moves StatefulSets' pods, and brings back pods whose node shut
down. It needs gcr installed and at least two nodes with it.

With the chart, `--set operator.enabled=true --set operator.registry=REPO`.
Without:

```sh
kubectl apply -f https://github.com/makhov/gcr/releases/latest/download/gcr-operator.yaml
kubectl -n gcr-system rollout status deployment/gcr-operator
```

[gcr-operator](operator.md) covers opting pods in, the registry, what
moves and what does not. For pods to survive a node shutting down, each
node also runs `gcr shutdown-guard`
([node shutdown](operator.md#node-shutdown)).

## Upgrading

- **One host:** run the new release's `gcr setup`.
- **Kubernetes:** `helm upgrade`, or apply the new `gcr.yaml` (and
  `gcr-operator.yaml`). The DaemonSet rolls one node at a time.

Pods already running keep the shim they started with; new ones get the
new shim. Upgrade `gcr.yaml` and `gcr-operator.yaml` together: a new
operator left with an older admission policy restores only StatefulSets'
and bare pods after a node shutdown
([node shutdown](operator.md#node-shutdown)).

## Uninstalling

On Kubernetes:

```sh
helm uninstall gcr -n gcr-system
# or: kubectl delete -f .../gcr-operator.yaml -f .../gcr.yaml
```

That leaves gcr on the nodes, and pods on it keep running. The nodes may
keep their `gcr.dev/runtime=ready` label, since the installer's
permission to remove it goes with the DaemonSet:
`kubectl label nodes --all gcr.dev/runtime-` takes it off.

On a host, once no containers are using gcr:

```sh
sudo rm /etc/containerd/conf.d/gcr.toml        # or /etc/k0s/containerd.d/gcr.toml
sudo systemctl restart containerd
sudo rm -rf /usr/local/bin/gcr /usr/local/bin/containerd-shim-gcr-v2 /var/lib/gcr
```

`/var/lib/gcr` also holds your snapshots and checkpoints. Keep what you
still want to fork from.
