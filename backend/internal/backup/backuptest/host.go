// ---
// relationships:
//   references: [menagerie]
// ---

// Package backuptest provides an isolated Git host for tests: an SSH server
// and an SSH agent in the test process, with keys generated per test.
// Only tests import it.
package backuptest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// AgentSocketName mirrors the Wyrwood agent socket's name.
const AgentSocketName = "ssh-agent.sock"

// Host is an isolated Git host: an in-process SSH server on the
// loopback address that runs git-receive-pack for repositories under its
// folder, and an in-process SSH agent on a Unix socket holding a key the
// server accepts. Every key is generated for the test; nothing is read
// from the host's SSH configuration. The Git subprocess runs only on the
// fixture's server side, as a real Git host would.
type Host struct {
	// Dir holds the host's repositories.
	Dir string
	// Addr is the SSH server's loopback address.
	Addr string
	// AgentPath is the agent's Unix socket.
	AgentPath string
	// KnownHosts is a known_hosts file naming the server's key.
	KnownHosts string
	// HostKey is the server's public host key.
	HostKey ssh.PublicKey

	mu       sync.Mutex
	commands []string
	sessions sync.WaitGroup
}

// New starts a Host for the test.
func New(t *testing.T) *Host {
	t.Helper()
	if _, err := exec.LookPath("git-receive-pack"); err != nil {
		if _, err := exec.LookPath("git"); err != nil {
			t.Fatal("the SSH fixture's server needs git on PATH")
		}
	}
	f := &Host{Dir: t.TempDir()}

	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	f.HostKey = hostSigner.PublicKey()

	_, userPriv, _ := ed25519.GenerateKey(rand.Reader)
	userSigner, _ := ssh.NewSignerFromKey(userPriv)
	authorized := userSigner.PublicKey().Marshal()

	config := &ssh.ServerConfig{
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if meta.User() == "git" && string(key.Marshal()) == string(authorized) {
				return nil, nil
			}
			return nil, errors.New("key not authorized")
		},
	}
	config.AddHostKey(hostSigner)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Cleanups run last first: this one, registered after Dir, stops the
	// server and waits for its Git processes before Dir is removed.
	t.Cleanup(func() {
		_ = l.Close()
		f.sessions.Wait()
	})
	f.Addr = l.Addr().String()
	go f.serve(l, config)

	keyring := agent.NewKeyring()
	if err := keyring.Add(agent.AddedKey{PrivateKey: userPriv}); err != nil {
		t.Fatal(err)
	}
	f.AgentPath = f.ServeAgent(t, keyring)

	f.KnownHosts = filepath.Join(f.Dir, "known_hosts")
	f.WriteKnownHosts(t, f.KnownHosts, f.HostKey)
	return f
}

// ServeAgent serves keyring on a new Unix socket and returns its path.
func (f *Host) ServeAgent(t *testing.T, keyring agent.Agent) string {
	t.Helper()
	sock, err := os.MkdirTemp("", "agent")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })
	path := filepath.Join(sock, AgentSocketName)
	al, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = al.Close() })
	go func() {
		for {
			c, err := al.Accept()
			if err != nil {
				return
			}
			go func() { _ = agent.ServeAgent(keyring, c); _ = c.Close() }()
		}
	}()
	return path
}

// WriteKnownHosts writes a known_hosts file that names key for the server.
func (f *Host) WriteKnownHosts(t *testing.T, path string, key ssh.PublicKey) {
	t.Helper()
	line := knownhosts.Line([]string{knownhosts.Normalize(f.Addr)}, key)
	if err := os.WriteFile(path, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// URL returns the SSH address of a repository under Dir.
func (f *Host) URL(name string) string {
	return fmt.Sprintf("ssh://git@%s%s", f.Addr, filepath.Join(f.Dir, name))
}

// Commands lists the commands clients ran on the server.
func (f *Host) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *Host) serve(l net.Listener, config *ssh.ServerConfig) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			_, chans, reqs, err := ssh.NewServerConn(conn, config)
			if err != nil {
				_ = conn.Close()
				return
			}
			go ssh.DiscardRequests(reqs)
			for ch := range chans {
				if ch.ChannelType() != "session" {
					_ = ch.Reject(ssh.UnknownChannelType, "session only")
					continue
				}
				channel, requests, err := ch.Accept()
				if err != nil {
					continue
				}
				f.sessions.Add(1)
				go func() {
					defer f.sessions.Done()
					f.session(channel, requests)
				}()
			}
		}()
	}
}

func (f *Host) session(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		f.mu.Lock()
		f.commands = append(f.commands, payload.Command)
		f.mu.Unlock()
		program, repo, ok := strings.Cut(payload.Command, " ")
		repo = strings.Trim(repo, "'")
		if !ok || (program != "git-receive-pack" && program != "git-upload-pack") || !strings.HasPrefix(repo, f.Dir+"/") {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)
		cmd := exec.Command("git", strings.TrimPrefix(program, "git-"), repo)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = channel, channel, channel.Stderr()
		code := uint32(0)
		if err := cmd.Run(); err != nil {
			code = 1
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = uint32(exit.ExitCode())
			}
		}
		_ = channel.CloseWrite()
		status := make([]byte, 4)
		binary.BigEndian.PutUint32(status, code)
		_, _ = channel.SendRequest("exit-status", false, status)
		return
	}
}
