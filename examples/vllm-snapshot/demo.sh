#!/bin/bash
# vLLM from scratch against vLLM from a gcr snapshot.
#
# Starts vLLM's OpenAI server on a GPU, cold: weights read from disk onto
# the GPU, the model compiled, CUDA graphs captured. Once it answers, gcr
# snapshots it, GPU memory included, and stops it. Then a new container
# starts from the snapshot. Both are timed to their first completion.
#
# Run as root on a node with containerd, gcr (gcr setup), the NVIDIA
# driver and its CDI spec (nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml).
#
#   sudo examples/vllm-snapshot/demo.sh
set -euo pipefail

IMAGE=${IMAGE:-docker.io/vllm/vllm-openai:v0.32.0}
MODEL=${MODEL:-Qwen/Qwen3-8B}
GPU=${GPU:-nvidia.com/gpu=0}
PORT=${PORT:-8000}
NS=${NS:-gcr-demo}
DATA=${DATA:-/var/lib/gcr-demo}
SNAPSHOT=${SNAPSHOT:-/var/lib/gcr/snapshots/vllm-$(basename "$MODEL")}
# The snapshot holds all the GPU memory vLLM reserved, weights and KV
# cache alike, and CRIU's CUDA plugin stages it in host memory: reserve a
# share both fit.
VLLM_ARGS=${VLLM_ARGS:---max-model-len 8192 --gpu-memory-utilization 0.3}
# k0s keeps its own containerd and ctr.
CTR=${CTR:-$(command -v k0s >/dev/null && echo "k0s ctr" || echo ctr)}

ctr() { $CTR -n "$NS" "$@"; }
now() { date +%s.%N; }
since() { awk -v a="$1" -v b="$(now)" 'BEGIN { printf "%.1f", b - a }'; }
say() { printf '%s\n' "$*" >&2; }

# The container: the GPU through CDI, the host's network (one server at a
# time on PORT), the model and the log from the host.
#   run ID [CTR-OPTION...] -- COMMAND...
run() {
	local id=$1 opts=()
	shift
	while [ "$1" != -- ]; do
		opts+=("$1")
		shift
	done
	shift
	ctr run -d --runtime io.containerd.gcr.v2 --device "$GPU" --net-host \
		--annotation gcr.dev/host-users=true \
		--mount "type=bind,src=$DATA/models,dst=/models,options=rbind:ro" \
		--mount "type=bind,src=$DATA/logs,dst=/logs,options=rbind:rw" \
		"${opts[@]}" "$IMAGE" "$id" "$@" >/dev/null
}

remove() {
	ctr task kill -s SIGKILL "$1" >/dev/null 2>&1 || true
	for _ in $(seq 50); do ctr task ls -q 2>/dev/null | grep -qx "$1" || break; sleep 0.2; ctr task rm "$1" >/dev/null 2>&1 || true; done
	ctr container rm "$1" >/dev/null 2>&1 || true
}

# Waits for the server to answer a completion; prints how long it took
# from start, in seconds.
first_completion() {
	local start=$1 id=$2
	for _ in $(seq 1800); do
		if curl -sf -m 30 "http://127.0.0.1:$PORT/v1/completions" -H 'Content-Type: application/json' \
			-d '{"model": "m", "prompt": "The capital of France is", "max_tokens": 8, "temperature": 0}' \
			-o "$DATA/logs/$id.answer"; then
			since "$start"
			return
		fi
		if ! ctr task ls 2>/dev/null | grep -q "^$id .*RUNNING"; then
			say "$id stopped before answering; its log:"
			tail -30 "$DATA/logs/vllm.log" >&2
			exit 1
		fi
		sleep 0.2
	done
	say "$id did not answer in 6 minutes"
	exit 1
}

mkdir -p "$DATA/models" "$DATA/logs" "$(dirname "$SNAPSHOT")"
for id in vllm-cold vllm-restored vllm-tar vllm-nydus; do remove "$id"; done
for s in "$SNAPSHOT" "$SNAPSHOT-tar" "$SNAPSHOT-nydus"; do gcr snapshot-rm "$s" >/dev/null 2>&1 || true; done

if ! ctr images ls -q | grep -qx "$IMAGE"; then
	say "pulling $IMAGE"
	ctr images pull "$IMAGE" >/dev/null
fi
if [ ! -f "$DATA/models/$MODEL/config.json" ]; then
	say "downloading $MODEL to $DATA/models (once)"
	ctr run --rm --net-host --mount "type=bind,src=$DATA/models,dst=/models,options=rbind:rw" \
		"$IMAGE" vllm-download hf download "$MODEL" --local-dir "/models/$MODEL" >/dev/null 2>&1
fi
: >"$DATA/logs/vllm.log"
serve="exec vllm serve /models/$MODEL --served-model-name m --port $PORT $VLLM_ARGS >>/logs/vllm.log 2>&1"

say "1. cold start: $MODEL from disk"
start=$(now)
run vllm-cold -- sh -c "$serve"
cold=$(first_completion "$start" vllm-cold)
say "   answered after ${cold}s: $(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["choices"][0]["text"].strip())' "$DATA/logs/vllm-cold.answer" 2>/dev/null)"
gpu_mib=$(nvidia-smi --query-gpu=memory.used --format=csv,noheader,nounits | head -1)

say "2. snapshot: CPU and GPU memory, while it runs"
start=$(now)
gcr -n "$NS" snapshot vllm-cold "$SNAPSHOT" >/dev/null
snap=$(since "$start")
size=$(du -sh "$SNAPSHOT" | cut -f1)
say "   ${snap}s, $size on disk ($gpu_mib MiB of GPU memory in use)"
remove vllm-cold

say "3. restore from the snapshot"
start=$(now)
run vllm-restored --annotation "gcr.dev/fork-from=$SNAPSHOT" -- true
restored=$(first_completion "$start" vllm-restored)
say "   answered after ${restored}s"

# With REGISTRY (host:port), the snapshot goes through it in both formats,
# and each comes back on a node that has only the image: the local copy is
# removed first. tar is pulled whole; nydus is mounted and read as the
# restore reads it (snapshot-pull -lazy).
pulled=""
if [ -n "${REGISTRY:-}" ]; then
	remove vllm-restored
	for format in tar nydus; do
		say "4. push, $format: $REGISTRY/gcr-demo/vllm:$format"
		start=$(now)
		gcr snapshot-push -format "$format" "$SNAPSHOT" "$REGISTRY/gcr-demo/vllm:$format" >/dev/null
		eval "push_$format=$(since "$start")"
	done
	gcr snapshot-rm "$SNAPSHOT" >/dev/null
	for format in tar nydus; do
		lazy=$([ "$format" = nydus ] && echo -lazy || true)
		say "5. pull ($format${lazy:+, lazy}) and restore"
		start=$(now)
		gcr snapshot-pull $lazy "$REGISTRY/gcr-demo/vllm:$format" "$SNAPSHOT-$format" >/dev/null
		pull=$(since "$start")
		run "vllm-$format" --annotation "gcr.dev/fork-from=$SNAPSHOT-$format" -- true
		total=$(first_completion "$start" "vllm-$format")
		say "   pulled in ${pull}s, answered after ${total}s"
		remove "vllm-$format"
		gcr snapshot-rm "$SNAPSHOT-$format" >/dev/null
		push=push_$format
		pulled+=$(printf '\n  from the registry, %-6s push %5ss   pull + restore to first completion %ss' "$format:" "${!push}" "$total")
	done
fi

# What the cold start spent its time on, as vLLM logs it.
breakdown=$(grep -E "Loading weights took|Model loading took|torch.compile takes|Graph capturing finished|init engine .* took" \
	"$DATA/logs/vllm.log" | sed 's/^.*\] //' | head -8)

cat <<EOF

$MODEL on $(nvidia-smi --query-gpu=name --format=csv,noheader | head -1)
  cold start to first completion      ${cold}s
  restore to first completion         ${restored}s
  snapshot (taken once, server running) ${snap}s, $size${pulled}

The cold start, by vLLM's log:
$(sed 's/^/  /' <<<"$breakdown")
EOF
