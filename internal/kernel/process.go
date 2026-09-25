package kernel

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/partite-ai/wacogo"
	"github.com/partite-ai/wacogo/host"
	"github.com/partite-ai/wacogo/wasi"
	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/partite-ai/pgzero/internal/abi"
	"github.com/partite-ai/pgzero/internal/gen/pgzero/host/fd"
	gennet "github.com/partite-ai/pgzero/internal/gen/pgzero/host/net"
	"github.com/partite-ai/pgzero/internal/gen/pgzero/host/proc"
	"github.com/partite-ai/pgzero/internal/gen/pgzero/host/sema"
	"github.com/partite-ai/pgzero/internal/gen/pgzero/host/shm"
	"github.com/partite-ai/pgzero/internal/gen/pgzero/host/sig"
	"github.com/partite-ai/pgzero/internal/osfs"
	"github.com/partite-ai/pgzero/internal/vmem"
)

const sigKill = abi.SIGKILL

// Process is one simulated process: an instance of the component running
// on its own goroutine, with private linear memory, a host fd table and
// POSIX-style signal state.
type Process struct {
	k      *Kernel
	pid    int32
	parent *Process
	argv   []string
	env    [][2]string
	cwd    string

	ctx    context.Context // cancelled to kill the process
	cancel context.CancelFunc

	mem *vmem.Memory
	fds *fdTable

	// Signals. sigPending points at the guest's pgzero_sigblock once it has
	// registered it; until then signals collect in early.
	sigMu      sync.Mutex
	sigPending *uint64
	early      uint64
	sigWake    chan struct{}
	alarm      alarmState

	// Lifecycle. status is a wait(2) status, valid once done is closed.
	exitMu    sync.Mutex
	exitSet   bool
	status    int32
	exited    bool
	done      chan struct{}
	childMu   sync.Mutex
	children  map[int32]*Process
	childEvts notifier

	shmAttached []attachment

	// parentGone is set (under k.mu) when the parent exits first.
	parentGone bool
}

func newProcess(k *Kernel, pid int32, parent *Process, argv []string, env [][2]string) *Process {
	ctx, cancel := context.WithCancel(context.Background())
	return &Process{
		k:        k,
		pid:      pid,
		parent:   parent,
		argv:     argv,
		env:      env,
		ctx:      ctx,
		cancel:   cancel,
		sigWake:  make(chan struct{}, 1),
		done:     make(chan struct{}),
		children: map[int32]*Process{},
	}
}

// Pid returns the process id.
func (p *Process) Pid() int32 { return p.pid }

// Wait blocks until the process exits and returns its wait(2) status.
func (p *Process) Wait() int32 {
	<-p.done
	return p.status
}

// Done is closed when the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// errExit unwinds the guest after proc.exit.
var errExit = errors.New("process exited")

// errKilled unwinds the guest from a blocking host call after SIGKILL.
var errKilled = errors.New("process killed")

func (p *Process) run() {
	status := p.execute()
	p.finish(status)
}

func (p *Process) execute() int32 {
	k := p.k
	ctx := p.ctx
	k.debugf("[%d] start %q", p.pid, p.argv)

	entries := make([]*preopens.PreopenEntry, len(k.cfg.Mounts))
	for i, m := range k.cfg.Mounts {
		fsys := m.FS
		if fsys == nil {
			fsys = osfs.Dir(m.HostPath)
		}
		entries[i] = &preopens.PreopenEntry{Path: m.GuestPath, Root: ".", FS: fsys}
	}
	// Stdio goes through host fds 0-2 (see wasm/libpgzero/src/fd.c); the WASI
	// streams are only a fallback for anything that bypasses them.
	world, err := wasi.NewWorld(ctx, k.rt.engine, &wasi.Config{
		Args:       p.argv,
		Env:        p.env,
		InitialCwd: p.cwd,
		Stdin:      strings.NewReader(""),
		Stdout:     k.cfg.Stderr,
		Stderr:     k.cfg.Stderr,
		Preopens:   preopens.NewMultiFSPreopens(entries),
	})
	if err != nil {
		k.debugf("[%d] wasi world: %v", p.pid, err)
		return 127 << 8
	}
	defer world.Close(context.Background())
	var hostInsts []*host.ComponentInstance

	imports := world.Imports()
	defer func() {
		for _, inst := range hostInsts {
			inst.Close(context.Background())
		}
	}()
	type hostInstance struct {
		name string
		new  func() (*host.ComponentInstance, error)
	}
	for _, hi := range []hostInstance{
		{proc.InterfaceName, func() (*host.ComponentInstance, error) {
			return k.rt.procFac.NewInstance(ctx, procHost{p}, nil)
		}},
		{sig.InterfaceName, func() (*host.ComponentInstance, error) {
			return k.rt.sigFac.NewInstance(ctx, sigHost{p}, nil)
		}},
		{fd.InterfaceName, func() (*host.ComponentInstance, error) {
			return k.rt.fdFac.NewInstance(ctx, fdHost{p}, nil)
		}},
		{gennet.InterfaceName, func() (*host.ComponentInstance, error) {
			return k.rt.netFac.NewInstance(ctx, netHost{p}, nil)
		}},
		{shm.InterfaceName, func() (*host.ComponentInstance, error) {
			return k.rt.shmFac.NewInstance(ctx, shmHost{p}, nil)
		}},
		{sema.InterfaceName, func() (*host.ComponentInstance, error) {
			return k.rt.semaFac.NewInstance(ctx, semaHost{p}, nil)
		}},
	} {
		inst, err := hi.new()
		if err != nil {
			k.debugf("[%d] host instance %s: %v", p.pid, hi.name, err)
			return 127 << 8
		}
		hostInsts = append(hostInsts, inst)
		imports = append(imports, wacogo.WithInstanceImport(hi.name, inst.Core()))
	}

	comp, err := k.rt.program(p.argv[0])
	if err != nil {
		fmt.Fprintf(k.cfg.Stderr, "pgzero: %v\n", err)
		return 127 << 8
	}
	ictx := experimental.WithMemoryAllocator(ctx, vmem.Allocator(func(m *vmem.Memory) { p.mem = m }))
	inst, err := comp.Instantiate(ictx, imports...)
	if err != nil {
		k.debugf("[%d] instantiate: %v", p.pid, err)
		return 127 << 8
	}
	defer inst.Close(context.Background())
	// Deferred after inst.Close, so it runs first: other goroutines write
	// signals straight into guest memory, which inst.Close frees.
	defer p.stopSignals()

	run := inst.ExportedInstance("wasi:cli/run@0.2.12")
	if run == nil {
		k.debugf("[%d] component does not export wasi:cli/run", p.pid)
		return 127 << 8
	}
	res, err := run.ExportedFunc("run").Call(ctx)

	p.exitMu.Lock()
	exitSet, status := p.exitSet, p.status
	p.exitMu.Unlock()
	switch {
	case exitSet:
		return status
	case p.ctx.Err() != nil:
		return abi.SIGKILL
	case err != nil:
		fmt.Fprintf(k.cfg.Stderr, "pgzero: process %d (%s) crashed: %v\n", p.pid, p.name(), err)
		return abi.SIGABRT
	}
	if r, ok := res[0].(*wacogo.ValResult); ok && r.IsOk() {
		return 0
	}
	return 1 << 8
}

func (p *Process) name() string {
	if len(p.argv) > 1 {
		return strings.Join(p.argv[:2], " ")
	}
	if len(p.argv) == 1 {
		return p.argv[0]
	}
	return "?"
}

// finish tears the process down after its guest code has stopped.
func (p *Process) finish(status int32) {
	p.k.debugf("[%d] exit status %#x", p.pid, status)

	p.stopSignals()
	p.alarm.stop()

	if p.fds != nil {
		p.fds.closeAll()
	}
	p.k.shm.detachAll(p)
	p.cancel()

	p.exitMu.Lock()
	p.status = status
	p.exited = true
	p.exitMu.Unlock()
	close(p.done)

	// Our unreaped children can never be waited for now; running ones
	// release their pids themselves when they exit.
	k := p.k
	k.mu.Lock()
	p.childMu.Lock()
	for pid, c := range p.children {
		c.parentGone = true
		select {
		case <-c.done:
			k.forgetLocked(pid)
		default:
		}
	}
	p.children = nil
	p.childMu.Unlock()
	// Keep our pid until the parent reaps us (waitpid).
	orphan := p.parent == nil || p.parentGone
	if orphan {
		k.forgetLocked(p.pid)
	}
	k.mu.Unlock()

	if !orphan {
		p.parent.childEvts.notify()
		p.parent.Signal(abi.SIGCHLD)
	}
}

// exit records the process's exit status; the caller then unwinds the
// guest with errExit.
func (p *Process) exit(status int32) error {
	p.exitMu.Lock()
	if !p.exitSet {
		p.exitSet = true
		p.status = status
	}
	p.exitMu.Unlock()
	return errExit
}

// Signal sends sig to the process.
func (p *Process) Signal(sig int32) {
	if sig == sigKill {
		p.cancel()
		return
	}
	p.sigMu.Lock()
	if p.sigPending != nil {
		atomic.OrUint64(p.sigPending, 1<<sig)
	} else {
		p.early |= 1 << sig
	}
	p.sigMu.Unlock()
	select {
	case p.sigWake <- struct{}{}:
	default:
	}
}

// stopSignals detaches the guest's signal control block, so signals no
// longer touch guest memory. Must happen before the memory is freed.
func (p *Process) stopSignals() {
	p.sigMu.Lock()
	p.sigPending = nil
	p.sigMu.Unlock()
}

// registerSignals attaches the guest's signal control block.
func (p *Process) registerSignals(addr uint32) int32 {
	pending, ok := p.mem.Uint64Ptr(addr)
	if !ok {
		return -abi.EFAULT
	}
	if _, ok := p.mem.Slice(addr, abi.SizeofSigblock); !ok {
		return -abi.EFAULT
	}
	p.sigMu.Lock()
	p.sigPending = pending
	atomic.OrUint64(pending, p.early)
	p.early = 0
	p.sigMu.Unlock()
	return 0
}

// interrupted reports whether a signal that should interrupt a blocking
// call is pending: one that is neither blocked nor ignored.
func (p *Process) interrupted() bool {
	p.sigMu.Lock()
	defer p.sigMu.Unlock()
	if p.sigPending == nil {
		return p.early != 0
	}
	pending := atomic.LoadUint64(p.sigPending)
	blocked := atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(p.sigPending), 8)))
	ignored := atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(p.sigPending), 16)))
	return pending&^blocked&^ignored != 0
}

type wakeReason int

const (
	woken wakeReason = iota
	interrupted
	timedOut
	killed
)

// block waits until one of chs is closed (nil channels never are), the
// deadline passes (zero means never), a signal interrupts the wait, or the
// process is killed.
func (p *Process) block(deadline time.Time, chs ...<-chan struct{}) wakeReason {
	var timer *time.Timer
	var timerC <-chan time.Time
	if !deadline.IsZero() {
		timer = time.NewTimer(time.Until(deadline))
		defer timer.Stop()
		timerC = timer.C
	}
	cases := make([]reflect.SelectCase, 0, len(chs)+3)
	cases = append(cases,
		reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(p.ctx.Done())},
		reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(p.sigWake)},
		reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timerC)},
	)
	for _, ch := range chs {
		cases = append(cases, reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ch)})
	}
	for {
		if p.ctx.Err() != nil {
			return killed
		}
		if p.interrupted() {
			return interrupted
		}
		i, _, _ := reflect.Select(cases)
		switch i {
		case 0:
			return killed
		case 1:
			continue // re-check whether the signal interrupts us
		case 2:
			return timedOut
		default:
			return woken
		}
	}
}

// spawn starts a child running argv with environment env, inheriting our
// descriptors.
func (p *Process) spawn(argv []string, env [][2]string, cwd string) int32 {
	child := p.k.newProcess(p, argv, env)
	if child == nil {
		return -abi.EAGAIN
	}
	child.cwd = cwd
	child.fds = p.fds.cloneForExec()
	p.childMu.Lock()
	p.children[child.pid] = child
	p.childMu.Unlock()
	go child.run()
	return child.pid
}

// waitpid reaps an exited child. pid -1 means any child.
func (p *Process) waitpid(pid int32, options int32) (int32, int32, int32) {
	for {
		ch := p.childEvts.wait()
		p.childMu.Lock()
		found := false
		for cpid, c := range p.children {
			if pid != -1 && cpid != pid {
				continue
			}
			found = true
			select {
			case <-c.done:
				delete(p.children, cpid)
				p.childMu.Unlock()
				p.k.forget(c)
				return cpid, c.status, 0
			default:
			}
		}
		p.childMu.Unlock()
		if !found {
			return 0, 0, -abi.ECHILD
		}
		if options&abi.WNOHANG != 0 {
			return 0, 0, 0
		}
		switch p.block(time.Time{}, ch) {
		case interrupted:
			return 0, 0, -abi.EINTR
		case killed:
			return 0, 0, -abi.EINTR
		}
	}
}

// kill implements kill(2). Every process is its own process group, so a
// negative pid addresses the process -pid.
func (p *Process) kill(pid, sig int32) int32 {
	if pid < 0 {
		pid = -pid
	}
	if pid == 0 {
		pid = p.pid
	}
	if sig < 0 || sig >= abi.NSIG {
		return -abi.EINVAL
	}
	target := p.k.lookup(pid)
	if target == nil {
		return p.k.killForeign(pid, sig)
	}
	if sig != 0 {
		target.Signal(sig)
	}
	return 0
}

// alarmState implements the ITIMER_REAL timer.
type alarmState struct {
	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time
	interval time.Duration
}

func (a *alarmState) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
	}
}

// setAlarm arms (or with value 0 disarms) the timer and returns the time
// that was left on the previous one.
func (p *Process) setAlarm(value, interval time.Duration) time.Duration {
	a := &p.alarm
	a.mu.Lock()
	defer a.mu.Unlock()
	var remaining time.Duration
	if a.timer != nil {
		a.timer.Stop()
		a.timer = nil
		if r := time.Until(a.deadline); r > 0 {
			remaining = r
		}
	}
	if value <= 0 {
		return remaining
	}
	a.interval = interval
	a.deadline = time.Now().Add(value)
	var fire func()
	fire = func() {
		a.mu.Lock()
		if a.interval > 0 && a.timer != nil {
			a.deadline = time.Now().Add(a.interval)
			a.timer = time.AfterFunc(a.interval, fire)
		} else {
			a.timer = nil
		}
		a.mu.Unlock()
		p.Signal(abi.SIGALRM)
	}
	a.timer = time.AfterFunc(value, fire)
	return remaining
}
