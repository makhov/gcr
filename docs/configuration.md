# Configuration

Most people need none of this. gcr's defaults run containers hardened, in
a user namespace, and that is the right setting for most workloads.

You come here for one of three reasons:

1. **A workload does not work** under gcr's defaults, and you need to know
   which protection to relax, and where. Start with
   [recipes](#recipes), and
   [what the hardening breaks](compatibility.md) if you have an error
   message in hand.
2. **You want a feature** that is off until someone allows it: forks,
   time travel, migration. See [turning features on](#turning-features-on).
3. **You want stricter settings than the defaults** for untrusted code.
   See [the strict recipe](#untrusted-code).

## The two kinds of settings, and why

| | Set by | Applies to | Examples |
| --- | --- | --- | --- |
| **Handler options** | whoever runs the node | every container of that runtime handler | all the security settings, what pods may ask for |
| **Annotations** | whoever creates the container or pod | that container | time travel, forks, lazy restore, migration |

The split is the point: a workload cannot loosen its own isolation. Every
security setting is a handler option, so a pod author cannot turn
hardening off by adding an annotation. The one exception,
`gcr.dev/host-users`, works only where the handler allows it.

That is also how you give different workloads different settings: define
several handlers, and let each pod pick one with a RuntimeClass. A
handler is just a name in containerd's configuration with an options
table under it.

## Where options go

**Kubernetes and anything else using the CRI.** In the handler's
`options` table in containerd's configuration, then restart containerd:

```toml
version = 3
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr]
  runtime_type = 'io.containerd.gcr.v2'
  pod_annotations = ['gcr.dev/*']        # let gcr's annotations through
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr.options]
    Hardening = 'always'
    UserNamespaces = 'always'
```

Or keep them in a file, which is easier to manage with configuration
management:

```toml
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr.options]
    ConfigPath = '/etc/gcr/gcr.toml'
```

```toml
# /etc/gcr/gcr.toml
Hardening = 'always'
UserNamespaces = 'always'
```

With `ConfigPath` set, gcr reads only that file and ignores other options
in the table.

**containerd 1.7** uses configuration version 2, where the table is
`[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.gcr]`.

**CRI-O** names the file with `runtime_config_path` in the handler's
table, which it passes to gcr like `ConfigPath` ([CRI-O](crio.md)).
Options that need gcr to mount the rootfs (`UserNamespaces = 'always'`)
cannot work there.

**ctr and containerd clients** pass a file per container:
`ctr run --runtime-config-path /etc/gcr/gcr.toml …` ([ctr](ctr.md)).

gcr checks the values when it creates a container: a wrong one fails the
create with an error naming the setting and the values it accepts, rather
than running something less safe than you asked for.

## Turning features on

Time travel is an annotation, so on a node where pods may use
`gcr.dev/*` it needs nothing further. Forks and migration are different:
their annotations name a path or an address on the node, so a pod author
who could set them freely would have more than their own container. The
handler has to allow them.

| To let pods | Set on the handler |
| --- | --- |
| Take and use snapshots (`gcr.dev/fork-from`) | `PodAnnotations = ['gcr.dev/fork-from']` and `SnapshotRoots = [...]` |
| Receive migrations (`gcr.dev/migrate-in`, `gcr.dev/migrate-token`) | `PodAnnotations` with those; `gcr setup` already registers a `gcr-migrate` handler for this |
| Choose their user namespace range (`gcr.dev/userns-base`) | `PodAnnotations = ['gcr.dev/userns-base']` |
| Bound what time travel may ask for | `TimeTravel` with `MinInterval` and `MaxKeep` |

Containers created outside Kubernetes are not restricted this way:
whoever reaches containerd's socket is root already.

## Recipes

Each of these is a handler plus the RuntimeClass that selects it. Install
the RuntimeClass once, and pods opt in with `runtimeClassName`.

### Untrusted code

Stricter than the defaults: a user namespace always, hardening even for
pods that ask for `Unconfined`, and no nested user namespaces.

```toml
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

A container that cannot run in a user namespace (a privileged one, one
sharing the host's namespaces) is refused here with the reason, instead
of quietly running as host root. That refusal is the feature.

Still a shared kernel: for code from several hostile tenants on one host,
use a VM-based runtime ([what gcr is good for](tradeoffs.md)).

### Agent sandboxes that fork

Pods may fork, but only from directories you nominate:

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-forks]
  runtime_type = 'io.containerd.gcr.v2'
  pod_annotations = ['gcr.dev/*']
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-forks.options]
    PodAnnotations = ['gcr.dev/fork-from']
    SnapshotRoots = ['/var/lib/gcr/snapshots', '/var/lib/gcr/layers']
```

Include the store that `gcr snapshot-pull` writes to
(`/var/lib/gcr/layers`), or pulled snapshots will not fork.
[A warm pool across the fleet](examples/warm-pool.md) uses this.

### Sandboxing tools inside containers

bubblewrap, rootless podman or buildah, Nix builds: these mount inside a
user namespace of their own, which Landlock forbids.

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-nested]
  runtime_type = 'io.containerd.gcr.v2'
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-nested.options]
    Landlock = 'off'      # mounts in nested namespaces still go through the supervisor
    Hardening = 'always'  # so Unconfined pods get gcr's profile and guarded nested namespaces
```

Landlock off costs you the protection against escapes through leaked file
descriptors (CVE-2024-21626); the mount allowlist and the syscall filter
stay. Use this handler for the workloads that need it, not as the
default.

### Node agents

CNI plugins, eBPF observability, VPN daemons: they create tunnels, load
BPF programs, or use raw and packet sockets. Run them on runc, or on a
handler with `Hardening = 'off'` and `NetlinkPolicy = 'off'`. Most are
privileged anyway, which gcr does not harden.

### Docker

Docker takes the same options through `runtimeType` entries in
`daemon.json`, one per handler ([Docker](docker.md)).

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
