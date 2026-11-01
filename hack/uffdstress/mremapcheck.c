// mremapcheck moves sub-ranges out of one sparsely filled anonymous
// mapping, one at a time, as CRIU's restorer moves VMAs out of its premap
// area, and checks that each move carries exactly the pages it should: as
// many resident at the destination as there were at the source, each with
// its own stamp, and none left behind. gcr#60: on GitHub's runners a moved
// VMA came out with one page more than it went in with.
//
//   mremapcheck [-s seconds]
//
// It exits 1 if a move gained, lost or swapped a page.
#define _GNU_SOURCE
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>

#define PAGE 4096UL

static uint64_t rng(uint64_t *s)
{
	*s ^= *s << 13;
	*s ^= *s >> 7;
	*s ^= *s << 17;
	return *s;
}

static long resident(char *p, unsigned long len)
{
	unsigned long n = len / PAGE;
	unsigned char *v = malloc(n);
	long r = 0;
	if (mincore(p, len, v)) {
		perror("mincore");
		exit(2);
	}
	for (unsigned long i = 0; i < n; i++)
		r += v[i] & 1;
	free(v);
	return r;
}

int main(int argc, char **argv)
{
	int seconds = 60, opt;
	while ((opt = getopt(argc, argv, "s:")) != -1)
		if (opt == 's')
			seconds = atoi(optarg);
	uint64_t s = (uint64_t)time(NULL) | 1;
	time_t end = time(NULL) + seconds;
	long moves = 0, bad = 0, iters = 0;

	while (time(NULL) < end) {
		iters++;
		// VMAs of 1 to 600 pages (hog's run from one page to ~2 MiB
		// and beyond), packed back to back in one premap.
		enum { N = 48 };
		unsigned long len[N], off[N], total = 0;
		for (int i = 0; i < N; i++) {
			len[i] = 1 + rng(&s) % (rng(&s) % 4 ? 64 : 600);
			off[i] = total;
			total += len[i];
		}
		char *pre = mmap(NULL, total * PAGE, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
		if (pre == MAP_FAILED) {
			perror("mmap");
			return 2;
		}
		// Some pages written (stamped), some read (the zero page),
		// most left alone: as a restore leaves lazy pages.
		for (unsigned long p = 0; p < total; p++) {
			uint64_t r = rng(&s) % 8;
			if (r == 0)
				*(uint64_t *)(pre + p * PAGE) = p | (1UL << 63);
			else if (r == 1)
				(void)*(volatile uint64_t *)(pre + p * PAGE);
		}
		// Destinations: a separate reserved range, each VMA at its own
		// offset plus a gap, in shuffled order, as targets are placed.
		unsigned long span = total + N * 8;
		char *dst = mmap(NULL, span * PAGE, PROT_NONE, MAP_PRIVATE | MAP_ANONYMOUS | MAP_NORESERVE, -1, 0);
		if (dst == MAP_FAILED) {
			perror("mmap");
			return 2;
		}
		int order[N];
		for (int i = 0; i < N; i++)
			order[i] = i;
		for (int i = N - 1; i > 0; i--) {
			int j = rng(&s) % (i + 1), t = order[i];
			order[i] = order[j];
			order[j] = t;
		}
		for (int k = 0; k < N; k++) {
			int i = order[k];
			char *from = pre + off[i] * PAGE, *to = dst + (off[i] + 8 * i) * PAGE;
			unsigned long l = len[i] * PAGE;
			long before = resident(from, l);
			// The stamps it should carry.
			uint64_t *want = calloc(len[i], sizeof(*want));
			unsigned char *v = malloc(len[i]);
			mincore(from, l, v);
			for (unsigned long p = 0; p < len[i]; p++)
				want[p] = (v[p] & 1) ? *(uint64_t *)(from + p * PAGE) : ~0UL;
			if (mremap(from, l, l, MREMAP_MAYMOVE | MREMAP_FIXED, to) != to) {
				perror("mremap");
				return 2;
			}
			moves++;
			long after = resident(to, l);
			mincore(to, l, v);
			int wrong = 0;
			for (unsigned long p = 0; p < len[i]; p++) {
				int was = want[p] != ~0UL, is = v[p] & 1;
				if (was != is || (is && *(uint64_t *)(to + p * PAGE) != want[p]))
					wrong++;
			}
			if (before != after || wrong) {
				if (bad++ < 20)
					fprintf(stderr,
						"iter %ld: VMA %d of %lu pages: %ld resident before the move, %ld after, %d pages "
						"wrong\n",
						iters, i, len[i], before, after, wrong);
			}
			free(want);
			free(v);
		}
		munmap(pre, total * PAGE);
		munmap(dst, span * PAGE);
	}
	printf("mremapcheck: %ld iterations, %ld moves, %ld wrong\n", iters, moves, bad);
	return bad ? 1 : 0;
}
