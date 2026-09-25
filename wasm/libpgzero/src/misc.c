/*
 * misc.c
 *	  Odds and ends of POSIX that wasi-libc lacks.
 */
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/resource.h>
#include <sys/stat.h>
#include <termios.h>
#include <time.h>
#include <unistd.h>

#include "internal.h"

/* WASI has no file modes; remember the mask so umask() round-trips. */
static mode_t current_umask = 022;

mode_t
umask(mode_t mask)
{
	mode_t		old = current_umask;

	current_umask = mask & 0777;
	return old;
}

/* There is only one user, so ownership changes always succeed. */
int
chown(const char *path, uid_t owner, gid_t group)
{
	return 0;
}

int
fchown(int fd, uid_t owner, gid_t group)
{
	return 0;
}

/* The shadow stack's bounds, defined by wasm-ld. */
extern char __stack_low[];
extern char __stack_high[];

int
getrlimit(int resource, struct rlimit *rlim)
{
	switch (resource)
	{
		case RLIMIT_STACK:
			rlim->rlim_cur = rlim->rlim_max = __stack_high - __stack_low;
			return 0;
		case RLIMIT_NOFILE:
			rlim->rlim_cur = rlim->rlim_max = 4096;
			return 0;
		default:
			rlim->rlim_cur = rlim->rlim_max = RLIM_INFINITY;
			return 0;
	}
}

int
setrlimit(int resource, const struct rlimit *rlim)
{
	errno = EPERM;
	return -1;
}

int
getrusage(int who, struct rusage *usage)
{
	/* No CPU accounting: report wall time since start as user time. */
	clock_t		c = clock();

	memset(usage, 0, sizeof(*usage));
	if (who == RUSAGE_SELF && c != (clock_t) -1)
	{
		usage->ru_utime.tv_sec = c / CLOCKS_PER_SEC;
		usage->ru_utime.tv_usec = (c % CLOCKS_PER_SEC) * (1000000 / CLOCKS_PER_SEC);
	}
	return 0;
}

/* Shared memory and semaphores for Postgres's port layer. */

int
pgzero_shm_create(size_t size)
{
	return sysret(host_shm_create(size));
}

ssize_t
pgzero_shm_size(int id)
{
	return sysret(host_shm_size(id));
}

int
pgzero_shm_attach(int id, void *addr)
{
	return sysret(host_shm_attach(id, ADDR(addr)));
}

int
pgzero_shm_remove(int id)
{
	return sysret(host_shm_remove(id));
}

int
pgzero_sema_create(int initial)
{
	return sysret(host_sema_create(initial));
}

int
pgzero_sema_lock(int id)
{
	return sysret(host_sema_lock(id));
}

int
pgzero_sema_try_lock(int id)
{
	return sysret(host_sema_try_lock(id));
}

int
pgzero_sema_unlock(int id)
{
	return sysret(host_sema_unlock(id));
}

int
pgzero_sema_reset(int id)
{
	return sysret(host_sema_reset(id));
}

/*
 * wasi-libc's mmap emulation only makes private copies, so there is never
 * anything to write back.
 */
int
msync(void *addr, size_t len, int flags)
{
	return 0;
}

/* Postgres uses its own timezone library; the C library's is unused. */
void
tzset(void)
{
}

int
tcgetattr(int fd, struct termios *t)
{
	errno = ENOTTY;
	return -1;
}

int
tcsetattr(int fd, int action, const struct termios *t)
{
	errno = ENOTTY;
	return -1;
}

/*
 * realpath() by lexical normalisation, checking that the result exists.
 * (wasi-libc's cannot resolve paths outside the current directory's
 * preopen.) Mounts contain no symlinks that matter to Postgres.
 */
char *
__wrap_realpath(const char *restrict path, char *restrict resolved)
{
	char		buf[PATH_MAX];
	char		out[PATH_MAX];
	size_t		len = 0;
	const char *p;
	struct stat st;

	if (path == NULL)
	{
		errno = EINVAL;
		return NULL;
	}
	if (path[0] != '/')
	{
		if (getcwd(buf, sizeof(buf)) == NULL)
			return NULL;
		if (strlen(buf) + 1 + strlen(path) + 1 > sizeof(buf))
		{
			errno = ENAMETOOLONG;
			return NULL;
		}
		strcat(buf, "/");
		strcat(buf, path);
	}
	else
	{
		if (strlen(path) + 1 > sizeof(buf))
		{
			errno = ENAMETOOLONG;
			return NULL;
		}
		strcpy(buf, path);
	}

	out[0] = '\0';
	for (p = buf; *p;)
	{
		const char *end;
		size_t		n;

		while (*p == '/')
			p++;
		end = strchr(p, '/');
		n = end ? (size_t) (end - p) : strlen(p);
		if (n == 0 || (n == 1 && p[0] == '.'))
			;
		else if (n == 2 && p[0] == '.' && p[1] == '.')
		{
			while (len > 0 && out[len - 1] != '/')
				len--;
			if (len > 0)
				len--;
			out[len] = '\0';
		}
		else
		{
			if (len + 1 + n + 1 > sizeof(out))
			{
				errno = ENAMETOOLONG;
				return NULL;
			}
			out[len++] = '/';
			memcpy(out + len, p, n);
			len += n;
			out[len] = '\0';
		}
		p += n;
	}
	if (len == 0)
		strcpy(out, "/");

	if (stat(out, &st) < 0)
		return NULL;
	if (resolved)
		return strcpy(resolved, out);
	return strdup(out);
}
