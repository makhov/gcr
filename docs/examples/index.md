# Examples

Whole use cases, each one something you can run.

<div class="grid cards" markdown>

-   :material-robot-outline: **[Agent sandboxes](agent-sandbox.md)**

    ---

    Give an agent undo and let it try several fixes at once. A scripted
    agent that drops a database, takes it back, then forks three ways.

    *Docker, one host, no GPU. 5 minutes.*

-   :material-fire: **[Warm starts](warm-starts.md)**

    ---

    Snapshot a server once it has loaded its model, start the next one
    from that moment. vLLM answers in 9 s instead of 136.

    *ctr, one host; the GPU part needs an NVIDIA GPU.*

-   :material-server-network: **[Node maintenance](node-maintenance.md)**

    ---

    Drain a Kubernetes node without restarting what runs on it. The pods
    move, memory and all, in a few hundred milliseconds.

    *A cluster with two or more nodes.*

-   :material-cloud-upload-outline: **[A warm pool across the fleet](warm-pool.md)**

    ---

    Build a snapshot once, push it to a registry, fork from it on every
    node that needs one.

    *One host to build it, a registry, any number of hosts.*

</div>

All four assume gcr is installed ([Installation](../install.md)). The
`gcr` command itself always runs as root, because its control sockets are
root's. If you have not run a container on gcr yet,
[the quickstart](../getting-started.md) is the shorter way in.
