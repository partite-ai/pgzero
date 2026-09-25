// Package abi holds the guest ABI: wasi-libc constant values (consts.go,
// generated) and the struct layouts the host reads from guest memory.
package abi

//go:generate sh -c "cd ../.. && go run ./wasm/cmd/genabi"

// FDBase is the first host file descriptor number above stdio (see
// wasm/libpgzero/src/internal.h).
const FDBase = 1 << 20

// Struct layouts (wasm32, wasi-libc; checked by wasm/libpgzero/src/abi_check.c).
const (
	SizeofPollfd     = 8 // int fd; short events; short revents
	OffPollfdFD      = 0
	OffPollfdEvents  = 4
	OffPollfdRevents = 6

	SizeofSockaddrIn  = 16 // u16 family; u16 port (BE); u32 addr
	SizeofSockaddrIn6 = 32 // u16 family; u16 port; u32 flowinfo; [16]u8 addr; u32 scope; padding
	SizeofSockaddrUn  = 110
	SunPathLen        = 108

	SizeofSigblock = 24 // u64 pending, blocked, ignored
)
