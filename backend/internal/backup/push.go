// ---
// relationships:
//   implements: menagerie
//   references: [backup-force-push-overwrites-branch]
// ---

package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
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
// Pushed is false when the remote branch already held the local commit. The
// first push of a create, from any job, first copies a deleted workspace's
// branch of that name, and ends the create's mark only when it has landed.
func (r *Remote) Push(ctx context.Context, folder, workspace string, progress io.Writer) (pushed bool, err error) {
	repo, branch, err := open(folder, workspace)
	if err != nil {
		return false, err
	}
	if _, err := repo.Reference(branch, false); err != nil {
		return false, fmt.Errorf("workspace branch %s has no commit: %w", branch.Short(), err)
	}
	unfinished := createUnfinished(folder)
	if unfinished {
		archived, err := r.Archive(ctx, folder, workspace, time.Now(), progress)
		if err != nil {
			return false, err
		}
		if archived != "" && progress != nil {
			fmt.Fprintf(progress, "kept the earlier backup of branch %s as %s\n", branch.Short(), archived)
		}
	}
	defer func() {
		if err == nil && unfinished {
			err = os.Remove(markerPath(folder))
		}
	}()
	return r.push(ctx, repo, branch, true, progress)
}

// Extend pushes the workspace branch to the branch of the same name in the
// repository without force: the push lands only when the repository has no
// such branch or its branch is the local commit or one of its ancestors,
// as the repository decides when it takes the push, so a branch another
// writer moved since it was read is never overwritten. Pushed is false
// when the branch already held the local commit. It archives nothing.
func (r *Remote) Extend(ctx context.Context, folder, workspace string, progress io.Writer) (pushed bool, err error) {
	repo, branch, err := open(folder, workspace)
	if err != nil {
		return false, err
	}
	if _, err := repo.Reference(branch, false); err != nil {
		return false, fmt.Errorf("workspace branch %s has no commit: %w", branch.Short(), err)
	}
	return r.push(ctx, repo, branch, false, progress)
}

// push pushes branch to the branch of the same name, with force or not.
func (r *Remote) push(ctx context.Context, repo *git.Repository, branch plumbing.ReferenceName, force bool, progress io.Writer) (bool, error) {
	s, err := r.open(ctx, progress)
	if err != nil {
		return false, err
	}
	defer s.close()
	ctx, auth, progress := s.ctx, s.auth, s.progress
	spec := branch.String() + ":" + branch.String()
	if force {
		spec = "+" + spec
	}
	remote := git.NewRemote(repo.Storer, &config.RemoteConfig{Name: remoteName, URLs: []string{r.URL}})
	err = remote.PushContext(ctx, &git.PushOptions{
		RemoteName: remoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(spec)},
		Auth:       auth,
		Progress:   progress,
		Force:      force,
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
	return agentAuth(socket, "", nil, knownHosts)
}

// AgentKeyAuth authenticates SSH pushes as AgentAuth does, offering only
// the agent's key whose public key is publicKey, in authorized-keys form,
// so that no other key in the agent reaches the Git host. When the agent
// does not offer that key, the transfer fails naming its fingerprint. A
// URL that is not SSH is refused: the key is no credential for it.
func AgentKeyAuth(socket, publicKey string, knownHosts ...string) Auth {
	return agentAuth(socket, publicKey, nil, knownHosts)
}

// AgentKeyAuthRefusing is AgentKeyAuth, failing the transfer before any
// connection when refuse gives an error for the key as the agent lists it,
// with its comment: a key that has since become one no Git host is given.
func AgentKeyAuthRefusing(socket, publicKey string, refuse func(*agent.Key) error, knownHosts ...string) Auth {
	return agentAuth(socket, publicKey, refuse, knownHosts)
}

// agentAuth is AgentAuth, offering only the key publicKey when it is not
// "", and refusing it when refuse, if not nil, gives an error for it.
func agentAuth(socket, publicKey string, refuse func(*agent.Key) error, knownHosts []string) Auth {
	return func(ctx context.Context, url string) (transport.AuthMethod, func(), error) {
		endpoint, err := transport.NewEndpoint(url)
		if err != nil {
			return nil, nil, fmt.Errorf("workspace repository address: %w", err)
		}
		if endpoint.Protocol != "ssh" {
			if publicKey != "" {
				return nil, nil, NewCredentialsError(errors.New("an SSH key is the credential of an SSH URL only"))
			}
			return nil, func() {}, nil
		}
		var wanted ssh.PublicKey
		if publicKey != "" {
			if wanted, _, _, _, err = ssh.ParseAuthorizedKey([]byte(publicKey)); err != nil {
				return nil, nil, errors.New("ssh agent: the repository's key is not a public key")
			}
		}
		keyring, release, err := DialAgent(ctx, socket, agentWait)
		if err != nil {
			return nil, nil, fmt.Errorf("ssh agent: %w", err)
		}
		user := endpoint.User
		if user == "" {
			user = "git"
		}
		signers := keyring.Signers
		if wanted != nil {
			signer, err := agentSigner(keyring, wanted, refuse)
			if err != nil {
				release()
				return nil, nil, err
			}
			signers = func() ([]ssh.Signer, error) { return []ssh.Signer{signer}, nil }
		}
		auth := &gitssh.PublicKeysCallback{User: user, Callback: signers}
		if len(knownHosts) > 0 {
			callback, err := gitssh.NewKnownHostsCallback(knownHosts...)
			if err != nil {
				release()
				return nil, nil, fmt.Errorf("known hosts: %w", err)
			}
			auth.HostKeyCallback = callback
		}
		return auth, release, nil
	}
}

// DefaultAgentWait is how long one request to the SSH agent may wait for
// its reply during a transfer. Wyrwood bounds each request itself: a
// signature waits for OpenBao up to its sign timeout, 30 seconds by
// default and 2 minutes at most. The wait is longer, so a healthy agent
// answers or fails each request first, and only an agent that will not
// answer reaches it. The transfer's own deadline still ends it sooner.
const DefaultAgentWait = 150 * time.Second

// agentWait is the wait AgentAuth and AgentKeyAuth give the agent; tests
// shorten it.
var agentWait = DefaultAgentWait

// DialAgent dials the SSH agent at socket for an operation of ctx, and
// returns its client and the release that closes the connection. Each
// request waits at most wait for its reply, and the connection closes when
// ctx ends, so a stalled agent fails the request rather than holding the
// operation past its deadline. Between requests nothing reads the
// connection, so an idle connection never times out.
func DialAgent(ctx context.Context, socket string, wait time.Duration) (agent.ExtendedAgent, func(), error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	release := func() {
		stop()
		_ = conn.Close()
	}
	return agent.NewClient(&agentRequests{conn: conn, wait: wait}), release, nil
}

// agentRequests is the agent connection as the agent client sees it: a
// reader and writer with no Close. Given no Close, the client makes each
// request in turn, writing it and then reading its reply, with no reader
// left waiting between requests. So the deadline set at each write bounds
// that request's reply alone.
type agentRequests struct {
	conn net.Conn
	wait time.Duration
}

func (c *agentRequests) Write(p []byte) (int, error) {
	if err := c.conn.SetDeadline(time.Now().Add(c.wait)); err != nil {
		return 0, err
	}
	return c.conn.Write(p)
}

func (c *agentRequests) Read(p []byte) (int, error) { return c.conn.Read(p) }

// agentSigner is the signer of keyring's key wanted, or an error naming
// wanted's fingerprint when keyring does not offer it or refuse, if not
// nil, gives an error for the key as keyring lists it.
func agentSigner(keyring agent.ExtendedAgent, wanted ssh.PublicKey, refuse func(*agent.Key) error) (ssh.Signer, error) {
	fingerprint := ssh.FingerprintSHA256(wanted)
	if refuse != nil {
		listed, err := keyring.List()
		if err != nil {
			return nil, fmt.Errorf("ssh agent: %w", err)
		}
		for _, k := range listed {
			if bytes.Equal(k.Marshal(), wanted.Marshal()) {
				if err := refuse(k); err != nil {
					return nil, fmt.Errorf("ssh agent: the repository's key %s %w", fingerprint, err)
				}
			}
		}
	}
	signers, err := keyring.Signers()
	if err != nil {
		return nil, fmt.Errorf("ssh agent: %w", err)
	}
	for _, signer := range signers {
		if bytes.Equal(signer.PublicKey().Marshal(), wanted.Marshal()) {
			return signer, nil
		}
	}
	return nil, fmt.Errorf("ssh agent: the agent does not offer the repository's key %s; check that Menagerie's Wyrwood policy offers it, or change the repository's credential", fingerprint)
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

// NewCredentialsError is a *CredentialsError of cause, whose text must
// name no token and no URL: a failure that keeps its text for any URL.
func NewCredentialsError(cause error) error { return &CredentialsError{cause: cause} }

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
		values = append(values, basicForms(basic.Username, basic.Password)...)
	}
	t.progress = newRedactor(progress, values...)
	return t, nil
}

// close writes the progress text left and releases the credentials.
func (t *transfer) close() {
	_ = t.progress.Close()
	t.release()
}

// basicForms are the forms a remote can write HTTP basic credentials in:
// the password, and user:password in base64 and base64url, padded and
// unpadded.
func basicForms(username, password string) []string {
	pair := []byte(username + ":" + password)
	return []string{
		password,
		base64.StdEncoding.EncodeToString(pair),
		base64.RawStdEncoding.EncodeToString(pair),
		base64.URLEncoding.EncodeToString(pair),
		base64.RawURLEncoding.EncodeToString(pair),
	}
}

// archiveRef keeps the commit Archive copies while it copies it. It is not
// FetchedRef, which an apply-branch job holds across its own first push.
const archiveRef plumbing.ReferenceName = "refs/menagerie/archive"

// ArchivePrefix starts the branch that keeps an earlier workspace's backup.
const ArchivePrefix = "archive/"

// Archive keeps what the workspace repository holds on the workspace's branch
// under archive/<workspace>-<UTC stamp> before a new workspace of that name
// pushes over it, because the backup is the only copy left of the workspace
// that a delete removed. It returns the new branch's short name, or "" when
// the repository has no such branch or the branch already holds the local
// commit. Only the next push replaces the branch, so a failure loses nothing.
func (r *Remote) Archive(ctx context.Context, folder, workspace string, now time.Time, progress io.Writer) (string, error) {
	repo, branch, err := open(folder, workspace)
	if err != nil {
		return "", err
	}
	fetched, err := r.fetchInto(ctx, folder, workspace, branch.Short(), archiveRef, progress)
	if errors.Is(err, ErrBranchNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = repo.Storer.RemoveReference(archiveRef) }()
	if local, err := repo.Reference(branch, false); err == nil && local.Hash() == fetched {
		return "", nil
	}
	name := ArchivePrefix + workspace + "-" + now.UTC().Format("20060102T150405Z")
	s, err := r.open(ctx, progress)
	if err != nil {
		return "", err
	}
	defer s.close()
	remote := git.NewRemote(repo.Storer, &config.RemoteConfig{Name: remoteName, URLs: []string{r.URL}})
	err = remote.PushContext(s.ctx, &git.PushOptions{
		RemoteName: remoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec(archiveRef.String() + ":refs/heads/" + name)},
		Auth:       s.auth,
		Progress:   s.progress,
	})
	if err != nil {
		return "", fmt.Errorf("archive %s: %w", branch.Short(), transportFailure(s.ctx, r.URL, err))
	}
	return name, nil
}
