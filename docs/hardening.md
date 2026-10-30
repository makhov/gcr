# Hardening

Whatever seccomp profile a workload brings (or none, as Kubernetes pods
default to), gcr refuses calls behind most container escapes and kernel
exploits reachable from containers:

- with EPERM: `io_uring_*`, `bpf`, `perf_event_open`, `userfaultfd`,
  module loading and kexec, `open_by_handle_at`;
- with ENOSYS, as if the kernel lacked it: the keyring (`add_key`,
  `keyctl`, `request_key`), which is not namespaced (runc and other callers
  carry on without a session keyring then), and the new mount API
  (`fsopen`, `fsconfig`, `fsmount`, `fspick`, `open_tree`, `move_mount`,
  `mount_setattr`): `mount(8)` from util-linux 2.40 on falls back to
  `mount(2)`, which the supervisor checks, only on ENOSYS;
- when the container has no profile of its own, also: `AF_PACKET`,
  `AF_VSOCK`, `AF_KEY` and `AF_ALG` sockets, and raw IP sockets (`ping` uses
  ICMP datagram sockets, which gcr opens); `clone3` returns ENOSYS so libc
  falls back to `clone`, whose flags the filter can see. User namespaces
  the container makes itself are guarded (below).

A container in a user namespace that can mount (it has `CAP_SYS_ADMIN`
without being privileged; there that capability only reaches the
namespace) has `mount(2)` handed to gcr's supervisor. The supervisor allows `proc`,
`sysfs`, `tmpfs`, `devpts`, `mqueue`, `cgroup2` and `overlay`, and bind
mounts, remounts and propagation changes; other filesystems the kernel would
allow in a user namespace (`ramfs`, `fuse`, ...) are refused, and so is a
remount that would make a read-only mount writable. It performs the
mount itself, from the arguments it copied and checked (so another thread
cannot swap them after the check), in the caller's namespaces, with its
IDs, capabilities and AppArmor profile, so the kernel's own checks are the
caller's. In a
user namespace the supervisor also sees `setns(2)`: joining the network
namespace a process is already in is a no-op that gcr answers itself
(dockerd does it, and the kernel would want rights over a pod network
namespace's owner); all other calls run as made. The filter is inherited,
so all of this holds for nested containers too, whatever profile they ask
for.

Privileged means `CAP_SYS_ADMIN` and access to all devices (what
`--privileged` gives); `CAP_SYS_ADMIN` alone is not.

**Nested user namespaces.** Chrome's sandbox (Puppeteer, Playwright),
bubblewrap and rootless podman/buildah make user namespaces of their own.
Root in one can reach kernel code an unprivileged process cannot, so in
gcr's own profile they are guarded rather than refused
(`NestedUserNamespaces = 'guarded'`, the default; tools that mount in
them, such as bubblewrap and rootless podman, also need `Landlock = 'off'`,
see [compatibility](compatibility.md)):

- mounts go through the supervisor's filesystem allowlist, also in nested
  namespaces;
- netfilter (`NETLINK_NETFILTER` sockets) is refused to containers without
  `CAP_NET_ADMIN`; nf_tables is where most exploits reached this way go;
- the rest of the filter (raw and packet sockets, `io_uring`, `bpf`, the
  keyring) holds inside as well, and Landlock still refuses a confined
  container any mount.

What root in a new network namespace can ask the kernel to create there
is limited by the netlink policy (next section).
`NestedUserNamespaces = 'deny'` refuses `CLONE_NEWUSER` instead. A workload's own seccomp profile decides for
itself; containerd's `RuntimeDefault` refuses it, on runc as well. Ubuntu
hosts restrict unprivileged user namespaces with AppArmor
(`kernel.apparmor_restrict_unprivileged_userns`), which takes the
capabilities away inside unless a profile allows it, as with runc.

Privileged containers are exempt. Kubernetes pods with a profile
(`RuntimeDefault`, `Localhost`) get the denied set merged in. Pods that run
`Unconfined` stay unconfined by default, as CRI conformance requires
(`Seccomp: 0`). That is Kubernetes' own default, which a runtime cannot
tell from an explicit choice, so on nodes running gcr set the kubelet's
`seccompDefault: true` (GA since Kubernetes 1.27):

```yaml
# KubeletConfiguration
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
seccompDefault: true
```

Every pod without a profile then runs `RuntimeDefault`, and gets gcr's
denied set with it; only pods that ask for `Unconfined` stay unconfined.
Set the policy per runtime handler, in the runtime's options:
`Hardening = 'standard'` (default), `'denied'` (also gives unconfined pods
the denied set), `'always'` (gives them gcr's whole profile), or `'off'`.
`denied` and `always` fail CRI conformance's two tests of unconfined
seccomp. A workload cannot weaken any of them.
`test/e2e/hardening` probes every refused call.

## Netlink policy

Root in a network namespace, including one a container makes for itself,
configures it over netlink, and each kind of link, qdisc or filter it may
create is kernel code of its own (bonding, macvlan, VXLAN, tunnels, the
teql scheduler, ...). seccomp sees the socket's protocol, not the kind in
the message, so gcr decides in the kernel: a BPF LSM program on
`netlink_send` checks every `NETLINK_ROUTE` request from a gcr container
and allows

- link kinds `veth`, `dummy`, `bridge` and `vlan` (what container
  networking inside a container needs: Docker-in-Docker makes veth pairs
  and a bridge);
- qdisc, class and filter kinds `pfifo_fast`, `pfifo`, `bfifo`, `fq_codel`,
  `fq`, `codel`, `sfq`, `tbf`, `htb`, `prio`, `mq`, `cake`, `netem`,
  `ingress`, `clsact`, `u32`, `flower`, `matchall`, `basic`, `fw`.

Other kinds are refused with EPERM; messages that name no kind (addresses,
routes, neighbours, bringing links up) pass, as do other netlink protocols
(netfilter is seccomp's business, above). It covers the cgroups of hardened
containers, nested containers included, and nothing else on the node; the
program and its maps are pinned under `/sys/fs/bpf/gcr` and shared by all
shims. Set `NetlinkPolicy = 'off'` per runtime handler to skip it.

**The node needs the BPF LSM enabled:** `bpf` in the kernel's `lsm=` list
(`CONFIG_BPF_LSM=y` is common, the boot parameter is not; see
`hack/lima.yaml`). Without it the shim logs once and containers run with
seccomp alone. A GitHub-hosted runner cannot change its kernel command
line, so the tests that check the policy's refusals run on a self-hosted
runner instead (`hack/runner-setup.sh`, the `bpf-lsm` workflow); they skip
elsewhere, unless `GCR_E2E_REQUIRE_BPF` is set, which turns the skip into
a failure.

## Landlock

gcr confines each container's processes with Landlock to the container's
root filesystem: full access beneath its `/`, nothing outside it. Inside,
this changes nothing; the container's mounts and permissions decide. What
it stops is reaching the host's files through anything that leads outside:
a leaked host file descriptor or working directory (runc's CVE-2024-21626),
a `/proc/<pid>/root` link into another mount namespace, or whatever a
mount namespace bug would expose. Pipes, sockets and namespace files live
on internal filesystems Landlock does not restrict.

- **Applied in the container's init,** through an init hook in gcr's runc
  fork. That happens once the root filesystem is final and before
  capabilities drop, so no `no_new_privs` is needed and setuid binaries
  keep working. Everything the init starts inherits it.
- **Not for `exec`.** Landlock only lets a process trace or inspect
  (`/proc/<pid>/fd`, `environ`, `root`) processes in its own or a nested
  domain, and an exec would get a domain of its own. Debuggers, `strace -p`,
  `jcmd` and the like would stop working on the container.
- **Not for containers that can mount** (`CAP_SYS_ADMIN`, Docker-in-Docker):
  Landlock forbids mount changes. Privileged containers are exempt too.
- **Kept across checkpoint/restore** with the CRIU fork's `--landlock-fs`.
  The kernel cannot report a process's Landlock domain, so CRIU cannot dump
  it; gcr asks for the same confinement again. With a CRIU without it, gcr
  logs that restored containers are not confined.
- **Not for containers that join another container's PID namespace**:
  ephemeral debug containers (`kubectl debug --target`) and
  `shareProcessNamespace` pods. They join it to trace and inspect the
  processes there, which a sibling Landlock domain could not. The container
  they join stays confined.

Set it per runtime handler: `Landlock = 'standard'` (default) or `'off'`.
Kernels without Landlock run containers unconfined.
`test/e2e/landlock` passes a host directory descriptor into a container
and checks it leads nowhere, before and after a restore.

## Docker-in-Docker

dockerd runs in a gcr pod with a user namespace without being privileged:

- **User namespace from Kubernetes** (`hostUsers: false`, or CRI's
  `userns_options`). containerd then creates the pod's network namespace
  inside it, and dockerd can configure it. A pod in gcr's own user
  namespace (`gcr.dev/host-users: "false"`, `UserNamespaces = 'always'`)
  has a network namespace the host owns, which dockerd cannot manage.
- **Capabilities `SYS_ADMIN` and `NET_ADMIN`.** In the user namespace they
  reach only the pod's own namespaces. Mounts go through the supervisor
  (above), and `pivot_root`, which default profiles leave out, is allowed.
- **A delegated cgroup.** gcr gives a container in a user namespace with
  `CAP_SYS_ADMIN` its cgroup (the directory, `cgroup.procs`,
  `cgroup.threads`, `cgroup.subtree_control`) and a writable
  `/sys/fs/cgroup`; its cgroup namespace keeps it inside that subtree.
- **seccomp `RuntimeDefault`.** The hardening and supervision above are
  merged into it.
- **AppArmor `Unconfined` on AppArmor hosts**, as for privileged DinD. Under
  the default CRI profile the nested runtime's processes end up with labels
  that profile does not let signal each other.
- **dockerd flags `--iptables=false --bridge=none`.** Nested containers
  share the pod's network (`--network host`).

Nested `docker run` and `docker build` work. Nested `--privileged` does not:
the pod's `/sys` is read-only, and the kernel refuses a new read-write
sysfs under it. `test/e2e/dind` runs it all through CRI.
