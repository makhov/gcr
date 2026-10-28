#!/bin/bash
# Run critest's nginx image in a gcr pod and show why it exits.
set -u
D=$(mktemp -d)
IMG=registry.k8s.io/e2e-test-images/nginx:1.14-2
cat > "$D/pod.json" <<EOF
{"metadata": {"name": "ngx", "namespace": "default", "uid": "ngx-$$"}, "log_directory": "$D",
 "port_mappings": [{"container_port": 80}]}
EOF
cat > "$D/ctr.json" <<EOF
{"metadata": {"name": "nginx"}, "image": {"image": "$IMG"}, "log_path": "nginx.log"}
EOF
crictl pull $IMG >/dev/null
POD=$(crictl runp --runtime gcr "$D/pod.json")
CTR=$(crictl create "$POD" "$D/ctr.json" "$D/pod.json")
crictl start "$CTR" >/dev/null
sleep 2
crictl ps -a --pod "$POD" | tail -1
echo "== logs"; crictl logs "$CTR" 2>&1 | tail -5
crictl rmp -f "$POD" >/dev/null
rm -rf "$D"
