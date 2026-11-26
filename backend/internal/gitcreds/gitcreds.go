// ---
// relationships:
//   implements: [menagerie, choose-a-credential-per-backup-repository]
//   references: [workspace-api, configuration]
// ---

// Package gitcreds resolves the credential a backup repository's
// transfers authenticate with: one of the configured GitHub Apps, by name,
// or one of the keys Wyrwood's SSH agent offers, by public key. A
// registration from an earlier release, which names none, keeps the
// credential its URL chose: the first App whose server holds an https URL,
// and every key the agent offers for an SSH URL.
package gitcreds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/githubapp"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// Key is an SSH key the agent offers for Git: its name, the agent's comment
// on it, which Wyrwood sets to the Transit key's name; its SHA256
// fingerprint; and its public key in authorized-keys form with no comment,
// the line a Git host takes as a deploy key or a user's key.
type Key struct {
	Name        string
	Fingerprint string
	PublicKey   string
}

// App is a configured GitHub App as a manager chooses it: its name and its
// server's origin.
type App struct {
	Name   string
	Server string
}

// Source gives each credential its transfers' authentication.
type Source struct {
	// Apps are the configured GitHub Apps, in configuration order.
	Apps githubapp.Apps
	// Socket is the SSH agent's socket, Wyrwood's ssh-agent.sock.
	Socket string
	// KnownHosts are the known hosts files that verify SSH host keys; none
	// is the default files.
	KnownHosts []string
	// Excluded selects a key that is never a Git credential, provisioning's
	// operator key, as its configuration selects it: by name, public key or
	// SHA256 fingerprint. Empty excludes none.
	Excluded string
	// ListWait is how long the agent may take to list its keys, for the
	// key list and the operator-key check; zero is DefaultListWait.
	// Transfers wait as long as their job: see backup.DialAgent.
	ListWait time.Duration
}

// DefaultListWait is how long the agent may take to list its keys before
// the key list and the operator-key check fail. Wyrwood fetches each
// Transit key's public key from OpenBao in turn, which a healthy OpenBao
// answers well within it; a manager waits on the list, so it is kept
// short, and Validate can be tried again.
const DefaultListWait = 15 * time.Second

// list lists the agent's keys for an operation of ctx, within the list
// wait; see backup.DialAgent.
func (s *Source) list(ctx context.Context) ([]*agent.Key, error) {
	wait := s.ListWait
	if wait == 0 {
		wait = DefaultListWait
	}
	keyring, release, err := backup.DialAgent(ctx, s.Socket, wait)
	if err != nil {
		return nil, errors.New("the SSH agent could not be reached")
	}
	defer release()
	listed, err := keyring.List()
	if err != nil {
		return nil, errors.New("the SSH agent did not list its keys")
	}
	return listed, nil
}

// FitError refuses a credential for a URL. Its message names what to
// choose, and no URL.
type FitError struct{ Message string }

func (e *FitError) Error() string { return e.Message }

// AppChoices are the configured Apps, in configuration order.
func (s *Source) AppChoices() []App {
	out := make([]App, 0, len(s.Apps))
	for _, a := range s.Apps {
		out = append(out, App{Name: a.Name, Server: a.Server})
	}
	return out
}

// Keys lists the keys the agent offers, in the agent's order, without the
// excluded key.
func (s *Source) Keys(ctx context.Context) ([]Key, error) {
	listed, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	// The agent may list one key under several names: leave out every
	// entry of a key the selection names under any of them.
	excluded := map[string]bool{}
	for _, k := range listed {
		if s.Excluded != "" && selects(s.Excluded, k) {
			excluded[string(k.Marshal())] = true
		}
	}
	out := []Key{}
	for _, k := range listed {
		if excluded[string(k.Marshal())] {
			continue
		}
		out = append(out, Key{Name: k.Comment, Fingerprint: ssh.FingerprintSHA256(k), PublicKey: authorized(k)})
	}
	return out, nil
}

// selects reports whether selection names key: its SHA256 fingerprint, its
// public key with or without a comment, or its comment, as Wyrwood's
// operator.SelectAgentKey reads a selection.
func selects(selection string, key *agent.Key) bool {
	if ssh.FingerprintSHA256(key) == selection || key.Comment == selection {
		return true
	}
	parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(selection))
	return err == nil && bytes.Equal(parsed.Marshal(), key.Marshal())
}

// excluded reports whether key is the excluded key: by its fingerprint or
// public key, or by its name in the agent, which lists it with its
// comment. An agent that cannot be reached or does not list its keys in
// time is an error: the key may be the excluded one.
func (s *Source) excluded(ctx context.Context, key ssh.PublicKey) (bool, error) {
	if s.Excluded == "" {
		return false, nil
	}
	if selects(s.Excluded, &agent.Key{Format: key.Type(), Blob: key.Marshal()}) {
		return true, nil
	}
	listed, err := s.list(ctx)
	if err != nil {
		return false, err
	}
	for _, k := range listed {
		if bytes.Equal(k.Marshal(), key.Marshal()) && selects(s.Excluded, k) {
			return true, nil
		}
	}
	return false, nil
}

// authorized is key in authorized-keys form with no comment.
func authorized(key ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

// NormalKey is publicKey, a public key in authorized-keys form with or
// without a comment, in the form a credential records: with no comment.
func NormalKey(publicKey string) (string, bool) {
	parsed, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(publicKey))
	if err != nil || len(bytes.TrimSpace(rest)) != 0 {
		return "", false
	}
	return authorized(parsed), true
}

// Fit checks that c is a credential a new registration of url can have: an
// SSH URL takes an SSH key that is not the excluded key; an https URL that
// an App's server holds takes an App that serves it; any other https URL
// takes none. The URL is one ValidURL accepts. A *FitError refuses it.
func (s *Source) Fit(ctx context.Context, url string, c store.Credential) error {
	endpoint, err := transport.NewEndpoint(url)
	if err != nil {
		return &FitError{Message: "the URL is not a valid Git URL"}
	}
	if endpoint.Protocol == "ssh" {
		switch {
		case c.App != "":
			return &FitError{Message: "an SSH URL takes an SSH key as its credential, not a GitHub App"}
		case c.Key == "":
			return &FitError{Message: "choose the SSH key that reaches the repository"}
		}
		parsed, _, _, _, err := ssh.ParseAuthorizedKey([]byte(c.Key))
		if err != nil {
			return &FitError{Message: "the SSH key is not a public key in authorized-keys form"}
		}
		excluded, err := s.excluded(ctx, parsed)
		if err != nil {
			return &FitError{Message: err.Error() + ", so Menagerie could not check that the SSH key is not provisioning's operator key; try again"}
		}
		if excluded {
			return &FitError{Message: "the SSH key is provisioning's operator key, which no Git host is given; choose another"}
		}
		return nil
	}
	if c.Key != "" {
		return &FitError{Message: "an https URL takes a GitHub App as its credential, not an SSH key"}
	}
	serving, _, served := s.Apps.Serving(url)
	if c.App == "" {
		if served {
			return &FitError{Message: fmt.Sprintf("choose the GitHub App for the repository: %s, or another App on its server", serving.Name)}
		}
		return nil
	}
	app, ok := s.Apps.Named(c.App)
	if !ok {
		return &FitError{Message: fmt.Sprintf("no GitHub App named %s is configured", c.App)}
	}
	if _, ok := app.Repository(url); !ok {
		return &FitError{Message: fmt.Sprintf("GitHub App %s does not serve the URL: it serves %s/{owner}/{repository} URLs with no user name, query or fragment", app.Name, app.Server)}
	}
	return nil
}

// Auth is the authentication of every transfer with c: only the App it
// names, only the key it names, or for the zero credential the earlier
// rule chosen by URL.
func (s *Source) Auth(c store.Credential) backup.Auth {
	switch {
	case c.Key != "":
		return backup.AgentKeyAuthRefusing(s.Socket, c.Key, s.refuse, s.KnownHosts...)
	case c.App != "":
		return s.appAuth(c.App)
	}
	auth := backup.AgentAuth(s.Socket, s.KnownHosts...)
	if len(s.Apps) > 0 {
		auth = backup.TokenAuth(s.Apps.Tokens(githubapp.Write), auth)
	}
	return auth
}

// refuse refuses the excluded key as a transfer's agent lists it: a key a
// repository recorded before the configuration excluded it is no Git
// credential either.
func (s *Source) refuse(key *agent.Key) error {
	if s.Excluded != "" && selects(s.Excluded, key) {
		return errors.New("is provisioning's operator key, which no Git host is given; change the repository's credential")
	}
	return nil
}

// appAuth sends a contents write token of the App name, and fails a URL
// that App does not serve or an App the configuration no longer names.
func (s *Source) appAuth(name string) backup.Auth {
	return func(ctx context.Context, url string) (transport.AuthMethod, func(), error) {
		app, ok := s.Apps.Named(name)
		if !ok {
			return nil, nil, backup.NewCredentialsError(fmt.Errorf("no GitHub App named %s is configured; change the repository's credential", name))
		}
		tokens := app.Tokens(githubapp.Write)
		if !tokens.Serves(url) {
			return nil, nil, backup.NewCredentialsError(fmt.Errorf("GitHub App %s does not serve the repository's URL", app.Name))
		}
		return backup.TokenAuth(tokens, nil)(ctx, url)
	}
}
