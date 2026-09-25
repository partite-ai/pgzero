/* pgzero: process and identity functions provided by libpgzero. */
#ifndef PGZERO_UNISTD_H
#define PGZERO_UNISTD_H
#include_next <unistd.h>
#ifdef __cplusplus
extern "C" {
#endif
pid_t		getpid(void);
pid_t		getppid(void);
pid_t		setsid(void);
pid_t		getpgrp(void);
int			setpgid(pid_t, pid_t);
uid_t		getuid(void);
uid_t		geteuid(void);
gid_t		getgid(void);
gid_t		getegid(void);
unsigned	alarm(unsigned);
int			pause(void);
int			chown(const char *, uid_t, gid_t);
int			fchown(int, uid_t, gid_t);
#ifdef __cplusplus
}
#endif
#endif
