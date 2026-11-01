# Agent sandboxes: undo and branching

An agent runs commands you cannot predict. Two things make that safer and
faster, and neither is possible with an ordinary container:

- **Undo.** Mark a point before a command. If the command goes wrong, put
  the sandbox back: files, processes, and what those processes held in
  memory.
- **Branching.** Fork the sandbox, running services and all, try three
  fixes at once, keep the one that passed.

[`examples/agent-sandbox`](https://github.com/makhov/gcr/tree/main/examples/agent-sandbox)
is a 200-line Python wrapper around `docker` and `gcr` that does both,
with no dependencies beyond the standard library.

## Run it

On a host with Docker on gcr ([Quickstart](../getting-started.md), or
`hack/docker-setup.sh`), as root:

```sh
git clone https://github.com/makhov/gcr && cd gcr
sudo python3 examples/agent-sandbox/demo.py
```

`demo.py` is a scripted agent. Its output, abridged:

```console
1. Undo
  agent$ kv drop; rm -rf /work [point #1]
      dropped
  agent$ kv keys; ls /work
      (empty)
      ls: /work: No such file or directory
  undo -> point #1 (0.09s; the point itself took 0.04s with the command)
  agent$ kv keys; kv get visits; ls /work
      plan user visits
      41
      calc.py
      test.sh

2. Branching
  agent$ cd /work && sh test.sh
      AssertionError: add(2, 3) = -1
  forked 3 sandboxes in 1.88s (memory read as touched)
  -- sb-a18f71d7: add instead
  sb-a18f71d7$ cd /work && sed -i 's/return a - b/return a + b/' calc.py
  sb-a18f71d7$ cd /work && sh test.sh
      PASS
  memory: sandbox 8 MiB; forks 1 MiB, 1 MiB, 1 MiB
  kept sb-a18f71d7; its service still has the store:
  sb-a18f71d7$ kv keys
      plan user visits
```

`kv` is a service inside the sandbox that keeps its data **in memory
only**. The undo brings those keys back, which no filesystem snapshot
could have done. That is the whole point: these are dumps of running
processes, not of disks.

## Using it from an agent

```python
from sandbox import Sandbox

sb = Sandbox.create("python:3.12-alpine")

r = sb.run("pip install requests", undoable=True)   # network="host" for this
if not r.ok:
    sb.undo()

forks = sb.fork(3)          # three sandboxes from right here
```

An agent framework's tool calls map onto `run`, `undo` and `fork`. The
three calls are all there is.

## How it works

`sandbox.py` starts the container with
[`gcr.dev/time-travel=manual`](../time-travel.md#points-on-request), so
points are taken only when it asks for one, and none in between:

| It calls | gcr does |
| --- | --- |
| `run(cmd, undoable=True)` | `gcr point <id> "<cmd>"` before `docker exec` |
| `undo()` | `gcr rewind <id> <point>`, newest first |
| `fork(n)` | `gcr snapshot` once, then n `docker run --annotation gcr.dev/fork-from=…` |

Two details worth copying into your own version:

- **Sandboxes have no network by default** (`--network none`). A rewind
  restores the processes' network as it was, which only works where
  Docker did not set one up; `network="host"` is the other option that
  allows it. [Docker networking](../docker.md#networking).
- **Forks read their memory as they touch it** where
  `vm.unprivileged_userfaultfd` is 1
  ([lazy restore](../fork.md#lazy-restore)), which is why the three forks
  above cost 1 MiB each against the sandbox's 8 MiB. Without it they copy
  the memory whole, which is slower and larger but works everywhere.

## What it costs

- A point freezes the container while its memory is dumped: about 90 ms
  at 64 MiB, 260–390 ms at 1 GiB, and that much disk
  ([the numbers](../tradeoffs.md#checkpoint-restore-and-time-travel)).
  The first point and rewind in a container are slower than the rest,
  because nothing is cached yet.
- Forks of one snapshot share its user namespace range, so they are
  isolated from other containers but not from each other at the host-ID
  level. That suits fanning one workload out, not running two tenants
  ([fork](../fork.md)).
- Take a point when no command is running inside: a command `docker exec`
  started belongs to the shim, not to the container, and CRIU cannot dump
  it.
