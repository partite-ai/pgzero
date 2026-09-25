/*
 * internal.h
 *	  Shared helpers for libpgzero.
 */
#ifndef PGZERO_INTERNAL_H
#define PGZERO_INTERNAL_H

#include <errno.h>
#include <stdint.h>

#include "pgzero.h"
#include "imports.h"

/* Host file descriptors: stdio and everything from PGZERO_FD_BASE up. */
#define PGZERO_FD_BASE (1 << 20)

static inline int
is_host_fd(int fd)
{
	return (fd >= 0 && fd <= 2) || fd >= PGZERO_FD_BASE;
}

extern int64_t pgzero_shm_fd_size(int fd);

/* Guest address of a pointer, as the host sees it. */
#define ADDR(p) ((uint32_t) (uintptr_t) (p))

/*
 * Convert a host result (>= 0, or -errno) to the POSIX convention, and give
 * any signal that arrived during the call a chance to run - the host call
 * is our equivalent of returning from a system call.
 */
static inline int
sysret(int32_t r)
{
	pgzero_check_signals();
	if (r < 0)
	{
		errno = -r;
		return -1;
	}
	return r;
}

#endif							/* PGZERO_INTERNAL_H */
