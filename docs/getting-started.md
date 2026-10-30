# Getting started

This page installs gcr on a node and runs containers with it: from
`ctr`, from Kubernetes on containerd, and from CRI-O. Then it takes a
container through a snapshot and a fork. To try it on a Mac or
another machine without a spare Linux node, use the
[development VM](development.md#a-development-vm).

## Requirements

- Linux 6.12+ with cgroup v2, run as root
- containerd 1.7 or later (tested: 1.7.29, 2.0.7, 2.2.1, 2.4.2), or
  CRI-O with some features missing (below)
- libseccomp (runtime and, to build, headers)
- CRIU for checkpoint/restore; time travel needs the fork with
  `--seccomp-notify-socket` ([time travel](time-travel.md)), and keeping Landlock across a
  restore needs its `--landlock-fs`; GPU checkpoints need its CUDA plugin

## Install

Build on the Linux host (cgo is required) and install both binaries:

```sh
make install          # containerd-shim-gcr-v2 and gcr into /usr/local/bin
make criu             # optional: build and install the CRIU fork from ../criu
```

containerd finds the shim by name on `PATH`: the runtime type
`io.containerd.gcr.v2` maps to `containerd-shim-gcr-v2`.

## With ctr

No configuration is needed; name the runtime type directly:

```sh
ctr image pull docker.io/library/busybox:latest
ctr run -d --runtime io.containerd.gcr.v2 docker.io/library/busybox:latest demo sleep 3600
ctr task ls
ctr task kill -s KILL demo && ctr task rm demo && ctr container rm demo
```

Checkpoint and restore go through containerd as usual:

```sh
ctr container checkpoint --task --rw demo localhost/demo-ckpt:1
ctr task kill -s KILL demo && ctr task rm demo && ctr container rm demo
ctr container restore --live --rw demo localhost/demo-ckpt:1
```

## With Kubernetes (CRI)

Register the runtime with containerd's CRI plugin in
`/etc/containerd/config.toml` and restart containerd:

```toml
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]
  runtime_type = 'io.containerd.gcr.v2'
  # Pass gcr's pod annotations (e.g. time travel) through to the runtime.
  pod_annotations = ['gcr.dev/*']
```

Restart containerd after installing a new gcr: it reads the runtime's
features (user namespaces, idmapped mounts) at startup.

Then add a RuntimeClass and select it from pods:

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gcr
handler: gcr
---
apiVersion: v1
kind: Pod
metadata:
  name: demo
  annotations:
    gcr.dev/time-travel: execve   # optional, see time-travel.md
spec:
  runtimeClassName: gcr
  containers:
    - name: demo
      image: busybox
      command: ["sleep", "3600"]
```

If the kubelet uses the systemd cgroup driver, gcr follows it (CRI sends
`slice:prefix:name` cgroup paths).

containerd 1.7 takes config version 2, where the table is
`[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.gcr]`. Listing a
runtime there replaces the default `runc` entry, so list that too.

gcr is built on containerd 2.4's shim library. containerd 2.3 and later
start it with their shim bootstrap protocol and use the task API's version
3. For older releases (and CRI-O) gcr answers the way shims did before
2.3 and serves the task API's version 2 as well, whose messages are the
same.

## With CRI-O

CRI-O runs containerd shims as runtimes of type `vm` (the way it runs
Kata Containers):

```toml
# /etc/crio/crio.conf.d/10-gcr.conf
[crio.runtime.runtimes.gcr]
runtime_path = "/usr/local/bin/containerd-shim-gcr-v2"
runtime_type = "vm"
runtime_root = "/run/gcr-crio"
# runtime_config_path = "/etc/gcr/gcr.toml"   # gcr's options (docs/configuration.md)
```

The RuntimeClass is the same as above. Pods run with gcr's hardening and
Landlock; exec, logs, resource updates, stats and OOM kills work, and
CRI conformance passes except for port forwarding, which CRI-O does not
implement for `vm` runtimes. Not available under CRI-O:

- **gcr's own [user namespaces](user-namespaces.md)** (`gcr.dev/host-users=false`,
  `UserNamespaces = 'always'`). CRI-O mounts the container's rootfs
  itself, and gcr idmaps the layers containerd hands it. Pods with
  `hostUsers: false` get CRI-O's user namespace, as with any runtime.
- **Snapshots, forks, time travel and live migration.** They need gcr to
  mount the overlay rootfs (to read and replace its writable layer, and
  to stack snapshot layers under it). gcr refuses them with an error
  that says so.
- **Checkpoint and restore through the CRI**, which CRI-O does not
  implement for `vm` runtimes.

## A first fork

With gcr installed and containerd running, start a container that takes
a while to get ready, here a Python process that has built a large
object in memory:

```sh
ctr image pull docker.io/library/python:3.13
ctr run -d --runtime io.containerd.gcr.v2 docker.io/library/python:3.13 warm \
  python3 -c 'import time; data = list(range(1_000_000)); print("ready", flush=True); time.sleep(1e9)'
```

Snapshot it while it runs, and start two containers from the snapshot.
They carry on from the moment of the snapshot, with `data` already
built; the command given to them is not run:

```sh
sleep 2
gcr snapshot warm /var/lib/gcr/snapshots/warm
for i in 1 2; do
  ctr run -d --runtime io.containerd.gcr.v2 \
    --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm \
    docker.io/library/python:3.13 fork-$i true
done
ctr task ls
```

Clean up the containers first, then the snapshot, which gcr refuses to
remove while forks use it:

```sh
for c in fork-1 fork-2 warm; do ctr task rm -f $c; ctr container rm $c; done
gcr snapshot-rm /var/lib/gcr/snapshots/warm
```

Next: [fork](fork.md) for what a fork shares and how to re-seed it,
[time travel](time-travel.md), [live migration](migration.md), and
[configuration](configuration.md) for the runtime handler's options.
