// ---
// relationships:
//   implements: menagerie
//   references: [configuration, workspace-api]
// ---

// Package registrycreds supplies container registry credentials to
// devcontainer-builder from OpenBao, read through the Wyrwood vault socket.
// A registry that configuration does not name is anonymous. A named
// registry whose credentials cannot be read fails closed with an *Error.
// Credential values never appear in an error.
package registrycreds

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/distribution/reference"
	"github.com/wyrd-company/devcontainer-builder/pkg/registryauth"
)

// DockerHub is the registry name every Docker Hub alias normalizes to.
const DockerHub = "docker.io"

// dockerHubAliases are the names that refer to Docker Hub, as
// devcontainer-builder's registryauth treats them.
var dockerHubAliases = map[string]bool{
	"docker.io":                   true,
	"index.docker.io":             true,
	"registry-1.docker.io":        true,
	"https://index.docker.io/v1/": true,
}

var hostPattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*|\[[0-9a-f:.]+\])(:[0-9]{1,5})?$`)

// NormalizeRegistry returns the canonical name of a registry host: lower
// case, with every Docker Hub alias as DockerHub. It refuses a value that is
// not a host or host:port.
func NormalizeRegistry(host string) (string, error) {
	h := strings.ToLower(host)
	if dockerHubAliases[h] {
		return DockerHub, nil
	}
	if !hostPattern.MatchString(h) {
		return "", fmt.Errorf("registry %q is not a host or host:port", host)
	}
	return h, nil
}

// Error reports that a configured registry's credentials are unavailable.
// Reason is a fixed description; it never holds a credential, a response
// body or an OpenBao error text.
type Error struct {
	Registry string
	Reason   string
	cause    error
}

func (e *Error) Error() string {
	if e.Registry == "" {
		return "registry credentials are unavailable: " + e.Reason
	}
	return fmt.Sprintf("registry credentials for %s are unavailable: %s", e.Registry, e.Reason)
}

// Unwrap returns the context error that ended the read, if any.
func (e *Error) Unwrap() error { return e.cause }

// Fetcher reads the credentials stored at a secret path.
type Fetcher interface {
	Fetch(ctx context.Context, path string) (registryauth.Credentials, error)
}

// Entry names the secret path of one registry's credentials.
type Entry struct {
	Registry string
	Path     string
}

// Registries is the process's registry credential configuration.
type Registries struct {
	fetcher Fetcher
	paths   map[string]string
}

// New returns the Registries for entries. Registry names are normalized;
// two entries for one registry are refused.
func New(fetcher Fetcher, entries []Entry) (*Registries, error) {
	paths := make(map[string]string, len(entries))
	for _, e := range entries {
		name, err := NormalizeRegistry(e.Registry)
		if err != nil {
			return nil, err
		}
		if _, dup := paths[name]; dup {
			return nil, fmt.Errorf("registry %q is configured twice", name)
		}
		paths[name] = e.Path
	}
	if len(paths) > 0 && fetcher == nil {
		return nil, errors.New("registry credentials need a fetcher")
	}
	return &Registries{fetcher: fetcher, paths: paths}, nil
}

// Operation returns the credential provider of one operation. It reads a
// registry's secret on first use, with ctx, and keeps the result for the
// operation. A nil Registries yields a provider that is anonymous for every
// registry.
func (r *Registries) Operation(ctx context.Context) *Provider {
	return &Provider{ctx: ctx, registries: r, results: map[string]*result{}}
}

// Provider answers devcontainer-builder's credential requests for one
// operation. It satisfies buildkit.RegistryAuth and the Engine pull
// credential interface, and is safe for concurrent use.
type Provider struct {
	ctx        context.Context
	registries *Registries

	mu      sync.Mutex
	results map[string]*result
}

type result struct {
	done  chan struct{}
	creds registryauth.Credentials
	err   error
}

// Credentials returns the credentials for a registry host. A host that no
// entry names is anonymous.
func (p *Provider) Credentials(host string) (registryauth.Credentials, error) {
	name, err := NormalizeRegistry(host)
	if err != nil || p.registries == nil {
		return registryauth.Credentials{}, nil
	}
	path, ok := p.registries.paths[name]
	if !ok {
		return registryauth.Credentials{}, nil
	}
	p.mu.Lock()
	res, started := p.results[name]
	if !started {
		res = &result{done: make(chan struct{})}
		p.results[name] = res
	}
	p.mu.Unlock()
	if !started {
		res.creds, res.err = p.fetch(name, path)
		close(res.done)
	}
	<-res.done
	return res.creds, res.err
}

func (p *Provider) fetch(name, path string) (registryauth.Credentials, error) {
	creds, err := p.registries.fetcher.Fetch(p.ctx, path)
	if err == nil {
		return creds, nil
	}
	var e *Error
	if errors.As(err, &e) {
		return registryauth.Credentials{}, &Error{Registry: name, Reason: e.Reason, cause: e.cause}
	}
	return registryauth.Credentials{}, &Error{Registry: name, Reason: "the credential source failed"}
}

// EngineAuth returns the Engine API X-Registry-Auth value for an image
// reference, or "" when its registry is anonymous.
func (p *Provider) EngineAuth(ref string) (string, error) {
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return "", fmt.Errorf("registry credentials for image reference: %w", err)
	}
	host := reference.Domain(named)
	creds, err := p.Credentials(host)
	if err != nil || creds.Anonymous() {
		return "", err
	}
	return registryauth.Static{host: creds}.EngineAuth(ref)
}
