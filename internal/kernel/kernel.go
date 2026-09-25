// Package kernel is pgzero's process host: it runs each Postgres process as a
// separate instance of the Postgres component and implements the
// pgzero:host interfaces (wasm/wit/pgzero.wit) those instances import - processes,
// signals, host file descriptors (stdio, pipes, sockets), shared memory and
// semaphores.
//
// A Runtime holds what is expensive and shareable - the wasm engine and
// compiled programs - and any number of Kernels, each an independent
// "machine" with its own processes, filesystem and IPC, can run on it.
package kernel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/partite-ai/pgzero/internal/abi"
	"github.com/partite-ai/pgzero/internal/gen/pgzero/host/fd"
	gennet "github.com/partite-ai/pgzero/internal/gen/pgzero/host/net"
	"github.com/partite-ai/pgzero/internal/gen/pgzero/host/proc"
	"github.com/partite-ai/pgzero/internal/gen/pgzero/host/sema"
	"github.com/partite-ai/pgzero/internal/gen/pgzero/host/shm"
	"github.com/partite-ai/pgzero/internal/gen/pgzero/host/sig"
)

// RuntimeConfig configures a Runtime.
type RuntimeConfig struct {
	// Programs maps program names (the base name of argv[0], e.g.
	// "postgres" or "initdb") to component binaries.
	Programs map[string][]byte

	// CacheDir, if set, caches compiled code across runs.
	CacheDir string

	// DebugInfo makes crash stack traces show source locations from the
	// components' DWARF data. Building those traces is expensive, and
	// happens on every process exit, so leave it off unless debugging.
	DebugInfo bool
}

// Runtime is the wasm engine and the compiled programs, shared by any
// number of Kernels.
type Runtime struct {
	cfg    RuntimeConfig
	engine *wacogo.Engine

	progMu sync.Mutex
	progs  map[string]*program

	procFac *proc.Factory
	sigFac  *sig.Factory
	fdFac   *fd.Factory
	netFac  *gennet.Factory
	shmFac  *shm.Factory
	semaFac *sema.Factory
}

// NewRuntime creates a Runtime. Programs are compiled on first use.
func NewRuntime(ctx context.Context, cfg RuntimeConfig) (*Runtime, error) {
	rc := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 |
			experimental.CoreFeaturesExtendedConst |
			experimental.CoreFeaturesExceptionHandling |
			experimental.CoreFeaturesThreads).
		// Runs wasm as if in a syscall (see the wazero fork): without it a
		// process in a long CPU loop stalls GC stop-the-world for everyone.
		// It also lets SIGKILL stop a process by cancelling its context.
		WithCloseOnContextDone(true).
		WithDebugInfoEnabled(cfg.DebugInfo)
	if cfg.CacheDir != "" {
		cache, err := wazero.NewCompilationCacheWithDir(cfg.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("compilation cache: %w", err)
		}
		rc = rc.WithCompilationCache(cache)
	}
	rt := &Runtime{
		cfg:    cfg,
		engine: wacogo.NewEngine(ctx, wacogo.WithRuntimeConfig(rc)),
		progs:  map[string]*program{},
	}
	var err error
	if rt.procFac, err = proc.NewFactory(ctx, rt.engine); err == nil {
		if rt.sigFac, err = sig.NewFactory(ctx, rt.engine); err == nil {
			if rt.fdFac, err = fd.NewFactory(ctx, rt.engine); err == nil {
				if rt.netFac, err = gennet.NewFactory(ctx, rt.engine); err == nil {
					if rt.shmFac, err = shm.NewFactory(ctx, rt.engine); err == nil {
						rt.semaFac, err = sema.NewFactory(ctx, rt.engine)
					}
				}
			}
		}
	}
	if err != nil {
		rt.engine.Close(ctx)
		return nil, err
	}
	return rt, nil
}

// Close releases the engine. Kernels using the Runtime must be closed
// first.
func (rt *Runtime) Close(ctx context.Context) error {
	return rt.engine.Close(ctx)
}

// program is a compiled component, compiled on first use.
type program struct {
	once sync.Once
	comp *wacogo.Component
	err  error
}

// program returns the compiled component for argv0.
func (rt *Runtime) program(argv0 string) (*wacogo.Component, error) {
	name := path.Base(argv0)
	bin, ok := rt.cfg.Programs[name]
	if !ok {
		return nil, fmt.Errorf("%s: no such program", name)
	}
	rt.progMu.Lock()
	prog := rt.progs[name]
	if prog == nil {
		prog = &program{}
		rt.progs[name] = prog
	}
	rt.progMu.Unlock()
	prog.once.Do(func() {
		prog.comp, prog.err = rt.engine.LoadComponent(context.Background(), bytes.NewReader(bin))
		if prog.err != nil {
			prog.err = fmt.Errorf("load %s: %w", name, prog.err)
		}
	})
	return prog.comp, prog.err
}

// Precompile compiles the named programs now rather than on first use.
func (rt *Runtime) Precompile(names ...string) error {
	for _, n := range names {
		if _, err := rt.program(n); err != nil {
			return err
		}
	}
	return nil
}

// HasProgram reports whether argv0 names a known program.
func (rt *Runtime) HasProgram(argv0 string) bool {
	_, ok := rt.cfg.Programs[path.Base(argv0)]
	return ok
}

// Mount makes a filesystem visible to every process at a guest path:
// either a host directory (HostPath) or any fs.FS (FS), such as an
// in-memory filesystem.
type Mount struct {
	GuestPath string // absolute, e.g. "/data"
	HostPath  string
	FS        fs.FS
}

// Config configures a Kernel.
type Config struct {
	// Mounts are the filesystems visible to processes.
	//
	// Unix-domain sockets live in the kernel: a process can connect to one
	// another process listens on, and so can the host, with DialUnix. When
	// a socket's path is inside a HostPath mount, it is also created as a
	// real socket at the corresponding host path.
	Mounts []Mount

	// Env is the environment of the first process, and Cwd its working
	// directory (a guest path).
	Env [][2]string
	Cwd string

	// Stdio of the first process; children inherit it (or whatever the
	// parent has redirected it to).
	Stdin          io.Reader
	Stdout, Stderr io.Writer

	// Debugf, if set, receives kernel trace messages.
	Debugf func(format string, args ...any)
}

// Kernel runs processes.
type Kernel struct {
	rt  *Runtime
	cfg Config

	mu    sync.Mutex
	procs map[int32]*Process

	shm  shmTable
	sems semaTable

	unixMu sync.Mutex
	unix   map[string]*virtualListener // listening Unix sockets by guest path
}

// New creates a Kernel on rt.
func New(rt *Runtime, cfg Config) *Kernel {
	if cfg.Stdin == nil {
		cfg.Stdin = strings.NewReader("")
	}
	if cfg.Stdout == nil {
		cfg.Stdout = os.Stdout
	}
	if cfg.Stderr == nil {
		cfg.Stderr = os.Stderr
	}
	if cfg.Cwd == "" {
		cfg.Cwd = "/"
	}
	k := &Kernel{
		rt:    rt,
		cfg:   cfg,
		procs: map[int32]*Process{},
		unix:  map[string]*virtualListener{},
	}
	k.shm.init()
	k.sems.init()
	return k
}

// Close kills all processes and releases the kernel's resources.
func (k *Kernel) Close() error {
	k.mu.Lock()
	var running []*Process
	for _, p := range k.procs {
		running = append(running, p)
	}
	k.mu.Unlock()
	for _, p := range running {
		p.Signal(sigKill)
	}
	for _, p := range running {
		<-p.done
	}
	k.shm.closeAll()
	return nil
}

// Start starts a process with no parent and the kernel's stdio.
func (k *Kernel) Start(argv []string) (*Process, error) {
	p := k.newProcess(nil, argv, k.cfg.Env)
	if p == nil {
		return nil, errors.New("out of process ids")
	}
	p.cwd = k.cfg.Cwd
	p.fds = newFDTable()
	p.fds.install(0, newOpenFile(newReaderFile(k.cfg.Stdin), 0), false)
	p.fds.install(1, newOpenFile(newWriterFile(k.cfg.Stdout), 0), false)
	p.fds.install(2, newOpenFile(newWriterFile(k.cfg.Stderr), 0), false)
	go p.run()
	return p, nil
}

// Pids. Postgres guards its data directory with a lock file holding the
// postmaster's pid, and treats the lock as stale unless kill(pid, 0) finds
// that process. So pids must be distinct across all kernels, in this Go
// process and in other pgzero processes on the host, and each must be
// recognizable: a pid is (host pid mod 2^20) << 11 | n, with n in
// [1, 2048) allocated across all kernels of this Go process and reused
// once free.
const (
	pidBits      = 11
	pidsPerHost  = 1 << pidBits
	hostPidRange = 1 << 20
)

var pids struct {
	mu     sync.Mutex
	next   int32
	owners map[int32]*Kernel
}

func pidBase() int32 { return int32(os.Getpid()%hostPidRange) << pidBits }

// hostPidOf returns the host pid of the pgzero process that owns pid, as far
// as it can be recovered (host pids above 2^20 wrap).
func hostPidOf(pid int32) int { return int(pid >> pidBits) }

func allocPid(k *Kernel) int32 {
	pids.mu.Lock()
	defer pids.mu.Unlock()
	if pids.owners == nil {
		pids.owners = map[int32]*Kernel{}
		pids.next = 1
	}
	base := pidBase()
	for range pidsPerHost {
		n := pids.next
		pids.next = pids.next%(pidsPerHost-1) + 1
		if pids.owners[base+n] == nil {
			pids.owners[base+n] = k
			return base + n
		}
	}
	return 0
}

func releasePid(pid int32) {
	pids.mu.Lock()
	defer pids.mu.Unlock()
	delete(pids.owners, pid)
}

func pidOwner(pid int32) *Kernel {
	pids.mu.Lock()
	defer pids.mu.Unlock()
	return pids.owners[pid]
}

func (k *Kernel) newProcess(parent *Process, argv []string, env [][2]string) *Process {
	// Exited processes keep their pid until reaped (see forget), so pids
	// aren't reused while a parent may still wait for them.
	pid := allocPid(k)
	if pid == 0 {
		// All pids in use; the caller sees EAGAIN, as from fork.
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	p := newProcess(k, pid, parent, argv, env)
	k.procs[pid] = p
	return p
}

func (k *Kernel) lookup(pid int32) *Process {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.procs[pid]
}

func (k *Kernel) forget(p *Process) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.forgetLocked(p.pid)
}

func (k *Kernel) forgetLocked(pid int32) {
	if _, ok := k.procs[pid]; ok {
		delete(k.procs, pid)
		releasePid(pid)
	}
}

func (k *Kernel) debugf(format string, args ...any) {
	if k.cfg.Debugf != nil {
		k.cfg.Debugf(format, args...)
	}
}

// mountFor returns the mount containing an absolute guest path (the
// longest match) and the path relative to it.
func (k *Kernel) mountFor(guest string) (Mount, string, bool) {
	guest = path.Clean(guest)
	best := -1
	for i, m := range k.cfg.Mounts {
		mp := path.Clean(m.GuestPath)
		if guest == mp || strings.HasPrefix(guest, strings.TrimSuffix(mp, "/")+"/") {
			if best < 0 || len(mp) > len(path.Clean(k.cfg.Mounts[best].GuestPath)) {
				best = i
			}
		}
	}
	if best < 0 {
		return Mount{}, "", false
	}
	m := k.cfg.Mounts[best]
	return m, strings.TrimPrefix(strings.TrimPrefix(guest, path.Clean(m.GuestPath)), "/"), true
}

// hostPath maps an absolute guest path to a host path, if it is inside a
// HostPath mount.
func (k *Kernel) hostPath(guest string) (string, bool) {
	m, rel, ok := k.mountFor(guest)
	if !ok || m.HostPath == "" {
		return "", false
	}
	return filepath.Join(m.HostPath, filepath.FromSlash(rel)), true
}

// killForeign handles kill(2) for a pid that isn't one of ours: it may
// belong to another kernel in this Go process, or to another pgzero process
// on the host (e.g. another server using the same data directory, as
// Postgres's lock file check asks). Its existence can be checked, but it
// can't be signalled.
func (k *Kernel) killForeign(pid, sig int32) int32 {
	if pid <= 0 {
		return -abi.ESRCH
	}
	owner := hostPidOf(pid)
	switch {
	case owner == hostPidOf(pidBase()):
		if pidOwner(pid) == nil {
			return -abi.ESRCH
		}
	case owner == 0:
		return -abi.ESRCH
	default:
		if err := syscall.Kill(owner, 0); err != nil && err != syscall.EPERM {
			return -abi.ESRCH
		}
	}
	if sig != 0 {
		return -abi.EPERM
	}
	return 0
}

// DialUnix connects to the Unix-domain socket a process in this kernel
// listens on at guest path addr, without any host socket: the connection
// is an in-memory pipe.
func (k *Kernel) DialUnix(ctx context.Context, addr string) (net.Conn, error) {
	k.unixMu.Lock()
	l := k.unix[path.Clean(addr)]
	k.unixMu.Unlock()
	if l == nil {
		return nil, &net.OpError{Op: "dial", Net: "unix", Addr: &net.UnixAddr{Name: addr, Net: "unix"},
			Err: syscall.ECONNREFUSED}
	}
	return l.dial(ctx)
}

// listenUnix registers a listening Unix socket at a guest path.
func (k *Kernel) listenUnix(addr string) (*virtualListener, error) {
	addr = path.Clean(addr)
	k.unixMu.Lock()
	defer k.unixMu.Unlock()
	if _, ok := k.unix[addr]; ok {
		return nil, syscall.EADDRINUSE
	}
	l := &virtualListener{
		k:      k,
		addr:   &net.UnixAddr{Name: addr, Net: "unix"},
		conns:  make(chan net.Conn),
		closed: make(chan struct{}),
	}
	k.unix[addr] = l
	return l, nil
}

// createSocketFile creates an empty file where a virtual socket is bound,
// when that path is in a filesystem (not host directory) mount: Postgres,
// for one, chmods its socket after binding. (In host directory mounts the
// real socket that is also created takes its place.)
func (k *Kernel) createSocketFile(guest string) {
	m, rel, ok := k.mountFor(guest)
	if !ok || m.FS == nil || rel == "" {
		return
	}
	root, err := m.FS.Open(".")
	if err != nil {
		return
	}
	defer root.Close()
	if oa, ok := preopens.As[preopens.OpenAter](root); ok {
		if f, err := oa.OpenAt(rel, os.O_CREATE|os.O_WRONLY, 0o777); err == nil {
			f.Close()
		}
	}
}

// virtualListener is a Unix socket that exists only inside the kernel.
type virtualListener struct {
	k      *Kernel
	addr   *net.UnixAddr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *virtualListener) dial(ctx context.Context) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-l.closed:
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	}
	client.Close()
	server.Close()
	return nil, &net.OpError{Op: "dial", Net: "unix", Addr: l.addr, Err: syscall.ECONNREFUSED}
}

func (l *virtualListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *virtualListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		l.k.unixMu.Lock()
		if l.k.unix[l.addr.Name] == l {
			delete(l.k.unix, l.addr.Name)
		}
		l.k.unixMu.Unlock()
	})
	return nil
}

func (l *virtualListener) Addr() net.Addr { return l.addr }
