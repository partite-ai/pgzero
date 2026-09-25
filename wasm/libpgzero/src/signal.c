/*
 * signal.c
 *	  POSIX signals on top of the pgzero host.
 *
 * The host cannot interrupt running wasm, so signals are delivered at safe
 * points: when a host call returns (see sysret) and whenever Postgres runs
 * CHECK_FOR_INTERRUPTS. The host sets bits in pgzero_signals.pending; this
 * file owns the dispositions and the blocked mask, and runs handlers.
 *
 * Blocking host calls (poll, sleep, accept, ...) return EINTR when a signal
 * that is neither blocked nor ignored becomes pending, so a process waiting
 * in its latch still wakes up promptly.
 */
#include <signal.h>
#include <stdbool.h>
#include <string.h>
#include <sys/time.h>
#include <unistd.h>

#include "internal.h"

#define BIT(sig) (1ULL << (sig))

pgzero_sigblock pgzero_signals __attribute__((aligned(8)));

/*
 * wasi-libc defines SIG_IGN and SIG_ERR as the addresses of these functions
 * (normally supplied by its signal emulation library, which we replace).
 * They are only compared against, never called.
 */
void
__SIG_IGN(int sig)
{
}

void
__SIG_ERR(int sig)
{
}

static struct sigaction actions[_NSIG];

/* Signals whose default action is to be ignored. */
#define DEFAULT_IGNORED (BIT(SIGCHLD) | BIT(SIGURG) | BIT(SIGWINCH))

static bool
valid_signal(int sig)
{
	return sig > 0 && sig < _NSIG;
}

/* Recompute the ignored mask the host consults. */
static void
update_ignored(void)
{
	uint64_t	ignored = 0;

	for (int sig = 1; sig < _NSIG; sig++)
	{
		void		(*h) (int) = actions[sig].sa_handler;

		if (h == SIG_IGN || (h == SIG_DFL && (DEFAULT_IGNORED & BIT(sig))))
			ignored |= BIT(sig);
	}
	__atomic_store_n(&pgzero_signals.ignored, ignored, __ATOMIC_RELEASE);
}

__attribute__((constructor))
static void
pgzero_signal_init(void)
{
	update_ignored();
	host_sig_register(ADDR(&pgzero_signals));
}

static void
set_blocked(uint64_t blocked)
{
	/* SIGKILL and SIGSTOP cannot be blocked. */
	blocked &= ~(BIT(SIGKILL) | BIT(SIGSTOP));
	__atomic_store_n(&pgzero_signals.blocked, blocked, __ATOMIC_RELEASE);
}

static void
run_handler(int sig)
{
	struct sigaction act = actions[sig];
	uint64_t	saved = pgzero_signals.blocked;

	if (act.sa_handler == SIG_IGN)
		return;
	if (act.sa_handler == SIG_DFL)
	{
		if (DEFAULT_IGNORED & BIT(sig))
			return;
		/* Default action: terminate, reported to waitpid as a signal death. */
		host_exit(sig);
	}

	if (act.sa_flags & SA_RESETHAND)
	{
		actions[sig].sa_handler = SIG_DFL;
		actions[sig].sa_flags &= ~SA_SIGINFO;
		update_ignored();
	}

	set_blocked(saved | act.sa_mask | ((act.sa_flags & SA_NODEFER) ? 0 : BIT(sig)));
	if (act.sa_flags & SA_SIGINFO)
	{
		siginfo_t	info;

		memset(&info, 0, sizeof(info));
		info.si_signo = sig;
		act.sa_sigaction(sig, &info, NULL);
	}
	else
		act.sa_handler(sig);
	set_blocked(saved);
}

void
pgzero_dispatch_signals(void)
{
	for (;;)
	{
		uint64_t	deliverable = __atomic_load_n(&pgzero_signals.pending, __ATOMIC_ACQUIRE) &
			~pgzero_signals.blocked;
		int			sig;

		if (deliverable == 0)
			return;
		sig = __builtin_ctzll(deliverable);
		__atomic_fetch_and(&pgzero_signals.pending, ~BIT(sig), __ATOMIC_ACQ_REL);
		run_handler(sig);
	}
}

int
sigaction(int sig, const struct sigaction *restrict act, struct sigaction *restrict oact)
{
	if (!valid_signal(sig) || (act && (sig == SIGKILL || sig == SIGSTOP)))
	{
		errno = EINVAL;
		return -1;
	}
	if (oact)
		*oact = actions[sig];
	if (act)
	{
		actions[sig] = *act;
		update_ignored();
		/* Discard a pending signal that is now ignored, as POSIX requires. */
		if (pgzero_signals.ignored & BIT(sig))
			__atomic_fetch_and(&pgzero_signals.pending, ~BIT(sig), __ATOMIC_ACQ_REL);
	}
	return 0;
}

void		(*signal(int sig, void (*handler) (int))) (int)
{
	struct sigaction act,
				oact;

	memset(&act, 0, sizeof(act));
	act.sa_handler = handler;
	act.sa_flags = SA_RESTART;
	if (sigaction(sig, &act, &oact) < 0)
		return SIG_ERR;
	return oact.sa_handler;
}

int
sigprocmask(int how, const sigset_t *restrict set, sigset_t *restrict oset)
{
	uint64_t	cur = pgzero_signals.blocked;

	if (oset)
		*oset = cur;
	if (set)
	{
		switch (how)
		{
			case SIG_BLOCK:
				cur |= *set;
				break;
			case SIG_UNBLOCK:
				cur &= ~*set;
				break;
			case SIG_SETMASK:
				cur = *set;
				break;
			default:
				errno = EINVAL;
				return -1;
		}
		set_blocked(cur);
		/* Deliver anything we just unblocked. */
		pgzero_check_signals();
	}
	return 0;
}

int
pthread_sigmask(int how, const sigset_t *restrict set, sigset_t *restrict oset)
{
	return sigprocmask(how, set, oset) < 0 ? errno : 0;
}

int
sigpending(sigset_t *set)
{
	*set = __atomic_load_n(&pgzero_signals.pending, __ATOMIC_ACQUIRE) & pgzero_signals.blocked;
	return 0;
}

int
sigsuspend(const sigset_t *mask)
{
	uint64_t	saved = pgzero_signals.blocked;

	set_blocked(*mask);
	/* Returns -EINTR once something deliverable is pending. */
	while (host_sleep(UINT64_MAX) == 0)
		;
	pgzero_dispatch_signals();
	set_blocked(saved);
	errno = EINTR;
	return -1;
}

int
pause(void)
{
	sigset_t	cur = pgzero_signals.blocked;

	return sigsuspend(&cur);
}

int
raise(int sig)
{
	return kill(getpid(), sig);
}

int
kill(pid_t pid, int sig)
{
	return sysret(host_kill(pid, sig));
}

int
sigemptyset(sigset_t *set)
{
	*set = 0;
	return 0;
}

int
sigfillset(sigset_t *set)
{
	*set = ~0ULL;
	return 0;
}

int
sigaddset(sigset_t *set, int sig)
{
	if (!valid_signal(sig))
	{
		errno = EINVAL;
		return -1;
	}
	*set |= BIT(sig);
	return 0;
}

int
sigdelset(sigset_t *set, int sig)
{
	if (!valid_signal(sig))
	{
		errno = EINVAL;
		return -1;
	}
	*set &= ~BIT(sig);
	return 0;
}

int
sigismember(const sigset_t *set, int sig)
{
	if (!valid_signal(sig))
	{
		errno = EINVAL;
		return -1;
	}
	return (*set & BIT(sig)) != 0;
}

/* Interval timers: only ITIMER_REAL, which the host turns into SIGALRM. */

static uint64_t
tv_usec(const struct timeval *tv)
{
	return (uint64_t) tv->tv_sec * 1000000 + tv->tv_usec;
}

int
setitimer(int which, const struct itimerval *restrict value, struct itimerval *restrict ovalue)
{
	int64_t		remaining;

	if (which != ITIMER_REAL)
	{
		errno = EINVAL;
		return -1;
	}
	remaining = host_set_alarm(tv_usec(&value->it_value), tv_usec(&value->it_interval));
	if (remaining < 0)
		return sysret((int32_t) remaining);
	if (ovalue)
	{
		memset(ovalue, 0, sizeof(*ovalue));
		ovalue->it_value.tv_sec = remaining / 1000000;
		ovalue->it_value.tv_usec = remaining % 1000000;
	}
	return 0;
}

int
getitimer(int which, struct itimerval *value)
{
	errno = ENOSYS;
	return -1;
}

unsigned
alarm(unsigned seconds)
{
	int64_t		remaining = host_set_alarm((uint64_t) seconds * 1000000, 0);

	return remaining > 0 ? (unsigned) ((remaining + 999999) / 1000000) : 0;
}

/*
 * Consume a pending signal from set. Callers (libpq's SIGPIPE handling)
 * block the signals and check sigpending() first, so this normally finds
 * one immediately.
 */
int
sigwait(const sigset_t *restrict set, int *restrict sig)
{
	for (;;)
	{
		uint64_t	match = __atomic_load_n(&pgzero_signals.pending, __ATOMIC_ACQUIRE) & *set;

		if (match != 0)
		{
			int			s = __builtin_ctzll(match);

			__atomic_fetch_and(&pgzero_signals.pending, ~BIT(s), __ATOMIC_ACQ_REL);
			*sig = s;
			return 0;
		}
		host_sleep(1000);
	}
}
