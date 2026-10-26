// ---
// relationships:
//   implements: menagerie
//   references: [backup-force-push-overwrites-branch]
// ---

package backup

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/wyrd-company/menagerie/backend/internal/backup/gitclient"
)

// AgentSocketName is the SSH agent socket's name in the Wyrwood consumer
// directory.
const AgentSocketName = "ssh-agent.sock"

// remoteName names the in-memory remote of a push. It is never written to
// the workspace repository's configuration.
const remoteName = "menagerie-backup"

// Auth returns the transport credentials of one push to url, and a function
// that releases them. Nil credentials push without any.
type Auth func(ctx context.Context, url string) (transport.AuthMethod, func(), error)

// Remote is the workspace repository that holds one backup branch per
// workspace.
type Remote struct {
	URL  string
	Auth Auth
}

// Push force-pushes the workspace branch to the branch of the same name in
// the workspace repository, replacing whatever it held. It reads the
// remote's advertised refs, as every push does, and fetches nothing.
// Pushed is false when the remote branch already held the local commit.
func (r *Remote) Push(ctx context.Context, folder, workspace string, progress io.Writer) (pushed bool, err error) {
	repo, branch, err := open(folder, workspace)
	if err != nil {
		return false, err
	}
	if _, err := repo.Reference(branch, false); err != nil {
		return false, fmt.Errorf("workspace branch %s has no commit: %w", branch.Short(), err)
	}
	s, err := r.open(ctx, progress)
	if err != nil {
		return false, err
	}
	defer s.close()
	ctx, auth, progress := s.ctx, s.auth, s.progress
	remote := git.NewRemote(repo.Storer, &config.RemoteConfig{Name: remoteName, URLs: []string{r.URL}})
	err = remote.PushContext(ctx, &git.PushOptions{
		RemoteName: remoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec("+" + branch.String() + ":" + branch.String())},
		Auth:       auth,
		Progress:   progress,
		Force:      true,
	})
	if errors.Is(err, git.NoErrAlreadyUpToDate) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("push %s: %w", branch.Short(), transportFailure(ctx, r.URL, err))
	}
	return true, nil
}

// Tokens supplies tokens for HTTPS repository URLs.
type Tokens interface {
	// Serves reports whether url is a URL the source has tokens for.
	Serves(url string) bool
	// Token returns the token of url and the user name that goes with it.
	// Its error names no token and no URL.
	Token(ctx context.Context, url string) (username, token string, err error)
}

// TokenAuth authenticates pushes and fetches to a URL tokens serves with
// HTTP basic authentication, the token as the password; every other URL
// gets the credentials of otherwise. A token is only ever sent to a URL
// tokens serves, and a push or fetch with HTTP credentials follows no
// redirect.
func TokenAuth(tokens Tokens, otherwise Auth) Auth {
	return func(ctx context.Context, url string) (transport.AuthMethod, func(), error) {
		if !tokens.Serves(url) {
			if otherwise == nil {
				return nil, func() {}, nil
			}
			return otherwise(ctx, url)
		}
		username, token, err := tokens.Token(ctx, url)
		if err != nil {
			return nil, nil, &CredentialsError{cause: err}
		}
		return &githttp.BasicAuth{Username: username, Password: token}, func() {}, nil
	}
}

// AgentAuth authenticates SSH pushes with the SSH agent listening on
// socket, such as Wyrwood's ssh-agent.sock. The agent holds the key; no key
// or token is read or stored. The socket is dialed for each push. Host keys
// are verified against knownHosts files, or when none are given, against
// the default known hosts files (SSH_KNOWN_HOSTS, ~/.ssh/known_hosts and
// /etc/ssh/ssh_known_hosts). A URL that is not SSH pushes without
// credentials.
func AgentAuth(socket string, knownHosts ...string) Auth {
	return func(ctx context.Context, url string) (transport.AuthMethod, func(), error) {
		endpoint, err := transport.NewEndpoint(url)
		if err != nil {
			return nil, nil, fmt.Errorf("workspace repository address: %w", err)
		}
		if endpoint.Protocol != "ssh" {
			return nil, func() {}, nil
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh agent: %w", err)
		}
		user := endpoint.User
		if user == "" {
			user = "git"
		}
		auth := &gitssh.PublicKeysCallback{User: user, Callback: agent.NewClient(conn).Signers}
		if len(knownHosts) > 0 {
			callback, err := gitssh.NewKnownHostsCallback(knownHosts...)
			if err != nil {
				_ = conn.Close()
				return nil, nil, fmt.Errorf("known hosts: %w", err)
			}
			auth.HostKeyCallback = callback
		}
		return auth, func() { _ = conn.Close() }, nil
	}
}

// CredentialsError reports that tokens had no credentials for the
// workspace repository. Its text is the Tokens error's, which names no
// token and no URL.
type CredentialsError struct{ cause error }

func (e *CredentialsError) Error() string {
	return "workspace repository credentials: " + e.cause.Error()
}

// Unwrap returns the Tokens error.
func (e *CredentialsError) Unwrap() error { return e.cause }

// transfer is what one push or fetch uses: its context, credentials and
// progress writer.
type transfer struct {
	ctx      context.Context
	auth     transport.AuthMethod
	progress io.WriteCloser
	release  func()
}

// open gets the transfer's credentials and wraps progress so that the
// remote's text names neither the repository URL nor a token. A transfer
// with HTTP credentials follows no redirect, so they never reach a
// redirect's target.
func (r *Remote) open(ctx context.Context, progress io.Writer) (*transfer, error) {
	t := &transfer{ctx: ctx, release: func() {}}
	if r.Auth != nil {
		auth, release, err := r.Auth(ctx, r.URL)
		if err != nil {
			return nil, credentialFailure(ctx, r.URL, err)
		}
		t.auth, t.release = auth, release
	}
	values := []string{r.URL}
	if _, ok := t.auth.(githttp.AuthMethod); ok {
		t.ctx = gitclient.WithoutRedirects(ctx)
	}
	if basic, ok := t.auth.(*githttp.BasicAuth); ok {
		values = append(values, basic.Password, base64.StdEncoding.EncodeToString([]byte(basic.Username+":"+basic.Password)))
	}
	t.progress = newRedactor(progress, values...)
	return t, nil
}

// close writes the progress text left and releases the credentials.
func (t *transfer) close() {
	_ = t.progress.Close()
	t.release()
}
