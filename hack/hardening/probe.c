// Calls system calls gcr's hardening denies and prints "name errno" for
// each, plus control calls that must keep working. Without the filter the
// denied calls fail with other errors (EINVAL, EFAULT, EBADF...), so EPERM
// (ENOSYS for clone3) means the filter refused them.
// Build: cc -static -O2 -o probe probe.c
#define _GNU_SOURCE
#include <errno.h>
#include <sched.h>
#include <stdio.h>
#include <string.h>
#include <sys/socket.h>
#include <sys/syscall.h>
#include <unistd.h>
#include <netinet/in.h>

#ifndef AF_VSOCK
#define AF_VSOCK 40
#endif
#ifndef AF_ALG
#define AF_ALG 38
#endif

static void report(const char *name, long ret) {
	printf("%s %s\n", name, ret < 0 ? strerrorname_np(errno) : "OK");
}

int main(void) {
	report("io_uring_setup", syscall(SYS_io_uring_setup, 0, NULL));
	report("bpf", syscall(SYS_bpf, -1, NULL, 0));
	report("perf_event_open", syscall(SYS_perf_event_open, NULL, 0, -1, -1, 0));
	report("userfaultfd", syscall(SYS_userfaultfd, -1));
	report("keyctl", syscall(SYS_keyctl, -1, 0, 0, 0, 0));
	report("fsopen", syscall(SYS_fsopen, NULL, 0));
	report("open_tree", syscall(SYS_open_tree, -1, NULL, 0));
	report("init_module", syscall(SYS_init_module, NULL, 0, NULL));
	report("open_by_handle_at", syscall(SYS_open_by_handle_at, -1, NULL, 0));
	report("unshare_user", syscall(SYS_unshare, CLONE_NEWUSER));
	report("clone3", syscall(SYS_clone3, NULL, 0));
	report("socket_packet", socket(AF_PACKET, SOCK_RAW, 0));
	report("socket_raw", socket(AF_INET, SOCK_RAW, IPPROTO_ICMP));
	report("socket_vsock", socket(AF_VSOCK, SOCK_STREAM, 0));
	report("socket_alg", socket(AF_ALG, SOCK_SEQPACKET, 0));
	// Controls: must keep working.
	report("control_socket_tcp", socket(AF_INET, SOCK_STREAM, 0));
	report("control_socket_ping", socket(AF_INET, SOCK_DGRAM, IPPROTO_ICMP));
	pid_t pid = fork();
	if (pid == 0)
		_exit(0);
	report("control_fork", pid);
	return 0;
}
