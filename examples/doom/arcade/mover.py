"""Moves the doom pod to another node every INTERVAL seconds, through
gcr's Migration resource, and keeps /srv/status.json up to date for the
page: where the game runs, how many times it has moved, and how long it
was paused each time.

Talks to the Kubernetes API with the pod's service account, and Python's
standard library alone.
"""

import json
import os
import ssl
import time
import urllib.request

SA = "/var/run/secrets/kubernetes.io/serviceaccount"
API = "https://kubernetes.default.svc"
NS = open(f"{SA}/namespace").read().strip()
SELECTOR = os.environ.get("SELECTOR", "app=doom")
INTERVAL = float(os.environ.get("INTERVAL", "20"))
STATUS = "/srv/status.json"
CTX = ssl.create_default_context(cafile=f"{SA}/ca.crt")


def api(method, path, body=None):
    token = open(f"{SA}/token").read().strip()
    req = urllib.request.Request(
        API + path,
        method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers={"Authorization": f"Bearer {token}", "Content-Type": "application/json"},
    )
    with urllib.request.urlopen(req, context=CTX, timeout=30) as r:
        return json.load(r)


def game_pod():
    """The running doom pod, not one being deleted or a migration's target
    still waiting for the game."""
    pods = api("GET", f"/api/v1/namespaces/{NS}/pods?labelSelector={SELECTOR}")["items"]
    for p in pods:
        ready = any(c["type"] == "Ready" and c["status"] == "True" for c in p["status"].get("conditions", []))
        if ready and not p["metadata"].get("deletionTimestamp"):
            return p
    return None


state = {"node": None, "pod": None, "moves": 0, "moving": False, "last": None, "history": []}


def publish():
    tmp = STATUS + ".tmp"
    with open(tmp, "w") as f:
        json.dump(state, f)
    os.replace(tmp, STATUS)


def move(pod):
    m = api("POST", f"/apis/gcr.dev/v1alpha1/namespaces/{NS}/migrations", {
        "apiVersion": "gcr.dev/v1alpha1",
        "kind": "Migration",
        "metadata": {"generateName": "doom-"},
        "spec": {"pod": pod},
    })
    name = m["metadata"]["name"]
    for _ in range(600):
        time.sleep(0.5)
        s = api("GET", f"/apis/gcr.dev/v1alpha1/namespaces/{NS}/migrations/{name}").get("status", {})
        if s.get("phase") in ("Succeeded", "Failed"):
            api("DELETE", f"/apis/gcr.dev/v1alpha1/namespaces/{NS}/migrations/{name}")
            return s
    return {"phase": "Failed", "message": "no result in 5 minutes"}


def main():
    next_move = time.time() + INTERVAL
    while True:
        try:
            p = game_pod()
            if p:
                state["node"], state["pod"] = p["spec"]["nodeName"], p["metadata"]["name"]
            state["next"] = max(0, round(next_move - time.time()))
            publish()
            if p and time.time() >= next_move:
                state["moving"] = True
                publish()
                s = move(p["metadata"]["name"])
                state["moving"] = False
                hop = {"from": s.get("node"), "to": s.get("targetNode"), "downtime": s.get("downtime"),
                       "ok": s.get("phase") == "Succeeded", "message": s.get("message", "")}
                if hop["ok"]:
                    state["moves"] += 1
                state["last"] = hop
                state["history"] = ([hop] + state["history"])[:10]
                print(json.dumps(hop), flush=True)
                next_move = time.time() + INTERVAL
        except Exception as e:  # the API server restarting, say: try again
            print(f"mover: {e}", flush=True)
            state["moving"] = False
        time.sleep(0.5)


if __name__ == "__main__":
    main()
