// ---
// relationships:
//   verifies: menagerie
// ---

package wyrwood

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyrd-company/wyrwood/operator"
	"golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"
)

const (
	sampleAgent   = "sample-host"
	sampleName    = "sample-container"
	operatorName  = "sample-operator"
	otherKeyName  = "sample-git"
	bindPolicy    = "sample-policy"
	waitForAnswer = 10 * time.Second
)

// fakeController speaks the operator subsystem's protocol, keeps bindings by
// container key as the controller does, and refuses what its grant lacks.
type fakeController struct {
	t        *testing.T
	listener net.Listener
	hostKey  ssh.Signer

	mu       sync.Mutex
	approved map[string]bool
	offered  []string
	users    []string
	grant    operator.Grant
	bindings []operator.Binding
	nextID   int
	writes   int
	requests []operator.Request
	// answer, when set, replaces the answer to a request.
	answer func(operator.Request) *operator.Response
	// stall holds every request until the channel closes.
	stall chan struct{}
}

func startController(t *testing.T) *fakeController {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeController{
		t: t, listener: listener, hostKey: hostKey, approved: map[string]bool{},
		grant: operator.Grant{
			Agents: []string{sampleAgent}, Policies: []string{bindPolicy},
			Actions: []operator.Action{operator.ActionRead, operator.ActionBind},
		},
	}
	t.Cleanup(func() { _ = listener.Close() })
	go f.serve()
	return f
}

func (f *fakeController) url() string { return "ssh://" + f.listener.Addr().String() }

func (f *fakeController) fingerprint() string {
	return ssh.FingerprintSHA256(f.hostKey.PublicKey())
}

func (f *fakeController) serve() {
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.serveConnection(connection)
	}
}

func (f *fakeController) serveConnection(connection net.Conn) {
	defer func() { _ = connection.Close() }()
	config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		fingerprint := ssh.FingerprintSHA256(key)
		f.offered = append(f.offered, fingerprint)
		f.users = append(f.users, meta.User())
		if !f.approved[fingerprint] {
			return nil, errors.New("pending")
		}
		return &ssh.Permissions{}, nil
	}}
	config.AddHostKey(f.hostKey)
	server, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go ssh.DiscardRequests(requests)
	for candidate := range channels {
		if candidate.ChannelType() != "session" {
			_ = candidate.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		channel, channelRequests, err := candidate.Accept()
		if err != nil {
			return
		}
		go func() {
			for request := range channelRequests {
				subsystem := struct{ Name string }{}
				ok := request.Type == "subsystem" && ssh.Unmarshal(request.Payload, &subsystem) == nil && subsystem.Name == operator.SubsystemName
				_ = request.Reply(ok, nil)
			}
		}()
		f.serveRequests(channel)
	}
}

func (f *fakeController) serveRequests(channel io.ReadWriteCloser) {
	reader := bufio.NewReader(channel)
	for {
		line, err := operator.ReadLine(reader, operator.MaximumLineBytes)
		if err != nil {
			return
		}
		var request operator.Request
		if err := json.Unmarshal(line, &request); err != nil {
			f.t.Errorf("request line: %v", err)
			return
		}
		f.mu.Lock()
		stall := f.stall
		f.mu.Unlock()
		if stall != nil {
			<-stall
		}
		encoded, err := json.Marshal(f.respond(request))
		if err != nil {
			f.t.Errorf("answer: %v", err)
			return
		}
		if _, err := channel.Write(append(encoded, '\n')); err != nil {
			return
		}
	}
}

func (f *fakeController) respond(request operator.Request) *operator.Response {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if f.answer != nil {
		if answer := f.answer(request); answer != nil {
			answer.Version = operator.ProtocolVersion
			return answer
		}
	}
	body, err := f.dispatch(request)
	response := &operator.Response{Version: operator.ProtocolVersion}
	var refusal *operator.Error
	if errors.As(err, &refusal) {
		response.Error = refusal
		return response
	}
	if err != nil {
		f.t.Errorf("dispatch: %v", err)
	}
	if response.Body, err = json.Marshal(body); err != nil {
		f.t.Errorf("answer body: %v", err)
	}
	return response
}

var refuseGrant = &operator.Error{Code: operator.CodeOutsideGrant, Message: "this key's grant does not cover this request"}

func (f *fakeController) dispatch(request operator.Request) (any, error) {
	switch request.Operation {
	case operator.OperationBindingList:
		var list operator.BindingList
		if err := json.Unmarshal(request.Body, &list); err != nil {
			return nil, err
		}
		if !f.grant.Can(operator.ActionRead) {
			return nil, refuseGrant
		}
		kept := []operator.Binding{}
		for _, binding := range f.bindings {
			if binding.Agent == list.Agent {
				kept = append(kept, binding)
			}
		}
		return operator.Bindings{Bindings: kept}, nil
	case operator.OperationBind:
		var bind operator.BindRequest
		if err := json.Unmarshal(request.Body, &bind); err != nil {
			return nil, err
		}
		if !f.grant.Can(operator.ActionBind) || !f.grant.Covers(bind.Agent, bind.Policies) {
			return nil, refuseGrant
		}
		for _, existing := range f.bindings {
			if existing.Agent != bind.Agent || existing.Key != bind.Key {
				continue
			}
			if !slices.Equal(existing.Policies, bind.Policies) || !reflect.DeepEqual(existing.Settings, bind.Settings) {
				return nil, &operator.Error{Code: operator.CodeBindingDiffer, Message: "this container key is bound to another selection; unbind it first"}
			}
			return existing, nil
		}
		f.nextID++
		f.writes++
		created := operator.Binding{
			ID: fmt.Sprintf("binding-%d", f.nextID), Agent: bind.Agent, Key: bind.Key, Policies: bind.Policies, Settings: bind.Settings,
			Status: operator.BindingStatus{Readiness: operator.ReadinessWaiting, LastError: "sample-last-error"},
		}
		f.bindings = append(f.bindings, created)
		return created, nil
	case operator.OperationUnbind:
		var unbind operator.UnbindRequest
		if err := json.Unmarshal(request.Body, &unbind); err != nil {
			return nil, err
		}
		if !f.grant.Can(operator.ActionBind) || !f.grant.AllowsAgent(unbind.Agent) {
			return nil, refuseGrant
		}
		for i, existing := range f.bindings {
			if existing.Agent == unbind.Agent && unbind.Key != nil && existing.Key == *unbind.Key {
				f.bindings = slices.Delete(f.bindings, i, i+1)
				f.writes++
				return operator.UnbindResult{Removed: true}, nil
			}
		}
		return operator.UnbindResult{}, nil
	}
	return nil, &operator.Error{Code: operator.CodeInvalid, Message: "this operation is not supported"}
}

func (f *fakeController) approve(fingerprint string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approved[fingerprint] = true
}

func (f *fakeController) set(change func(*fakeController)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *fakeController) writesSoFar() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes
}

func (f *fakeController) offeredKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.offered)
}

// startAgent serves a keyring on a socket, with a Git key before and after the
// operator key, and returns the socket and the operator key's fingerprint.
func startAgent(t *testing.T) (string, string) {
	t.Helper()
	keyring := sshagent.NewKeyring()
	addKey(t, keyring, otherKeyName+"-first")
	fingerprint := addKey(t, keyring, operatorName)
	addKey(t, keyring, otherKeyName+"-last")
	socket := filepath.Join(t.TempDir(), "ssh-agent.sock")
	serveAgent(t, keyring, socket)
	return socket, fingerprint
}

// serveAgent serves a keyring on a socket until the returned listener closes.
func serveAgent(t *testing.T, keyring sshagent.Agent, socket string) net.Listener {
	t.Helper()
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = connection.Close() }()
				_ = sshagent.ServeAgent(keyring, connection)
			}()
		}
	}()
	return listener
}

func addKey(t *testing.T, keyring sshagent.Agent, comment string) string {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := keyring.Add(sshagent.AddedKey{PrivateKey: private, Comment: comment}); err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	return ssh.FingerprintSHA256(key)
}

type fixture struct {
	controller  *fakeController
	client      *Client
	fingerprint string
}

// newFixture returns a client whose operator key the controller has approved.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := newPendingFixture(t)
	f.controller.approve(f.fingerprint)
	return f
}

func newPendingFixture(t *testing.T) *fixture {
	t.Helper()
	controller := startController(t)
	socket, fingerprint := startAgent(t)
	client, err := New(Config{
		Controller: controller.url(), HostKey: controller.fingerprint(), Agent: sampleAgent,
		OperatorKey: operatorName, Socket: socket,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{controller: controller, client: client, fingerprint: fingerprint}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitForAnswer)
	t.Cleanup(cancel)
	return ctx
}

func sampleSelection() Selection {
	return Selection{Policies: []string{bindPolicy}, AccessGroup: 1000, Values: map[string]string{"key": "sample-key"}}
}

func TestBindUnbindAndReadLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := testContext(t)

	if _, found, err := f.client.Read(ctx, sampleName); err != nil || found {
		t.Fatalf("read before bind: found %v, %v", found, err)
	}
	first, err := f.client.Bind(ctx, sampleName, sampleSelection())
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if first.Key != (operator.Key{Kind: operator.KeyName, Value: sampleName}) || first.Agent != sampleAgent {
		t.Fatalf("binding = %+v", first)
	}
	second, err := f.client.Bind(ctx, sampleName, sampleSelection())
	if err != nil {
		t.Fatalf("repeated bind: %v", err)
	}
	if second.ID != first.ID || f.controller.writesSoFar() != 1 {
		t.Fatalf("repeated bind made %q after %q, %d writes", second.ID, first.ID, f.controller.writesSoFar())
	}
	read, found, err := f.client.Read(ctx, sampleName)
	if err != nil || !found {
		t.Fatalf("read: found %v, %v", found, err)
	}
	if read.ID != first.ID || read.Status.Readiness != operator.ReadinessWaiting || read.Status.LastError != "sample-last-error" {
		t.Fatalf("read = %+v", read)
	}
	removed, err := f.client.Unbind(ctx, sampleName)
	if err != nil || !removed.Removed {
		t.Fatalf("unbind: %+v, %v", removed, err)
	}
	again, err := f.client.Unbind(ctx, sampleName)
	if err != nil || again.Removed {
		t.Fatalf("repeated unbind: %+v, %v", again, err)
	}
	if f.controller.writesSoFar() != 2 {
		t.Fatalf("writes = %d, want one bind and one unbind", f.controller.writesSoFar())
	}
	if _, found, err := f.client.Read(ctx, sampleName); err != nil || found {
		t.Fatalf("read after unbind: found %v, %v", found, err)
	}
}

func TestBindSendsADeclaredReportBinding(t *testing.T) {
	f := newFixture(t)
	if _, err := f.client.Bind(testContext(t), sampleName, sampleSelection()); err != nil {
		t.Fatal(err)
	}
	f.controller.mu.Lock()
	defer f.controller.mu.Unlock()
	var sent operator.BindRequest
	if err := json.Unmarshal(f.controller.requests[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	group := uint32(1000)
	want := operator.BindRequest{
		Agent: sampleAgent, Key: operator.Key{Kind: operator.KeyName, Value: sampleName}, Policies: []string{bindPolicy},
		Settings: operator.Settings{AccessGroup: &group, Enforcement: "report", Values: map[string]string{"key": "sample-key"}},
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("bind request = %+v, want %+v", sent, want)
	}
}

func TestAGroupOfZeroIsSent(t *testing.T) {
	f := newFixture(t)
	selection := sampleSelection()
	selection.AccessGroup = 0
	binding, err := f.client.Bind(testContext(t), sampleName, selection)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Settings.AccessGroup == nil || *binding.Settings.AccessGroup != 0 {
		t.Fatalf("access group = %v", binding.Settings.AccessGroup)
	}
}

func TestOnlyTheSelectedKeyIsOffered(t *testing.T) {
	f := newFixture(t)
	if _, _, err := f.client.Read(testContext(t), sampleName); err != nil {
		t.Fatal(err)
	}
	offered := slices.Compact(f.controller.offeredKeys())
	if !slices.Equal(offered, []string{f.fingerprint}) {
		t.Fatalf("offered %v, want only %s", offered, f.fingerprint)
	}
	f.controller.mu.Lock()
	defer f.controller.mu.Unlock()
	for _, user := range f.controller.users {
		if user != operator.User("menagerie-"+sampleAgent) {
			t.Fatalf("user = %q", user)
		}
	}
}

func TestSelectionByFingerprint(t *testing.T) {
	f := newFixture(t)
	f.client.config.OperatorKey = f.fingerprint
	if _, _, err := f.client.Read(testContext(t), sampleName); err != nil {
		t.Fatal(err)
	}
	if got := slices.Compact(f.controller.offeredKeys()); !slices.Equal(got, []string{f.fingerprint}) {
		t.Fatalf("offered %v", got)
	}
}

func TestARecreatedSocketWorks(t *testing.T) {
	controller := startController(t)
	keyring := sshagent.NewKeyring()
	controller.approve(addKey(t, keyring, operatorName))
	socket := filepath.Join(t.TempDir(), "ssh-agent.sock")
	first := serveAgent(t, keyring, socket)
	client, err := New(Config{
		Controller: controller.url(), HostKey: controller.fingerprint(), Agent: sampleAgent,
		OperatorKey: operatorName, Socket: socket,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	if _, _, err := client.Read(ctx, sampleName); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.Read(ctx, sampleName); !errors.Is(err, operator.ErrKeySelection) {
		t.Fatalf("read with the socket gone = %v", err)
	}
	serveAgent(t, keyring, socket)
	if _, _, err := client.Read(ctx, sampleName); err != nil {
		t.Fatalf("read with a recreated socket: %v", err)
	}
}

func TestLabelFallsBackWhenTheAgentNameIsTooLong(t *testing.T) {
	config := Config{
		Controller: "ssh://controller.example.test", HostKey: "SHA256:" + strings.Repeat("a", 43),
		Agent: strings.Repeat("a", 60), OperatorKey: operatorName, Socket: "/run/sample.sock",
	}
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if client.label != "menagerie" {
		t.Fatalf("label = %q", client.label)
	}
	config.Agent = "sample.host"
	if client, err = New(config); err != nil || client.label != "menagerie-sample.host" {
		t.Fatalf("label = %v, %v", client, err)
	}
}

func TestPendingKeyFailsNamingItsFingerprint(t *testing.T) {
	f := newPendingFixture(t)
	ctx := testContext(t)
	calls := map[string]func() error{
		"bind":   func() error { _, err := f.client.Bind(ctx, sampleName, sampleSelection()); return err },
		"unbind": func() error { _, err := f.client.Unbind(ctx, sampleName); return err },
		"read":   func() error { _, _, err := f.client.Read(ctx, sampleName); return err },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if !errors.Is(err, operator.ErrNotApproved) {
				t.Fatalf("error = %v", err)
			}
			for _, want := range []string{f.fingerprint, "Wyrwood " + name + " of " + sampleName} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
		})
	}
	if !slices.Contains(f.controller.offeredKeys(), f.fingerprint) {
		t.Fatal("the key was not presented, so the controller holds nothing to approve")
	}
}

func TestRevokedKeyMidSessionFailsNamingItsFingerprint(t *testing.T) {
	f := newFixture(t)
	f.controller.set(func(c *fakeController) {
		c.answer = func(operator.Request) *operator.Response {
			return &operator.Response{Error: &operator.Error{Code: operator.CodeNotApproved, Message: "this key is not approved"}}
		}
	})
	_, err := f.client.Bind(testContext(t), sampleName, sampleSelection())
	if !errors.Is(err, operator.ErrNotApproved) || !strings.Contains(err.Error(), f.fingerprint) {
		t.Fatalf("error = %v", err)
	}
}

func TestACallOutsideTheGrantIsAClearJobError(t *testing.T) {
	cases := map[string]struct {
		grant func(*operator.Grant)
		call  func(context.Context, *Client) error
		wants []string
	}{
		"bind without the action": {
			grant: func(g *operator.Grant) { g.Actions = []operator.Action{operator.ActionRead} },
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Bind(ctx, sampleName, sampleSelection())
				return err
			},
			wants: []string{"Wyrwood bind of " + sampleName, "does not allow this bind"},
		},
		"bind of a policy outside the allowlist": {
			grant: func(g *operator.Grant) { g.Policies = []string{"sample-other"} },
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Bind(ctx, sampleName, sampleSelection())
				return err
			},
			wants: []string{"Wyrwood bind of " + sampleName, sampleAgent},
		},
		"bind on another agent": {
			grant: func(g *operator.Grant) { g.Agents = []string{"sample-other"} },
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Bind(ctx, sampleName, sampleSelection())
				return err
			},
			wants: []string{"Wyrwood bind of " + sampleName, sampleAgent},
		},
		"unbind without the action": {
			grant: func(g *operator.Grant) { g.Actions = []operator.Action{operator.ActionRead} },
			call: func(ctx context.Context, c *Client) error {
				_, err := c.Unbind(ctx, sampleName)
				return err
			},
			wants: []string{"Wyrwood unbind of " + sampleName, "does not allow this unbind"},
		},
		"read without the action": {
			grant: func(g *operator.Grant) { g.Actions = []operator.Action{operator.ActionBind} },
			call: func(ctx context.Context, c *Client) error {
				_, _, err := c.Read(ctx, sampleName)
				return err
			},
			wants: []string{"Wyrwood read of " + sampleName, "does not allow this read"},
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.controller.set(func(c *fakeController) { test.grant(&c.grant) })
			err := test.call(testContext(t), f.client)
			if !errors.Is(err, operator.ErrOutsideGrant) {
				t.Fatalf("error = %v", err)
			}
			for _, want := range append(test.wants, "new key and a new approval") {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
			if writes := f.controller.writesSoFar(); writes != 0 {
				t.Fatalf("writes = %d", writes)
			}
		})
	}
}

func TestADifferentSelectionFailsAndChangesNothing(t *testing.T) {
	changes := map[string]func(*Selection){
		"policies": func(s *Selection) { s.Policies = []string{bindPolicy, bindPolicy} },
		"group":    func(s *Selection) { s.AccessGroup = 2000 },
		"values":   func(s *Selection) { s.Values = map[string]string{"key": "sample-other"} },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			ctx := testContext(t)
			first, err := f.client.Bind(ctx, sampleName, sampleSelection())
			if err != nil {
				t.Fatal(err)
			}
			selection := sampleSelection()
			change(&selection)
			_, err = f.client.Bind(ctx, sampleName, selection)
			if !errors.Is(err, operator.ErrBindingDiffers) || !strings.Contains(err.Error(), "the controller answered: this container key is bound to another selection") {
				t.Fatalf("error = %v", err)
			}
			read, found, err := f.client.Read(ctx, sampleName)
			if err != nil || !found || read.ID != first.ID || !reflect.DeepEqual(read.Settings, first.Settings) {
				t.Fatalf("read = %+v, %v, %v", read, found, err)
			}
			if f.controller.writesSoFar() != 1 {
				t.Fatalf("writes = %d", f.controller.writesSoFar())
			}
		})
	}
}

func TestReadFindsOnlyTheNameBinding(t *testing.T) {
	f := newFixture(t)
	ctx := testContext(t)
	f.controller.set(func(c *fakeController) {
		c.bindings = []operator.Binding{
			{ID: "other-name", Agent: sampleAgent, Key: operator.Key{Kind: operator.KeyName, Value: "sample-other"}},
			{ID: "other-kind", Agent: sampleAgent, Key: operator.Key{Kind: operator.KeyCompose, Value: sampleName}},
			{ID: "wanted", Agent: sampleAgent, Key: operator.Key{Kind: operator.KeyName, Value: sampleName}},
		}
	})
	read, found, err := f.client.Read(ctx, sampleName)
	if err != nil || !found || read.ID != "wanted" {
		t.Fatalf("read = %+v, %v, %v", read, found, err)
	}
	f.controller.mu.Lock()
	defer f.controller.mu.Unlock()
	var list operator.BindingList
	if err := json.Unmarshal(f.controller.requests[0].Body, &list); err != nil || list.Agent != sampleAgent {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestReadDoesNotTakeABindingOfAnotherAgent(t *testing.T) {
	f := newFixture(t)
	f.controller.set(func(c *fakeController) {
		c.answer = func(request operator.Request) *operator.Response {
			body, err := json.Marshal(operator.Bindings{Bindings: []operator.Binding{
				{ID: "elsewhere", Agent: "sample-other", Key: operator.Key{Kind: operator.KeyName, Value: sampleName}},
			}})
			if err != nil {
				t.Error(err)
			}
			return &operator.Response{Body: body}
		}
	})
	if read, found, err := f.client.Read(testContext(t), sampleName); err != nil || found {
		t.Fatalf("read = %+v, %v, %v", read, found, err)
	}
}

func TestUnbindReportsWhatDidNotFinish(t *testing.T) {
	f := newFixture(t)
	f.controller.set(func(c *fakeController) {
		c.answer = func(operator.Request) *operator.Response {
			body, err := json.Marshal(operator.UnbindResult{Removed: true, TokenLive: true, Warning: "sample-warning"})
			if err != nil {
				t.Error(err)
			}
			return &operator.Response{Body: body}
		}
	})
	result, err := f.client.Unbind(testContext(t), sampleName)
	if err != nil || !result.Removed || !result.TokenLive || result.Warning != "sample-warning" {
		t.Fatalf("unbind = %+v, %v", result, err)
	}
}

func TestRefusalsKeepTheirMeaning(t *testing.T) {
	cases := map[string]struct {
		refusal  operator.Error
		sentinel error
		want     string
		absent   string
	}{
		"pending": {
			operator.Error{Code: operator.CodePending, Message: "sample-pending-text"}, operator.ErrPending,
			"a repeat finishes it", "sample-pending-text",
		},
		"invalid keeps the controller's text": {
			operator.Error{Code: operator.CodeInvalid, Message: "the policy sample does not exist"}, operator.ErrInvalid,
			"the controller answered: the policy sample does not exist", "",
		},
		"unknown code": {
			operator.Error{Code: "from-a-newer-controller", Message: "sample-new-text"}, operator.ErrFailed,
			"the controller answered: sample-new-text", "",
		},
		"no text": {
			operator.Error{Code: operator.CodeFailed}, operator.ErrFailed,
			"the controller refused the request", "answered",
		},
		"lines are joined": {
			operator.Error{Code: operator.CodeFailed, Message: "first\nmenagerie: forged\r\x1b[2J"}, operator.ErrFailed,
			"first menagerie: forged", "\n",
		},
		"long text is cut": {
			operator.Error{Code: operator.CodeFailed, Message: strings.Repeat("a", maxMessage) + "sample-tail"}, operator.ErrFailed,
			strings.Repeat("a", maxMessage) + "...", "sample-tail",
		},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.controller.set(func(c *fakeController) {
				c.answer = func(operator.Request) *operator.Response {
					refusal := test.refusal
					return &operator.Response{Error: &refusal}
				}
			})
			_, err := f.client.Bind(testContext(t), sampleName, sampleSelection())
			if !errors.Is(err, test.sentinel) {
				t.Fatalf("error = %v", err)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error %q does not hold %q", err, test.want)
			}
			if test.absent != "" && strings.Contains(err.Error(), test.absent) {
				t.Fatalf("error %q holds %q", err, test.absent)
			}
			if strings.ContainsFunc(err.Error(), func(r rune) bool { return r < ' ' }) {
				t.Fatalf("error %q holds a control character", err)
			}
		})
	}
}

func TestAnotherHostKeyFails(t *testing.T) {
	f := newFixture(t)
	f.client.config.HostKey = "SHA256:" + strings.Repeat("a", 43)
	_, _, err := f.client.Read(testContext(t), sampleName)
	if !errors.Is(err, operator.ErrHostKeyMismatch) || !strings.Contains(err.Error(), "provisioning.wyrwood.host-key") {
		t.Fatalf("error = %v", err)
	}
	if got := f.controller.offeredKeys(); len(got) != 0 {
		t.Fatalf("the key went to a controller with another host key: %v", got)
	}
}

func TestUnreachableController(t *testing.T) {
	f := newFixture(t)
	if err := f.controller.listener.Close(); err != nil {
		t.Fatal(err)
	}
	_, _, err := f.client.Read(testContext(t), sampleName)
	if !errors.Is(err, operator.ErrUnreachable) || !strings.Contains(err.Error(), "cannot be reached") {
		t.Fatalf("error = %v", err)
	}
}

func TestKeySelectionFailures(t *testing.T) {
	cases := map[string]func(*Client){
		"no such key": func(c *Client) { c.config.OperatorKey = "sample-absent" },
		"no socket":   func(c *Client) { c.config.Socket = filepath.Join(filepath.Dir(c.config.Socket), "absent.sock") },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			change(f.client)
			_, _, err := f.client.Read(testContext(t), sampleName)
			if !errors.Is(err, operator.ErrKeySelection) || !strings.Contains(err.Error(), "provisioning.wyrwood.operator-key") {
				t.Fatalf("error = %v", err)
			}
			if got := f.controller.offeredKeys(); len(got) != 0 {
				t.Fatalf("offered %v", got)
			}
		})
	}
}

func TestAmbiguousKeyNameFails(t *testing.T) {
	f := newFixture(t)
	keyring := sshagent.NewKeyring()
	addKey(t, keyring, operatorName)
	addKey(t, keyring, operatorName)
	f.client.config.Socket = filepath.Join(t.TempDir(), "ssh-agent.sock")
	serveAgent(t, keyring, f.client.config.Socket)
	_, _, err := f.client.Read(testContext(t), sampleName)
	if !errors.Is(err, operator.ErrKeySelection) || !strings.Contains(err.Error(), "no single key") {
		t.Fatalf("error = %v", err)
	}
	if got := f.controller.offeredKeys(); len(got) != 0 {
		t.Fatalf("offered %v", got)
	}
}

// silentAgent accepts connections on a socket and never answers.
func silentAgent(t *testing.T) string {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "ssh-agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = connection.Close() })
		}
	}()
	return socket
}

func TestSilentAgentEndsAtTheTimeLimit(t *testing.T) {
	f := newFixture(t)
	f.client.config.Socket = silentAgent(t)
	f.client.timeout = 200 * time.Millisecond
	start := time.Now()
	_, _, err := f.client.Read(testContext(t), sampleName)
	if !errors.Is(err, operator.ErrKeySelection) {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s", elapsed)
	}
}

func TestSilentAgentEndsWithTheContext(t *testing.T) {
	f := newFixture(t)
	f.client.config.Socket = silentAgent(t)
	ctx, cancel := context.WithCancel(testContext(t))
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, _, err := f.client.Read(ctx, sampleName)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "the job ended first") {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s, so the cancel did not end the read of the agent", elapsed)
	}
}

func TestSilentControllerEndsAtTheTimeLimit(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.controller.set(func(c *fakeController) { c.stall = release })
	f.client.timeout = 200 * time.Millisecond
	start := time.Now()
	_, _, err := f.client.Read(testContext(t), sampleName)
	if !errors.Is(err, operator.ErrUnreachable) || !strings.Contains(err.Error(), "cannot be reached") {
		t.Fatalf("error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s, so the time limit did not reach the controller's connection", elapsed)
	}
}

func TestAStalledRequestEndsWithTheContext(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.controller.set(func(c *fakeController) { c.stall = release })
	ctx, cancel := context.WithCancel(testContext(t))
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := f.client.Bind(ctx, sampleName, sampleSelection())
	if !errors.Is(err, context.Canceled) || !errors.Is(err, operator.ErrUnreachable) {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "the job ended first") {
		t.Fatalf("error = %v", err)
	}
}

func TestAnEndedContextConnectsToNothing(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := f.client.Read(ctx, sampleName)
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "the job ended first") {
		t.Fatalf("error = %v", err)
	}
	if got := f.controller.offeredKeys(); len(got) != 0 {
		t.Fatalf("offered %v", got)
	}
}

func TestValidators(t *testing.T) {
	hostKey := "SHA256:" + strings.Repeat("a", 43)
	cases := []struct {
		name  string
		check func(string) error
		good  []string
		bad   []string
	}{
		{"controller", ValidController,
			[]string{"ssh://controller", "ssh://controller.example.test:2222", "ssh://192.0.2.1:22", "ssh://[2001:db8::1]:2222"},
			[]string{"", "controller:22", "http://controller", "ssh://", "ssh://:22", "ssh://user@controller", "ssh://controller/", "ssh://controller/path", "ssh://controller?x=1", "ssh://controller?", "ssh://controller#x", "ssh://controller:", "ssh://controller:0", "ssh://controller:65536", "ssh://controller:port"}},
		{"host key", ValidHostKey,
			[]string{hostKey, "SHA256:" + strings.Repeat("A", 42) + "+"},
			[]string{"", hostKey + "a", hostKey[:len(hostKey)-1], "MD5:" + strings.Repeat("a", 43), "ssh-ed25519 AAAA", hostKey + "\n", "SHA256:" + strings.Repeat("=", 43)}},
		{"agent", ValidAgent,
			[]string{"host", "sample-host", "host.example.test", strings.Repeat("a", 63)},
			[]string{"", "-host", "host-with space", "host/x", "host:", strings.Repeat("a", 64), strings.Repeat(strings.Repeat("a", 63)+".", 4) + "a"}},
		{"operator key", ValidOperatorKey,
			[]string{operatorName, "ssh-ed25519 AAAA sample-comment", hostKey},
			[]string{"", " sample", "sample ", "sample\nother", "sample\tother"}},
	}
	for _, test := range cases {
		for _, value := range test.good {
			if err := test.check(value); err != nil {
				t.Errorf("%s %q: %v", test.name, value, err)
			}
		}
		for _, value := range test.bad {
			err := test.check(value)
			if err == nil {
				t.Errorf("%s %q was accepted", test.name, value)
				continue
			}
			if len(value) > 8 && strings.Contains(err.Error(), value) {
				t.Errorf("%s error %q names its value", test.name, err)
			}
		}
	}
}

func TestNewRefusesWhatCannotWork(t *testing.T) {
	good := Config{
		Controller: "ssh://controller.example.test", HostKey: "SHA256:" + strings.Repeat("a", 43), Agent: sampleAgent,
		OperatorKey: operatorName, Socket: "/run/sample.sock",
	}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*Config){
		"controller":   func(c *Config) { c.Controller = "controller" },
		"host key":     func(c *Config) { c.HostKey = "sample" },
		"agent":        func(c *Config) { c.Agent = "not a host" },
		"operator key": func(c *Config) { c.OperatorKey = "" },
		"socket":       func(c *Config) { c.Socket = "" },
	}
	for name, change := range cases {
		config := good
		change(&config)
		if _, err := New(config); err == nil || !strings.Contains(err.Error(), name) && !strings.Contains(err.Error(), "agent socket") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
