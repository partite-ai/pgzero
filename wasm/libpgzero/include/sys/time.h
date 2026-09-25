/* pgzero: interval timers; ITIMER_REAL is implemented by the host. */
#ifndef PGZERO_SYS_TIME_H
#define PGZERO_SYS_TIME_H
#include_next <sys/time.h>
#ifdef __cplusplus
extern "C" {
#endif
#define ITIMER_REAL    0
#define ITIMER_VIRTUAL 1
#define ITIMER_PROF    2
struct itimerval
{
	struct timeval it_interval;
	struct timeval it_value;
};
int			getitimer(int, struct itimerval *);
int			setitimer(int, const struct itimerval *__restrict, struct itimerval *__restrict);
#ifdef __cplusplus
}
#endif
#endif
