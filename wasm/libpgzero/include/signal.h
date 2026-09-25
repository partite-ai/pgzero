/*
 * pgzero: <signal.h> for wasm32-wasip2.
 *
 * wasi-libc hides the POSIX signal API because WASI has no signals. pgzero
 * provides it in libpgzero: the host queues signals for a process, and they
 * are delivered at safe points (after host calls, and when Postgres checks
 * for interrupts). This header restores the declarations.
 */
#ifndef PGZERO_SIGNAL_H
#define PGZERO_SIGNAL_H

#ifndef _WASI_EMULATED_SIGNAL
#define _WASI_EMULATED_SIGNAL
#endif
#include_next <signal.h>

#include <sys/types.h>

#ifdef __cplusplus
extern "C" {
#endif

#define SIG_HOLD ((void (*)(int)) 2)

typedef struct
{
	int			si_signo;
	int			si_errno;
	int			si_code;
	pid_t		si_pid;
	uid_t		si_uid;
	int			si_status;
} siginfo_t;

struct sigaction
{
	union
	{
		void		(*sa_handler) (int);
		void		(*sa_sigaction) (int, siginfo_t *, void *);
	}			__sa_handler;
	sigset_t	sa_mask;
	int			sa_flags;
};
#define sa_handler   __sa_handler.sa_handler
#define sa_sigaction __sa_handler.sa_sigaction

#define SA_NOCLDSTOP  1
#define SA_NOCLDWAIT  2
#define SA_SIGINFO    4
#define SA_ONSTACK    0x08000000
#define SA_RESTART    0x10000000
#define SA_NODEFER    0x40000000
#define SA_RESETHAND  0x80000000

#define SIG_BLOCK     0
#define SIG_UNBLOCK   1
#define SIG_SETMASK   2

int			kill(pid_t, int);
int			sigemptyset(sigset_t *);
int			sigfillset(sigset_t *);
int			sigaddset(sigset_t *, int);
int			sigdelset(sigset_t *, int);
int			sigismember(const sigset_t *, int);
int			sigprocmask(int, const sigset_t *__restrict, sigset_t *__restrict);
int			pthread_sigmask(int, const sigset_t *__restrict, sigset_t *__restrict);
int			sigsuspend(const sigset_t *);
int			sigaction(int, const struct sigaction *__restrict, struct sigaction *__restrict);
int			sigpending(sigset_t *);
int			sigwait(const sigset_t *__restrict, int *__restrict);

#ifdef __cplusplus
}
#endif

#endif							/* PGZERO_SIGNAL_H */
