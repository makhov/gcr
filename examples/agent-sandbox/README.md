# Agent sandboxes: undo and branching

`sandbox.py` runs an agent's sandbox as a Docker container under gcr, and
gives it two things a container does not have elsewhere:

- **Undo.** `run(cmd, undoable=True)` takes a point just before the
  command; `undo()` puts the sandbox back to it: files, processes and
  their memory. A database the agent dropped, a server it killed, a
  directory it deleted all come back. Undo again goes one command further
  back.
- **Branching.** `fork(n)` snapshots the sandbox, which keeps running, and
  starts n sandboxes from that state. Each goes on alone, running
  processes included; an agent can try n approaches at once and keep the
  one that worked. With lazy restore a fork holds only the memory it
  touches (1 MiB each below, against 8 MiB for the sandbox).

`demo.py` is a scripted agent that does both:

```
$ sudo python3 demo.py
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

`kv` talks to a service in the sandbox holding its data in memory only:
the undo brings those keys back, which no snapshot of the filesystem
could.

## Running it

On a Linux node with Docker using gcr as a runtime ([Docker](../../docs/docker.md);
`hack/docker-setup.sh` sets one up), as root (gcr's control sockets are
root's):

```sh
sudo python3 examples/agent-sandbox/demo.py
```

Forks read their memory lazily where `vm.unprivileged_userfaultfd` is 1
([lazy restore](../../docs/fork.md#lazy-restore)), and copy it whole
otherwise.

## Using it from an agent

`sandbox.py` has no dependencies beyond Python's standard library:

```python
from sandbox import Sandbox

sb = Sandbox.create("python:3.12-alpine")
r = sb.run("pip install requests", undoable=True)   # network="host" for this
if not r.ok:
    sb.undo()
forks = sb.fork(3)
```

An agent framework's tool calls map onto `run`, `undo` and `fork`.
Sandboxes have no network by default: undo needs a network Docker did not
set up, so `network="none"` or `"host"` ([Docker networking](../../docs/docker.md#networking)).
