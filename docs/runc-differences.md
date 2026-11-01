# Differences from runc

gcr runs containers with runc's own libcontainer, so most workloads run
on it as they do on runc. This page lists what does not: first what is
different whatever the configuration, then what gcr turns on by default
that runc does not have, with what each breaks and how to turn it off.
[Compatibility](compatibility.md) has the call-by-call detail.

Fork, snapshots, time travel and migration change nothing until a
container asks for them with an annotation.

## Always different

- **Linux 6.12 or later, cgroup v2 only.** runc also runs on cgroup v1
  and older kernels.
- **No `runc` binary.** Containers run inside the shim. Tools that call
  `runc` (`runc list`, `runc exec`) or read runc's state directory do
  not see gcr's containers; gcr keeps its state under `/run/gcr`.
  containerd, `ctr`, `nerdctl`, `crictl` and `kubectl` are unaffected.
- **Forks of runc and CRIU.** gcr's libcontainer comes from
  [makhov/runc](https://github.com/makhov/runc), currently runc v1.5.2,
  so a runc security fix reaches gcr when it is merged there.
  Checkpoints and forks need CRIU; time travel, and keeping Landlock
  across a restore, need [makhov/criu](https://github.com/makhov/criu).
- **Checkpoint and restore of a container whose process has a
  terminal** are refused: CRIU restores stdio as pipes. The container
  itself runs with its terminal as on runc. Its output also goes only
  to containerd's fifo: a `file://` or `binary://` log URI (`ctr
  --log-uri`, nerdctl's log drivers) works for containers without one.
- **About 6 MiB of memory per pod** for the shim, against about 4 MiB
  for runc's. Start and stop are faster (no binary per operation):
  [the numbers](tradeoffs.md#compared-with-runc).
- **Under CRI-O**: no port forwarding, no gcr user namespaces, no time
  travel, no migration into a CRI-O container, and no CRI checkpoints.
  Snapshots and forks work ([CRI-O](crio.md)).
- **Under Docker**: no `docker checkpoint`, and no rewind on a bridge
  network ([Docker](docker.md#networking)).

## On by default

| Protection | Applies to | What it breaks | Turn it off |
| --- | --- | --- | --- |
| [User namespace](user-namespaces.md): root inside is an unprivileged range of host IDs | containers started outside Kubernetes; pods that ask (below) | volumes on NFS, CIFS and FUSE show files owned by `nobody`; device nodes with restrictive modes; raw sockets in a pod's network. Containers that are privileged, add `CAP_NET_ADMIN` in a pod network, or share the host's network, PID or IPC namespace run with host users instead | per container `gcr.dev/host-users=true`; per handler `UserNamespaces = 'never'` |
| [Denied system calls](compatibility.md#denied-system-calls-all-hardened-containers), whatever the seccomp profile | hardened containers: all but privileged ones and `Unconfined` pods | `io_uring` programs (PostgreSQL 18 with `io_method = io_uring`, tokio-uring, TigerBeetle); eBPF tools; `perf`; Kerberos with a `KEYRING:` cache; CRIU or QEMU run inside a container; tools that need the new mount API with no fallback | `Hardening = 'off'` |
| [gcr's own seccomp profile](compatibility.md#gcrs-own-profile-containers-without-a-seccomp-profile) | containers without a profile of their own (`ctr` and the containerd client); `Unconfined` pods only under `Hardening = 'always'` | packet capture (tcpdump), DHCP clients, raw-socket tools (ICMP traceroute, nmap SYN scans, VRRP, OSPF), `AF_ALG` crypto, legacy `ip tunnel`, firewall rules in a namespace the container made itself without `CAP_NET_ADMIN` | give the container a seccomp profile (`RuntimeDefault`); `Hardening = 'off'` |
| [Supervised mounts](compatibility.md#supervised-mounts-containers-that-can-mount) | containers in a user namespace with `CAP_SYS_ADMIN` (Docker-in-Docker) | `fuse`, `binfmt_misc`, `ramfs` and other filesystems outside the allowlist; remounting a read-only mount writable; nested `--privileged` containers | `Hardening = 'off'` |
| [Landlock](compatibility.md#landlock-containers-without-cap_sys_admin-by-default): the container cannot open anything outside its root | containers that are not privileged, lack `CAP_SYS_ADMIN`, and do not join another container's PID namespace | sandboxing tools that mount in a user namespace of their own (bubblewrap, Flatpak, rootless podman and buildah, Nix builds, `unshare -rm` then `mount`); host agents that pass a directory descriptor into a container | `Landlock = 'off'` |
| [Netlink policy](compatibility.md#netlink-policy-hardened-containers-bpf-lsm-enabled) (where the kernel has the BPF LSM) | hardened containers | WireGuard, VXLAN and Geneve, macvlan and ipvlan, bonds, tunnels made over netlink, `vcan`, qdiscs outside the common ones | `NetlinkPolicy = 'off'` |

What still works, and is tested: Docker-in-Docker and `docker build`
unprivileged, `ping`, `mount(8)` of allowed filesystems, debuggers and
`kubectl debug` (`exec` is never confined by Landlock).

A refused call fails with `EPERM`, or `ENOSYS` for the keyring, the new
mount API and `clone3`. [Troubleshooting](troubleshooting.md) says how to
tell which layer refused it.

## Under Kubernetes' defaults

Kubernetes decides two of these per pod, and its defaults turn them off:

- **seccomp `Unconfined`** is the default profile, and CRI conformance
  requires that such a pod get no filter at all. gcr cannot tell the
  default from a pod that asked for it, so it leaves such pods alone,
  netlink policy included.
- **`hostUsers: true`** is the default, and the kubelet sends it as if
  the pod had asked for it, so gcr's user namespace is off.

A pod on a stock node therefore gets Landlock and nothing else from the
table. For the rest:

- set the kubelet's `seccompDefault: true`, so pods without a profile run
  `RuntimeDefault`, which gcr hardens;
- run pods with `hostUsers: false` (Kubernetes' user namespaces, which gcr
  supports), or with the annotation `gcr.dev/host-users: "false"`.

Or have the handler decide regardless of the pod: `Hardening = 'denied'`
or `'always'` and `UserNamespaces = 'always'`. The first fails two CRI
conformance tests; [configuration](configuration.md#hardening) has the
details.

## A runc-compatible handler

To run some workloads exactly as runc would, keeping gcr's process-state
features, give them a handler with every protection off and select it
with a RuntimeClass:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-compat]
  runtime_type = 'io.containerd.gcr.v2'
  pod_annotations = ['gcr.dev/*']
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-compat.options]
    UserNamespaces = 'never'
    Hardening = 'off'
    Landlock = 'off'
    NetlinkPolicy = 'off'
```

What remains different is the first section. Prefer turning off only
the protection a workload needs: `Hardening = 'off'` alone keeps the
user namespace and Landlock, so the container is still not host root and
still cannot reach the host's files. Make a container privileged, or run
it with runc, only when it needs host privilege anyway (eBPF agents, CNI
plugins, packet capture on the host's network).
