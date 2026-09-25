/* pgzero: a single fixed group. */
#ifndef PGZERO_GRP_H
#define PGZERO_GRP_H
#include <sys/types.h>
#ifdef __cplusplus
extern "C" {
#endif
struct group
{
	char	   *gr_name;
	char	   *gr_passwd;
	gid_t		gr_gid;
	char	  **gr_mem;
};
struct group *getgrgid(gid_t);
struct group *getgrnam(const char *);
#ifdef __cplusplus
}
#endif
#endif
