---
hide:
  - navigation
  - toc
---

<div class="gcr-hero" markdown>

# Containers you can rewind, copy and move

gcr is a container runtime for Docker, containerd and Kubernetes. Your
containers run as they always do. And while they run, you can save one,
start copies of it, take it back to an earlier moment, or move it to
another machine, without restarting it.

```sh
curl -sSfL https://raw.githubusercontent.com/makhov/gcr/main/install.sh | sh
sudo gcr setup -docker
```

[Get started](getting-started.md){ .md-button .md-button--primary }
[See examples](examples/index.md){ .md-button }

</div>

## How to use it

<div class="grid cards" markdown>

-   **1. Install it**

    ---

    Two commands on a Linux host, or a Helm chart on a Kubernetes
    cluster. [Install](install.md)

-   **2. Run containers as usual**

    ---

    Choose gcr as the runtime: `docker run --runtime gcr`, or
    `runtimeClassName: gcr` in a pod. Your images and commands stay
    the same.

-   **3. Work with them while they run**

    ---

    Snapshot, copy, rewind or move a running container with one
    command, and its processes carry on from where they were.

</div>

```sh
kubectl gcr snapshot pod/app registry.example.com/app:warm   # an image that starts warm
kubectl gcr point pod/app "before the migration"             # mark this moment: #1
kubectl gcr rewind pod/app 1                                 # and go back to it
kubectl gcr migrate pod/app                                  # move it to another node
```

## Why gcr

<div class="grid cards" markdown>

-   :material-fire: **Start in seconds, not minutes**

    ---

    Save a container once it has loaded its model or warmed its cache,
    and start new ones from that moment. A vLLM server answers in 9
    seconds instead of 136.

-   :material-undo-variant: **Undo anything**

    ---

    Take a sandbox back to before a bad command: files, processes and
    what they held in memory. A dropped database comes back with its
    rows.

-   :material-source-branch: **Try many things at once**

    ---

    Copy a running environment, services and all, and try several
    approaches side by side. Copies share memory, so ten cost far less
    than ten fresh containers.

-   :material-truck-fast: **Maintenance without restarts**

    ---

    Drain a node and its pods move to another, memory and all, with a
    pause of a few hundred milliseconds. Long jobs and in-memory state
    survive.

-   :material-puzzle-check: **Nothing new to learn**

    ---

    A drop-in replacement for runc under containerd, Docker and
    Kubernetes. Same images, same tools, and containers start faster.

-   :material-shield-check: **Safer by default**

    ---

    Stricter isolation than runc out of the box, with user namespaces on.
    Even Docker-in-Docker runs without privileges.

</div>

<div class="gcr-cta" markdown>

## Try it in ten minutes

The quickstart installs gcr, starts a container, copies it and rewinds
it, on one Linux machine or a VM on your laptop.

[Start the quickstart](getting-started.md){ .md-button .md-button--primary }
[GitHub](https://github.com/makhov/gcr){ .md-button }

</div>

gcr shares the host's kernel, as runc does. For untrusted code from many
tenants, run it inside VMs. [What it is good for, and where it falls short](tradeoffs.md).
