/* pgzero: a single fixed user. */
#ifndef PGZERO_PWD_H
#define PGZERO_PWD_H
#include <sys/types.h>
#ifdef __cplusplus
extern "C" {
#endif
struct passwd
{
	char	   *pw_name;
	char	   *pw_passwd;
	uid_t		pw_uid;
	gid_t		pw_gid;
	char	   *pw_gecos;
	char	   *pw_dir;
	char	   *pw_shell;
};
struct passwd *getpwuid(uid_t);
struct passwd *getpwnam(const char *);
int			getpwuid_r(uid_t, struct passwd *, char *, size_t, struct passwd **);
int			getpwnam_r(const char *, struct passwd *, char *, size_t, struct passwd **);
#ifdef __cplusplus
}
#endif
#endif
