// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

// Package wyrwood manages a workspace's binding on the Wyrwood controller
// through Wyrwood's public operator package. The operator key stays in
// OpenBao: the Wyrwood agent signs with it on Menagerie's own ssh-agent.sock,
// and only that key is offered to the controller, so the controller records no
// other key of the socket as pending. Each operation opens its own connection,
// so that a socket the agent recreated between operations still works. An
// error's chain holds the operator package's sentinel, such as
// operator.ErrOutsideGrant, and this package logs nothing.
package wyrwood

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/wyrd-company/wyrwood/operator"
	"golang.org/x/crypto/ssh"
)

const (
	// labelPrefix starts the name proposed for a pending operator key, with
	// the agent's name after it, so that the hosts of one controller propose
	// different names. The controller's labels are unique once approved.
	labelPrefix = "menagerie-"
	// fallbackLabel is proposed when the agent's name makes too long a label.
	fallbackLabel = "menagerie"
	// enforcement keeps Wyrwood from recreating a container during a job.
	enforcement = "report"
	// requestTimeout bounds the signing connection, the connection to the
	// controller and each request. It is longer than the controller's own 15
	// seconds for a request, so that the controller's answer arrives first.
	requestTimeout = 30 * time.Second
	// maxMessage bounds the controller's text in an error, in runes.
	maxMessage = 200
)

var (
	// hostKeyPattern is ssh.FingerprintSHA256's form.
	hostKeyPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
	// agentPattern is the form Wyrwood gives an agent's name.
	agentPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*$`)
)

// ValidController accepts ssh://host[:port] with nothing else. Its error names
// no value.
func ValidController(raw string) error {
	invalid := errors.New("must be ssh://host[:port]")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" || u.User != nil || u.Path != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return invalid
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return invalid
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return invalid
	}
	return nil
}

// ValidHostKey accepts a SHA256 fingerprint, as `wyrwood controller host-key`
// prints it. Its error names no value.
func ValidHostKey(key string) error {
	if !hostKeyPattern.MatchString(key) {
		return errors.New("must be a SHA256 fingerprint, as wyrwood controller host-key prints it")
	}
	return nil
}

// ValidAgent accepts the form Wyrwood gives an agent's name. Its error names
// no value.
func ValidAgent(name string) error {
	if len(name) > 253 || !agentPattern.MatchString(name) {
		return errors.New("must be a host name of letters, digits, hyphens and dots")
	}
	return nil
}

// ValidOperatorKey accepts what can select a key: text on one line with no
// space at either end. A tab stays, because a public key's fields can be
// separated by one. Its error names no value.
func ValidOperatorKey(selection string) error {
	control := func(r rune) bool { return r != '\t' && unicode.IsControl(r) }
	if selection == "" || strings.TrimSpace(selection) != selection || strings.ContainsFunc(selection, control) {
		return errors.New("must be a key's name, public key or SHA256 fingerprint on one line")
	}
	return nil
}

// Error says why an operation on a binding did not finish.
type Error struct {
	// Operation is "bind", "unbind" or "read".
	Operation string
	// Name is the container name of the binding.
	Name string
	// Reason says what happened and holds no secret.
	Reason string
	causes []error
}

func (e *Error) Error() string {
	return fmt.Sprintf("Wyrwood %s of %s failed: %s", e.Operation, e.Name, e.Reason)
}

// Unwrap returns the operator package's error and, when the caller's context
// ended the operation, the context's error.
func (e *Error) Unwrap() []error { return e.causes }

// Config locates the controller and the operator key.
type Config struct {
	// Controller is the controller's ssh://host[:port].
	Controller string
	// HostKey is the controller's pinned SHA256 fingerprint.
	HostKey string
	// Agent is the name of this host's Wyrwood agent.
	Agent string
	// OperatorKey selects the operator key by name, public key or fingerprint.
	OperatorKey string
	// Socket is the path of Menagerie's own ssh-agent.sock.
	Socket string
}

// Client manages bindings of one Wyrwood agent.
type Client struct {
	config  Config
	label   string
	timeout time.Duration
}

// New returns a Client. It connects to nothing.
func New(config Config) (*Client, error) {
	if err := ValidController(config.Controller); err != nil {
		return nil, fmt.Errorf("wyrwood controller %w", err)
	}
	if err := ValidHostKey(config.HostKey); err != nil {
		return nil, fmt.Errorf("wyrwood host key %w", err)
	}
	if err := ValidAgent(config.Agent); err != nil {
		return nil, fmt.Errorf("wyrwood agent %w", err)
	}
	if err := ValidOperatorKey(config.OperatorKey); err != nil {
		return nil, fmt.Errorf("wyrwood operator key %w", err)
	}
	if config.Socket == "" {
		return nil, errors.New("wyrwood agent socket is required")
	}
	label := labelPrefix + config.Agent
	if !operator.ValidLabel(label) {
		label = fallbackLabel
	}
	return &Client{config: config, label: label, timeout: requestTimeout}, nil
}

// Selection is what Menagerie's stored workspace selection gives a binding.
type Selection struct {
	// Policies are the Wyrwood policies of the binding.
	Policies []string
	// AccessGroup is the numeric group of the consumer directory.
	AccessGroup uint32
	// Values are the policies' per-binding values.
	Values map[string]string
}

// Bind returns the name binding of a container, creating it when absent, with
// report enforcement and the controller's default token lifetimes. A repeat
// with an equal selection writes nothing, so it revokes no token. Another
// selection changes nothing and fails with operator.ErrBindingDiffers: unbind
// it while its container is stopped, then bind.
func (c *Client) Bind(ctx context.Context, name string, selection Selection) (operator.Binding, error) {
	group := selection.AccessGroup
	var binding operator.Binding
	err := c.do(ctx, "bind", name, func(client *operator.Client) (err error) {
		binding, err = client.Bind(ctx, operator.BindRequest{
			Agent:    c.config.Agent,
			Key:      nameKey(name),
			Policies: selection.Policies,
			Settings: operator.Settings{AccessGroup: &group, Enforcement: enforcement, Values: selection.Values},
		})
		return err
	})
	return binding, err
}

// Get reads the recorded binding ID. ErrNotFound and ErrOutsideGrant remain
// distinct; neither a missing ID nor a name omitted from Read proves removal.
func (c *Client) Get(ctx context.Context, id string) (operator.Binding, error) {
	var binding operator.Binding
	err := c.do(ctx, "read", id, func(client *operator.Client) (err error) {
		binding, err = client.GetBinding(ctx, id)
		return err
	})
	return binding, err
}

// Change replaces the policies, values and access group at the revision read,
// preserving all other settings. It can finish a saved pending assignment.
func (c *Client) Change(ctx context.Context, before operator.Binding, selection Selection) (operator.Binding, error) {
	settings := before.Settings
	settings.AccessGroup = &selection.AccessGroup
	settings.Values = selection.Values
	var binding operator.Binding
	err := c.do(ctx, "change", before.Key.Value, func(client *operator.Client) (err error) {
		binding, err = client.ChangeBinding(ctx, operator.BindingChange{
			ID: before.ID, Revision: before.Revision, Policies: selection.Policies, Settings: settings,
		})
		return err
	})
	return binding, err
}

// Unbind removes a container's name binding on agent, the agent Menagerie
// recorded it on. A binding that does not exist is a success with Removed
// false, so a repeat is safe; one outside the grant fails with
// operator.ErrOutsideGrant in its chain. The result's Warning and TokenLive say
// what part of a removal did not finish.
func (c *Client) Unbind(ctx context.Context, agent, name string) (operator.UnbindResult, error) {
	var result operator.UnbindResult
	err := c.do(ctx, "unbind", name, func(client *operator.Client) (err error) {
		key := nameKey(name)
		result, err = client.Unbind(ctx, operator.UnbindRequest{Agent: agent, Key: &key})
		return err
	})
	return result, err
}

// Read returns a container's name binding with its readiness, and whether it
// exists. A binding that holds a policy outside the grant reads as absent,
// because the controller leaves it out.
func (c *Client) Read(ctx context.Context, name string) (operator.Binding, bool, error) {
	var bindings []operator.Binding
	err := c.do(ctx, "read", name, func(client *operator.Client) (err error) {
		bindings, err = client.ListBindings(ctx, operator.BindingList{Agent: c.config.Agent})
		return err
	})
	if err != nil {
		return operator.Binding{}, false, err
	}
	for _, binding := range bindings {
		if binding.Agent == c.config.Agent && binding.Key == nameKey(name) {
			return binding, true, nil
		}
	}
	return operator.Binding{}, false, nil
}

func nameKey(name string) operator.Key { return operator.Key{Kind: operator.KeyName, Value: name} }

// do opens a connection with the selected key, runs one request and closes.
func (c *Client) do(ctx context.Context, operation, name string, request func(*operator.Client) error) error {
	fingerprint := ""
	err := func() error {
		agent, connection, err := operator.DialAgent(c.config.Socket)
		if err != nil {
			return err
		}
		defer func() { _ = connection.Close() }()
		// The operator package bounds its own connection, but not the agent's.
		_ = connection.SetDeadline(time.Now().Add(c.timeout))
		defer context.AfterFunc(ctx, func() { _ = connection.Close() })()
		signer, err := operator.SelectAgentKey(agent, c.config.OperatorKey)
		if err != nil {
			return err
		}
		fingerprint = ssh.FingerprintSHA256(signer.PublicKey())
		client, err := operator.Dial(ctx, operator.Config{
			URL: c.config.Controller, Signer: signer, HostKey: c.config.HostKey, Label: c.label, Timeout: c.timeout,
		})
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		return request(client)
	}()
	if err == nil {
		return nil
	}
	failure := &Error{Operation: operation, Name: name, Reason: reason(err, operation, c.config.Agent, c.config.OperatorKey, fingerprint), causes: []error{err}}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// A closed connection is what the job's end looks like from here.
		failure.Reason = "the job ended first"
		failure.causes = append(failure.causes, ctxErr)
	}
	return failure
}

// reason says what a person does next. The controller's own text is kept for a
// refusal this package has no words for, such as a selection that differs.
func reason(err error, operation, agent, selection, fingerprint string) string {
	switch {
	case errors.Is(err, operator.ErrKeySelection):
		// A public-key selector already identifies a fingerprint, even when
		// the socket cannot supply its signer. A name cannot tell us one.
		if key, _, _, _, parseErr := ssh.ParseAuthorizedKey([]byte(selection)); parseErr == nil {
			selection = ssh.FingerprintSHA256(key)
		}
		return fmt.Sprintf("ssh-agent.sock does not offer a single usable key for provisioning.wyrwood.operator-key %q; check that the Wyrwood agent runs on this host and that Menagerie's policy offers that key", selection)
	case errors.Is(err, operator.ErrNotApproved):
		return fmt.Sprintf("the operator key %s is not approved (pending, rejected or revoked); approve a pending key in the Wyrwood web interface", fingerprint)
	case errors.Is(err, operator.ErrOutsideGrant):
		return fmt.Sprintf("the operator key's grant does not allow this %s on agent %s; edit the key's grant in the Wyrwood web interface, then retry the job", operation, agent)
	case errors.Is(err, operator.ErrPending):
		return "the controller saved the binding and OpenBao has not applied it; a repeat finishes it"
	case errors.Is(err, operator.ErrHostKeyMismatch), errors.Is(err, operator.ErrHostKeyNotAccepted):
		return "the controller's host key does not match provisioning.wyrwood.host-key"
	case errors.Is(err, operator.ErrUnreachable):
		return "the controller cannot be reached"
	}
	var refusal *operator.Error
	if errors.As(err, &refusal) {
		if text := OneLine(refusal.Message); text != "" {
			return "the controller answered: " + text
		}
	}
	return "the controller refused the request"
}

// OneLine keeps the controller's text to one bounded line, so that it cannot
// pass as other lines of a job's console.
func OneLine(text string) string {
	runes := []rune(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, text))
	if len(runes) > maxMessage {
		return string(runes[:maxMessage]) + "..."
	}
	return strings.TrimSpace(string(runes))
}

// Policies reads the operator key's grant and the policies currently inside
// it on one fresh connection. Neither answer is cached between requests.
func (c *Client) Policies(ctx context.Context) (operator.Grant, []operator.Policy, error) {
	var identity operator.Identity
	var policies []operator.Policy
	err := c.do(ctx, "policies", "", func(client *operator.Client) (err error) {
		identity, err = client.Whoami(ctx)
		if err != nil {
			return err
		}
		policies, err = client.ListPolicies(ctx)
		return err
	})
	return identity.Grant, policies, err
}
