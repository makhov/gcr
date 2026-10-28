// Runs before the Go runtime when the shim re-executes itself with
// _GCR_NSEXEC set: the only point where the process is single-threaded,
// which joining a user namespace requires.
//
// _GCR_NSEXEC=mount performs a mount(2) on behalf of the process _GCR_PID,
// with arguments the supervisor already copied and checked: it joins that
// process's namespaces, root and working directory, takes on its user and
// group IDs and effective capabilities, so the kernel checks the mount
// exactly as it would the caller's, and exits with the errno (0 on success).
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <grp.h>
#include <linux/capability.h>
#include <sched.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mount.h>
#include <sys/prctl.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#define FAIL(e) _exit((e) ? (e) : EPERM)

static const struct {
	const char *name;
	int type;
} namespaces[] = {
	{"user", CLONE_NEWUSER}, {"mnt", CLONE_NEWNS},	 {"pid", CLONE_NEWPID},	      {"net", CLONE_NEWNET},
	{"ipc", CLONE_NEWIPC},	 {"uts", CLONE_NEWUTS}, {"cgroup", CLONE_NEWCGROUP},
};
#define NNS (sizeof(namespaces) / sizeof(namespaces[0]))

// open_namespaces opens all of pid's namespaces up front: once we join its
// mount namespace, /proc is the container's and pid means something else.
// A namespace we are already in gets -2: joining it fails for user
// namespaces and is a no-op for the others.
static int open_namespaces(int pid, int fds[NNS])
{
	char path[64], self[64];
	struct stat a, b;

	for (size_t i = 0; i < NNS; i++) {
		snprintf(path, sizeof(path), "/proc/%d/ns/%s", pid, namespaces[i].name);
		snprintf(self, sizeof(self), "/proc/self/ns/%s", namespaces[i].name);
		fds[i] = open(path, O_RDONLY | O_CLOEXEC);
		if (fds[i] < 0)
			return -1;
		if (fstat(fds[i], &a) == 0 && stat(self, &b) == 0 && a.st_dev == b.st_dev && a.st_ino == b.st_ino) {
			close(fds[i]);
			fds[i] = -2;
		}
	}
	return 0;
}

static int join(int fds[NNS], size_t i)
{
	return fds[i] == -2 ? 0 : setns(fds[i], namespaces[i].type);
}

struct creds {
	uid_t uid[3];
	gid_t gid[3];
	gid_t groups[64];
	int ngroups;
	unsigned long long capeff;
	int nspid; // the caller's pid in its own pid namespace
};

// read_creds reads the caller's credentials as seen from our current user
// namespace (so after joining the caller's, in the container's terms).
static int read_creds(int pid, struct creds *c)
{
	char path[64], line[512];
	FILE *f;
	int found = 0;

	snprintf(path, sizeof(path), "/proc/%d/status", pid);
	f = fopen(path, "re");
	if (!f)
		return -1;
	while (fgets(line, sizeof(line), f)) {
		if (sscanf(line, "Uid:\t%u\t%u\t%u", &c->uid[0], &c->uid[1], &c->uid[2]) == 3)
			found |= 1;
		else if (sscanf(line, "Gid:\t%u\t%u\t%u", &c->gid[0], &c->gid[1], &c->gid[2]) == 3)
			found |= 2;
		else if (sscanf(line, "CapEff:\t%llx", &c->capeff) == 1)
			found |= 4;
		else if (strncmp(line, "NSpid:", 6) == 0) {
			char *p = line + 6, *end;
			for (;;) { // the last value is the innermost namespace's
				long v = strtol(p, &end, 10);
				if (end == p)
					break;
				c->nspid = v;
				p = end;
			}
			found |= 8;
		}
		else if (strncmp(line, "Groups:", 7) == 0) {
			char *p = line + 7, *end;
			c->ngroups = 0;
			while (c->ngroups < 64) {
				unsigned long g = strtoul(p, &end, 10);
				if (end == p)
					break;
				c->groups[c->ngroups++] = g;
				p = end;
			}
		}
	}
	fclose(f);
	return found == 15 ? 0 : -1;
}

static int become(struct creds *c)
{
	struct __user_cap_header_struct hdr = {.version = _LINUX_CAPABILITY_VERSION_3};
	struct __user_cap_data_struct data[2] = {0};

	// Groups may be denied in user namespaces (setgroups "deny"); then the
	// caller has the same groups we already have.
	if (setgroups(c->ngroups, c->groups) < 0 && errno != EPERM)
		return -1;
	if (prctl(PR_SET_KEEPCAPS, 1, 0, 0, 0) < 0)
		return -1;
	if (setresgid(c->gid[0], c->gid[1], c->gid[2]) < 0)
		return -1;
	if (setresuid(c->uid[0], c->uid[1], c->uid[2]) < 0)
		return -1;
	data[0].effective = data[0].permitted = (__u32)c->capeff;
	data[1].effective = data[1].permitted = (__u32)(c->capeff >> 32);
	return syscall(SYS_capset, &hdr, data);
}

static const char *arg(const char *name)
{
	const char *v = getenv(name);
	return v && v[0] == '+' ? v + 1 : NULL; // "+value" set, absent: NULL
}

static void do_mount(void)
{
	const char *pidstr = getenv("_GCR_PID");
	int pid, rootfd, cwdfd, status;
	struct creds c = {0};
	unsigned long flags;
	pid_t child;

	if (!pidstr)
		FAIL(EINVAL);
	pid = atoi(pidstr);
	flags = strtoul(getenv("_GCR_FLAGS") ? getenv("_GCR_FLAGS") : "0", NULL, 10);

	int fds[NNS];
	if (open_namespaces(pid, fds) < 0)
		FAIL(errno);
	// The user namespace first: then the IDs in /proc (still the host's)
	// read in the container's terms, and the other joins are checked
	// against it.
	if (join(fds, 0) < 0)
		FAIL(errno);
	if (read_creds(pid, &c) < 0)
		FAIL(errno);
	for (size_t i = 1; i < NNS; i++)
		if (join(fds, i) < 0)
			FAIL(errno);
	// Root and working directory as the caller sees them. Opened from
	// inside: the host's view of the container's root is a mount in the
	// host's mount namespace, which the kernel will not mount onto.
	char root[64], cwd[64];
	snprintf(root, sizeof(root), "/proc/%d/root", c.nspid);
	snprintf(cwd, sizeof(cwd), "/proc/%d/cwd", c.nspid);
	rootfd = open(root, O_PATH | O_DIRECTORY | O_CLOEXEC);
	cwdfd = open(cwd, O_PATH | O_DIRECTORY | O_CLOEXEC);
	if (rootfd < 0 || cwdfd < 0)
		FAIL(errno);
	if (fchdir(rootfd) < 0 || chroot(".") < 0 || fchdir(cwdfd) < 0)
		FAIL(errno);

	// A new PID namespace applies to children: proc mounts must come from one.
	child = fork();
	if (child < 0)
		FAIL(errno);
	if (child == 0) {
		if (become(&c) < 0)
			_exit(errno ? errno : EPERM);
		if (mount(arg("_GCR_SOURCE"), arg("_GCR_TARGET"), arg("_GCR_FSTYPE"), flags, arg("_GCR_DATA")) < 0)
			_exit(errno ? errno : EPERM);
		_exit(0);
	}
	if (waitpid(child, &status, 0) < 0)
		FAIL(errno);
	_exit(WIFEXITED(status) ? WEXITSTATUS(status) : EINTR);
}

__attribute__((constructor)) static void gcr_nsexec(void)
{
	const char *mode = getenv("_GCR_NSEXEC");

	if (!mode)
		return;
	if (strcmp(mode, "mount") == 0)
		do_mount();
	_exit(EINVAL);
}
