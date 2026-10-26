// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, workspace, configuration, workspace-fills-its-backup-branch]
// ---

// Package workspacesocket gives each workspace a Unix socket that its
// devcontainer reaches at /run/menagerie/workspace.sock. A socket serves
// one workspace: the workspace is bound to the listener when it is
// created, so a caller is identified by the socket it connects to and the
// API has no way to name another workspace.
//
// The socket folder holds one folder per workspace, mounted read-only into
// that workspace's container:
//
//	<folder>/                         0700, the services' user's
//	<folder>/<workspace>/             0755
//	<folder>/<workspace>/workspace.sock   0666
//	<folder>/<workspace>/bin/workspace-commit
//
// Only the services' user writes below the folder, and every entry is
// checked with Lstat before use: no symbolic link is followed, and a
// workspace folder that is not a folder of that user is refused.
//
// Connections are bounded: each workspace has at most
// maxWorkspaceConnections at once and the process maxConnections, and a
// connection over either bound is closed as soon as it is accepted. A
// request's header and body each have a deadline and an idle connection is
// closed, but nothing times the wait for the job: its job time limit
// governs it.
//
// Diagnostics name the entry and the rule it breaks, never a path or an
// operating system error's text: the folder's path is configuration.
package workspacesocket

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/commits"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// ClientName is the client's file name in a workspace's bin folder.
const ClientName = "workspace-commit"

// Bounds of the socket connections.
const (
	maxWorkspaceConnections = 32
	maxConnections          = 256
	headerTimeout           = 10 * time.Second
	bodyTimeout             = 10 * time.Second
	idleTimeout             = 60 * time.Second
	// shutdownGrace is how long Close waits for the replies to requests
	// that are waiting for a job.
	shutdownGrace = 5 * time.Second
)

// bounds are the limits a listener is created with.
type bounds struct {
	workspaceConnections, connections int
	header, body, idle                time.Duration
}

// Starter starts commit jobs. *commits.Commits satisfies it.
type Starter interface {
	Start(name, message string) (*jobs.Job, *commits.Outcome, error)
}

// Options configure the sockets.
type Options struct {
	// Folder is the socket folder, at the same path on the host and in
	// Menagerie's container. No part of its path may be a symbolic link,
	// so the host's bind source is the path itself.
	Folder string
	// WorkspaceRoot is the folder of the workspaces. The socket folder
	// must be neither inside it nor contain it once symbolic links
	// resolve. Empty skips the check.
	WorkspaceRoot string
	// MinInterval is the shortest time between two accepted commits of a
	// workspace. Zero accepts every one.
	MinInterval time.Duration
	// Client is the workspace-commit executable copied into each
	// workspace's bin folder.
	Client string
	// Commits starts the commit jobs.
	Commits Starter
	// Context is the base context of every request. When it ends, or
	// Close is called, a request waiting for its job is answered
	// shutting-down.
	Context context.Context
	// Now defaults to time.Now.
	Now func() time.Time

	// bounds replace the connection bounds when set, in tests.
	bounds *bounds
}

// Sockets owns every workspace's listener.
type Sockets struct {
	opts   Options
	client []byte
	uid    int
	bounds bounds
	conns  connections
	// ctx is every request's base context; cancel ends it on Close.
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	listeners map[string]*listener
	limits    *limiter
}

type listener struct {
	path   string
	info   os.FileInfo
	server *http.Server
}

// Open checks the socket folder and reads the client. The folder must be
// a folder of this process's user with no symbolic link in its path, and
// apart from the workspace root; its mode is set to 0700.
func Open(opts Options) (*Sockets, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	s := &Sockets{
		opts:      opts,
		uid:       os.Geteuid(),
		bounds:    bounds{maxWorkspaceConnections, maxConnections, headerTimeout, bodyTimeout, idleTimeout},
		conns:     connections{per: map[string]int{}},
		listeners: map[string]*listener{},
		limits:    newLimiter(opts.MinInterval, opts.Now),
	}
	if opts.bounds != nil {
		s.bounds = *opts.bounds
	}
	s.ctx, s.cancel = context.WithCancel(opts.Context)
	if err := s.ownFolder(opts.Folder, 0o700, false); err != nil {
		return nil, fmt.Errorf("workspace-socket.folder %w", err)
	}
	if err := apart(opts.Folder, opts.WorkspaceRoot); err != nil {
		return nil, fmt.Errorf("workspace-socket.folder %w", err)
	}
	client, err := os.ReadFile(opts.Client)
	if err != nil {
		return nil, errors.New("the workspace socket client beside the menagerie executable cannot be read")
	}
	s.client = client
	return s, nil
}

// Rules an entry below the socket folder breaks.
var (
	// errNotOurs refuses an entry that is not this user's own folder.
	errNotOurs  = errors.New("must be a folder of the services' user, not a symbolic link")
	errMissing  = errors.New("must exist")
	errUnusable = errors.New("cannot be read, created or given its mode")
	// errLinkInPath and errOverlap refuse a socket folder.
	errLinkInPath = errors.New("must have no symbolic link in its path")
	errOverlap    = errors.New("must be neither inside the workspace root nor contain it, with symbolic links resolved")
)

// apart checks that no part of folder's path is a symbolic link, and
// that folder is neither inside root nor contains it once root's links
// resolve. A link in the path would make the host's bind source another
// folder than the one checked here, and a socket folder inside a
// workspace would let that workspace's container reach other
// workspaces' sockets.
func apart(folder, root string) error {
	for p := folder; p != filepath.Dir(p); p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return errUnusable
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errLinkInPath
		}
	}
	if root == "" {
		return nil
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return errors.New("cannot be compared with the workspace root, which cannot be resolved")
	}
	if within(resolved, folder) || within(folder, resolved) {
		return errOverlap
	}
	return nil
}

// within reports whether path is root or inside it.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// ownFolder checks that path is a folder of s's user, creating it with
// mode when create is set and it is missing, and sets its mode.
func (s *Sockets) ownFolder(path string, mode os.FileMode, create bool) error {
	info, err := os.Lstat(path)
	if create && errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, mode); err != nil {
			return errUnusable
		}
		info, err = os.Lstat(path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return errMissing
	}
	if err != nil {
		return errUnusable
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != s.uid {
		return errNotOurs
	}
	if info.Mode().Perm() != mode {
		if err := os.Chmod(path, mode); err != nil {
			return errUnusable
		}
	}
	return nil
}

// Ensure returns the workspace's socket folder, creating its listener
// when there is none or its socket file is no longer the listener's. A
// stale socket file is replaced, and the client copied in again, when the
// listener is created.
func (s *Sockets) Ensure(name string) (string, error) {
	if err := workspace.ValidName(name); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := filepath.Join(s.opts.Folder, name)
	if err := s.ownFolder(dir, 0o755, true); err != nil {
		return "", fmt.Errorf("workspace socket of %s: its folder %w", name, err)
	}
	path := filepath.Join(dir, devcontainer.WorkspaceSocketName)
	if l, ok := s.listeners[name]; ok {
		if info, err := os.Lstat(path); err == nil && os.SameFile(info, l.info) {
			return dir, nil
		}
		_ = l.server.Close()
		delete(s.listeners, name)
	}
	if err := s.copyClient(filepath.Join(dir, "bin")); err != nil {
		return "", fmt.Errorf("workspace socket of %s: %w", name, err)
	}
	l, err := s.listen(name, path)
	if err != nil {
		return "", fmt.Errorf("workspace socket of %s: %w", name, err)
	}
	s.listeners[name] = l
	return dir, nil
}

// copyClient writes the client into bin through a new file renamed over
// the old one, so a link there is replaced, never written through.
func (s *Sockets) copyClient(bin string) error {
	if err := s.ownFolder(bin, 0o755, true); err != nil {
		return fmt.Errorf("its bin folder %w", err)
	}
	errCopy := errors.New("the client cannot be copied into its bin folder")
	tmp, err := os.CreateTemp(bin, "."+ClientName+"-*")
	if err != nil {
		return errCopy
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(s.client); err != nil {
		_ = tmp.Close()
		return errCopy
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return errCopy
	}
	if err := tmp.Close(); err != nil {
		return errCopy
	}
	if err := os.Rename(tmp.Name(), filepath.Join(bin, ClientName)); err != nil {
		return errCopy
	}
	return nil
}

// listen replaces a stale socket file at path and serves name on a new
// listener there.
func (s *Sockets) listen(name, path string) (*listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.IsDir() {
			return nil, errors.New("its socket file is a folder")
		}
		// Remove unlinks a symbolic link itself, never its target.
		if err := os.Remove(path); err != nil {
			return nil, errors.New("its stale socket file cannot be removed")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("its socket file cannot be read")
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errors.New("its socket cannot be listened on")
	}
	// A replaced listener must not unlink its successor's file.
	ln.SetUnlinkOnClose(false)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		err = errors.New("its socket file is not the new socket")
	} else if os.Chmod(path, 0o666) != nil {
		err = errors.New("its socket file cannot be given its mode")
	}
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	server := &http.Server{
		Handler:           s.handler(name),
		BaseContext:       func(net.Listener) context.Context { return s.ctx },
		ReadHeaderTimeout: s.bounds.header,
		IdleTimeout:       s.bounds.idle,
	}
	go func() { _ = server.Serve(&boundedListener{Listener: ln, name: name, s: s}) }()
	return &listener{path: path, info: info, server: server}, nil
}

// Close answers the requests waiting for a job shutting-down, stops every
// listener and removes the socket files that are still its own.
func (s *Sockets) Close() {
	// Under the limiter's lock, so no request is between its shutdown
	// check and its job's start.
	s.limits.mu.Lock()
	s.cancel()
	s.limits.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, done := context.WithTimeout(context.Background(), shutdownGrace)
	defer done()
	var wg sync.WaitGroup
	for _, l := range s.listeners {
		wg.Go(func() {
			_ = l.server.Shutdown(ctx)
			_ = l.server.Close()
		})
	}
	wg.Wait()
	for name, l := range s.listeners {
		if info, err := os.Lstat(l.path); err == nil && os.SameFile(info, l.info) {
			_ = os.Remove(l.path)
		}
		delete(s.listeners, name)
	}
}

// connections counts the open connections of each workspace and of the
// process.
type connections struct {
	mu    sync.Mutex
	total int
	per   map[string]int
}

// take counts a new connection of name, or reports false when it would
// pass either bound.
func (c *connections) take(name string, b bounds) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.total >= b.connections || c.per[name] >= b.workspaceConnections {
		return false
	}
	c.total++
	c.per[name]++
	return true
}

func (c *connections) give(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.total--
	if c.per[name]--; c.per[name] == 0 {
		delete(c.per, name)
	}
}

// boundedListener closes a connection over the bounds as soon as it is
// accepted.
type boundedListener struct {
	net.Listener
	name string
	s    *Sockets
}

func (l *boundedListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if l.s.conns.take(l.name, l.s.bounds) {
			return &countedConn{Conn: conn, release: sync.OnceFunc(func() { l.s.conns.give(l.name) })}, nil
		}
		_ = conn.Close()
	}
}

// countedConn gives its count back when it closes.
type countedConn struct {
	net.Conn
	release func()
}

func (c *countedConn) Close() error {
	err := c.Conn.Close()
	c.release()
	return err
}
