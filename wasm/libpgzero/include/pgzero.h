/*
 * pgzero.h
 *	  pgzero-specific interfaces for code that knows it is running under the
 *	  pgzero host (the Postgres wasi port). Everything else is plain POSIX
 *	  provided by libpgzero.
 */
#ifndef PGZERO_H
#define PGZERO_H

#include <stdint.h>
#include <sys/types.h>

#ifdef __cplusplus
extern "C" {
#endif

/*
 * Start a new process running this program with the given NULL-terminated
 * argv, inheriting host file descriptors as fork+exec would. Returns the
 * child's pid, or -1 with errno set.
 */
extern pid_t pgzero_spawn(char *const argv[]);

/*
 * Shared memory segments (mapped at a caller-chosen, 64KiB-aligned address
 * that is already within linear memory) and counting semaphores shared by
 * all processes. All return -1 with errno set on failure.
 */
extern int	pgzero_shm_create(size_t size);
extern ssize_t pgzero_shm_size(int id);
extern int	pgzero_shm_attach(int id, void *addr);
extern int	pgzero_shm_remove(int id);

extern int	pgzero_sema_create(int initial);
extern int	pgzero_sema_lock(int id);
extern int	pgzero_sema_try_lock(int id);
extern int	pgzero_sema_unlock(int id);
extern int	pgzero_sema_reset(int id);

/*
 * Loadable modules linked statically into the program (see
 * wasm/cmd/genmodules). dlopen() looks modules up by file name without
 * directory or ".so"; both tables end with a zeroed entry.
 */
typedef struct pgzero_module_symbol
{
	const char *name;
	void	   *addr;
} pgzero_module_symbol;

typedef struct pgzero_module
{
	const char *name;
	const pgzero_module_symbol *symbols;
} pgzero_module;

extern const pgzero_module pgzero_modules[];

/*
 * Signal control block shared with the host; see wasm/wit/pgzero.wit (interface
 * sig). The host sets bits in pending asynchronously.
 */
typedef struct pgzero_sigblock
{
	uint64_t	pending;
	uint64_t	blocked;
	uint64_t	ignored;
} pgzero_sigblock;

extern pgzero_sigblock pgzero_signals;
extern void pgzero_dispatch_signals(void);

/*
 * Run handlers for any deliverable pending signals. Cheap enough to call
 * from CHECK_FOR_INTERRUPTS: a relaxed load and a test in the common case.
 */
static inline void
pgzero_check_signals(void)
{
	if (__builtin_expect((__atomic_load_n(&pgzero_signals.pending, __ATOMIC_RELAXED) &
						  ~pgzero_signals.blocked) != 0, 0))
		pgzero_dispatch_signals();
}

#ifdef __cplusplus
}
#endif

#endif							/* PGZERO_H */
