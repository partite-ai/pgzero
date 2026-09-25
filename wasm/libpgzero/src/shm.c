/*
 * shm.c
 *	  POSIX shared memory (shm_open + mmap), used for Postgres's dynamic
 *	  shared memory segments.
 *
 * shm_open() returns a host descriptor for a named host segment. Mapping
 * it allocates a 64KiB-aligned block of our own heap and has the host map
 * the segment over it; unmapping restores private memory and frees the
 * block. mmap()/munmap()/ftruncate() are linked with --wrap; anything that
 * isn't a shared memory descriptor goes to wasi-libc.
 */
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include "internal.h"

#define WASM_PAGE 65536

extern void *__real_mmap(void *, size_t, int, int, int, off_t);
extern int	__real_munmap(void *, size_t);
extern int	__real_ftruncate(int, off_t);

typedef struct mapping
{
	void	   *addr;
	size_t		len;
	struct mapping *next;
} mapping;

static mapping *mappings;

int
shm_open(const char *name, int oflag, mode_t mode)
{
	return sysret(host_shm_open(ADDR(name), strlen(name), oflag));
}

int
shm_unlink(const char *name)
{
	return sysret(host_shm_unlink(ADDR(name), strlen(name)));
}

int
__wrap_ftruncate(int fd, off_t length)
{
	if (!is_host_fd(fd))
		return __real_ftruncate(fd, length);
	if (length < 0 || length > UINT32_MAX)
	{
		errno = EINVAL;
		return -1;
	}
	return sysret(host_shm_truncate(fd, (uint32_t) length));
}

void *
__wrap_mmap(void *addr, size_t len, int prot, int flags, int fd, off_t off)
{
	mapping    *m;
	size_t		rounded;
	void	   *block;
	int32_t		rc;

	if (!(flags & MAP_SHARED) || fd < 0 || !is_host_fd(fd))
		return __real_mmap(addr, len, prot, flags, fd, off);
	if (off != 0 || len == 0 || (flags & MAP_FIXED))
	{
		errno = EINVAL;
		return MAP_FAILED;
	}
	rounded = (len + WASM_PAGE - 1) & ~(size_t) (WASM_PAGE - 1);
	m = malloc(sizeof(*m));
	block = aligned_alloc(WASM_PAGE, rounded);
	if (!m || !block)
	{
		free(m);
		free(block);
		errno = ENOMEM;
		return MAP_FAILED;
	}
	rc = host_shm_map_fd(fd, ADDR(block), rounded);
	if (rc < 0)
	{
		free(m);
		free(block);
		sysret(rc);
		return MAP_FAILED;
	}
	m->addr = block;
	m->len = rounded;
	m->next = mappings;
	mappings = m;
	return block;
}

int
__wrap_munmap(void *addr, size_t len)
{
	mapping   **mp;

	for (mp = &mappings; *mp; mp = &(*mp)->next)
	{
		mapping    *m = *mp;

		if (m->addr != addr)
			continue;
		*mp = m->next;
		host_shm_unmap(ADDR(m->addr), m->len);
		free(m->addr);
		free(m);
		return 0;
	}
	return __real_munmap(addr, len);
}

/* Size of a shared memory descriptor, or -1 if fd isn't one. */
int64_t
pgzero_shm_fd_size(int fd)
{
	int32_t		rc = host_shm_fd_size(fd);

	return rc < 0 ? -1 : rc;
}
