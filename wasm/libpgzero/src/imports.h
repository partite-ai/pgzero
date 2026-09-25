/*
 * imports.h
 *	  Raw canonical-ABI imports of the pgzero:host interfaces (wasm/wit/pgzero.wit).
 *	  Every function takes and returns scalars only, so the lowering is the
 *	  identity: s32/u32 -> i32, s64/u64 -> i64.
 */
#ifndef PGZERO_IMPORTS_H
#define PGZERO_IMPORTS_H

#include <stdint.h>

#define PGZERO_IMPORT(iface, name) \
	__attribute__((import_module("pgzero:host/" iface "@0.1.0"), import_name(name)))

/* proc */
PGZERO_IMPORT("proc", "getpid") int32_t host_getpid(void);
PGZERO_IMPORT("proc", "getppid") int32_t host_getppid(void);
PGZERO_IMPORT("proc", "spawn") int32_t host_spawn(uint32_t argv_addr, uint32_t envp_addr, uint32_t cwd_addr);
PGZERO_IMPORT("proc", "waitpid") int32_t host_waitpid(int32_t pid, uint32_t status_addr, int32_t options);
PGZERO_IMPORT("proc", "kill") int32_t host_kill(int32_t pid, int32_t sig);
PGZERO_IMPORT("proc", "exit") _Noreturn void host_exit(int32_t status);

/* sig */
PGZERO_IMPORT("sig", "register") int32_t host_sig_register(uint32_t block_addr);
PGZERO_IMPORT("sig", "set-alarm") int64_t host_set_alarm(uint64_t value_usec, uint64_t interval_usec);
PGZERO_IMPORT("sig", "sleep") int32_t host_sleep(uint64_t usec);

/* fd */
PGZERO_IMPORT("fd", "devnull") int32_t host_devnull(void);
PGZERO_IMPORT("fd", "pipe") int32_t host_pipe(uint32_t fds_addr, int32_t flags);
PGZERO_IMPORT("fd", "read") int32_t host_read(int32_t fd, uint32_t buf, uint32_t len);
PGZERO_IMPORT("fd", "write") int32_t host_write(int32_t fd, uint32_t buf, uint32_t len);
PGZERO_IMPORT("fd", "close") int32_t host_close(int32_t fd);
PGZERO_IMPORT("fd", "dup") int32_t host_dup(int32_t fd, int32_t new_fd);
PGZERO_IMPORT("fd", "fcntl") int32_t host_fcntl(int32_t fd, int32_t cmd, int32_t arg);
PGZERO_IMPORT("fd", "poll") int32_t host_poll(uint32_t fds_addr, uint32_t nfds, int32_t timeout_ms);

/* net */
PGZERO_IMPORT("net", "socket") int32_t host_socket(int32_t domain, int32_t type, int32_t protocol);
PGZERO_IMPORT("net", "bind") int32_t host_bind(int32_t fd, uint32_t addr, uint32_t addrlen);
PGZERO_IMPORT("net", "listen") int32_t host_listen(int32_t fd, int32_t backlog);
PGZERO_IMPORT("net", "accept") int32_t host_accept(int32_t fd, uint32_t addr, uint32_t addrlen_addr, int32_t flags);
PGZERO_IMPORT("net", "connect") int32_t host_connect(int32_t fd, uint32_t addr, uint32_t addrlen);
PGZERO_IMPORT("net", "recv") int32_t host_recv(int32_t fd, uint32_t buf, uint32_t len, int32_t flags);
PGZERO_IMPORT("net", "send") int32_t host_send(int32_t fd, uint32_t buf, uint32_t len, int32_t flags);
PGZERO_IMPORT("net", "shutdown") int32_t host_shutdown(int32_t fd, int32_t how);
PGZERO_IMPORT("net", "getsockopt") int32_t host_getsockopt(int32_t fd, int32_t level, int32_t name, uint32_t val, uint32_t len_addr);
PGZERO_IMPORT("net", "setsockopt") int32_t host_setsockopt(int32_t fd, int32_t level, int32_t name, uint32_t val, uint32_t len);
PGZERO_IMPORT("net", "getsockname") int32_t host_getsockname(int32_t fd, uint32_t addr, uint32_t addrlen_addr);
PGZERO_IMPORT("net", "getpeername") int32_t host_getpeername(int32_t fd, uint32_t addr, uint32_t addrlen_addr);

/* shm */
PGZERO_IMPORT("shm", "create") int32_t host_shm_create(uint32_t size);
PGZERO_IMPORT("shm", "size") int32_t host_shm_size(int32_t id);
PGZERO_IMPORT("shm", "attach") int32_t host_shm_attach(int32_t id, uint32_t addr);
PGZERO_IMPORT("shm", "remove") int32_t host_shm_remove(int32_t id);
PGZERO_IMPORT("shm", "open") int32_t host_shm_open(uint32_t name_addr, uint32_t name_len, int32_t flags);
PGZERO_IMPORT("shm", "unlink") int32_t host_shm_unlink(uint32_t name_addr, uint32_t name_len);
PGZERO_IMPORT("shm", "truncate") int32_t host_shm_truncate(int32_t fd, uint32_t size);
PGZERO_IMPORT("shm", "fd-size") int32_t host_shm_fd_size(int32_t fd);
PGZERO_IMPORT("shm", "map-fd") int32_t host_shm_map_fd(int32_t fd, uint32_t addr, uint32_t len);
PGZERO_IMPORT("shm", "unmap") int32_t host_shm_unmap(uint32_t addr, uint32_t len);

/* sema */
PGZERO_IMPORT("sema", "create") int32_t host_sema_create(int32_t initial);
PGZERO_IMPORT("sema", "lock") int32_t host_sema_lock(int32_t id);
PGZERO_IMPORT("sema", "try-lock") int32_t host_sema_try_lock(int32_t id);
PGZERO_IMPORT("sema", "unlock") int32_t host_sema_unlock(int32_t id);
PGZERO_IMPORT("sema", "reset") int32_t host_sema_reset(int32_t id);

#endif							/* PGZERO_IMPORTS_H */
