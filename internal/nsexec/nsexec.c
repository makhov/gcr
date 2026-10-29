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
#include <sys/statvfs.h>
#include <sys/syscall.h>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>

#ifndef PIDFD_THREAD
#define PIDFD_THREAD O_EXCL
#endif

#define FAIL(e) _exit((e) ? (e) : EPERM)

// The user namespace comes last: joining it gives up our host
// capabilities, and some of the others (a pod's network namespace) belong
// to the host's user namespace.
static const struct {
	const char *name;
	int type;
} namespaces[] = {
	{"mnt", CLONE_NEWNS},	 {"pid", CLONE_NEWPID},		{"net", CLONE_NEWNET},	{"ipc", CLONE_NEWIPC},
	{"uts", CLONE_NEWUTS}, {"cgroup", CLONE_NEWCGROUP}, {"user", CLONE_NEWUSER},
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
};

// read_creds reads the caller's credentials. Called once we are in its
// user namespace: /proc reports IDs in the terms of whoever opens the file.
static int read_creds(int procfd, struct creds *c)
{
	char line[512];
	FILE *f;
	int fd, found = 0;

	fd = openat(procfd, "status", O_RDONLY | O_CLOEXEC);
	if (fd < 0)
		return -1;
	f = fdopen(fd, "re");
	if (!f)
		return -1;
	while (fgets(line, sizeof(line), f)) {
		if (sscanf(line, "Uid:\t%u\t%u\t%u", &c->uid[0], &c->uid[1], &c->uid[2]) == 3)
			found |= 1;
		else if (sscanf(line, "Gid:\t%u\t%u\t%u", &c->gid[0], &c->gid[1], &c->gid[2]) == 3)
			found |= 2;
		else if (sscanf(line, "CapEff:\t%llx", &c->capeff) == 1)
			found |= 4;
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
	return found == 7 ? 0 : -1;
}

#define MAXLEVELS 32

// ns_ids reads a status field listing an ID at each PID namespace level,
// outermost first (NSpid, NStgid). Returns the number of levels.
static int ns_ids(int dirfd, const char *file, const char *field, int ids[MAXLEVELS])
{
	char line[512];
	size_t n = strlen(field);
	int fd, levels = -1;
	FILE *f;

	fd = openat(dirfd, file, O_RDONLY | O_CLOEXEC);
	if (fd < 0 || !(f = fdopen(fd, "re")))
		return -1;
	while (levels < 0 && fgets(line, sizeof(line), f)) {
		if (strncmp(line, field, n) != 0 || line[n] != ':')
			continue;
		char *p = line + n + 1, *end;
		levels = 0;
		while (levels < MAXLEVELS) {
			long v = strtol(p, &end, 10);
			if (end == p)
				break;
			ids[levels++] = v;
			p = end;
		}
	}
	fclose(f);
	return levels;
}

// The caller's process and thread IDs at each PID namespace level.
struct ids {
	int tgid[MAXLEVELS], tid[MAXLEVELS], levels;
};

// self_path rewrites /proc/self and /proc/thread-self to the caller's
// directories in the /proc we can see, which may belong to any of its
// PID namespaces: our own ID in that /proc tells which (we are a child in
// the caller's PID namespace, so our levels line up with its).
static const char *self_path(const char *path, int hostproc, const struct ids *caller, char *buf, size_t size)
{
	int self[MAXLEVELS], levels, level = -1, visible;
	char link[32];
	ssize_t n;

	if (!path || (strncmp(path, "/proc/self/", 11) != 0 && strncmp(path, "/proc/thread-self/", 18) != 0))
		return path;
	n = readlink("/proc/self", link, sizeof(link) - 1);
	if (n < 0)
		return path;
	link[n] = '\0';
	visible = atoi(link);
	levels = ns_ids(hostproc, "self/status", "NSpid", self);
	if (levels != caller->levels)
		return path;
	for (int i = 0; i < levels; i++)
		if (self[i] == visible)
			level = i;
	if (level < 0)
		return path;
	if (path[6] == 's')
		snprintf(buf, size, "/proc/%d/%s", caller->tgid[level], path + 11);
	else
		snprintf(buf, size, "/proc/%d/task/%d/%s", caller->tgid[level], caller->tid[level], path + 18);
	return buf;
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

// confine_like takes on the caller's AppArmor profile, so the mount is
// mediated by it as the caller's own would be (a profile may refuse
// mounts, as docker-default does). The shim is unconfined, which may
// change to any profile. Without AppArmor there is nothing to take on.
static int confine_like(int procfd, int hostproc)
{
	char label[256], cmd[300];
	ssize_t n;
	int fd = openat(procfd, "attr/apparmor/current", O_RDONLY | O_CLOEXEC);

	if (fd < 0)
		return errno == ENOENT || errno == EINVAL || errno == ENOTDIR ? 0 : -1;
	n = read(fd, label, sizeof(label) - 1);
	close(fd);
	if (n < 0)
		return -1;
	label[n] = '\0';
	// "profile (mode)\n", or "unconfined".
	label[strcspn(label, " \n")] = '\0';
	if (label[0] == '\0' || strcmp(label, "unconfined") == 0)
		return 0;
	snprintf(cmd, sizeof(cmd), "changeprofile %s", label);
	fd = openat(hostproc, "thread-self/attr/apparmor/current", O_WRONLY | O_CLOEXEC);
	if (fd < 0)
		return -1;
	n = write(fd, cmd, strlen(cmd));
	close(fd);
	return n < 0 ? -1 : 0;
}

static const char *arg(const char *name)
{
	const char *v = getenv(name);
	return v && v[0] == '+' ? v + 1 : NULL; // "+value" set, absent: NULL
}

// fd_path rewrites a path through one of the caller's file descriptors
// (/proc/self/fd/N, /proc/thread-self/fd/N: how runc names mount sources
// and targets) to a copy of that descriptor in this process.
static const char *fd_path(const char *path, int pidfd, char *buf, size_t size)
{
	static const char *const prefixes[] = {"/proc/self/fd/", "/proc/thread-self/fd/"};
	char *end;

	if (!path)
		return NULL;
	for (size_t i = 0; i < 2; i++) {
		size_t n = strlen(prefixes[i]);
		if (strncmp(path, prefixes[i], n) != 0)
			continue;
		long fd = strtol(path + n, &end, 10);
		if (end == path + n || (*end != '\0' && *end != '/'))
			return path;
		int copy = syscall(SYS_pidfd_getfd, pidfd, (int)fd, 0);
		if (copy < 0)
			FAIL(errno);
		snprintf(buf, size, "/proc/self/fd/%d%s", copy, end);
		return buf;
	}
	return path;
}

static void do_mount(void)
{
	const char *pidstr = getenv("_GCR_PID");
	const char *source, *target;
	char path[64], sbuf[4200], tbuf[4200];
	int pid, hostproc, procfd, pidfd, rootfd, cwdfd, status;
	struct creds c = {0};
	struct ids caller;
	unsigned long flags;
	pid_t child;

	if (!pidstr)
		FAIL(EINVAL);
	pid = atoi(pidstr);
	flags = strtoul(getenv("_GCR_FLAGS") ? getenv("_GCR_FLAGS") : "0", NULL, 10);

	// The caller is a thread, named by its ID on the host. Everything about
	// it comes through the host's /proc and a pidfd taken now: its own
	// /proc, if it can see one, may belong to an outer PID namespace.
	snprintf(path, sizeof(path), "/proc/%d", pid);
	hostproc = open("/proc", O_PATH | O_DIRECTORY | O_CLOEXEC);
	procfd = open(path, O_PATH | O_DIRECTORY | O_CLOEXEC);
	pidfd = syscall(SYS_pidfd_open, pid, PIDFD_THREAD);
	if (hostproc < 0 || procfd < 0 || pidfd < 0)
		FAIL(errno);
	caller.levels = ns_ids(procfd, "status", "NStgid", caller.tgid);
	if (caller.levels < 1 || ns_ids(procfd, "status", "NSpid", caller.tid) != caller.levels)
		FAIL(errno);
	source = fd_path(arg("_GCR_SOURCE"), pidfd, sbuf, sizeof(sbuf));
	target = fd_path(arg("_GCR_TARGET"), pidfd, tbuf, sizeof(tbuf));

	int fds[NNS];
	if (open_namespaces(pid, fds) < 0)
		FAIL(errno);
	for (size_t i = 0; i < NNS; i++)
		if (join(fds, i) < 0)
			FAIL(errno);
	if (read_creds(procfd, &c) < 0)
		FAIL(errno);
	// Root and working directory as the caller sees them, opened once in
	// its mount namespace (the kernel will not mount onto a path taken
	// from another one).
	rootfd = openat(procfd, "root", O_PATH | O_DIRECTORY | O_CLOEXEC);
	cwdfd = openat(procfd, "cwd", O_PATH | O_DIRECTORY | O_CLOEXEC);
	if (rootfd < 0 || cwdfd < 0)
		FAIL(errno);
	if (fchdir(rootfd) < 0 || chroot(".") < 0 || fchdir(cwdfd) < 0)
		FAIL(errno);

	// A new PID namespace applies to children: proc mounts must come from
	// one, and /proc/self/fd paths resolve in one.
	child = fork();
	if (child < 0)
		FAIL(errno);
	if (child == 0) {
		// The caller's profile and credentials before anything else is
		// done in its namespaces.
		if (confine_like(procfd, hostproc) < 0)
			_exit(errno ? errno : EPERM);
		if (become(&c) < 0)
			_exit(errno ? errno : EPERM);
		// Paths through the caller's descriptors are copies already.
		if (source == arg("_GCR_SOURCE"))
			source = self_path(source, hostproc, &caller, sbuf, sizeof(sbuf));
		if (target == arg("_GCR_TARGET"))
			target = self_path(target, hostproc, &caller, tbuf, sizeof(tbuf));
		// Nothing of the host's or the caller's stays open for the mount:
		// a process of the container could reach what this one holds.
		close(hostproc);
		close(procfd);
		close(pidfd);
		close(rootfd);
		close(cwdfd);
		for (size_t i = 0; i < NNS; i++)
			if (fds[i] >= 0)
				close(fds[i]);
		// A remount may not make writable what is read-only: a volume, or
		// the root filesystem, mounted read-only for the container (the
		// kernel locks those only when it copies them into a namespace).
		if ((flags & MS_REMOUNT) && !(flags & MS_RDONLY)) {
			struct statvfs st;
			if (statvfs(target, &st) < 0)
				_exit(errno ? errno : EPERM);
			if (st.f_flag & ST_RDONLY)
				_exit(EPERM);
		}
		if (mount(source, target, arg("_GCR_FSTYPE"), flags, arg("_GCR_DATA")) < 0)
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
	// Root on the host, about to enter a container's namespaces: nothing
	// there may ptrace this process or its child, nor read their memory
	// or descriptors through /proc. Set before the child is forked, which
	// inherits it; changing credentials only ever lowers it.
	if (prctl(PR_SET_DUMPABLE, 0, 0, 0, 0) < 0)
		_exit(errno);
	if (strcmp(mode, "mount") == 0)
		do_mount();
	_exit(EINVAL);
}
