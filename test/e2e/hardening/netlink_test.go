//go:build e2e && linux

package hardening

import (
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/netpolicy"
	"github.com/makhov/gcr/test/e2e/framework"
)

// clients is what real tools do: iproute2's ip and tc, in a user and
// network namespace the container makes, adding link, qdisc and filter
// kinds, plus ip's batched address flush (several messages in one
// sendmsg). Each line is "name result": OK, or the tool's last line.
const clients = `
apk add -q iproute2 util-linux >/dev/null 2>&1 || { echo "setup FAIL"; exit 1; }
exec unshare -Urn sh -c '
try() { n=$1; shift; if out=$("$@" 2>&1); then echo "$n OK"; else echo "$n $(echo "$out" | tail -1)"; fi; }
ip link add d0 type dummy && ip link add d1 type dummy && ip link set d0 up
try veth    ip link add v0 type veth peer name v1
try bridge  ip link add br0 type bridge
try vlan    ip link add vl0 link d0 type vlan id 10
try bond    ip link add b0 type bond
try macvlan ip link add mv0 link d0 type macvlan
try vxlan   ip link add vx0 type vxlan id 1 dev d0 dstport 4789
try netem   tc qdisc add dev d0 root netem delay 1ms
try hfsc    tc qdisc add dev d1 root hfsc default 1
try clsact  tc qdisc add dev v0 clsact
try flower  tc filter add dev v0 ingress protocol ip flower ip_proto tcp action drop
i=1; while [ $i -le 12 ]; do ip addr add 10.99.$i.1/24 dev d0; i=$((i+1)); done
try flush   ip addr flush dev d0
echo "left $(ip -4 addr show dev d0 | grep -c inet)"
for n in b0 mv0 vx0; do ip link show $n >/dev/null 2>&1 && echo "exists_$n YES" || echo "exists_$n NO"; done
'`

var (
	allowedByPolicy = []string{"veth", "bridge", "vlan", "netem", "clsact", "flower"}
	refusedByPolicy = []string{"bond", "macvlan", "vxlan", "hfsc"}
)

// TestNetlinkClients runs the clients script with hardening off first,
// to learn what this kernel can do at all (a kind whose module is missing
// fails either way), then under the policy: the allowed kinds still work,
// the others are refused and leave no device, and the batched flush works.
func TestNetlinkClients(t *testing.T) {
	if !netpolicy.Available() {
		t.Skip("the kernel's LSM list has no bpf")
	}
	withoutHostUsernsRestriction(t)
	ns := framework.NewNamespace(t, "gcr-e2e")
	run := func(t *testing.T, config string) map[string]string {
		t.Helper()
		// The host's network, for apk; the script then makes its own.
		out, code := ns.Output(framework.UniqueID("nlc"), framework.Spec{
			Image: "docker.io/library/alpine:3.22", Args: []string{"sh", "-c", clients}, RuntimeConfig: config,
			SpecOpts: []oci.SpecOpts{oci.WithHostNamespace(specs.NetworkNamespace)},
			Mounts:   []specs.Mount{framework.BindRO("/etc/resolv.conf", "/etc/resolv.conf")},
		})
		t.Logf("%s", strings.TrimSpace(out))
		if code != 0 {
			t.Fatalf("script exited %d", code)
		}
		// "name rest of line": the rest is OK or the tool's message.
		r := map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			if name, rest, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
				r[name] = rest
			}
		}
		return r
	}

	control := run(t, `Hardening = "off"`)
	if control["left"] != "0" || control["flush"] != "OK" {
		t.Fatalf("control: batched flush does not work: flush=%s left=%s", control["flush"], control["left"])
	}
	var refused []string
	for _, k := range refusedByPolicy {
		if control[k] == "OK" {
			refused = append(refused, k)
		} else {
			t.Logf("%s not available on this kernel (%s): skipped", k, control[k])
		}
	}
	if len(refused) == 0 {
		t.Skip("none of the refused kinds is available on this kernel")
	}

	got := run(t, "")
	expect(t, got, "OK", allowedByPolicy...)
	expect(t, got, "OK", "flush")
	expect(t, got, "0", "left")
	for _, k := range refused {
		if r := got[k]; !strings.Contains(r, "Operation not permitted") {
			t.Errorf("%s: got %q, want refused", k, r)
		}
	}
	expect(t, got, "NO", "exists_b0", "exists_mv0", "exists_vx0")
}
