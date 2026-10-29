# User namespaces

Containers run in a user namespace by default: root inside is an
unprivileged range of 65536 host IDs, one range per pod (all containers of a
pod share the sandbox's user namespace, so they can share volumes and
PID/IPC namespaces).

Kubernetes pods follow Kubernetes: `hostUsers` defaults to true there, and
kubelet sends it like an explicit true, so a runtime cannot change it. Use
`hostUsers: false` (containerd's own user namespaces, supported by gcr and
the better path: the pod's network namespace is created inside the user
namespace), enforced cluster-wide by policy if wanted, or opt a pod into
gcr's with the annotation `gcr.dev/host-users: "false"`.

To enforce user namespaces regardless of Kubernetes, set the policy on a
runtime handler (here a second handler, so a RuntimeClass picks it):

```toml
[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-userns]
  runtime_type = 'io.containerd.gcr.v2'
  [plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.gcr-userns.options]
    UserNamespaces = 'always'   # 'auto' (default) | 'always' | 'never'
```

`always` ignores `hostUsers` and the opt-out annotation; a container that
cannot run in a user namespace (privileged, host network/PID/IPC,
`CAP_NET_ADMIN` in a pod network) is refused rather than run as host root.
`never` turns gcr's user namespaces off. The options can also live in a
file: `ConfigPath = '/etc/gcr/gcr.toml'`.

For gcr's own user namespaces:

- Image layers are mounted idmapped, so files look root-owned inside; files
  the container creates are stored as its mapped IDs.
- Bind mounts on filesystems that support it (ext4, xfs, btrfs, tmpfs, f2fs,
  vfat) are idmapped, so volumes keep their on-disk owners.
- In a pod network namespace gcr sets `ip_unprivileged_port_start=0` and an
  open `ping_group_range` (root can bind port 80 and ping), applies the
  pod's `net.*` sysctls itself, and bind-mounts `/sys` read-only.
- Raw sockets do not work in a pod network namespace (it belongs to the
  host's user namespace); containers adding `CAP_NET_ADMIN` there get host
  users.
- Skipped for privileged containers, containers sharing the host's network,
  PID or IPC namespace, and specs that bring their own user namespace
  (Kubernetes `hostUsers: false`). Opt out with `gcr.dev/host-users=true`.
- A restore claims the range the checkpoint was taken with. A migration
  target must be created with the source's range,
  `gcr.dev/userns-base=<first host ID>`; the source refuses otherwise.
