#!/usr/bin/env python3
"""An agent in a gcr sandbox: undo and branching.

The agent here is a script, so the demo runs anywhere and shows the
mechanics; a model choosing the commands would drive the same calls.

1. Undo. The sandbox runs a service holding a key-value store in memory,
   next to a project directory. The agent runs a destructive command
   that wipes both. One undo brings back the files and the service's
   memory: the store's keys, which no snapshot of the filesystem has.
2. Branching. A test fails. The agent forks the sandbox three ways, tries
   a different fix in each, keeps the fork where the test passes and
   drops the others. Each fork starts from the sandbox as it was,
   service and memory included.

Run as root on a node with Docker and gcr (docs/docker.md):

    sudo python3 examples/agent-sandbox/demo.py
"""

import time

from sandbox import Sandbox

IMAGE = "docker.io/library/python:3.12-alpine"

# The sandbox's own process: a key-value store in memory, on a unix socket,
# with a client for it as /usr/local/bin/kv.
SERVICE = r'''
import os, socket
open("/usr/local/bin/kv", "w").write("""#!/usr/bin/env python3
import socket, sys
s = socket.socket(socket.AF_UNIX); s.connect("/run/kv.sock")
s.sendall(" ".join(sys.argv[1:]).encode()); print(s.recv(65536).decode())
""")
os.chmod("/usr/local/bin/kv", 0o755)
store = {}
srv = socket.socket(socket.AF_UNIX)
srv.bind("/run/kv.sock"); srv.listen()
while True:
    c, _ = srv.accept()
    cmd, *args = c.recv(65536).decode().split(" ", 2)
    if cmd == "set": store[args[0]] = args[1]; out = "ok"
    elif cmd == "get": out = store.get(args[0], "(none)")
    elif cmd == "keys": out = " ".join(sorted(store)) or "(empty)"
    elif cmd == "drop": store.clear(); out = "dropped"
    else: out = "?"
    c.sendall(out.encode()); c.close()
'''

PROJECT = r'''
mkdir -p /work && cd /work
cat > calc.py <<'EOF'
def add(a, b):
    return a - b
EOF
cat > test.sh <<'EOF'
python3 -c 'import calc; assert calc.add(2, 3) == 5, "add(2, 3) = %d" % calc.add(2, 3)' && echo PASS
EOF
'''

FIXES = {
    "swap the operands": "sed -i 's/return a - b/return b - a/' calc.py",
    "use abs": "sed -i 's/return a - b/return abs(a - b)/' calc.py",
    "add instead": "sed -i 's/return a - b/return a + b/' calc.py",
}


def step(agent, sb, cmd, undoable=False):
    r = sb.run(cmd, undoable=undoable)
    mark = f" [point #{r.point}]" if r.point else ""
    first, *rest = cmd.splitlines()
    print(f"  {agent}$ {first}{' ...' if rest else ''}{mark}")
    for line in (r.stdout + r.stderr).strip().splitlines():
        print(f"      {line}")
    return r


def lazy_ok():
    try:
        with open("/proc/sys/vm/unprivileged_userfaultfd") as f:
            return f.read().strip() == "1"
    except OSError:
        return False


def main():
    print("Starting a sandbox: a key-value service in memory, and a project.")
    t = time.monotonic()
    sb = Sandbox.create(IMAGE, ["python3", "-c", SERVICE])
    time.sleep(1)
    step("agent", sb, PROJECT.strip())
    for k, v in [("user", "ada"), ("plan", "pro"), ("visits", "41")]:
        step("agent", sb, f"kv set {k} {v}")
    print(f"  (ready in {time.monotonic() - t:.1f}s)\n")

    print("1. Undo")
    r = step("agent", sb, "kv drop; rm -rf /work", undoable=True)
    step("agent", sb, "kv keys; ls /work")
    t = time.monotonic()
    point = sb.undo()
    print(f"  undo -> point #{point} ({time.monotonic() - t:.2f}s; the point itself took "
          f"{r.seconds:.2f}s with the command)")
    step("agent", sb, "kv keys; kv get visits; ls /work")
    print()

    print("2. Branching")
    step("agent", sb, "cd /work && sh test.sh")
    lazy = lazy_ok()
    t = time.monotonic()
    forks = sb.fork(len(FIXES), lazy=lazy)
    print(f"  forked {len(forks)} sandboxes in {time.monotonic() - t:.2f}s"
          f"{' (memory read as touched)' if lazy else ''}")
    winner = None
    for (name, fix), f in zip(FIXES.items(), forks):
        print(f"  -- {f.name}: {name}")
        step(f.name, f, f"cd /work && {fix}")
        if "PASS" in step(f.name, f, "cd /work && sh test.sh").stdout and not winner:
            winner = f
    print(f"  memory: sandbox {sb.resident_mib()} MiB; forks "
          + ", ".join(f"{f.resident_mib()} MiB" for f in forks))
    for f in forks:
        if f is not winner:
            f.remove()
    print(f"  kept {winner.name if winner else 'none'}; its service still has the store:")
    if winner:
        step(winner.name, winner, "kv keys")

    for s in [winner, sb]:
        if s:
            s.remove()


if __name__ == "__main__":
    main()
