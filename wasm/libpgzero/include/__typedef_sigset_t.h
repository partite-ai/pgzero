/*
 * pgzero: wasi-libc's sigset_t is one byte, too small for SIGURG and SIGIO.
 * libpgzero implements every function that takes a sigset_t, so it can be
 * widened here without disagreeing with libc.
 */
#ifndef __wasilibc___typedef_sigset_t_h
#define __wasilibc___typedef_sigset_t_h
typedef unsigned long long sigset_t;
#endif
