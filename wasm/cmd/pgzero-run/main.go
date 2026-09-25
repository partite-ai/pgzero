// Command pgzero-run runs the wasm Postgres programs (postgres, initdb, ...)
// from a development build, with host directories mounted: each process a
// separate wasm instance, as in the library. The build and the regression
// tests use it; applications use package pgzero instead.
//
//	pgzero-run [flags] -- program [args...]
//
// For example, after `make pg`:
//
//	build/pgzero-run -data ./pgdata -- initdb -D /data -U postgres
//	build/pgzero-run -data ./pgdata -- postgres -D /data
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"syscall"
	"time"

	"github.com/partite-ai/pgzero/internal/abi"
	"github.com/partite-ai/pgzero/internal/kernel"
)

// The guest's view of the filesystem.
const (
	guestPrefix = "/usr/local/pgsql" // Postgres's install prefix
	guestData   = "/data"
	guestTmp    = "/tmp"
)

type listFlag []string

func (l *listFlag) String() string     { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error { *l = append(*l, v); return nil }

func main() {
	os.Exit(run())
}

func run() int {
	var (
		programs = flag.String("programs", "build/bin", "directory of program components (<name>.wasm)")
		install  = flag.String("install", "build/install"+guestPrefix, "Postgres install tree, mounted at "+guestPrefix)
		data     = flag.String("data", "", "host directory mounted at "+guestData)
		tmp      = flag.String("tmp", "", "host directory mounted at "+guestTmp+" (default: a new temporary directory)")
		debug    = flag.Bool("debug", false, "trace kernel activity")
		cache    = flag.String("cache", defaultCacheDir(), "compiled code cache directory (empty to disable)")
		cpuprof  = flag.String("cpuprofile", "", "write a CPU profile of the host to this file")
		trc      = flag.String("trace", "", "write a Go execution trace of the host to this file")
		dbgInfo  = flag.Bool("debuginfo", false, "show source locations in crash stack traces (slow)")
		mounts   listFlag
		env      listFlag
	)
	flag.Var(&mounts, "mount", "additional mount, guest=host (repeatable)")
	flag.Var(&env, "env", "environment variable, NAME=value (repeatable)")
	flag.Parse()
	argv := flag.Args()
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "usage: pgzero-run [flags] -- program [args...]")
		flag.PrintDefaults()
		return 2
	}

	if *cpuprof != "" {
		f, err := os.Create(*cpuprof)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pgzero-run: %v\n", err)
			return 1
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	if *trc != "" {
		f, err := os.Create(*trc)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pgzero-run: %v\n", err)
			return 1
		}
		trace.Start(f)
		defer trace.Stop()
	}

	progs, err := loadPrograms(*programs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgzero-run: %v\n", err)
		return 1
	}

	ms := []kernel.Mount{{GuestPath: guestPrefix, HostPath: *install}}
	if *data != "" {
		if err := os.MkdirAll(*data, 0o700); err != nil {
			fmt.Fprintf(os.Stderr, "pgzero-run: %v\n", err)
			return 1
		}
		ms = append(ms, kernel.Mount{GuestPath: guestData, HostPath: *data})
	}
	tmpDir := *tmp
	if tmpDir == "" {
		// Keep it short: Unix socket paths are limited to ~104 bytes.
		if tmpDir, err = os.MkdirTemp("/tmp", "pgzero"); err != nil {
			fmt.Fprintf(os.Stderr, "pgzero-run: %v\n", err)
			return 1
		}
		defer os.RemoveAll(tmpDir)
	}
	ms = append(ms, kernel.Mount{GuestPath: guestTmp, HostPath: tmpDir})
	for _, m := range mounts {
		g, h, ok := strings.Cut(m, "=")
		if !ok {
			fmt.Fprintf(os.Stderr, "pgzero-run: bad -mount %q\n", m)
			return 2
		}
		ms = append(ms, kernel.Mount{GuestPath: g, HostPath: h})
	}
	for i := range ms {
		if ms[i].HostPath, err = filepath.Abs(ms[i].HostPath); err != nil {
			fmt.Fprintf(os.Stderr, "pgzero-run: %v\n", err)
			return 1
		}
	}

	environ := [][2]string{
		{"PATH", guestPrefix + "/bin"},
		{"HOME", "/"},
		{"USER", "postgres"},
		{"LANG", "C"},
		{"TZ", "UTC"},
	}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		environ = append(environ, [2]string{k, v})
	}

	ctx := context.Background()
	rt, err := kernel.NewRuntime(ctx, kernel.RuntimeConfig{
		Programs:  progs,
		CacheDir:  *cache,
		DebugInfo: *dbgInfo,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgzero-run: %v\n", err)
		return 1
	}
	defer rt.Close(ctx)

	cfg := kernel.Config{
		Mounts: ms,
		Env:    environ,
		Cwd:    "/",
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
	if *debug {
		cfg.Debugf = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "%s pgzero: "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
		}
	}
	k := kernel.New(rt, cfg)
	defer k.Close()

	p, err := k.Start(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgzero-run: %v\n", err)
		return 1
	}

	// Forward interrupts, as a terminal would to the foreground process.
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			switch s {
			case os.Interrupt:
				p.Signal(abi.SIGINT)
			case syscall.SIGTERM:
				p.Signal(abi.SIGTERM)
			case syscall.SIGQUIT:
				p.Signal(abi.SIGQUIT)
			case syscall.SIGHUP:
				p.Signal(abi.SIGHUP)
			}
		}
	}()

	status := p.Wait()
	switch {
	case status&0x7f == 0:
		return int(status>>8) & 0xff
	default:
		fmt.Fprintf(os.Stderr, "pgzero-run: %s terminated by signal %d\n", argv[0], status&0x7f)
		return 128 + int(status&0x7f)
	}
}

func loadPrograms(dir string) (map[string][]byte, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.wasm"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no program components in %s", dir)
	}
	progs := map[string][]byte{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		progs[strings.TrimSuffix(filepath.Base(f), ".wasm")] = b
	}
	return progs, nil
}

func defaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "pgzero")
}
