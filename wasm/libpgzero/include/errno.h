/*
 * pgzero: errno as wasi-libc really defines it, plus values it lacks.
 *
 * The single-threaded wasm32-wasip2 libc declares errno _Thread_local but
 * defines it as a plain global. Postgres is built with the atomics and
 * bulk-memory features (it needs real atomics for spinlocks in shared
 * memory), and with those clang makes _Thread_local variables genuinely
 * thread-local, which then fails to link against libc's errno. Hide the
 * qualifier while libc declares errno so every object agrees with libc.
 */
#ifndef PGZERO_ERRNO_H
#define PGZERO_ERRNO_H
#pragma clang diagnostic push
#pragma clang diagnostic ignored "-Wkeyword-macro"
#pragma push_macro("_Thread_local")
#define _Thread_local
#include_next <errno.h>
#pragma pop_macro("_Thread_local")
#pragma clang diagnostic pop
#ifndef EHOSTDOWN
#define EHOSTDOWN 200
#endif
#endif
