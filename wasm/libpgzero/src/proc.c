/*
 * proc.c
 *	  Processes, exit, identity and the single fixed user.
 */
#include <grp.h>
#include <limits.h>
#include <pwd.h>
#include <stdlib.h>
#include <string.h>
#include <sys/wait.h>
#include <unistd.h>

#include "internal.h"

static pid_t my_pid;

pid_t
getpid(void)
{
	/* A process's pid never changes (there is no fork). */
	if (my_pid == 0)
		my_pid = host_getpid();
	return my_pid;
}

pid_t
getppid(void)
{
	return host_getppid();
}

extern char **environ;

pid_t
pgzero_spawn(char *const argv[])
{
	char		cwd[PATH_MAX];

	/* The child starts in our working directory, as across fork+exec. */
	if (getcwd(cwd, sizeof(cwd)) == NULL)
		strcpy(cwd, "/");
	return sysret(host_spawn(ADDR(argv), ADDR(environ), ADDR(cwd)));
}

pid_t
waitpid(pid_t pid, int *status, int options)
{
	return sysret(host_waitpid(pid, ADDR(status), options));
}

pid_t
wait(int *status)
{
	return waitpid(-1, status, 0);
}

/*
 * exit() runs atexit handlers and stdio cleanup, then calls _Exit, which we
 * wrap (--wrap=_Exit) so the host learns the exit status. The host's exit
 * takes a wait(2)-style status.
 */
_Noreturn void
__wrap__Exit(int code)
{
	host_exit((code & 0xff) << 8);
}

/*
 * _exit() too: wasi-libc's _exit reaches _Exit inside libc, where --wrap
 * does not apply, and Postgres uses _exit() in its emergency exit paths.
 */
_Noreturn void
__wrap__exit(int code)
{
	host_exit((code & 0xff) << 8);
}

/* Sessions and process groups: every process is its own group leader. */

pid_t
setsid(void)
{
	return getpid();
}

pid_t
getpgrp(void)
{
	return getpid();
}

int
setpgid(pid_t pid, pid_t pgid)
{
	return 0;
}

/*
 * Identity: a single unprivileged user. (Postgres refuses to run as root,
 * so this must not be 0.)
 */
#define PGZERO_UID 1000
#define PGZERO_GID 1000

uid_t
getuid(void)
{
	return PGZERO_UID;
}

uid_t
geteuid(void)
{
	return PGZERO_UID;
}

gid_t
getgid(void)
{
	return PGZERO_GID;
}

gid_t
getegid(void)
{
	return PGZERO_GID;
}

static char pw_name[] = "postgres";
static char pw_empty[] = "";
static char pw_dir[] = "/";

static struct passwd the_passwd = {
	.pw_name = pw_name,
	.pw_passwd = pw_empty,
	.pw_uid = PGZERO_UID,
	.pw_gid = PGZERO_GID,
	.pw_gecos = pw_empty,
	.pw_dir = pw_dir,
	.pw_shell = pw_empty,
};

struct passwd *
getpwuid(uid_t uid)
{
	return uid == PGZERO_UID ? &the_passwd : NULL;
}

struct passwd *
getpwnam(const char *name)
{
	return strcmp(name, pw_name) == 0 ? &the_passwd : NULL;
}

static int
copy_passwd(struct passwd *src, struct passwd *pwd, char *buf, size_t buflen,
			struct passwd **result)
{
	*result = NULL;
	if (src == NULL)
		return 0;
	if (buflen < sizeof(pw_name) + sizeof(pw_dir) + 1)
		return ERANGE;
	*pwd = *src;
	pwd->pw_name = memcpy(buf, pw_name, sizeof(pw_name));
	pwd->pw_dir = memcpy(buf + sizeof(pw_name), pw_dir, sizeof(pw_dir));
	pwd->pw_passwd = pwd->pw_gecos = pwd->pw_shell = buf + sizeof(pw_name) + sizeof(pw_dir);
	pwd->pw_passwd[0] = '\0';
	*result = pwd;
	return 0;
}

int
getpwuid_r(uid_t uid, struct passwd *pwd, char *buf, size_t buflen, struct passwd **result)
{
	return copy_passwd(getpwuid(uid), pwd, buf, buflen, result);
}

int
getpwnam_r(const char *name, struct passwd *pwd, char *buf, size_t buflen, struct passwd **result)
{
	return copy_passwd(getpwnam(name), pwd, buf, buflen, result);
}

static char *gr_mem[] = {NULL};
static struct group the_group = {
	.gr_name = pw_name,
	.gr_passwd = pw_empty,
	.gr_gid = PGZERO_GID,
	.gr_mem = gr_mem,
};

struct group *
getgrgid(gid_t gid)
{
	return gid == PGZERO_GID ? &the_group : NULL;
}

struct group *
getgrnam(const char *name)
{
	return strcmp(name, pw_name) == 0 ? &the_group : NULL;
}

/*
 * File ownership and modes. WASI has neither: stat() reports owner 0 and
 * no permission bits, and chmod() fails. Postgres insists that its data
 * directory is owned by the (non-root) user running it and has restrictive
 * permissions, so report every file as ours, with the modes Postgres
 * itself would create, and accept chmod() silently.
 */
#include <sys/stat.h>

extern int	__real_stat(const char *restrict, struct stat *restrict);
extern int	__real_lstat(const char *restrict, struct stat *restrict);
extern int	__real_fstat(int, struct stat *);
extern int	__real_fstatat(int, const char *restrict, struct stat *restrict, int);

static int
fix_stat(int rc, struct stat *st)
{
	if (rc == 0)
	{
		st->st_uid = PGZERO_UID;
		st->st_gid = PGZERO_GID;
		if ((st->st_mode & 0777) == 0)
			st->st_mode |= S_ISDIR(st->st_mode) ? 0700 : 0600;
	}
	return rc;
}

int
__wrap_stat(const char *restrict path, struct stat *restrict st)
{
	return fix_stat(__real_stat(path, st), st);
}

int
__wrap_lstat(const char *restrict path, struct stat *restrict st)
{
	return fix_stat(__real_lstat(path, st), st);
}

int
__wrap_fstat(int fd, struct stat *st)
{
	if (is_host_fd(fd))
	{
		int64_t		shm_size = pgzero_shm_fd_size(fd);

		/* shared memory objects; stdio, pipes and sockets */
		memset(st, 0, sizeof(*st));
		if (shm_size >= 0)
		{
			st->st_mode = S_IFREG | 0600;
			st->st_size = shm_size;
		}
		else
			st->st_mode = S_IFIFO | 0600;
		st->st_uid = PGZERO_UID;
		st->st_gid = PGZERO_GID;
		return 0;
	}
	return fix_stat(__real_fstat(fd, st), st);
}

int
__wrap_fstatat(int dirfd, const char *restrict path, struct stat *restrict st, int flags)
{
	return fix_stat(__real_fstatat(dirfd, path, st, flags), st);
}

/*
 * chmod() succeeds if the path exists (there are no modes to set);
 * Postgres relies on ENOENT, e.g. for CREATE TABLESPACE.
 */
int
__wrap_chmod(const char *path, mode_t mode)
{
	struct stat st;

	return __wrap_stat(path, &st);
}

int
__wrap_fchmod(int fd, mode_t mode)
{
	return 0;
}

/*
 * Start in the working directory the host gives us (the spawning
 * process's, as across fork+exec). wasi-libc has a binding for
 * wasi:cli/environment.initial-cwd but doesn't use it.
 */
#include <wasi/wasip2.h>

__attribute__((constructor))
static void
pgzero_init_cwd(void)
{
	wasip2_string_t cwd;
	char		buf[PATH_MAX];

	if (!environment_initial_cwd(&cwd))
		return;
	if (cwd.len > 0 && cwd.len < sizeof(buf))
	{
		memcpy(buf, cwd.ptr, cwd.len);
		buf[cwd.len] = '\0';
		(void) chdir(buf);
	}
	wasip2_string_free(&cwd);
}

/*
 * access(): there are no permission bits (see fix_stat), so a file that
 * exists is accessible. wasi-libc refuses R_OK for some files.
 */
int
__wrap_access(const char *path, int mode)
{
	struct stat st;

	return __wrap_stat(path, &st);
}

int
__wrap_faccessat(int dirfd, const char *path, int mode, int flags)
{
	struct stat st;

	return __wrap_fstatat(dirfd, path, &st, 0);
}
