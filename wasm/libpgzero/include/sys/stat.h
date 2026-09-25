/* pgzero: umask is tracked by libpgzero (WASI has no file modes to apply it to). */
#ifndef PGZERO_SYS_STAT_H
#define PGZERO_SYS_STAT_H
#include_next <sys/stat.h>
#ifdef __cplusplus
extern "C" {
#endif
mode_t		umask(mode_t);
#ifdef __cplusplus
}
#endif
#endif
