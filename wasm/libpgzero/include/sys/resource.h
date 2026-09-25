/*
 * pgzero: replaces wasi-libc's <sys/resource.h>, whose struct rusage has only
 * the CPU times. libpgzero implements everything declared here.
 */
#ifndef PGZERO_SYS_RESOURCE_H
#define PGZERO_SYS_RESOURCE_H
#include <sys/time.h>
#include <sys/types.h>
#ifdef __cplusplus
extern "C" {
#endif
typedef unsigned long long rlim_t;
struct rlimit
{
	rlim_t		rlim_cur;
	rlim_t		rlim_max;
};
#define RLIM_INFINITY (~0ULL)
#define RLIMIT_CPU     0
#define RLIMIT_FSIZE   1
#define RLIMIT_DATA    2
#define RLIMIT_STACK   3
#define RLIMIT_CORE    4
#define RLIMIT_NOFILE  7
#define RLIMIT_AS      9

struct rusage
{
	struct timeval ru_utime;
	struct timeval ru_stime;
	long		ru_maxrss;
	long		ru_ixrss;
	long		ru_idrss;
	long		ru_isrss;
	long		ru_minflt;
	long		ru_majflt;
	long		ru_nswap;
	long		ru_inblock;
	long		ru_oublock;
	long		ru_msgsnd;
	long		ru_msgrcv;
	long		ru_nsignals;
	long		ru_nvcsw;
	long		ru_nivcsw;
};
#define RUSAGE_SELF     0
#define RUSAGE_CHILDREN (-1)

int			getrlimit(int, struct rlimit *);
int			setrlimit(int, const struct rlimit *);
int			getrusage(int, struct rusage *);
#ifdef __cplusplus
}
#endif
#endif
