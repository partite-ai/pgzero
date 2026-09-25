/*
 * pgzero: the LLVM wasm setjmp/longjmp lowering only recognises setjmp and
 * longjmp, so map the sig* variants onto them (as Postgres does for WIN32).
 * Signal masks are not saved; libpgzero never runs handlers asynchronously.
 */
#ifndef PGZERO_SETJMP_H
#define PGZERO_SETJMP_H
#include_next <setjmp.h>
#undef sigsetjmp
#undef siglongjmp
#define sigjmp_buf jmp_buf
#define sigsetjmp(env, savemask) setjmp(env)
#define siglongjmp longjmp
#endif
