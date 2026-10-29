// gcr's netlink policy: a BPF LSM program on netlink_send that limits
// which network objects processes in gcr containers may ask the kernel to
// create over NETLINK_ROUTE. seccomp sees only a socket's family and
// protocol; the kind of a link, qdisc or filter is in the message, so the
// decision happens here, in the kernel, with no round trip to user space.
//
// Containers: a map of cgroup IDs the shim fills; a task is covered when
// any ancestor of its cgroup is in it (nested containers' cgroups are
// below their container's). Everything else on the node is untouched.
//
// Policy: an RTM_NEWLINK with a kind (IFLA_LINKINFO/IFLA_INFO_KIND) must
// name an allowed link kind, and RTM_NEWQDISC/TCLASS/TFILTER/CHAIN must
// name an allowed TCA_KIND; messages without a kind, other messages and
// other netlink protocols pass.
//
// The messages are walked as netlink_rcv_skb walks them, so the program
// judges what the kernel will act on and nothing else: a buffer holds
// one or more messages and often trailing room the sender did not use
// (iproute2 sends a 24-byte dump request in a 152-byte buffer), and the
// kernel stops at the first header that is not a whole message. Only a
// valid message the program has no budget left to inspect is refused.
//
// Build: make bpf (clang -target bpf, CO-RE); the object, next to the Go
// package, is embedded in the shim.
#include <linux/types.h>
#include <linux/bpf.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char LICENSE[] SEC("license") = "GPL";

// What we read of the kernel's structures; CO-RE finds the fields.
struct sock_common {
	unsigned short skc_family;
} __attribute__((preserve_access_index));

struct sock {
	struct sock_common __sk_common;
	__u16 sk_protocol;
} __attribute__((preserve_access_index));

struct sk_buff {
	unsigned int len;
	unsigned char *data;
} __attribute__((preserve_access_index));

#define NETLINK_ROUTE 0
#define RTM_NEWLINK 16
#define RTM_NEWQDISC 36
#define RTM_NEWTCLASS 40
#define RTM_NEWTFILTER 44
#define RTM_NEWCHAIN 100
#define IFLA_LINKINFO 18
#define IFLA_INFO_KIND 1
#define TCA_KIND 1
#define NLA_TYPE_MASK 0x3fff
#define NLMSG_HDRLEN 16
#define IFINFOMSG_LEN 16
#define TCMSG_LEN 20
#define NLA_HDRLEN 4
#define EPERM 1

#define MAX_CGROUP_LEVELS 16
#define MAX_MESSAGES 32
#define MAX_ATTRS 48
#define KIND_LEN 16

struct nlmsghdr {
	__u32 nlmsg_len;
	__u16 nlmsg_type;
	__u16 nlmsg_flags;
	__u32 nlmsg_seq;
	__u32 nlmsg_pid;
};

struct nlattr {
	__u16 nla_len;
	__u16 nla_type;
};

struct kind {
	char name[KIND_LEN];
};

// cgroups are the gcr containers' cgroup IDs (value unused).
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 65536);
	__type(key, __u64);
	__type(value, __u32);
} cgroups SEC(".maps");

// link_kinds and tc_kinds are the allowed kinds (value unused).
struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 128);
	__type(key, struct kind);
	__type(value, __u8);
} link_kinds SEC(".maps");

struct {
	__uint(type, BPF_MAP_TYPE_HASH);
	__uint(max_entries, 128);
	__type(key, struct kind);
	__type(value, __u8);
} tc_kinds SEC(".maps");

// stats counts decisions: 0 messages checked, 1 refused.
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 2);
	__type(key, __u32);
	__type(value, __u64);
} stats SEC(".maps");

enum { STAT_CHECKED = 0, STAT_REFUSED = 1 };

static __always_inline void count(__u32 what)
{
	__u64 *v = bpf_map_lookup_elem(&stats, &what);
	if (v)
		__sync_fetch_and_add(v, 1);
}

static __always_inline int in_container(void)
{
	for (int level = 0; level < MAX_CGROUP_LEVELS; level++) {
		__u64 id = bpf_get_current_ancestor_cgroup_id(level);
		if (!id)
			break;
		if (bpf_map_lookup_elem(&cgroups, &id))
			return 1;
	}
	return 0;
}

// find_attr looks for attribute type want in [off, end) of data. Returns
// 1 and the attribute's payload range, 0 if absent, -1 if undecided.
static __always_inline int find_attr(const unsigned char *data, __u32 off, __u32 end, __u16 want,
				     __u32 *payload, __u32 *payload_end)
{
	struct nlattr a;

	for (int i = 0; i < MAX_ATTRS; i++) {
		if (off + NLA_HDRLEN > end)
			return 0;
		if (bpf_probe_read_kernel(&a, sizeof(a), data + off))
			return -1;
		if (a.nla_len < NLA_HDRLEN || off + a.nla_len > end)
			return -1;
		if ((a.nla_type & NLA_TYPE_MASK) == want) {
			*payload = off + NLA_HDRLEN;
			*payload_end = off + a.nla_len;
			return 1;
		}
		off += (a.nla_len + 3) & ~3u;
	}
	return -1;
}

// repeated says whether attribute type want occurs more than once in
// [off, end) of data: 1 if so, 0 if not, -1 if undecided. The kernel's
// parser keeps the last of several, find_attr finds the first: "veth"
// then "vxlan" would pass as veth. No client sends a kind twice, so such
// a message is refused. (A separate pass, counting only: carrying the
// last match through the loop is more than the verifier will follow. A
// global function, so it is verified once, not at every call inlined in
// the loop over messages; its arguments must be scalars.)
__noinline int repeated(__u64 addr, __u32 off, __u32 end, __u16 want)
{
	const unsigned char *data = (const unsigned char *)addr;
	struct nlattr a;
	int seen = 0;

	for (int i = 0; i < MAX_ATTRS; i++) {
		if (off + NLA_HDRLEN > end)
			return 0;
		if (bpf_probe_read_kernel(&a, sizeof(a), data + off))
			return -1;
		if (a.nla_len < NLA_HDRLEN || off + a.nla_len > end)
			return -1;
		if ((a.nla_type & NLA_TYPE_MASK) == want) {
			if (seen)
				return 1;
			seen = 1;
		}
		off += (a.nla_len + 3) & ~3u;
	}
	return -1;
}

// kind_allowed says whether the kind in the attribute payload [off, end)
// is in map. The payload is a string of that length, with a trailing NUL
// only sometimes: iproute2 sends "veth" in four bytes, the kernel's own
// nla_strcmp compares what the length says. Reading to a NUL instead
// would run into the next attribute, so the length bounds the read.
static __always_inline int kind_allowed(const unsigned char *data, __u32 off, __u32 end, void *map)
{
	struct kind k;
	__u32 n = end - off;

	__builtin_memset(&k, 0, sizeof(k));
	if (off >= end || n >= KIND_LEN)
		return 0; // empty, or longer than any kind we allow
	for (__u32 i = 0; i < KIND_LEN - 1; i++) {
		char c;

		if (i >= n)
			break;
		if (bpf_probe_read_kernel(&c, sizeof(c), data + off + i))
			return 0;
		if (!c)
			break; // a payload that does carry its NUL
		k.name[i] = c;
	}
	return bpf_map_lookup_elem(map, &k) != NULL;
}

// check_message decides one netlink message at [off, off+len): 0 to pass,
// -1 to refuse.
static __always_inline int check_message(const unsigned char *data, __u32 off, __u32 len, __u16 type)
{
	__u32 end = off + len, attrs, p, pe, q, qe;
	int r;

	switch (type) {
	case RTM_NEWLINK:
		attrs = off + NLMSG_HDRLEN + IFINFOMSG_LEN;
		if (attrs > end)
			return 0;
		if (repeated((__u64)data, attrs, end, IFLA_LINKINFO))
			return -1;
		r = find_attr(data, attrs, end, IFLA_LINKINFO, &p, &pe);
		if (r < 0)
			return -1;
		if (r == 0)
			return 0;
		if (repeated((__u64)data, p, pe, IFLA_INFO_KIND))
			return -1;
		r = find_attr(data, p, pe, IFLA_INFO_KIND, &q, &qe);
		if (r < 0)
			return -1;
		if (r == 0)
			return 0;
		return kind_allowed(data, q, qe, &link_kinds) ? 0 : -1;
	case RTM_NEWQDISC:
	case RTM_NEWTCLASS:
	case RTM_NEWTFILTER:
	case RTM_NEWCHAIN:
		attrs = off + NLMSG_HDRLEN + TCMSG_LEN;
		if (attrs > end)
			return 0;
		if (repeated((__u64)data, attrs, end, TCA_KIND))
			return -1;
		r = find_attr(data, attrs, end, TCA_KIND, &p, &pe);
		if (r < 0)
			return -1;
		if (r == 0)
			return 0;
		return kind_allowed(data, p, pe, &tc_kinds) ? 0 : -1;
	}
	return 0;
}

SEC("lsm/netlink_send")
int BPF_PROG(gcr_netlink_send, struct sock *sk, struct sk_buff *skb)
{
	const unsigned char *data;
	struct nlmsghdr h;
	__u32 len, off = 0;

	if (BPF_CORE_READ(sk, sk_protocol) != NETLINK_ROUTE)
		return 0;
	if (!in_container())
		return 0;
	count(STAT_CHECKED);

	len = BPF_CORE_READ(skb, len);
	data = BPF_CORE_READ(skb, data);
	for (int i = 0; i < MAX_MESSAGES; i++) {
		if (off + NLMSG_HDRLEN > len)
			return 0; // no room for another header: the kernel stops too
		if (bpf_probe_read_kernel(&h, sizeof(h), data + off))
			goto refuse;
		if (h.nlmsg_len < NLMSG_HDRLEN || off + h.nlmsg_len > len)
			return 0; // not a whole message: the kernel ignores the rest
		if (check_message(data, off, h.nlmsg_len, h.nlmsg_type))
			goto refuse;
		off += (h.nlmsg_len + 3) & ~3u;
	}
	// Out of budget: refuse only if what follows is a message the kernel
	// would act on.
	if (off + NLMSG_HDRLEN <= len) {
		if (bpf_probe_read_kernel(&h, sizeof(h), data + off))
			goto refuse;
		if (h.nlmsg_len >= NLMSG_HDRLEN && off + h.nlmsg_len <= len)
			goto refuse;
	}
	return 0;

refuse:
	count(STAT_REFUSED);
	return -EPERM;
}
