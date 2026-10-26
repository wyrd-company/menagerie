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

// Starter starts commit jobs. *commits.Commits satisfies it.
type Starter interface {
	Start(name, message string) (*jobs.Job, *commits.Outcome, error)
}

// Options configure the sockets.
type Options struct {
	// Folder is the socket folder, at the same path on the host and in
	// Menagerie's container.
	Folder string
	// MinInterval is the shortest time between two accepted commits of a
	// workspace. Zero accepts every one.
	MinInterval time.Duration
	// Client is the workspace-commit executable copied into each
	// workspace's bin folder.
	Client string
	// Commits starts the commit jobs.
	Commits Starter
	// Context is the base context of every request.
	Context context.Context
	// Now defaults to time.Now.
	Now func() time.Time
}

// Sockets owns every workspace's listener.
type Sockets struct {
	opts   Options
	client []byte
	uid    int

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
// a folder of this process's user, not a symbolic link; its mode is set
// to 0700.
func Open(opts Options) (*Sockets, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Context == nil {
		opts.Context = context.Background()
	}
	s := &Sockets{opts: opts, uid: os.Geteuid(), listeners: map[string]*listener{}, limits: newLimiter(opts.MinInterval, opts.Now)}
	if err := s.ownFolder(opts.Folder, 0o700, false); err != nil {
		return nil, fmt.Errorf("workspace-socket.folder: %w", err)
	}
	client, err := os.ReadFile(opts.Client)
	if err != nil {
		return nil, fmt.Errorf("workspace socket client %s: %w", opts.Client, err)
	}
	s.client = client
	return s, nil
}

// errNotOurs refuses an entry that is not this user's own folder.
var errNotOurs = errors.New("is not a folder of the services' user (a symbolic link is never followed)")

// ownFolder checks that path is a folder of s's user, creating it with
// mode when create is set and it is missing, and sets its mode.
func (s *Sockets) ownFolder(path string, mode os.FileMode, create bool) error {
	info, err := os.Lstat(path)
	if create && errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(path, mode); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != s.uid {
		return fmt.Errorf("%s %w", path, errNotOurs)
	}
	if info.Mode().Perm() != mode {
		if err := os.Chmod(path, mode); err != nil {
			return fmt.Errorf("%s: %w", path, err)
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
		return "", fmt.Errorf("workspace socket: %w", err)
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
		return "", fmt.Errorf("workspace socket: %w", err)
	}
	l, err := s.listen(name, path)
	if err != nil {
		return "", fmt.Errorf("workspace socket: %w", err)
	}
	s.listeners[name] = l
	return dir, nil
}

// copyClient writes the client into bin through a new file renamed over
// the old one, so a link there is replaced, never written through.
func (s *Sockets) copyClient(bin string) error {
	if err := s.ownFolder(bin, 0o755, true); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(bin, "."+ClientName+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(s.client); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(bin, ClientName))
}

// listen replaces a stale socket file at path and serves name on a new
// listener there.
func (s *Sockets) listen(name, path string) (*listener, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.IsDir() {
			return nil, fmt.Errorf("%s is a folder", path)
		}
		// Remove unlinks a symbolic link itself, never its target.
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	// A replaced listener must not unlink its successor's file.
	ln.SetUnlinkOnClose(false)
	info, err := os.Lstat(path)
	if err == nil && info.Mode()&os.ModeSocket == 0 {
		err = fmt.Errorf("%s is not the new socket", path)
	}
	if err == nil {
		err = os.Chmod(path, 0o666)
	}
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	server := &http.Server{
		Handler:     s.handler(name),
		BaseContext: func(net.Listener) context.Context { return s.opts.Context },
	}
	go func() { _ = server.Serve(ln) }()
	return &listener{path: path, info: info, server: server}, nil
}

// Close stops every listener and removes the socket files that are still
// its own.
func (s *Sockets) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, l := range s.listeners {
		_ = l.server.Close()
		if info, err := os.Lstat(l.path); err == nil && os.SameFile(info, l.info) {
			_ = os.Remove(l.path)
		}
		delete(s.listeners, name)
	}
}
