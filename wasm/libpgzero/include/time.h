/* pgzero: tzset exists (as a no-op; Postgres has its own timezone code). */
#ifndef PGZERO_TIME_H
#define PGZERO_TIME_H
#include_next <time.h>
#ifdef __cplusplus
extern "C" {
#endif
void		tzset(void);
#ifdef __cplusplus
}
#endif
#endif
