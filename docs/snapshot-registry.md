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
  - Each part is a zstd-compressed tar, packed deterministically, so a
    layer that several snapshots share is stored once in the registry
    and uploaded once.
  - The tars keep numeric owners, xattrs and overlay's whiteouts.
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
