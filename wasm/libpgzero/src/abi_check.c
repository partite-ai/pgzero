/*
 * abi_check.c
 *	  Compile-time checks of the struct layouts the host reads and writes
 *	  in guest memory (see internal/abi/abi.go).
 */
#include <netinet/in.h>
#include <poll.h>
#include <stddef.h>
#include <sys/socket.h>
#include <sys/un.h>

#include "pgzero.h"

_Static_assert(sizeof(struct pollfd) == 8, "pollfd size");
_Static_assert(offsetof(struct pollfd, fd) == 0, "pollfd.fd");
_Static_assert(offsetof(struct pollfd, events) == 4, "pollfd.events");
_Static_assert(offsetof(struct pollfd, revents) == 6, "pollfd.revents");

_Static_assert(sizeof(sa_family_t) == 2, "sa_family_t");
_Static_assert(sizeof(struct sockaddr_in) == 16, "sockaddr_in size");
_Static_assert(offsetof(struct sockaddr_in, sin_port) == 2, "sin_port");
_Static_assert(offsetof(struct sockaddr_in, sin_addr) == 4, "sin_addr");
_Static_assert(sizeof(struct sockaddr_in6) == 32, "sockaddr_in6 size");
_Static_assert(offsetof(struct sockaddr_in6, sin6_port) == 2, "sin6_port");
_Static_assert(offsetof(struct sockaddr_in6, sin6_flowinfo) == 4, "sin6_flowinfo");
_Static_assert(offsetof(struct sockaddr_in6, sin6_addr) == 8, "sin6_addr");
_Static_assert(offsetof(struct sockaddr_in6, sin6_scope_id) == 24, "sin6_scope_id");
_Static_assert(sizeof(struct sockaddr_un) == 110, "sockaddr_un size");
_Static_assert(offsetof(struct sockaddr_un, sun_path) == 2, "sun_path");
_Static_assert(sizeof(struct sockaddr_storage) >= sizeof(struct sockaddr_un), "sockaddr_storage");
_Static_assert(sizeof(socklen_t) == 4, "socklen_t");

_Static_assert(sizeof(pgzero_sigblock) == 24, "sigblock size");
_Static_assert(offsetof(pgzero_sigblock, blocked) == 8, "sigblock.blocked");
_Static_assert(offsetof(pgzero_sigblock, ignored) == 16, "sigblock.ignored");
