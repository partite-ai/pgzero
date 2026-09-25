// Package pgzero runs PostgreSQL inside a Go program: a real Postgres
// server, compiled to WebAssembly, with each of its processes running as a
// separate wasm instance in this process. There is nothing to install.
//
// A Server keeps its database cluster in memory (the default) or in a host
// directory, and clients connect to it through an in-memory connection
// (see Server.Dial) - no network port is involved unless asked for:
//
//	srv, err := pgzero.Start(ctx, pgzero.Config{})
//	if err != nil { ... }
//	defer srv.Close()
//
//	cfg, _ := pgx.ParseConfig(srv.ConnString("postgres"))
//	cfg.DialFunc = srv.Dial
//	conn, err := pgx.ConnectConfig(ctx, cfg)
//
// Package pgzerox does this for pgx and pgxpool.
package pgzero

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/partite-ai/wacogo/wasi/filesystem/preopens"

	"github.com/partite-ai/pgzero/internal/abi"
	"github.com/partite-ai/pgzero/internal/assets"
	"github.com/partite-ai/pgzero/internal/kernel"
	"github.com/partite-ai/pgzero/internal/memfs"
)

// Where things live in the guest.
const (
	guestPrefix = "/usr/local/pgsql"
	guestData   = "/data"
	socketDir   = "/tmp"
	defaultPort = 5432
	superuser   = "postgres"
)

// Config configures a Server. The zero value is an in-memory server.
type Config struct {
	// DataDir is a host directory holding the database cluster, which then
	// persists across servers. A missing or empty directory is initialized
	// with a fresh cluster. If DataDir is empty, the cluster lives in
	// memory and is discarded by Close.
	DataDir string

	// Settings are server configuration parameters, as in postgresql.conf,
	// e.g. {"fsync": "off", "work_mem": "64MB"}.
	Settings map[string]string

	// TCP makes the server also accept connections on 127.0.0.1 (and ::1),
	// on Port, or a free port if Port is 0; see Server.TCPAddr. Without it,
	// the server is only reachable through Server.Dial.
	TCP  bool
	Port int

	// Logs receives the server log. If nil, it is discarded (but the tail
	// is included in startup errors).
	Logs io.Writer
}

// Server is a running Postgres server.
type Server struct {
	k          *kernel.Kernel
	postmaster *kernel.Process
	port       int
	tcp        bool
	logs       *logTail

	closeOnce sync.Once
	closeErr  error
}

// shared is what all servers in the process share: the compiled programs
// and the unpacked install tree and template cluster.
var shared struct {
	once     sync.Once
	err      error
	rt       *kernel.Runtime
	install  *memfs.FS
	template *memfs.FS
}

func setup() error {
	shared.once.Do(func() {
		shared.err = func() error {
			postgres, err := gunzip(assets.Postgres)
			if err != nil {
				return err
			}
			initdb, err := gunzip(assets.Initdb)
			if err != nil {
				return err
			}
			var cacheDir string
			if dir, err := os.UserCacheDir(); err == nil {
				cacheDir = filepath.Join(dir, "pgzero")
			}
			rt, err := kernel.NewRuntime(context.Background(), kernel.RuntimeConfig{
				Programs: map[string][]byte{"postgres": postgres, "initdb": initdb},
				CacheDir: cacheDir,
			})
			if err != nil {
				return err
			}
			if err := rt.Precompile("postgres"); err != nil {
				return err
			}
			install := memfs.New()
			if err := addTarGz(install, assets.Install); err != nil {
				return fmt.Errorf("install tree: %w", err)
			}
			template := memfs.New()
			if err := addTarGz(template, assets.Template); err != nil {
				return fmt.Errorf("template cluster: %w", err)
			}
			shared.rt, shared.install, shared.template = rt, install, template
			return nil
		}()
	})
	return shared.err
}

func gunzip(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func addTarGz(f *memfs.FS, b []byte) error {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return err
	}
	return f.AddTar(r)
}

// Start starts a server and waits until it accepts connections.
func Start(ctx context.Context, cfg Config) (*Server, error) {
	if err := setup(); err != nil {
		return nil, fmt.Errorf("pgzero: %w", err)
	}

	data := kernel.Mount{GuestPath: guestData}
	if cfg.DataDir == "" {
		data.FS = shared.template.Clone()
	} else {
		if err := initDataDir(cfg.DataDir); err != nil {
			return nil, fmt.Errorf("pgzero: %w", err)
		}
		dir, err := filepath.Abs(cfg.DataDir)
		if err != nil {
			return nil, fmt.Errorf("pgzero: %w", err)
		}
		data.HostPath = dir
	}

	port := cfg.Port
	if port == 0 {
		port = defaultPort
		if cfg.TCP {
			p, err := freePort()
			if err != nil {
				return nil, fmt.Errorf("pgzero: %w", err)
			}
			port = p
		}
	}

	logs := &logTail{w: cfg.Logs}
	k := kernel.New(shared.rt, kernel.Config{
		Mounts: []kernel.Mount{
			{GuestPath: guestPrefix, FS: preopens.ImmutableFS{FS: shared.install}},
			data,
			// Unix sockets live in the kernel; only their lock files are here.
			{GuestPath: socketDir, FS: memfs.New()},
		},
		Env: [][2]string{
			{"PATH", guestPrefix + "/bin"},
			{"HOME", "/"},
			{"USER", superuser},
			{"LANG", "C"},
			{"TZ", "UTC"},
		},
		Stdout: logs,
		Stderr: logs,
	})

	listen := ""
	if cfg.TCP {
		listen = "localhost"
	}
	argv := []string{"postgres", "-D", guestData, "-p", strconv.Itoa(port),
		"-c", "listen_addresses=" + listen,
		"-c", "unix_socket_directories=" + socketDir}
	names := make([]string, 0, len(cfg.Settings))
	for name := range cfg.Settings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		argv = append(argv, "-c", name+"="+cfg.Settings[name])
	}

	pm, err := k.Start(argv)
	if err != nil {
		k.Close()
		return nil, fmt.Errorf("pgzero: %w", err)
	}
	s := &Server{k: k, postmaster: pm, port: port, tcp: cfg.TCP, logs: logs}
	if err := s.waitReady(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("pgzero: %w", err)
	}
	return s, nil
}

// initDataDir makes dir hold a cluster, copying in the template if it is
// missing or empty.
func initDataDir(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "PG_VERSION")); err == nil {
		return nil
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("%s is not empty and not a database cluster", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	r, err := gzip.NewReader(bytes.NewReader(assets.Template))
	if err != nil {
		return err
	}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(h.Name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(filepath.Separator)) {
			return fmt.Errorf("template: bad path %q", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitReady waits until the server accepts connections.
func (s *Server) waitReady(ctx context.Context) error {
	for {
		ready, err := s.probe(ctx)
		if ready {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-s.postmaster.Done():
			return fmt.Errorf("postgres exited during startup:\n%s", s.logs.tail())
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// probe sends a startup message and reports whether the server accepted
// it. "The database system is starting up" means not ready yet.
func (s *Server) probe(ctx context.Context) (bool, error) {
	c, err := s.Dial(ctx, "unix", "")
	if err != nil {
		return false, nil // not listening yet
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))

	params := "user\x00" + superuser + "\x00database\x00postgres\x00\x00"
	msg := make([]byte, 8, 8+len(params))
	binary.BigEndian.PutUint32(msg, uint32(8+len(params)))
	binary.BigEndian.PutUint32(msg[4:], 3<<16) // protocol 3.0
	msg = append(msg, params...)
	if _, err := c.Write(msg); err != nil {
		return false, nil
	}
	var hdr [5]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return false, nil
	}
	body := make([]byte, max(int(binary.BigEndian.Uint32(hdr[1:]))-4, 0))
	if _, err := io.ReadFull(c, body); err != nil {
		return false, nil
	}
	switch hdr[0] {
	case 'R':
		c.Write([]byte{'X', 0, 0, 0, 4}) // Terminate
		return true, nil
	case 'E':
		fields := map[byte]string{}
		for _, f := range bytes.Split(body, []byte{0}) {
			if len(f) > 1 {
				fields[f[0]] = string(f[1:])
			}
		}
		if fields['C'] == "57P03" { // cannot_connect_now: starting up
			return false, nil
		}
		return false, fmt.Errorf("server refused connection: %s", fields['M'])
	}
	return false, fmt.Errorf("unexpected startup response %q", hdr[0])
}

// Dial connects to the server, ignoring network and addr: the connection
// is an in-memory pipe to the server's Unix socket. It fits
// pgconn.Config.DialFunc (and is used by pgx for cancel requests too).
func (s *Server) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return s.k.DialUnix(ctx, fmt.Sprintf("%s/.s.PGSQL.%d", socketDir, s.port))
}

// ConnString returns a libpq-style connection string for database, as
// superuser "postgres". It names the server's Unix socket, which only
// Server.Dial can reach; with Config.TCP, TCPAddr works with any client.
func (s *Server) ConnString(database string) string {
	return fmt.Sprintf("host=%s port=%d user=%s dbname=%s sslmode=disable",
		socketDir, s.port, superuser, quoteConnValue(database))
}

func quoteConnValue(v string) string {
	if v != "" && !strings.ContainsAny(v, " '\\") {
		return v
	}
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(v) + "'"
}

// TCPAddr returns the address the server listens on for TCP connections,
// or "" without Config.TCP.
func (s *Server) TCPAddr() string {
	if !s.tcp {
		return ""
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(s.port))
}

// Close shuts the server down (fast shutdown: sessions are terminated,
// then a checkpoint is written) and releases it. An in-memory cluster is
// discarded.
func (s *Server) Close() error {
	s.closeOnce.Do(func() {
		s.postmaster.Signal(abi.SIGINT)
		select {
		case <-s.postmaster.Done():
			if status := s.postmaster.Wait(); status != 0 {
				s.closeErr = fmt.Errorf("pgzero: postgres exited with status %#x:\n%s", status, s.logs.tail())
			}
		case <-time.After(time.Minute):
			s.closeErr = errors.New("pgzero: postgres did not shut down; killed")
		}
		s.k.Close()
	})
	return s.closeErr
}

// logTail forwards the server log and keeps its last few KB for errors.
type logTail struct {
	w   io.Writer
	mu  sync.Mutex
	buf []byte
}

const logTailSize = 8 << 10

func (l *logTail) Write(p []byte) (int, error) {
	l.mu.Lock()
	l.buf = append(l.buf, p...)
	if len(l.buf) > 2*logTailSize {
		l.buf = append([]byte(nil), l.buf[len(l.buf)-logTailSize:]...)
	}
	l.mu.Unlock()
	if l.w != nil {
		return l.w.Write(p)
	}
	return len(p), nil
}

func (l *logTail) tail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buf
	if len(b) > logTailSize {
		b = b[len(b)-logTailSize:]
	}
	return string(b)
}
