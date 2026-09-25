/*
 * net.c
 *	  Sockets. All sockets are host descriptors; see fd.c.
 *
 * Linked with --wrap for each function, replacing wasi-libc's
 * wasi:sockets-based implementation.
 */
#include <arpa/inet.h>
#include <netdb.h>
#include <netinet/in.h>
#include <stdbool.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>

#include "internal.h"

int
__wrap_socket(int domain, int type, int protocol)
{
	return sysret(host_socket(domain, type, protocol));
}

int
__wrap_bind(int fd, const struct sockaddr *addr, socklen_t len)
{
	return sysret(host_bind(fd, ADDR(addr), len));
}

int
__wrap_listen(int fd, int backlog)
{
	return sysret(host_listen(fd, backlog));
}

int
__wrap_accept4(int fd, struct sockaddr *restrict addr, socklen_t *restrict len, int flags)
{
	return sysret(host_accept(fd, ADDR(addr), ADDR(len), flags));
}

int
__wrap_accept(int fd, struct sockaddr *restrict addr, socklen_t *restrict len)
{
	return __wrap_accept4(fd, addr, len, 0);
}

int
__wrap_connect(int fd, const struct sockaddr *addr, socklen_t len)
{
	return sysret(host_connect(fd, ADDR(addr), len));
}

ssize_t
__wrap_recv(int fd, void *buf, size_t len, int flags)
{
	return sysret(host_recv(fd, ADDR(buf), len, flags));
}

ssize_t
__wrap_send(int fd, const void *buf, size_t len, int flags)
{
	return sysret(host_send(fd, ADDR(buf), len, flags));
}

int
__wrap_shutdown(int fd, int how)
{
	return sysret(host_shutdown(fd, how));
}

int
__wrap_getsockopt(int fd, int level, int name, void *restrict val, socklen_t *restrict len)
{
	return sysret(host_getsockopt(fd, level, name, ADDR(val), ADDR(len)));
}

int
__wrap_setsockopt(int fd, int level, int name, const void *val, socklen_t len)
{
	return sysret(host_setsockopt(fd, level, name, ADDR(val), len));
}

int
__wrap_getsockname(int fd, struct sockaddr *restrict addr, socklen_t *restrict len)
{
	return sysret(host_getsockname(fd, ADDR(addr), ADDR(len)));
}

int
__wrap_getpeername(int fd, struct sockaddr *restrict addr, socklen_t *restrict len)
{
	return sysret(host_getpeername(fd, ADDR(addr), ADDR(len)));
}

/*
 * Name resolution. Postgres resolves listen_addresses, pg_hba.conf entries
 * and client addresses; numeric addresses and "localhost" cover the server's
 * needs. AF_UNIX paths never reach here (Postgres handles them itself).
 */

struct ai_block
{
	struct addrinfo ai;
	union
	{
		struct sockaddr_in in;
		struct sockaddr_in6 in6;
	}			addr;
};

static struct addrinfo *
make_ai(int family, const void *host_addr, uint16_t port, const struct addrinfo *hints)
{
	struct ai_block *b = calloc(1, sizeof(*b));

	if (!b)
		return NULL;
	b->ai.ai_family = family;
	b->ai.ai_socktype = hints && hints->ai_socktype ? hints->ai_socktype : SOCK_STREAM;
	b->ai.ai_protocol = hints ? hints->ai_protocol : 0;
	b->ai.ai_addr = (struct sockaddr *) &b->addr;
	if (family == AF_INET)
	{
		b->addr.in.sin_family = AF_INET;
		b->addr.in.sin_port = htons(port);
		memcpy(&b->addr.in.sin_addr, host_addr, sizeof(struct in_addr));
		b->ai.ai_addrlen = sizeof(struct sockaddr_in);
	}
	else
	{
		b->addr.in6.sin6_family = AF_INET6;
		b->addr.in6.sin6_port = htons(port);
		memcpy(&b->addr.in6.sin6_addr, host_addr, sizeof(struct in6_addr));
		b->ai.ai_addrlen = sizeof(struct sockaddr_in6);
	}
	return &b->ai;
}

void
__wrap_freeaddrinfo(struct addrinfo *ai)
{
	while (ai)
	{
		struct addrinfo *next = ai->ai_next;

		free(ai);				/* the addrinfo heads its ai_block */
		ai = next;
	}
}

int
__wrap_getaddrinfo(const char *restrict node, const char *restrict service,
				   const struct addrinfo *restrict hints, struct addrinfo **restrict res)
{
	int			family = hints ? hints->ai_family : AF_UNSPEC;
	int			flags = hints ? hints->ai_flags : 0;
	long		port = 0;
	struct in_addr v4;
	struct in6_addr v6;
	bool		have_v4 = false,
				have_v6 = false;
	struct addrinfo *head = NULL,
			  **tail = &head;

	if (family != AF_UNSPEC && family != AF_INET && family != AF_INET6)
		return EAI_FAMILY;
	if (service && *service)
	{
		char	   *end;

		port = strtol(service, &end, 10);
		if (*end != '\0' || port < 0 || port > 65535)
			return EAI_SERVICE;
	}

	if (node == NULL)
	{
		if (flags & AI_PASSIVE)
		{
			v4.s_addr = htonl(INADDR_ANY);
			v6 = in6addr_any;
		}
		else
		{
			v4.s_addr = htonl(INADDR_LOOPBACK);
			v6 = in6addr_loopback;
		}
		have_v4 = have_v6 = true;
	}
	else if (inet_pton(AF_INET, node, &v4) == 1)
		have_v4 = true;
	else if (inet_pton(AF_INET6, node, &v6) == 1)
		have_v6 = true;
	else if (!(flags & AI_NUMERICHOST) && strcmp(node, "localhost") == 0)
	{
		v4.s_addr = htonl(INADDR_LOOPBACK);
		v6 = in6addr_loopback;
		have_v4 = have_v6 = true;
	}
	else
		return EAI_NONAME;

	if (have_v6 && family != AF_INET)
	{
		if (!(*tail = make_ai(AF_INET6, &v6, port, hints)))
			goto oom;
		tail = &(*tail)->ai_next;
	}
	if (have_v4 && family != AF_INET6)
	{
		if (!(*tail = make_ai(AF_INET, &v4, port, hints)))
			goto oom;
		tail = &(*tail)->ai_next;
	}
	if (head == NULL)
		return EAI_NONAME;
	*res = head;
	return 0;

oom:
	__wrap_freeaddrinfo(head);
	return EAI_MEMORY;
}

int
__wrap_getnameinfo(const struct sockaddr *restrict sa, socklen_t salen,
				   char *restrict host, socklen_t hostlen,
				   char *restrict serv, socklen_t servlen, int flags)
{
	const void *addr;
	uint16_t	port;

	/* Always numeric: there is no reverse DNS. */
	if (sa->sa_family == AF_INET)
	{
		addr = &((const struct sockaddr_in *) sa)->sin_addr;
		port = ntohs(((const struct sockaddr_in *) sa)->sin_port);
	}
	else if (sa->sa_family == AF_INET6)
	{
		addr = &((const struct sockaddr_in6 *) sa)->sin6_addr;
		port = ntohs(((const struct sockaddr_in6 *) sa)->sin6_port);
	}
	else
		return EAI_FAMILY;

	if (host && hostlen > 0 &&
		inet_ntop(sa->sa_family, addr, host, hostlen) == NULL)
		return EAI_OVERFLOW;
	if (serv && servlen > 0 &&
		snprintf(serv, servlen, "%u", port) >= (int) servlen)
		return EAI_OVERFLOW;
	return 0;
}
