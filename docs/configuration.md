# Configuration

gcr has two kinds of settings:

- **Runtime handler options** are set by the node's operator and apply to
  every container started through that handler. All of gcr's security
  settings are here, so a workload cannot loosen them.
- **Annotations** are set per container (or per pod) and turn on features
  such as fork, time travel or migration. The only security-related one,
  `gcr.dev/host-users`, can opt a container out of gcr's user namespace,
  and only where the handler allows it.

To run different workloads with different settings, define several
handlers and pick one per pod with a RuntimeClass (see Recipes).

## Where options are set

**Kubernetes (CRI).** Set them in the handler's `options` table in
containerd's config, then restart containerd:

```toml
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]
  runtime_type = 'io.containerd.gcr.v2'
  pod_annotations = ['gcr.dev/*']        # let gcr's annotations through
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr.options]
    Hardening = 'always'
    UserNamespaces = 'always'
```

Or keep them in a file of their own:

```toml
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr.options]
    ConfigPath = '/etc/gcr/gcr.toml'
```

```toml
# /etc/gcr/gcr.toml
Hardening = 'always'
UserNamespaces = 'always'
```

When `ConfigPath` is set, gcr reads only that file and ignores any other
options in the table.

**CRI-O.** Name the file with `runtime_config_path` in the handler's
table ([getting started](getting-started.md#with-cri-o)); CRI-O passes it to gcr like `ConfigPath`.
Options that need gcr to mount the rootfs (`UserNamespaces = 'always'`)
fail there.

**ctr or containerd's client.** Pass the file per container:

```sh
ctr run --runtime io.containerd.gcr.v2 --runtime-config-path /etc/gcr/gcr.toml IMAGE ID CMD...
```

Without options, every setting takes its default. gcr checks the values
when it creates a container. An unknown value fails the create with an
error that names the setting and the values it accepts.

## Handler options

| Option | Values | Default |
| --- | --- | --- |
| `UserNamespaces` | `auto`, `always`, `never` | `auto` |
| `Hardening` | `standard`, `denied`, `always`, `off` | `standard` |
| `NestedUserNamespaces` | `guarded`, `deny` | `guarded` |
| `Landlock` | `standard`, `off` | `standard` |
| `NetlinkPolicy` | `auto`, `off` | `auto` |
| `MigrationTLS` | a table: `Cert`, `Key`, `CA` (file paths) | unset: plain HTTP |
| `PodAnnotations` | restricted annotations pods may use | none |
| `SnapshotRoots` | directories snapshots may be forked from | none |
| `TimeTravel` | a table: `MinInterval` (duration), `MaxKeep` (number) | `100ms`, `50` |

### `UserNamespaces`

Whether gcr runs containers in a user namespace of its own, where root is
an unprivileged range of 65,536 host IDs (one range per pod).

- `auto`:
  - Containers outside Kubernetes get one.
  - Kubernetes pods get one only when the pod asks for it with
    `gcr.dev/host-users: "false"`. A pod with `hostUsers: false` gets
    containerd's user namespace instead, which gcr also supports.
  - A container that cannot run in one runs with host users. That covers
    privileged containers, containers sharing the host's network, PID or
    IPC namespace, and containers with `CAP_NET_ADMIN` in a pod's network
    namespace.
  - `gcr.dev/host-users: "true"` opts a container out.
- `always`:
  - Every container gets one, whatever Kubernetes or the annotation say.
  - A container that cannot run in one is refused with the reason, rather
    than run as host root.
- `never`: gcr never makes one. A spec that brings its own user namespace
  (`hostUsers: false`) still gets it.

### `Hardening`

The seccomp-based layer.

- `standard`:
  - Hardens every container except privileged ones and Kubernetes pods
    whose seccomp profile is `Unconfined`. `Unconfined` is the Kubernetes
    default, and a runtime can't tell it apart from an explicit choice.
  - To harden every pod that does not ask for `Unconfined`, set the
    kubelet's `seccompDefault: true`: pods without a profile then run
    `RuntimeDefault`, which gcr hardens ([hardening](hardening.md)). This keeps
    CRI conformance.
- `denied`:
  - Also gives `Unconfined` pods the denied set (the first two rows
    below), but none of the rows that look at arguments: sockets and
    nested user namespaces stay as the pod asked.
  - CRI conformance expects `Unconfined` pods to have no seccomp filter
    at all (`Seccomp: 0`), so two of its tests fail.
- `always`:
  - Also hardens `Unconfined` Kubernetes pods, with gcr's whole profile.
    This is the operator overriding what the pod asked for.
  - The same conformance tests fail.
- `off`: gcr leaves the workload's seccomp profile as it is.

What "hardened" means depends on whether the container brought a seccomp
profile of its own:

| | No profile (`ctr`; `Unconfined` pod under `always`; under `denied`, the first two rows) | Own profile (`RuntimeDefault`, `Localhost`, a spec's) |
| --- | --- | --- |
| Denied set, EPERM: `io_uring_*`, `bpf`, `perf_event_open`, `userfaultfd`, modules, kexec, `open_by_handle_at` | yes | yes, overriding the profile |
| As absent, ENOSYS: the keyring (`add_key`, `keyctl`, `request_key`) and the new mount API (`fsopen`, `open_tree`, `mount_setattr`, ...) | yes | yes, overriding the profile |
| `AF_PACKET`, `AF_VSOCK`, `AF_KEY`, `AF_ALG`, raw IP sockets refused | yes | no: the profile decides |
| `clone3` as absent (ENOSYS), so libc uses `clone` | yes | no |
| Tunnel ioctls (`SIOCADDTUNNEL`, `SIOCCHGTUNNEL`, `SIOCADDPRL`, `SIOCCHGPRL`) refused | yes | no |
| Nested user namespaces: guarded or denied (`NestedUserNamespaces`) | yes | no: the profile decides |
| ICMP datagram sockets opened (`ping` without raw sockets) | yes, in a network namespace the container owns | no |
| `mount(2)` through the supervisor (filesystem allowlist) | in a user namespace: with `CAP_SYS_ADMIN`, or nested namespaces guarded and no Landlock | in a user namespace, with `CAP_SYS_ADMIN` |
| `setns(2)` through the supervisor | with `CAP_SYS_ADMIN` in a user namespace | same |
| `pivot_root` allowed | n/a (everything not refused is allowed) | with `CAP_SYS_ADMIN` in a user namespace |
| Netlink policy (if loaded) | yes | yes |

Rules that look at a call's arguments, such as which socket family, are
merged only into gcr's own profile. Combining them safely with a profile's
own conditions on the same calls cannot be done in one filter. A pod that
brings a profile therefore gets the denied set, and its profile decides
the rest. containerd's `RuntimeDefault` refuses new user namespaces to
containers without `CAP_SYS_ADMIN`, and leaves sockets to capabilities:
raw and packet sockets need `CAP_NET_RAW`, which a pod can drop.

The mount supervisor allows `proc`, `sysfs`, `tmpfs`, `devpts`, `mqueue`,
`cgroup2` and `overlay`, plus bind mounts, remounts and propagation
changes. It refuses every other filesystem, and any remount that would
make a read-only mount writable. It does the mount itself, as the caller
(its namespaces, credentials and AppArmor profile), so the kernel's own
checks still apply.

### `NestedUserNamespaces`

User namespaces a container makes for itself (Chrome's sandbox,
bubblewrap, rootless podman). This applies only in gcr's own profile; a
workload's profile decides for itself.

- `guarded`:
  - Allows them.
  - Mounts in them go through the supervisor's allowlist.
  - `NETLINK_NETFILTER` sockets are refused unless the container has
    `CAP_NET_ADMIN`.
  - The rest of the profile holds inside too.
  - Under Landlock (the default for containers without `CAP_SYS_ADMIN`) no
    mount works in them at all. See [compatibility](compatibility.md).
- `deny`: `unshare` and `clone` with `CLONE_NEWUSER` fail with EPERM.

### `Landlock`

Confines each container's processes to its root filesystem. This is
independent of `Hardening`: `Hardening = 'off'` leaves Landlock on.

- `standard` confines every container except:
  - privileged containers;
  - containers with `CAP_SYS_ADMIN`, since Landlock forbids mounting;
  - containers that join another container's PID namespace (debug
    containers, `shareProcessNamespace`);
  - all containers, on kernels without Landlock.

  Processes started with `exec` are never confined. What Landlock
  refuses is in the kernel's audit log where audit is on
  ([troubleshooting](troubleshooting.md#which-layer-refused-it)).
- `off` confines nothing.

A restored container is confined again only with the CRIU fork's
`--landlock-fs`. Without it, gcr logs that the container is not confined.

### `NetlinkPolicy`

A BPF LSM program that checks which kinds of network links, qdiscs,
classes and filters hardened containers may create ([hardening](hardening.md#netlink-policy) lists
them).

- `auto`:
  - On when the kernel has the BPF LSM enabled (`bpf` in the `lsm=` boot
    parameter).
  - Without it, the shim logs once and carries on with seccomp alone.
- `off`: containers of this handler are not covered.

The program and its allowlist are shared by every gcr shim on the node,
which is why the allowlist is fixed in the code and not set per handler.
`off` takes a handler's containers out of it.

### `MigrationTLS`

Secures live migration to and from the handler's containers. Memory,
files and the token all go over one connection to the target's
`gcr.dev/migrate-in` address (CRIU's page servers listen on loopback
only), so this covers all of it.

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr.options.MigrationTLS]
  Cert = '/etc/gcr/tls/node.crt'
  Key = '/etc/gcr/tls/node.key'
  CA = '/etc/gcr/tls/ca.crt'
```

- As a target, with `Cert` and `Key`, the container's receiver serves
  TLS 1.3 only. With `CA` as well, it requires each source to present a
  certificate signed by it.
- As a source, with any of the three set, it migrates only to `https://`
  targets and verifies them against `CA` (the system's roots without
  one), presenting `Cert` and `Key` if set. The target's certificate must
  be valid for the host in the URL.
- `Cert` and `Key` go together. The files are read when a migration
  starts, so they can be replaced in place.

In a `ConfigPath` file the table is `[MigrationTLS]`. Set it on both
hosts' handlers. The token
(`gcr.dev/migrate-token`) is checked either way.

### `PodAnnotations`

Pod annotations come from whoever may create pods. Four of gcr's would
give such a tenant more than its own container, so pods may use them
only where the handler lists them:

| Restricted | What a pod author could do with it |
| --- | --- |
| `gcr.dev/fork-from` | Choose a host directory for CRIU to restore from as root, and the layers a fork mounts. |
| `gcr.dev/migrate-in` | Make the shim listen on any address of the node's network, and restore what is sent there. |
| `gcr.dev/migrate-token` | Choose the token that listener accepts. |
| `gcr.dev/userns-base` | Choose its host IDs, such as a range another tenant left files owned by. |

```toml
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-forks.options]
    PodAnnotations = ['gcr.dev/fork-from']
    SnapshotRoots = ['/var/lib/gcr/snapshots', '/var/lib/gcr/layers']
```

A pod using one its handler does not list fails to start with an error
that names it. Containers created outside Kubernetes (`ctr`, the
containerd client) are not restricted: whoever can reach containerd's
socket is root already. Which pods may use a handler that lists them is
a matter for Kubernetes (RuntimeClass, admission).

### `SnapshotRoots`

The directories snapshots may be forked from. With any set, a
`gcr.dev/fork-from` path, and the snapshot of every layer it names, must
be under one of them, reached without a symlink, and owned by root and
writable by no one else from the root down: whoever could write a
snapshot could choose what CRIU restores and what a fork mounts. Pods
may fork only where this is set. Include the store `gcr snapshot-pull`
puts layers in (`/var/lib/gcr/layers` by default).

### `TimeTravel`

Every point on a time-travel timeline is a dump of the container's
memory, kept on the node's disk. The handler bounds what a container's
annotations may ask for: points no more often than `MinInterval`, and no
more than `MaxKeep` kept. A container asking for more fails to start.

```toml
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr.options.TimeTravel]
    MinInterval = '1s'
    MaxKeep = 20
```

## Annotations

On Kubernetes they reach gcr only if the handler lists them in
`pod_annotations` (`gcr.dev/*` above), and they are set on the pod. The
restricted ones (`fork-from`, `migrate-in`, `migrate-token`,
`userns-base`) also need the handler's `PodAnnotations`.

| Annotation | Value | What it does |
| --- | --- | --- |
| `gcr.dev/host-users` | `true` / `false` | Opt out of gcr's user namespace (`true`), or, on Kubernetes, into it (`false`). `UserNamespaces = 'always'` ignores it. |
| `gcr.dev/userns-base` | first host ID | Ask for a specific range. Migration targets use it to match the source. |
| `gcr.dev/time-travel` | `execve` or `manual` | Take a point on the timeline after each exec, or only when asked (`gcr point`). |
| `gcr.dev/time-travel-interval` | duration, default `1s` | At most one point per interval. |
| `gcr.dev/time-travel-keep` | number, default `20` | How many points to keep. |
| `gcr.dev/fork-from` | absolute path | Start from a snapshot (`gcr snapshot`) instead of the image's entrypoint. |
| `gcr.dev/fork-signal` | signal name or number (`SIGUSR2`, `USR2`, `12`) | Send this signal to a fork's init once it is restored, so it can re-seed or re-register. Needs `gcr.dev/fork-from`. Every fork also finds `/.gcr/fork.json` (`id`, `snapshot`, `created`). |
| `gcr.dev/lazy-restore` | `true` | Run a restored or forked container before its memory has loaded. Needs `vm.unprivileged_userfaultfd = 1`. |
| `gcr.dev/memory-merge` | `true` | Let the kernel (KSM) merge identical memory with other containers that opt in. Needs KSM running. |
| `gcr.dev/migrate-in` | `host:port` | Make the container a migration target, listening there. |
| `gcr.dev/migrate-token` | secret, 16 characters or more | The token a migration source must present. |

`gcr.dev/landlock` is gcr's own marker. It overwrites any value a
workload sets.

There is no annotation to turn hardening or Landlock off for one
container. A workload that needs less has to run under a handler
configured for it. Some of what a pod asks for changes what applies, as
the platform intends: privileged runs with no hardening, `CAP_SYS_ADMIN`
runs without Landlock, and a seccomp profile of its own decides the
argument rules. Who may ask for those is a matter for Kubernetes admission
(Pod Security Standards), as it is with runc.

## Recipes

**A default and a strict handler.** The strict one is for untrusted code:
user namespaces always, hardening even for `Unconfined` pods, no nested
user namespaces.

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]
  runtime_type = 'io.containerd.gcr.v2'
  pod_annotations = ['gcr.dev/*']

[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-strict]
  runtime_type = 'io.containerd.gcr.v2'
  pod_annotations = ['gcr.dev/*']
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-strict.options]
    UserNamespaces = 'always'
    Hardening = 'always'
    NestedUserNamespaces = 'deny'
```

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gcr-strict
handler: gcr-strict
```

**Sandboxing tools inside containers** (bubblewrap, rootless podman or
buildah, Nix builds). They need to mount in a user namespace of their
own, which Landlock forbids:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-nested]
  runtime_type = 'io.containerd.gcr.v2'
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-nested.options]
    Landlock = 'off'      # mounts in nested namespaces go through the supervisor's allowlist
    Hardening = 'always'  # so Unconfined pods get gcr's profile and its guarded nested namespaces
```

**Node agents** (CNI plugins, eBPF observability, VPN daemons) create
tunnels, load BPF programs, or use raw and packet sockets. Run them with
runc, or with a gcr handler that has `Hardening = 'off'` and
`NetlinkPolicy = 'off'`. Most of them are privileged anyway, which gcr
does not harden.

## Seeing what applied

The shim logs why it left something out, at debug level (containerd's
`[debug] level = 'debug'`), one line per container:

- `no user namespace`, with the reason;
- `not hardened`, with the reason;
- `no Landlock`, with the reason.

At info level it logs every mount the supervisor refuses, with the
filesystem type and flags. The netlink policy counts the messages it
checked and refused:

```sh
bpftool map dump pinned /sys/fs/bpf/gcr/netlink/stats   # key 0: checked, key 1: refused (per CPU)
```

A refused call reaches the program as `EPERM` ("Operation not
permitted"). The keyring calls, the new mount API and `clone3` return
`ENOSYS` ("Function not implemented") instead.
