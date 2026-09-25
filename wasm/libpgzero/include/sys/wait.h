/* pgzero: waitpid is implemented by the host's process table. */
#ifndef PGZERO_SYS_WAIT_H
#define PGZERO_SYS_WAIT_H
#include <sys/types.h>
#ifdef __cplusplus
extern "C" {
#endif
#define WNOHANG    1
#define WUNTRACED  2
#define WEXITSTATUS(s) (((s) & 0xff00) >> 8)
#define WTERMSIG(s)    ((s) & 0x7f)
#define WSTOPSIG(s)    WEXITSTATUS(s)
#define WIFEXITED(s)   (!WTERMSIG(s))
#define WIFSTOPPED(s)  ((short) ((((s) & 0xffff) * 0x10001U) >> 8) > 0x7f00)
#define WIFSIGNALED(s) (((s) & 0xffff) - 1U < 0xffu)
pid_t		wait(int *);
pid_t		waitpid(pid_t, int *, int);
#ifdef __cplusplus
}
#endif
#endif
