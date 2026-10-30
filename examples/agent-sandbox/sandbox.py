"""Sandboxes for agents on gcr, through Docker.

A sandbox is a Docker container run by gcr. An agent runs commands in it
one at a time; any command run as undoable can be taken back, files and
processes both (a point on the container's timeline is taken just before
it). A sandbox can be forked: each fork goes on from the same state, files,
processes and memory, on its own.

It drives the docker and gcr commands, so it needs Docker with gcr as a
runtime (docs/docker.md) and root, for gcr's control sockets.
"""

import os
import shlex
import subprocess
import time
import uuid
from dataclasses import dataclass

# Docker's containerd namespace, where gcr finds its containers.
NAMESPACE = "moby"
SNAPSHOTS = "/var/lib/gcr/snapshots"


@dataclass
class Result:
    exit_code: int
    stdout: str
    stderr: str
    seconds: float
    point: int = 0  # the point taken before the command, 0 if none

    @property
    def ok(self):
        return self.exit_code == 0


def _run(*args, check=True):
    p = subprocess.run(args, capture_output=True, text=True)
    if check and p.returncode != 0:
        raise RuntimeError(f"{shlex.join(args)}: {p.stderr.strip() or p.stdout.strip()}")
    return p


class Sandbox:
    def __init__(self, name, image, network):
        self.name = name
        self.image = image
        self.network = network
        self.id = _run("docker", "inspect", "-f", "{{.Id}}", name).stdout.strip()
        self._undo = []  # points taken before undoable commands, newest last
        self._snapshots = []

    @classmethod
    def create(cls, image, command=None, name=None, network="none", fork_from=None, lazy=False):
        """Starts a sandbox from image, running command (by default, one
        that only keeps it up), or from a snapshot of another sandbox.

        Undo needs a network Docker did not set up: "none" (the default)
        or "host" (docs/docker.md#networking).
        """
        name = name or "sb-" + uuid.uuid4().hex[:8]
        args = ["docker", "run", "-d", "--runtime", "gcr", "--name", name, "--network", network,
                "--annotation", "gcr.dev/time-travel=manual",
                "--annotation", "gcr.dev/time-travel-keep=50"]
        if fork_from:
            args += ["--annotation", "gcr.dev/fork-from=" + fork_from]
            if lazy:
                args += ["--annotation", "gcr.dev/lazy-restore=true"]
            command = ["true"]  # not run: the snapshot's processes are
        args.append(image)
        args += command or ["sh", "-c", "trap 'exit 0' TERM; while :; do sleep 3600 & wait; done"]
        _run(*args)
        return cls(name, image, network)

    def run(self, cmd, undoable=False):
        """Runs cmd (a shell command line) and waits for it. With undoable,
        undo() takes it back."""
        point = 0
        if undoable:
            point = self.point(cmd)
            self._undo.append(point)
        start = time.monotonic()
        p = _run("docker", "exec", self.name, "sh", "-c", cmd, check=False)
        return Result(p.returncode, p.stdout, p.stderr, time.monotonic() - start, point)

    def point(self, label=""):
        """Takes a point on the sandbox's timeline now; returns its number."""
        out = _run("gcr", "-n", NAMESPACE, "point", self.id, label).stdout
        return int(out.strip().lstrip("#"))

    def undo(self):
        """Puts the sandbox back to before its last undoable command, then
        the one before that; returns the point it went back to."""
        if not self._undo:
            raise RuntimeError("nothing to undo")
        point = self._undo.pop()
        self.rewind(point)
        return point

    def rewind(self, point):
        _run("gcr", "-n", NAMESPACE, "rewind", self.id, str(point))

    def fork(self, n=1, lazy=False):
        """Snapshots the sandbox, which keeps running, and starts n sandboxes
        from the snapshot. With lazy, each fork reads the memory it touches,
        when it touches it (gcr.dev/lazy-restore)."""
        snap = os.path.join(SNAPSHOTS, self.name + "-" + uuid.uuid4().hex[:6])
        os.makedirs(SNAPSHOTS, exist_ok=True)
        _run("gcr", "-n", NAMESPACE, "snapshot", self.id, snap)
        self._snapshots.append(snap)
        return [Sandbox.create(self.image, network=self.network, fork_from=snap, lazy=lazy) for _ in range(n)]

    def resident_mib(self):
        """Memory the sandbox's processes hold, in MiB."""
        pids = _run("docker", "top", self.name, "-eo", "pid").stdout.split()[1:]
        kib = 0
        for pid in pids:
            try:
                with open(f"/proc/{pid}/status") as f:
                    for line in f:
                        if line.startswith("VmRSS:"):
                            kib += int(line.split()[1])
            except FileNotFoundError:
                pass
        return kib // 1024

    def remove(self):
        _run("docker", "rm", "-f", self.name, check=False)
        for snap in self._snapshots:
            # Refused while forks of it run; those remove theirs.
            _run("gcr", "snapshot-rm", snap, check=False)
