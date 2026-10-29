# What gcr's hardening breaks

Most containers run on gcr exactly as they do on runc: it passes CRI
conformance, and Docker-in-Docker, `docker build` and `ping` work. This
page lists what does not, grouped by the layer that refuses it, with the
way around each. [Configuration](configuration.md) describes the knobs.

**Which programs are named here.** The tests check every refusal: that
the call fails, with which error, and that it succeeds with the layer off.
The programs named below are known to use those calls. They have not each
been run on gcr, unless a line says so. A refused call shows up as `EPERM`
("Operation not permitted"), or `ENOSYS` ("Function not implemented")
where noted. Well-written programs fall back, and others exit with that
error.

**Under Kubernetes defaults most of this does not apply.** A pod with the
default `Unconfined` seccomp profile and `hostUsers` left at true gets no
seccomp hardening and no user namespace from gcr, only Landlock. With the
kubelet's `seccompDefault: true`, pods without a profile run
`RuntimeDefault` instead and get the denied set. The seccomp sections
apply to:

- containers started outside Kubernetes;
- pods with a seccomp profile, which every pod has under `seccompDefault`;
- `Unconfined` pods under `Hardening = 'denied'` (the denied set only);
- every pod under `Hardening = 'always'`.

## Denied system calls (all hardened containers)

These are refused whatever the container's seccomp profile allows.

| Refused | Who uses it | Way around |
| --- | --- | --- |
| `io_uring_*` | TigerBeetle, glommio and tokio-uring programs, PostgreSQL 18 with `io_method = io_uring`. libuv (Node.js) falls back to threads. | Configure the program for epoll, aio or a thread pool where it can. Otherwise use a handler with `Hardening = 'off'`. |
| `bpf` | eBPF tools and agents: bpftrace, bcc, Cilium, Tetragon, Falco's eBPF probe. These need `CAP_BPF` or `CAP_SYS_ADMIN` in any case. | Run them privileged (not hardened), or with runc. |
| `perf_event_open` | `perf`; profilers in their hardware-counter mode, such as async-profiler's default `cpu` mode. containerd's `RuntimeDefault` refuses it too. | Use the profiler's timer mode (async-profiler `-e itimer`), or profile from the host. |
| `userfaultfd` | CRIU's lazy pages or QEMU post-copy migration run inside a container. Firecracker snapshot loading with its UFFD backend. | Use a handler with `Hardening = 'off'`, or keep these tools on the host. |
| New mount API: `fsopen`, `fsconfig`, `fsmount`, `fspick`, `open_tree`, `move_mount`, `mount_setattr`, refused with `ENOSYS` | Tools that need it with no fallback: nested runtimes making idmapped mounts, systemd features built on it. `mount(8)` from util-linux 2.40+ falls back to `mount(2)` on `ENOSYS`, and that is tested with Debian trixie's 2.41. Docker-in-Docker works, also tested. | Mounts through `mount(2)` work as the supervisor allows. For the rest, use `Hardening = 'off'`. |
| `init_module`, `finit_module`, `delete_module`, `kexec_*` | Nothing unprivileged can use them anyway. | — |
| `open_by_handle_at` | Userspace NFS servers (nfs-ganesha), some fanotify-based file watchers. These need `CAP_DAC_READ_SEARCH` anyway. | Run privileged or with runc. |
| `add_key`, `keyctl`, `request_key`: `ENOSYS` | Kerberos with a `KEYRING:` credential cache, which some distributions' `krb5.conf` defaults to. fscrypt and ecryptfs key management, `cifs-utils`. runc and systemd carry on without a keyring. | Kerberos: `KRB5CCNAME=FILE:/tmp/krb5cc_$(id -u)` or `default_ccache_name = FILE:...`. Otherwise `Hardening = 'off'`. |

## gcr's own profile (containers without a seccomp profile)

These apply only when gcr supplies the whole profile: containers outside
Kubernetes, and `Unconfined` pods under `Hardening = 'always'`. A
container that brings a profile is left to it here.

| Refused | Who uses it | Way around |
| --- | --- | --- |
| `AF_PACKET` sockets | tcpdump, Wireshark/tshark, DHCP clients (dhclient, udhcpc, dhcpcd), arping, lldpd, keepalived's gratuitous ARP | Capture from the host, or give the container a seccomp profile of its own (`RuntimeDefault` lets `CAP_NET_RAW` decide). |
| Raw IP sockets (`SOCK_RAW`) | traceroute's ICMP and TCP modes, nmap SYN scans, hping3, keepalived (VRRP), routing daemons speaking OSPF or PIM (FRR, BIRD). `ping` works: gcr opens ICMP datagram sockets for it. | UDP traceroute (the default) or `tracepath`, nmap `-sT`. Or a profile of the container's own. |
| `AF_VSOCK` | VM guest agents (qemu-guest-agent, Kata's agent), Nitro Enclaves tools | Not useful in a container in practice. |
| `AF_KEY` | Legacy IPsec over PF_KEY (ipsec-tools/racoon, strongSwan's `kernel-pfkey` plugin). strongSwan's default netlink path is unaffected. | Use the netlink XFRM path. |
| `AF_ALG` | The kernel crypto API from user space: iwd, OpenSSL's `afalg` engine, libkcapi, some cryptsetup operations | Use user-space crypto (OpenSSL's default). |
| `clone3`: `ENOSYS` | glibc, Go, systemd and crun fall back to `clone`. | Only programs that call `clone3` with no fallback would notice. |
| Tunnels made by `ioctl` (`SIOCADDTUNNEL`, `SIOCCHGTUNNEL` and the PRL calls on the `ipip`, `gre` and `sit` devices), with `CAP_NET_ADMIN` | Old `ip tunnel add` (iproute2 before netlink support), legacy tunnel scripts | `ip link add ... type ipip` and the like over netlink, which the netlink policy decides. |
| `NETLINK_NETFILTER` without `CAP_NET_ADMIN` (`NestedUserNamespaces = 'guarded'`) | Firewall rules in a namespace the container made itself: rootless Docker, and podman networks that need nftables or iptables (netavark bridge networks). podman's default rootless networking (pasta) needs none. | Give the container `CAP_NET_ADMIN`, or use host networking for the nested containers. |
| `CLONE_NEWUSER` (`NestedUserNamespaces = 'deny'`) | Chrome's and Chromium's sandbox (Puppeteer, Playwright, Electron), bubblewrap, Flatpak, rootless podman and buildah, `unshare -r`, Nix and Bazel sandboxes | Use the default, `guarded`. Or run Chrome with `--no-sandbox` and forgo its sandbox. |

## Landlock (containers without `CAP_SYS_ADMIN`, by default)

Landlock adds nothing inside the container's root, but forbids two
things.

- **Any mount, also in a user namespace the container makes.** Tools that
  build a sandbox by mounting in a user namespace of their own fail at
  their first mount. The tests check this: a nested `tmpfs` mount is
  refused under Landlock and allowed without it. Affected:
  - bubblewrap and Flatpak;
  - rootless podman and buildah;
  - `unshare -rm` followed by `mount`;
  - Nix's build sandbox;
  - Bazel's `linux-sandbox` (Bazel falls back to its process-wrapper
    strategy).

  Chrome's sandbox uses a user namespace and `chroot` without mounting,
  so it should be unaffected; this has not been tested on gcr.

  **Way around:** a handler with `Landlock = 'off'`. In gcr's own profile,
  mounts in nested namespaces then go through the supervisor's allowlist
  (see the next section). Alternatively, give the container
  `CAP_SYS_ADMIN`, which leaves it out of Landlock and sends its mounts to
  the supervisor. Do that only together with a user namespace, where the
  capability reaches no further than the container's own namespaces.
- **Opening files relative to anything outside the container's root.**
  This means a host directory or working directory passed in, or
  `/proc/<pid>/root` of a process in another mount namespace. It is the
  point of Landlock (CVE-2024-21626), but a host agent that passes a
  directory descriptor into a container for it to open files under will
  fail. Descriptors that are already open (pipes, sockets, files) keep
  working.

`exec`'d processes, debug containers and `shareProcessNamespace` pods are
never confined, so debuggers and `kubectl debug` keep working.

## Supervised mounts (containers that can mount)

Containers in a user namespace with `CAP_SYS_ADMIN`, and nested
namespaces in gcr's own profile without Landlock, may mount `proc`,
`sysfs`, `tmpfs`, `devpts`, `mqueue`, `cgroup2` and `overlay`, plus bind
mounts, remounts and propagation changes, under the container's AppArmor
profile. A remount may not make writable what is read-only (a read-only
volume, or `readOnlyRootFilesystem`), whoever mounted it. Containers
without a user namespace are not supervised: the kernel and the LSM
decide, as with runc. Refused:

| Refused | Who uses it | Way around |
| --- | --- | --- |
| `fuse` | sshfs, rclone mount, s3fs, gcsfuse, fuse-overlayfs (older rootless podman storage), AppImages | Mount on the host and bind it in. AppImages: `--appimage-extract-and-run`. Rootless podman: native overlay (Linux 5.13+), which is allowed. |
| `binfmt_misc` | Registering qemu-user emulators for multi-architecture builds (`docker run --privileged tonistiigi/binfmt`) | Register on the host; the registration is node-wide anyway. |
| `ramfs` | Scratch space without a size limit | `tmpfs`. |
| Others: `hugetlbfs`, `bpf`, `tracefs`, `debugfs`, block-device filesystems, `nfs`, `cifs` | DPDK (`hugetlbfs`), tracing tools, loop mounts. The kernel refuses most of these in a user namespace anyway. | Mount on the host and bind it in. |

Nested `--privileged` containers (in Docker-in-Docker) do not work. The
pod's `/sys` is read-only, and the kernel refuses a new read-write sysfs
under it.

## Netlink policy (hardened containers, BPF LSM enabled)

Containers may create links of kind `veth`, `dummy`, `bridge` and `vlan`,
and the common qdiscs, classes and filters (the README lists them). Other
kinds are refused:

| Refused | Who uses it |
| --- | --- |
| `wireguard` | wg-quick, kernel-mode WireGuard VPNs (Tailscale and others using `tun` are unaffected) |
| `vxlan`, `geneve` | Overlay networks: flannel's vxlan backend, Docker Swarm overlays in Docker-in-Docker |
| `macvlan`, `ipvlan` | Docker macvlan and ipvlan networks, some CNI plugins |
| `bond`, `team`, `vrf`, `macsec`, tunnels (`gre`, `ipip`, `sit`, `ip6tnl`), `xfrm` | Network appliances, routing labs, IPsec route-based VPNs |
| `vcan`, `vxcan` | CAN bus testing (automotive CI) |
| qdiscs and filters outside the list (`hfsc`, `drr`, `red`, `pie`, `mqprio`, `taprio`, `bpf`, ...) | Traffic shaping setups beyond the common ones |

`tun` and `tap` devices are made through `/dev/net/tun`, not through these
messages, and work. Addresses, routes and bringing links up are not
checked. tc actions are not checked.

**Way around:** the allowlist is node-wide and fixed in the code. Run
such containers under a handler with `NetlinkPolicy = 'off'` (or
`Hardening = 'off'`). Network agents that are privileged are not hardened
and not covered.

## gcr's user namespaces (where they apply)

Root in the container is an unprivileged range of host IDs. That changes
some things:

- **Volumes on filesystems without idmapped mounts** (NFS, CIFS, FUSE,
  overlay): files show up owned by `nobody` (65534), and files the
  container writes are owned by its range's host IDs. Writes may be
  refused where the volume expects real IDs. ext4, xfs, btrfs, tmpfs, f2fs
  and vfat are idmapped and look normal.
- **Raw sockets in a pod's network namespace** fail even with
  `CAP_NET_RAW`, because the namespace belongs to the host's user
  namespace. Kubernetes' `hostUsers: false` avoids this: containerd
  creates the pod's network namespace inside the user namespace.
- **`CAP_NET_ADMIN` in a pod network:** the container runs with host users
  under `auto`, and is refused under `always`.
- **Host network, PID or IPC, or privileged:** the same, host users or
  refused.
- **Device nodes** owned by host root appear as owned by `nobody`. A
  device with a restrictive mode (`0660 root:video`) cannot be opened.
- **`/sys` is read-only** in a pod's network namespace, and the pod's
  `net.*` sysctls are applied by gcr, not by the container.

Opt a container out with `gcr.dev/host-users: "true"` under `auto`, or use
a handler with `UserNamespaces = 'never'`.

## Not caused by hardening

- **Processes CRIU cannot save** cannot be checkpointed, forked, rewound
  or migrated, but run normally.
- **Containers with GPUs** cannot use time travel.
