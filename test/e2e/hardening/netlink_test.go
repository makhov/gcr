//go:build e2e && linux

package hardening

import (
	"slices"
	"strings"
	"testing"

	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/makhov/gcr/internal/netpolicy"
	"github.com/makhov/gcr/test/e2e/framework"
)

// clients drives iproute2 the way real programs do, in a user and network
// namespace the container makes for itself: its messages carry the
// attributes and the slack the hand-built ones in probe do not (a kind
// without its NUL, a dump request in a buffer far longer than itself),
// which is where a policy that reads them goes wrong. Each line is
// "name result": OK, or the tool's complaint.
const clients = `
apk add -q iproute2 util-linux >/dev/null 2>&1 || { echo "setup FAILED"; exit 1; }
exec unshare -Urn sh -c '
run() { n=$1; shift; if out=$("$@" 2>&1); then echo "$n OK"; else echo "$n ${out##*: }"; fi; }
ip link add d0 type dummy && ip link set d0 up
run veth    ip link add k-veth type veth peer name k-veth1
run bridge  ip link add k-bridge type bridge
run bond    ip link add k-bond type bond
run macvlan ip link add k-macvlan link d0 type macvlan
run vxlan   ip link add k-vxlan type vxlan id 7 dev d0 dstport 4789
run netem   tc qdisc replace dev d0 root netem delay 1ms
run fq      tc qdisc replace dev d0 root fq_codel
run hfsc    tc qdisc replace dev d0 root hfsc default 1
run clsact  tc qdisc add dev k-veth clsact
run flower  tc filter add dev k-veth ingress protocol ip flower ip_proto tcp action drop
run addr    ip addr add 10.9.9.1/24 dev d0
run dump    ip -4 addr show
i=1; while [ $i -le 14 ]; do ip addr add 10.8.$i.1/24 dev d0; i=$((i+1)); done
run flush   ip addr flush dev d0
for n in k-bond k-macvlan k-vxlan; do
  ip link show $n >/dev/null 2>&1 && echo "$n EXISTS" || echo "$n ABSENT"
done
'`

// What the policy allows and refuses among the kinds clients asks for,
// and the requests that name no kind, which it must not touch.
var (
	allowedKinds = []string{"veth", "bridge", "netem", "fq", "clsact", "flower"}
	// The link kinds among the refused ones leave a device behind if the
	// kernel ever acts on them; hfsc is a qdisc.
	refusedLinks  = []string{"bond", "macvlan", "vxlan"}
	refusedKinds  = append([]string{"hfsc"}, refusedLinks...)
	plainRequests = []string{"addr", "dump", "flush"}
)

// TestNetlinkClients checks the policy against real clients. It first runs
// them with hardening off, to see what this kernel can do at all (a kind
// whose module is missing fails either way), then under the policy.
func TestNetlinkClients(t *testing.T) {
	if !netpolicy.Available() {
		t.Skip("the kernel's LSM list has no bpf")
	}
	withoutHostUsernsRestriction(t)
	ns := framework.NewNamespace(t, "gcr-e2e")
	run := func(t *testing.T, config string) map[string]string {
		t.Helper()
		// The host's network, so apk can install iproute2; the script
		// then makes a network namespace of its own.
		out, code := ns.Output(framework.UniqueID("nlc"), framework.Spec{
			Image: "docker.io/library/alpine:3.22", Args: []string{"sh", "-c", clients}, RuntimeConfig: config,
			SpecOpts: []oci.SpecOpts{oci.WithHostNamespace(specs.NetworkNamespace)},
			Mounts:   []specs.Mount{framework.BindRO("/etc/resolv.conf", "/etc/resolv.conf")},
		})
		t.Logf("%s", strings.TrimSpace(out))
		if code != 0 {
			t.Fatalf("clients exited %d", code)
		}
		r := map[string]string{}
		for _, line := range strings.Split(out, "\n") {
			if name, result, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
				r[name] = result
			}
		}
		return r
	}

	// Control: without gcr's hardening every request below goes through,
	// so what the policy run refuses is the policy's doing.
	control := run(t, `Hardening = "off"`)
	for _, name := range append(append([]string{}, allowedKinds...), plainRequests...) {
		if control[name] != "OK" {
			t.Fatalf("control: %s did not work without hardening (%s)", name, control[name])
		}
	}
	var refused []string
	for _, k := range refusedKinds {
		if control[k] == "OK" {
			refused = append(refused, k)
		} else {
			t.Logf("%s is not available on this kernel (%s): not checked", k, control[k])
		}
	}
	if len(refused) == 0 {
		t.Skip("none of the refused kinds is available on this kernel")
	}

	_, before, err := netpolicy.PinnedStats()
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, "")
	// The allowed kinds and the requests that name none still work: a
	// container configures its own network, Docker-in-Docker included.
	expect(t, got, "OK", allowedKinds...)
	expect(t, got, "OK", plainRequests...)
	for _, k := range refused {
		if !strings.Contains(got[k], "Operation not permitted") {
			t.Errorf("%s: got %q, want refused", k, got[k])
		}
		// A refused request must have made nothing.
		if slices.Contains(refusedLinks, k) {
			expect(t, got, "ABSENT", "k-"+k)
		}
	}
	// The refusals came from the policy and not from something else on
	// the host: its counter moved, once per refused kind at least.
	_, after, err := netpolicy.PinnedStats()
	if err != nil {
		t.Fatal(err)
	}
	if after < before+uint64(len(refused)) {
		t.Errorf("policy refused %d messages, want at least %d", after-before, len(refused))
	}
}
