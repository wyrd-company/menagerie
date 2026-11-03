// ---
// relationships:
//   implements: workspace-api
//   references: [menagerie, t3-code-pairing-token]
// ---

// Package t3code mints T3 Code pairing tokens inside a workspace's running
// container, through Docker exec. The token leaves only as the return value:
// no error, log or file here holds what the container printed, because a
// failed command's output can hold a token.
package t3code

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
)

// Paths and arguments of the t3code-server devcontainer Feature.
const (
	// ConfigPath is the Feature's record of its service user and home.
	ConfigPath = "/usr/local/lib/t3code-server/config.env"
	// Executable is the Feature's t3 command.
	Executable = "/usr/local/bin/t3"
	// Label names the token in the T3 Code server's connections list.
	Label = "menagerie"
)

// maxOutput bounds what is read from the Feature's config and from the
// command's standard output. Both are a few hundred bytes.
const maxOutput = 64 << 10

var (
	// ErrNotInstalled says the container has no usable t3code-server Feature.
	ErrNotInstalled = errors.New("T3 Code is not installed in this workspace's container")
	// ErrNotRunning says the container stopped or was removed.
	ErrNotRunning = errors.New("this workspace's container is not running")
	// ErrFailed says the container answered but gave no token. Its text is
	// fixed: what the command printed is never part of it.
	ErrFailed = errors.New("T3 Code in this workspace's container did not issue a pairing token")
)

// PairingToken is a one-time T3 Code pairing token.
type PairingToken struct {
	Token     string `json:"token"`
	ExpiresAt string `json:"expiresAt"`
}

// Docker is the part of the Docker library a Minter uses.
// *client.Client satisfies it.
type Docker interface {
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
	CopyFromContainer(ctx context.Context, id, path string) (io.ReadCloser, container.PathStat, error)
	ContainerExecCreate(ctx context.Context, id string, options container.ExecOptions) (container.ExecCreateResponse, error)
	ContainerExecAttach(ctx context.Context, execID string, options container.ExecAttachOptions) (types.HijackedResponse, error)
	ContainerExecInspect(ctx context.Context, execID string) (container.ExecInspect, error)
}

// Minter mints pairing tokens over Docker exec.
type Minter struct {
	docker Docker
}

// NewMinter returns a Minter on docker.
func NewMinter(docker Docker) *Minter { return &Minter{docker: docker} }

// Mint issues a pairing token from the T3 Code server of the running
// container id, as the Feature's service user and with its data directory.
// The token keeps T3 Code's default scopes and lifetime. It returns
// ErrNotInstalled, ErrNotRunning, ErrFailed or the Docker library's error.
func (m *Minter) Mint(ctx context.Context, id string) (PairingToken, error) {
	service, err := m.readService(ctx, id)
	if err != nil {
		return PairingToken{}, err
	}
	output, code, err := m.exec(ctx, id, service)
	if err != nil {
		return PairingToken{}, err
	}
	switch code {
	case 0:
		return parseToken(output)
	case 126, 127:
		return PairingToken{}, ErrNotInstalled
	default:
		return PairingToken{}, fmt.Errorf("%w: its command exited with code %d", ErrFailed, code)
	}
}

type service struct {
	user string
	home string
}

func (m *Minter) readService(ctx context.Context, id string) (service, error) {
	archive, _, err := m.docker.CopyFromContainer(ctx, id, ConfigPath)
	if errdefs.IsNotFound(err) {
		// Both a missing file and a removed container answer 404.
		if _, err := m.docker.ContainerInspect(ctx, id); errdefs.IsNotFound(err) {
			return service{}, ErrNotRunning
		}
		return service{}, ErrNotInstalled
	}
	if err != nil {
		return service{}, fmt.Errorf("read %s: %w", ConfigPath, err)
	}
	defer archive.Close()
	entries := tar.NewReader(archive)
	header, err := entries.Next()
	if err != nil || header.Typeflag != tar.TypeReg {
		return service{}, ErrNotInstalled
	}
	return parseConfig(io.LimitReader(entries, maxOutput))
}

var (
	plainUser = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*$`)
	plainHome = regexp.MustCompile(`^/[A-Za-z0-9._/-]*$`)
)

// parseConfig reads the Feature's config without a shell. The Feature writes
// each value with bash's %q, which leaves a plain name unquoted; any other
// value is refused, since it never goes through a shell to be unquoted.
func parseConfig(r io.Reader) (service, error) {
	var s service
	lines := bufio.NewScanner(r)
	for lines.Scan() {
		key, value, _ := strings.Cut(lines.Text(), "=")
		switch key {
		case "T3CODE_SERVER_USER":
			s.user = value
		case "T3CODE_SERVER_HOME":
			s.home = value
		}
	}
	if lines.Err() != nil || !plainUser.MatchString(s.user) || !plainHome.MatchString(s.home) {
		return service{}, ErrNotInstalled
	}
	return s, nil
}

// exec runs the mint command and returns its standard output and exit code.
// It returns when ctx ends, whatever the command does. The Docker API cannot
// stop an exec, so a command that hangs stays in its container; a token it
// issues later has no reader and expires.
func (m *Minter) exec(ctx context.Context, id string, s service) ([]byte, int, error) {
	created, err := m.docker.ContainerExecCreate(ctx, id, container.ExecOptions{
		User:         s.user,
		Cmd:          command(s),
		AttachStdout: true,
		AttachStderr: true,
	})
	if errdefs.IsNotFound(err) || errdefs.IsConflict(err) {
		return nil, 0, ErrNotRunning
	}
	if err != nil {
		return nil, 0, fmt.Errorf("create exec: %w", err)
	}
	attached, err := m.docker.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if errdefs.IsNotFound(err) || errdefs.IsConflict(err) {
		return nil, 0, ErrNotRunning
	}
	if err != nil {
		return nil, 0, fmt.Errorf("attach exec: %w", err)
	}
	copied := make(chan error, 1)
	output := &limitedBuffer{limit: maxOutput}
	go func() {
		_, err := stdcopy.StdCopy(output, io.Discard, attached.Reader)
		copied <- err
	}()
	// Closing the stream ends the copy; its channel has room for the result.
	defer attached.Close()
	select {
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	case err := <-copied:
		// A cut stream or a command that prints without end gives no token.
		if err != nil {
			return nil, 0, ErrFailed
		}
	}
	info, err := m.docker.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		return nil, 0, fmt.Errorf("inspect exec: %w", err)
	}
	return output.Bytes(), info.ExitCode, nil
}

func command(s service) []string {
	return []string{Executable, "auth", "pairing", "create", "--base-dir", s.home + "/.t3", "--label", Label, "--json"}
}

// parseToken reads the command's --json output. A parser error can quote its
// input, so it never reaches the returned error.
func parseToken(output []byte) (PairingToken, error) {
	var issued struct {
		Credential string `json:"credential"`
		ExpiresAt  string `json:"expiresAt"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output), &issued); err != nil {
		return PairingToken{}, ErrFailed
	}
	if issued.Credential == "" {
		return PairingToken{}, ErrFailed
	}
	if _, err := time.Parse(time.RFC3339Nano, issued.ExpiresAt); err != nil {
		return PairingToken{}, ErrFailed
	}
	return PairingToken{Token: issued.Credential, ExpiresAt: issued.ExpiresAt}, nil
}

// limitedBuffer holds up to limit bytes and fails the write that passes it.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

var errOutputTooLong = errors.New("output too long")

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errOutputTooLong
	}
	return b.Buffer.Write(p)
}
