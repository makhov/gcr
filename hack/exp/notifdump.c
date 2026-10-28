// Experiment: can CRIU checkpoint a process held in a seccomp user
// notification, and what happens to it after restore?
//
// The child installs a filter that sends mkdir() to a listener, then loops:
// count++, write count to /tmp/nd/count, mkdir(), log the result.
// The parent is the supervisor: on each notification it writes the child's
// pid to /tmp/nd/held and waits for /tmp/nd/go before letting mkdir run.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/filter.h>
#include <linux/seccomp.h>
#include <stddef.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

static void writef(const char *path, long v) {
	FILE *f = fopen(path, "w");
	if (f) { fprintf(f, "%ld\n", v); fclose(f); }
}

static int install_filter(void) {
	struct sock_filter f[] = {
		BPF_STMT(BPF_LD | BPF_W | BPF_ABS, offsetof(struct seccomp_data, nr)),
		BPF_JUMP(BPF_JMP | BPF_JEQ | BPF_K, __NR_mkdirat, 0, 1),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_USER_NOTIF),
		BPF_STMT(BPF_RET | BPF_K, SECCOMP_RET_ALLOW),
	};
	struct sock_fprog prog = { .len = sizeof(f) / sizeof(f[0]), .filter = f };
	prctl(PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0);
	return syscall(__NR_seccomp, SECCOMP_SET_MODE_FILTER,
		       SECCOMP_FILTER_FLAG_NEW_LISTENER, &prog);
}

static void send_fd(int sock, int fd) {
	char c = 0;
	struct iovec iov = { .iov_base = &c, .iov_len = 1 };
	char buf[CMSG_SPACE(sizeof(int))] = {0};
	struct msghdr m = { .msg_iov = &iov, .msg_iovlen = 1,
			    .msg_control = buf, .msg_controllen = sizeof(buf) };
	struct cmsghdr *cm = CMSG_FIRSTHDR(&m);
	cm->cmsg_level = SOL_SOCKET; cm->cmsg_type = SCM_RIGHTS;
	cm->cmsg_len = CMSG_LEN(sizeof(int));
	memcpy(CMSG_DATA(cm), &fd, sizeof(int));
	sendmsg(sock, &m, 0);
}

static int recv_fd(int sock) {
	char c;
	struct iovec iov = { .iov_base = &c, .iov_len = 1 };
	char buf[CMSG_SPACE(sizeof(int))] = {0};
	struct msghdr m = { .msg_iov = &iov, .msg_iovlen = 1,
			    .msg_control = buf, .msg_controllen = sizeof(buf) };
	if (recvmsg(sock, &m, 0) <= 0) return -1;
	int fd;
	memcpy(&fd, CMSG_DATA(CMSG_FIRSTHDR(&m)), sizeof(int));
	return fd;
}

static void child(int sock) {
	int lfd = install_filter();
	if (lfd < 0) { perror("seccomp"); exit(1); }
	send_fd(sock, lfd);
	close(lfd);
	close(sock);
	setsid();
	int fd = open("/dev/null", O_RDWR);
	dup2(fd, 0); dup2(fd, 1); dup2(fd, 2);
	FILE *log = fopen("/tmp/nd/child.log", "a");
	setvbuf(log, NULL, _IOLBF, 0);
	for (long i = 1;; i++) {
		writef("/tmp/nd/count", i);
		char d[64];
		snprintf(d, sizeof(d), "/tmp/nd/d%ld", i);
		int r = mkdir(d, 0755);
		fprintf(log, "count=%ld mkdir=%d errno=%d\n", i, r, r ? errno : 0);
		usleep(200000);
	}
}

int main(void) {
	mkdir("/tmp/nd", 0755);
	int sv[2];
	socketpair(AF_UNIX, SOCK_STREAM, 0, sv);
	pid_t pid = fork();
	if (pid == 0) { close(sv[0]); child(sv[1]); }
	close(sv[1]);
	int lfd = recv_fd(sv[0]);
	printf("child=%d listener=%d\n", pid, lfd);
	fflush(stdout);

	struct seccomp_notif *req = calloc(1, 512);
	struct seccomp_notif_resp *resp = calloc(1, 512);
	for (;;) {
		memset(req, 0, 512);
		if (ioctl(lfd, SECCOMP_IOCTL_NOTIF_RECV, req) < 0) {
			perror("notif recv");
			return 0;
		}
		// Hold the call only when asked to (/tmp/nd/hold exists).
		if (access("/tmp/nd/hold", F_OK) == 0) {
			writef("/tmp/nd/held", req->pid);
			while (access("/tmp/nd/go", F_OK) != 0)
				usleep(50000);
			unlink("/tmp/nd/go");
			unlink("/tmp/nd/hold");
			unlink("/tmp/nd/held");
		}
		memset(resp, 0, 512);
		resp->id = req->id;
		resp->flags = SECCOMP_USER_NOTIF_FLAG_CONTINUE;
		if (ioctl(lfd, SECCOMP_IOCTL_NOTIF_SEND, resp) < 0)
			printf("notif send: %s\n", strerror(errno));
	}
}
