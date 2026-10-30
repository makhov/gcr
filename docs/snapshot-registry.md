# Moving snapshots between hosts

Snapshots travel through any OCI registry: one backed by S3, ECR, GHCR,
Harbor, Zot.

```sh
# where the snapshot was taken
gcr snapshot-push /var/lib/gcr/snapshots/warm registry.example.com/snapshots/warm:v1
# anywhere else
gcr snapshot-pull registry.example.com/snapshots/warm:v1 /var/lib/gcr/snapshots/warm
ctr run -d --runtime io.containerd.gcr.v2 \
  --annotation gcr.dev/fork-from=/var/lib/gcr/snapshots/warm IMAGE fork-1 CMD...
```

- **What gets pushed.** A snapshot is one artifact: its process images,
  and the writable layers a fork mounts (its own, and those of the
  snapshots it was forked from).
  - Each part is packed deterministically, so a layer that several
    snapshots share is stored once in the registry and uploaded once.
  - Parts keep numeric owners, xattrs and overlay's whiteouts.
- **Where a pull puts things.**
  - The snapshot goes to the directory given.
  - The layers beneath it go to a store (`-store`, default
    `/var/lib/gcr/layers`), shared by every snapshot pulled onto the host
    that has them.
  - Both are counted like snapshots taken there (`gcr snapshot-users`,
    `gcr snapshot-rm`).
- **Credentials** come from Docker's config (`docker login`). Registries
  on loopback are spoken to over plain HTTP; `-plain-http` forces it for
  others.
- **User namespace ranges.** A snapshot's files and process images
  carry host IDs from the range its container mapped, and its forks map
  that range.
  - A pull keeps the range if it is free on the target.
  - If another container has it there, the pull moves the snapshot into
    a free range: file owners and the IDs in capability and ACL xattrs
    are shifted, and the user namespace maps in the CRIU images
    (`userns-*.img`) are rewritten. CRIU records every other ID as the
    container sees it. `gcr snapshot-pull` says when it did this.
  - Snapshots taken from the same container in the same range share one
    range on the target, as forks of one snapshot do, so their layers
    in the store are shared too.
  - A move costs a pass over the snapshot's files; a moved snapshot's
    layers are a copy of their own in the store, and pushing it again
    packs new blobs.
- **What else the target needs.** The image the snapshot was taken
  from, the same architecture (checked), and a CPU with the features
  the processes were using (CRIU checks).

A registry storing in S3, for example the distribution registry's own:

```yaml
# config.yml for docker.io/library/registry:3
version: 0.1
storage:
  s3:
    region: eu-west-1
    bucket: gcr-snapshots
http:
  addr: :5000
```

## Formats

`gcr snapshot-push -format` chooses how the parts are packed; a pull
reads either.

- **`tar`** (the default): each part is a zstd-compressed tar.
- **`nydus`**: each part is a [nydus](https://github.com/dragonflyoss/nydus)
  image (RAFS v6, which EROFS can read): a bootstrap, the part's metadata,
  and a blob of its file data in compressed chunks, deduplicated within the
  part. A node can mount such a part with `nydusd` and read only what it
  touches, instead of downloading all of it. Pushing and pulling need
  `nydus-image` and `nydusd` (nydus 2.4) in `PATH`.

  A plain pull of a nydus snapshot downloads it whole, checks each blob
  against its digest, and lays it out as a `tar` one is: forks and
  everything else work the same.

## Lazy pulls

`gcr snapshot-pull -lazy` mounts a nydus snapshot instead of downloading
it: its process images, its writable layer and the layers beneath it are
each served by a `nydusd` that reads from the registry the chunks a reader
touches, and keeps them. A fork of it reads the files it opens; with
`gcr.dev/lazy-restore=true` its memory is read as its processes touch it,
too, so a fork of a large snapshot starts after reading a fraction of it.

What it buys, for a fork of a Python process holding 1 GiB of random data
(incompressible), pulled from a registry on the same host (arm64 VM):

| | pull | until the fork runs | total | read from the registry by then |
| --- | --- | --- | --- | --- |
| plain pull | 3.25 s | 0.58 s | 3.83 s | 1027 MiB |
| `-lazy` | 0.07 s | 3.16 s | 3.23 s | 1027 MiB |
| `-lazy`, `gcr.dev/lazy-restore=true` | 0.06 s | 0.35 s | **0.41 s** | **28 MiB** |

Without lazy restore, CRIU reads all of the memory before the processes
run, so the lazy pull only moves the wait. With it, the fork runs after
the pages it touched; CRIU then copies the rest in the background until
the fork has all its memory, so the whole snapshot is read within seconds
either way. Over a real network the time saved grows with the snapshot.

- **Lifetime.** The `nydusd`s run on their own and serve the snapshot
  until `gcr snapshot-rm` removes it, which unmounts its parts and they
  exit. They do not survive a reboot: pull the snapshot again after one.
- **User namespace range.** A lazily pulled snapshot cannot be moved into
  another range (that rewrites every file), so its range must be free on
  the node; the pull says so if it is not, and a plain pull moves it.
- **Integrity.** A plain pull checks each blob against its digest. A
  lazy pull reads chunks as they are needed, and `nydusd` cannot check
  RAFS v6 chunks: it trusts what the registry, over TLS, returns.
- **Credentials.** `nydusd` gets the username and password from Docker's
  config; registries that hand out only tokens are not supported yet.
- **Kernels.** `nydusd` serves the mounts over FUSE, which every kernel
  has. Kernels built with EROFS on-demand loading
  (`CONFIG_EROFS_FS_ONDEMAND`, `CONFIG_CACHEFILES_ONDEMAND`) could serve
  them faster through fscache; gcr does not use that yet.

