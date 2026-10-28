// Calls system calls gcr's hardening denies and prints "name errno" for
// each, plus control calls that must keep working. Without the filter the
// denied calls fail with other errors (EINVAL, EFAULT, EBADF...), so EPERM
// (ENOSYS for clone3) means the filter refused them.
//
// "probe mount" (run with CAP_SYS_ADMIN) instead checks mounts gcr performs
// for the container, by their exact return value: a refusal must be -1
// with errno set, and paths through /proc/self must mean the caller.
//
// "probe leak SOCKET" receives a directory descriptor over the unix socket
// SOCKET (as a leaked host descriptor would reach a container) and reads
// "secret" through it: hack/e2e-landlock.sh.
// Build: cc -static -O2 -o probe probe.c
#define _GNU_SOURCE
#include <errno.h>
#include <sched.h>
#include <stdio.h>
#include <string.h>
#include <fcntl.h>
#include <stdlib.h>
#include <sys/mount.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
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

// report_exact is report for calls that must return exactly 0 or -1.
static void report_exact(const char *name, long ret) {
	if (ret != 0 && ret != -1)
		printf("%s BADRET(%ld)\n", name, ret);
	else
		report(name, ret);
}

// same_file reports whether two paths name the same inode.
static const char *same_file(const char *a, const char *b) {
	struct stat x, y;
	if (stat(a, &x) < 0 || stat(b, &y) < 0)
		return "MISSING";
	return x.st_dev == y.st_dev && x.st_ino == y.st_ino ? "OK" : "DIFFERENT";
}

static int mounts(void) {
	char path[64];
	int fd;

	mkdir("/mnt", 0755);
	report_exact("mount_ramfs", mount("none", "/mnt", "ramfs", 0, NULL));
	report_exact("mount_tmpfs", mount("none", "/mnt", "tmpfs", 0, NULL));

	// dockerd keeps network namespaces by bind-mounting them so.
	close(open("/mnt/netns", O_CREAT | O_WRONLY, 0644));
	snprintf(path, sizeof(path), "/proc/self/task/%ld/ns/net", syscall(SYS_gettid));
	report_exact("mount_self_task_ns", mount(path, "/mnt/netns", NULL, MS_BIND, NULL));
	printf("mount_self_task_ns_same %s\n", same_file("/mnt/netns", "/proc/self/ns/net"));
	close(open("/mnt/netns2", O_CREAT | O_WRONLY, 0644));
	report_exact("mount_thread_self_ns", mount("/proc/thread-self/ns/net", "/mnt/netns2", NULL, MS_BIND, NULL));
	printf("mount_thread_self_ns_same %s\n", same_file("/mnt/netns2", "/proc/self/ns/net"));

	// runc names mount sources and targets by file descriptor.
	mkdir("/mnt/src", 0755);
	mkdir("/mnt/dst", 0755);
	close(open("/mnt/src/marker", O_CREAT | O_WRONLY, 0644));
	fd = open("/mnt/dst", O_PATH | O_DIRECTORY);
	snprintf(path, sizeof(path), "/proc/self/fd/%d", fd);
	report_exact("mount_fd_target", mount("/mnt/src", path, NULL, MS_BIND, NULL));
	printf("mount_fd_target_same %s\n", same_file("/mnt/dst/marker", "/mnt/src/marker"));
	return 0;
}

static int leak(const char *path) {
	struct sockaddr_un addr = {.sun_family = AF_UNIX};
	char byte, cbuf[CMSG_SPACE(sizeof(int))], secret[64] = "";
	struct iovec iov = {.iov_base = &byte, .iov_len = 1};
	struct msghdr msg = {.msg_iov = &iov, .msg_iovlen = 1, .msg_control = cbuf, .msg_controllen = sizeof(cbuf)};
	struct cmsghdr *cmsg;
	int sk, dirfd, fd;

	strncpy(addr.sun_path, path, sizeof(addr.sun_path) - 1);
	sk = socket(AF_UNIX, SOCK_STREAM, 0);
	if (sk < 0 || connect(sk, (struct sockaddr *)&addr, sizeof(addr)) < 0 || recvmsg(sk, &msg, 0) < 1) {
		printf("leak NOFD %s\n", strerrorname_np(errno));
		return 1;
	}
	cmsg = CMSG_FIRSTHDR(&msg);
	if (!cmsg || cmsg->cmsg_type != SCM_RIGHTS) {
		printf("leak NOFD\n");
		return 1;
	}
	memcpy(&dirfd, CMSG_DATA(cmsg), sizeof(int));
	fd = openat(dirfd, "secret", O_RDONLY);
	if (fd < 0) {
		printf("leak %s\n", strerrorname_np(errno));
		return 0;
	}
	if (read(fd, secret, sizeof(secret) - 1) < 0)
		secret[0] = 0;
	printf("leak OK %s", secret);
	return 0;
}

int main(int argc, char **argv) {
	if (argc > 2 && strcmp(argv[1], "leak") == 0)
		return leak(argv[2]);
	if (argc > 1 && strcmp(argv[1], "mount") == 0)
		return mounts();
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
