/*
 * fd.c
 *	  File descriptor calls, split between wasi-libc and the host.
 *
 * Files are wasi-libc descriptors, backed by wasi:filesystem. Stdio (0-2),
 * pipes and sockets are host descriptors, so that - as with real fork+exec -
 * a spawned child inherits them, stderr can be redirected into a pipe with
 * dup2(), and poll() can wait on sockets and pipes while staying
 * interruptible by signals.
 *
 * Each function here is linked with --wrap=<name>: every reference to
 * <name>, including those inside wasi-libc (stdio uses read, readv, writev
 * and close), resolves to __wrap_<name>, and __real_<name> is the original.
 */
#include <fcntl.h>
#include <poll.h>
#include <stdarg.h>
#include <stdbool.h>
#include <sys/uio.h>
#include <time.h>
#include <unistd.h>

#include "internal.h"

extern ssize_t __real_read(int, void *, size_t);
extern ssize_t __real_readv(int, const struct iovec *, int);
extern ssize_t __real_write(int, const void *, size_t);
extern int	__real_close(int);
extern int	__real_fcntl(int, int, ...);
extern int	__real_poll(struct pollfd *, nfds_t, int);
extern int	__real_dup(int);
extern int	__real_dup2(int, int);

ssize_t
__wrap_read(int fd, void *buf, size_t len)
{
	if (!is_host_fd(fd))
		return __real_read(fd, buf, len);
	return sysret(host_read(fd, ADDR(buf), len));
}

/*
 * wasi-libc writes files through a WASI output stream, which may accept
 * only part of a large write. That is allowed, but Postgres treats a short
 * write to a regular file as an error (disk full), as real systems don't do
 * that, so keep going until everything is written.
 */
static ssize_t
write_file_fully(int fd, const char *buf, size_t len)
{
	size_t		total = 0;

	while (total < len)
	{
		ssize_t		n = __real_write(fd, buf + total, len - total);

		if (n < 0)
			return total > 0 ? (ssize_t) total : -1;
		if (n == 0)
			break;
		total += n;
	}
	return total;
}

ssize_t
__wrap_write(int fd, const void *buf, size_t len)
{
	if (!is_host_fd(fd))
		return write_file_fully(fd, buf, len);
	return sysret(host_write(fd, ADDR(buf), len));
}

ssize_t
__wrap_readv(int fd, const struct iovec *iov, int iovcnt)
{
	ssize_t		total = 0;

	if (!is_host_fd(fd))
		return __real_readv(fd, iov, iovcnt);
	for (int i = 0; i < iovcnt; i++)
	{
		ssize_t		n;

		if (iov[i].iov_len == 0)
			continue;
		n = __wrap_read(fd, iov[i].iov_base, iov[i].iov_len);
		if (n < 0)
			return total > 0 ? total : -1;
		total += n;
		if ((size_t) n < iov[i].iov_len)
			break;
	}
	return total;
}

ssize_t
__wrap_writev(int fd, const struct iovec *iov, int iovcnt)
{
	ssize_t		total = 0;

	/* Both kinds of descriptor go through __wrap_write, one vector at a time. */
	for (int i = 0; i < iovcnt; i++)
	{
		ssize_t		n;

		if (iov[i].iov_len == 0)
			continue;
		n = __wrap_write(fd, iov[i].iov_base, iov[i].iov_len);
		if (n < 0)
			return total > 0 ? total : -1;
		total += n;
		if ((size_t) n < iov[i].iov_len)
			break;
	}
	return total;
}

int
__wrap_close(int fd)
{
	if (!is_host_fd(fd))
		return __real_close(fd);
	return sysret(host_close(fd));
}

int
__wrap_fcntl(int fd, int cmd, ...)
{
	va_list		ap;
	int			arg;

	/* Every command we use takes an int (or nothing). */
	va_start(ap, cmd);
	arg = va_arg(ap, int);
	va_end(ap);

	if (!is_host_fd(fd))
		return __real_fcntl(fd, cmd, arg);
	return sysret(host_fcntl(fd, cmd, arg));
}

int
__wrap_dup(int fd)
{
	if (!is_host_fd(fd))
		return __real_dup(fd);
	return sysret(host_dup(fd, -1));
}

int
__wrap_dup2(int fd, int new_fd)
{
	if (!is_host_fd(fd) && !is_host_fd(new_fd))
		return __real_dup2(fd, new_fd);
	if (!is_host_fd(fd) || !is_host_fd(new_fd))
	{
		/* Can't mix a wasi-libc file with a host descriptor. */
		errno = EBADF;
		return -1;
	}
	return sysret(host_dup(fd, new_fd));
}

int
__wrap_pipe2(int fds[2], int flags)
{
	return sysret(host_pipe(ADDR(fds), flags));
}

int
__wrap_pipe(int fds[2])
{
	return __wrap_pipe2(fds, 0);
}

int
__wrap_poll(struct pollfd *fds, nfds_t nfds, int timeout)
{
	bool		any_host = false,
				any_libc = false;

	for (nfds_t i = 0; i < nfds; i++)
	{
		if (fds[i].fd < 0)
			continue;
		if (is_host_fd(fds[i].fd))
			any_host = true;
		else
			any_libc = true;
	}
	if (any_libc && !any_host)
		return __real_poll(fds, nfds, timeout);
	if (any_libc)
	{
		/* Postgres only polls sockets and pipes. */
		errno = EINVAL;
		return -1;
	}
	/* Also covers nfds == 0, i.e. an interruptible sleep. */
	return sysret(host_poll(ADDR(fds), nfds, timeout));
}

/*
 * Sleeps go through the host so that they are interruptible, like a real
 * nanosleep.
 */
int
__wrap_nanosleep(const struct timespec *req, struct timespec *rem)
{
	uint64_t	usec = (uint64_t) req->tv_sec * 1000000 + (req->tv_nsec + 999) / 1000;

	if (rem)
		rem->tv_sec = rem->tv_nsec = 0;
	return sysret(host_sleep(usec));
}

/*
 * wasi-libc's preadv/pwritev may transfer only the first vector. Postgres
 * reads and writes several blocks at once with them, so do one vector at a
 * time with pread/pwrite, which transfer everything for regular files.
 */
ssize_t
__wrap_pwritev(int fd, const struct iovec *iov, int iovcnt, off_t offset)
{
	ssize_t		total = 0;

	for (int i = 0; i < iovcnt; i++)
	{
		const char *p = iov[i].iov_base;
		size_t		left = iov[i].iov_len;

		while (left > 0)
		{
			ssize_t		n = pwrite(fd, p, left, offset + total);

			if (n < 0)
				return total > 0 ? total : -1;
			if (n == 0)
				return total;
			p += n;
			left -= n;
			total += n;
		}
	}
	return total;
}

ssize_t
__wrap_preadv(int fd, const struct iovec *iov, int iovcnt, off_t offset)
{
	ssize_t		total = 0;

	for (int i = 0; i < iovcnt; i++)
	{
		char	   *p = iov[i].iov_base;
		size_t		left = iov[i].iov_len;

		while (left > 0)
		{
			ssize_t		n = pread(fd, p, left, offset + total);

			if (n < 0)
				return total > 0 ? total : -1;
			if (n == 0)
				return total;	/* end of file */
			p += n;
			left -= n;
			total += n;
		}
	}
	return total;
}
