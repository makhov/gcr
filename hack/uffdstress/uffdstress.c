// uffdstress drives userfaultfd the way CRIU's lazy restore does, without
// CRIU, to tell a kernel bug from a CRIU one (gcr#60): anonymous memory is
// filled in part ("eager" pages), moved into place with mremap (CRIU's
// premap), registered for missing faults, and then a separate process (the
// lazy-pages daemon) serves faults and copies the rest in the background
// while threads of the first read and write it. Every page carries a stamp,
// so it reports:
//
//   - a page that reads back without its stamp (zeros, where CRIU's
//     restores crashed): its fault never reached the daemon;
//   - UFFDIO_COPY failing with EEXIST on a page the daemon never filled:
//     something other than the daemon populated it first.
//
// Run as root (or with vm.unprivileged_userfaultfd=1):
//
//   uffdstress [-s seconds] [-m MiB] [-t threads] [-z]
//
// -z checks the checker: it reads one lazy page in 1000 in the premap,
// before registration, which maps the zero page there behind
// userfaultfd's back; both reports must then come up.
//
// It exits 1 if it saw either, 0 otherwise.
#define _GNU_SOURCE
#include <errno.h>
#include <fcntl.h>
#include <linux/userfaultfd.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/ioctl.h>
#include <sys/mman.h>
#include <sys/syscall.h>
#include <sys/wait.h>
#include <time.h>
#include <unistd.h>

#define PAGE 4096UL

static unsigned long npages, iter;
static char *mem;
static atomic_int stop_threads;
static atomic_long reads, bad_reads;
static int nthreads = 8;

// Stamps: the page's index and the iteration, at the page's start; the
// threads write only past it.
static uint64_t stamp(unsigned long page) { return (iter << 40) | page | (1UL << 63); }

// lazy says whether a page is left for the daemon: runs of 1-64 pages,
// about half of them, different each iteration.
static unsigned char *lazy;

static uint64_t rng(uint64_t *s)
{
	*s ^= *s << 13;
	*s ^= *s >> 7;
	*s ^= *s << 17;
	return *s;
}

static void die(const char *what)
{
	perror(what);
	exit(2);
}

static void *worker(void *arg)
{
	uint64_t s = (uintptr_t)arg * 0x9e3779b97f4a7c15ULL + iter + 1;
	while (!atomic_load(&stop_threads)) {
		unsigned long p = rng(&s) % npages;
		volatile uint64_t *w = (uint64_t *)(mem + p * PAGE);
		uint64_t v = w[0];
		atomic_fetch_add(&reads, 1);
		if (v != stamp(p)) {
			if (atomic_fetch_add(&bad_reads, 1) < 20)
				fprintf(stderr, "iter %lu: page %lu (%p, %s) reads %#lx, want %#lx\n", iter, p,
					(void *)w, lazy[p] ? "lazy" : "eager", (unsigned long)v,
					(unsigned long)stamp(p));
		}
		if (rng(&s) % 4 == 0)
			w[8 + rng(&s) % 500] = v; // a write, as a running program's
	}
	return NULL;
}

// The daemon's state: its buffer, which pages it has filled, how many
// lazy ones are left, and the EEXISTs on pages it never filled.
static int d_uffd;
static char *d_buf;
static unsigned char *d_done;
static unsigned long d_left, d_foreign;

static void filled(unsigned long p)
{
	if (!d_done[p]) {
		d_done[p] = 1;
		d_left--;
	}
}

// copy fills pages [p, p+n) with their stamps.
static void copy(unsigned long p, unsigned long n)
{
	for (unsigned long i = 0; i < n; i++) {
		memset(d_buf + i * PAGE, 0, PAGE);
		*(uint64_t *)(d_buf + i * PAGE) = stamp(p + i);
	}
	struct uffdio_copy c = { .dst = (uintptr_t)(mem + p * PAGE), .src = (uintptr_t)d_buf, .len = n * PAGE };
	if (ioctl(d_uffd, UFFDIO_COPY, &c) == 0) {
		for (unsigned long i = 0; i < n; i++)
			filled(p + i);
		return;
	}
	unsigned long copied = c.copy > 0 ? (unsigned long)c.copy / PAGE : 0;
	for (unsigned long i = 0; i < copied; i++)
		filled(p + i);
	if (errno == EAGAIN)
		return;
	if (errno != EEXIST)
		die("UFFDIO_COPY");
	// The page after the copied ones is there already. Only the threads'
	// faults go through the daemon, so if it never filled that page, the
	// page was filled behind userfaultfd's back.
	unsigned long q = p + copied;
	if (!d_done[q] && d_foreign++ < 20)
		fprintf(stderr, "iter %lu: EEXIST on page %lu (%p, %s), which the daemon never filled\n", iter, q,
			(void *)(mem + q * PAGE), lazy[q] ? "lazy" : "eager");
	filled(q);
}

// daemon_run serves faults and copies the lazy pages in growing chunks, as
// CRIU's lazy-pages daemon does, then exits with how many EEXISTs it met
// on pages it had not filled itself.
static int daemon_run(int uffd)
{
	unsigned long next = 0, xfer = 16;
	d_uffd = uffd;
	d_buf = aligned_alloc(PAGE, 1024 * PAGE);
	d_done = calloc(npages, 1);
	if (!d_buf || !d_done)
		die("alloc");
	for (unsigned long p = 0; p < npages; p++)
		d_left += lazy[p];

	while (d_left) {
		struct pollfd pfd = { .fd = uffd, .events = POLLIN };
		if (poll(&pfd, 1, 0) > 0) {
			struct uffd_msg m;
			if (read(uffd, &m, sizeof(m)) == sizeof(m) && m.event == UFFD_EVENT_PAGEFAULT) {
				unsigned long p = (m.arg.pagefault.address - (uintptr_t)mem) / PAGE;
				if (!d_done[p])
					copy(p, 1);
				else {
					// Filled since it faulted: wake it.
					struct uffdio_range r = { .start = (uintptr_t)(mem + p * PAGE), .len = PAGE };
					ioctl(uffd, UFFDIO_WAKE, &r);
				}
				xfer = 16;
				continue;
			}
		}
		// Background: the next run of lazy pages not filled yet.
		while (next < npages && (!lazy[next] || d_done[next]))
			next++;
		if (next >= npages) {
			next = 0;
			continue;
		}
		unsigned long n = 1;
		while (next + n < npages && n < xfer && lazy[next + n] && !d_done[next + n])
			n++;
		copy(next, n);
		if (xfer < 1024)
			xfer += 16;
	}
	return d_foreign > 255 ? 255 : (int)d_foreign;
}

int main(int argc, char **argv)
{
	int seconds = 60, mib = 384, opt, zero = 0;
	while ((opt = getopt(argc, argv, "s:m:t:z")) != -1) {
		switch (opt) {
		case 's': seconds = atoi(optarg); break;
		case 'm': mib = atoi(optarg); break;
		case 't': nthreads = atoi(optarg); break;
		case 'z': zero = 1; break;
		default: fprintf(stderr, "usage: uffdstress [-s seconds] [-m MiB] [-t threads] [-z]\n"); return 2;
		}
	}
	npages = (unsigned long)mib << 20 >> 12;
	size_t size = npages * PAGE;
	lazy = malloc(npages);
	time_t end = time(NULL) + seconds;
	long foreign_total = 0;
	uint64_t s = 42;

	for (iter = 1; time(NULL) < end; iter++) {
		for (unsigned long p = 0; p < npages;) {
			unsigned long run = 1 + rng(&s) % 64;
			int l = rng(&s) & 1;
			for (unsigned long i = 0; i < run && p < npages; i++)
				lazy[p++] = l;
		}
		// Premap: fill the eager pages, then move the mapping into
		// place, as CRIU's restorer does.
		char *pre = mmap(NULL, size, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
		char *dst = mmap(NULL, size, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS | MAP_NORESERVE, -1, 0);
		if (pre == MAP_FAILED || dst == MAP_FAILED)
			die("mmap");
		for (unsigned long p = 0; p < npages; p++) {
			if (!lazy[p])
				*(uint64_t *)(pre + p * PAGE) = stamp(p);
			else if (zero && p % 1000 == 0)
				(void)*(volatile uint64_t *)(pre + p * PAGE);
		}
		mem = mremap(pre, size, size, MREMAP_MAYMOVE | MREMAP_FIXED, dst);
		if (mem == MAP_FAILED)
			die("mremap");

		int uffd = syscall(SYS_userfaultfd, O_CLOEXEC | O_NONBLOCK);
		if (uffd < 0)
			die("userfaultfd");
		struct uffdio_api api = { .api = UFFD_API };
		if (ioctl(uffd, UFFDIO_API, &api))
			die("UFFDIO_API");
		struct uffdio_register reg = { .range = { .start = (uintptr_t)mem, .len = size },
					       .mode = UFFDIO_REGISTER_MODE_MISSING };
		if (ioctl(uffd, UFFDIO_REGISTER, &reg))
			die("UFFDIO_REGISTER");

		pid_t d = fork();
		if (d < 0)
			die("fork");
		if (d == 0)
			_exit(daemon_run(uffd));
		close(uffd);

		atomic_store(&stop_threads, 0);
		pthread_t th[64];
		for (int i = 0; i < nthreads && i < 64; i++)
			pthread_create(&th[i], NULL, worker, (void *)(uintptr_t)(i + 1));
		int st;
		if (waitpid(d, &st, 0) < 0)
			die("waitpid");
		atomic_store(&stop_threads, 1);
		for (int i = 0; i < nthreads && i < 64; i++)
			pthread_join(th[i], NULL);
		if (!WIFEXITED(st) || WEXITSTATUS(st) == 2) {
			fprintf(stderr, "iter %lu: daemon failed (%#x)\n", iter, st);
			return 2;
		}
		foreign_total += WEXITSTATUS(st);
		// Every page once more, now that all are filled.
		for (unsigned long p = 0; p < npages; p++)
			if (*(uint64_t *)(mem + p * PAGE) != stamp(p) && atomic_fetch_add(&bad_reads, 1) < 20)
				fprintf(stderr, "iter %lu: after the copy, page %lu (%s) reads %#lx\n", iter, p,
					lazy[p] ? "lazy" : "eager", (unsigned long)*(uint64_t *)(mem + p * PAGE));
		munmap(mem, size);
	}
	printf("uffdstress: %lu iterations of %d MiB, %d threads: %ld reads, %ld wrong, %ld EEXIST on pages "
	       "the daemon never filled\n",
	       iter - 1, mib, nthreads, atomic_load(&reads), atomic_load(&bad_reads), foreign_total);
	return atomic_load(&bad_reads) || foreign_total ? 1 : 0;
}
