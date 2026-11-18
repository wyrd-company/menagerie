// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, provisioning-declaration, configuration]
// ---

// Package provisioning gives a workspace whose definition has a provisioning
// declaration what it declares, in the order the first start needs: its
// Xolotl key and its Wyrwood binding, then its container without a start, then
// the sockets, and only then the start. Steps wraps the devcontainer runtime,
// so every job that can create a container runs the steps. Without a
// declaration, each job runs as the runtime runs it. Every step is safe to
// repeat, so a start retries a job that failed. Menagerie's data holds each
// container name bound and each key issued for a workspace, recorded before the
// request that makes it and removed once Wyrwood or Xolotl confirms it gone, so
// what a job made is known whatever the definition says later.
package provisioning

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/wyrd-company/wyrwood/operator"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/wyrwood"
	"github.com/wyrd-company/menagerie/backend/internal/xolotl"
)

// Job error kinds of provisioning.
const (
	KindInvalid     = "provisioning-invalid"
	KindUnavailable = "provisioning-unavailable"
	KindNotReady    = "provisioning-not-ready"
	// KindDeleteRefused is a delete refused while a container can still write
	// to the workspace folder.
	KindDeleteRefused = "delete-refused"
	// KindDeleteUnfinished is a job refused on a folder that a delete began to
	// remove.
	KindDeleteUnfinished = "delete-unfinished"
	// KindBindingOutsideGrant is a delete or a retire whose unbind Wyrwood
	// answers as outside the operator key's grant. The name stays in
	// Menagerie's data.
	KindBindingOutsideGrant = "binding-outside-grant"
)

// DefaultReadinessWait bounds each wait on a binding's status when
// configuration names no other.
const DefaultReadinessWait = 2 * time.Minute

// pollInterval is the time between two reads of a binding's status.
const pollInterval = time.Second

// socketNames are the sockets a binding gives a container.
var socketNames = []string{"vault.sock", "ssh-agent.sock"}

// Runtime is what the steps use of the devcontainer runtime.
type Runtime interface {
	Status(ctx context.Context, folder string) (devcontainer.Status, error)
	Start(ctx context.Context, folder string, p devcontainer.Progress) error
	Stop(ctx context.Context, folder string, p devcontainer.Progress) error
	Rebuild(ctx context.Context, folder string, p devcontainer.Progress) error
	Mounting(ctx context.Context, folder string) ([]string, error)
	CheckHostPrerequisites(ctx context.Context, folder string, p devcontainer.Progress) error
	Definition(folder string) (devcontainer.Definition, error)
	ContainerHolder(ctx context.Context, name string) (folder string, found bool, err error)
	BeginStart(folder string, p devcontainer.Progress) (BringUp, error)
	BeginRebuild(folder string, p devcontainer.Progress) (BringUp, error)
	MountingOutside(ctx context.Context, folder string, p devcontainer.Progress) ([]string, error)
	RemoveContainers(ctx context.Context, folder string, p devcontainer.Progress) error
	RemoveFolder(ctx context.Context, folder string, p devcontainer.Progress, emptied func() error) error
}

// BringUp is one start or rebuild as separate steps. *devcontainer.BringUp
// satisfies it.
type BringUp interface {
	Build(ctx context.Context) error
	Create(ctx context.Context) (string, error)
	Up(ctx context.Context) error
}

// Adapt makes a Runtime of the devcontainer runtime.
func Adapt(r *devcontainer.Runtime) Runtime { return adapted{r} }

type adapted struct{ *devcontainer.Runtime }

func (a adapted) BeginStart(folder string, p devcontainer.Progress) (BringUp, error) {
	b, err := a.Runtime.BeginStart(folder, p)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func (a adapted) BeginRebuild(folder string, p devcontainer.Progress) (BringUp, error) {
	b, err := a.Runtime.BeginRebuild(folder, p)
	if err != nil {
		return nil, err
	}
	return b, nil
}

// Bindings manages Wyrwood bindings. *wyrwood.Client satisfies it. Bind and
// Read act on this host's agent; Unbind names the agent the binding was
// recorded on.
type Bindings interface {
	Bind(ctx context.Context, name string, selection wyrwood.Selection) (operator.Binding, error)
	Unbind(ctx context.Context, agent, name string) (operator.UnbindResult, error)
	Read(ctx context.Context, name string) (operator.Binding, bool, error)
}

// Keys issues and revokes Xolotl client keys. *xolotl.Client satisfies it.
type Keys interface {
	Issue(ctx context.Context, policy, name string) (xolotl.Issued, error)
	Revoke(ctx context.Context, policy, name string) (xolotl.Revocation, error)
}

// Clients are the host's provisioning services. A nil one is a section that
// configuration lacks.
type Clients struct {
	Bindings Bindings
	// Agent is provisioning.wyrwood.agent, which Bindings binds on and each
	// name bound is recorded with; "" without Bindings.
	Agent string
	Keys  Keys
	// KeyPolicy names the key policy used when a workspace first gets a key.
	KeyPolicy string
	// Wait bounds each wait on a binding's status; zero is
	// DefaultReadinessWait.
	Wait time.Duration
}

// Options compose Steps.
type Options struct {
	Clients
	Runtime Runtime
	// Data is Menagerie's data, which holds what each workspace was given.
	Data *store.Store
	// Poll is the time between two reads of a binding's status; zero is
	// pollInterval.
	Poll time.Duration
}

// Steps runs a job's container operations with their provisioning.
type Steps struct{ opts Options }

// New returns Steps.
func New(opts Options) *Steps {
	if opts.Wait <= 0 {
		opts.Wait = DefaultReadinessWait
	}
	if opts.Poll <= 0 {
		opts.Poll = pollInterval
	}
	return &Steps{opts: opts}
}

// Status reports a workspace's container state.
func (s *Steps) Status(ctx context.Context, folder string) (devcontainer.Status, error) {
	return s.opts.Runtime.Status(ctx, folder)
}

// Stop stops a workspace's container.
func (s *Steps) Stop(ctx context.Context, folder string, p devcontainer.Progress) error {
	return s.opts.Runtime.Stop(ctx, folder, p)
}

// Mounting lists the running containers that mount a folder.
func (s *Steps) Mounting(ctx context.Context, folder string) ([]string, error) {
	return s.opts.Runtime.Mounting(ctx, folder)
}

// Start starts a workspace's devcontainer, creating what is missing.
func (s *Steps) Start(ctx context.Context, folder string, p devcontainer.Progress) error {
	return s.run(ctx, folder, p, bringKind{})
}

// Rebuild replaces a workspace's container from its definition.
func (s *Steps) Rebuild(ctx context.Context, folder string, p devcontainer.Progress) error {
	return s.run(ctx, folder, p, bringKind{rebuild: true})
}

// Create starts a workspace that has no commit yet. It checks the host, and
// save commits and backs the workspace up: after issue-key and bind with a
// declaration, and before the start. Failing, save leaves the container as it
// was, and its error is the job's.
func (s *Steps) Create(ctx context.Context, folder string, p devcontainer.Progress, save func(context.Context) error) error {
	return s.run(ctx, folder, p, bringKind{save: save})
}

// bringKind says which job a run belongs to.
type bringKind struct {
	rebuild bool
	save    func(context.Context) error
}

func (s *Steps) run(ctx context.Context, folder string, p devcontainer.Progress, kind bringKind) error {
	if err := s.refuseRemoving(ctx, folder); err != nil {
		return err
	}
	declaration, err := s.declaration(folder, p)
	if err != nil {
		return err
	}
	// A job without a declaration can retire the current binding too. Import
	// its selection before either path can write or unbind anything.
	if err := s.EnsureImported(ctx, folder); err != nil {
		return fail(KindUnavailable, err.Error())
	}
	if declaration != nil {
		err = s.provision(ctx, folder, declaration, p, kind)
	} else {
		err = s.plain(ctx, folder, p, kind)
	}
	if err != nil {
		return err
	}
	if err := s.retire(ctx, folder, declaration, p); err != nil {
		return &AfterStartError{Step: "retiring what the definition no longer declares", Err: err}
	}
	return s.opts.Data.AppliedWithoutName(ctx, filepath.Base(folder))
}

// AfterStartError says that a job started the devcontainer and a later step
// failed: verify, or retire, which keeps the rest for the next job. A job's
// message must not say that the devcontainer did not start.
type AfterStartError struct {
	// Step says what failed after the start.
	Step string
	Err  error
}

func (e *AfterStartError) Error() string {
	return "the devcontainer started, but " + e.Step + " failed: " + e.Err.Error()
}
func (e *AfterStartError) Unwrap() error { return e.Err }

// declaration reads a workspace's declaration. A definition that cannot be
// read has none here, and the runtime's start reports it.
func (s *Steps) declaration(folder string, p devcontainer.Progress) (*Declaration, error) {
	def, err := s.opts.Runtime.Definition(folder)
	if err != nil || def.Block == nil {
		return nil, nil
	}
	p.Phase("read-definition")
	declaration, err := Parse(def)
	if err != nil {
		return nil, &jobs.Failure{Kind: KindInvalid, Err: err}
	}
	for _, name := range declaration.Ignored {
		fmt.Fprintln(p.Log(""), "ignoring legacy customizations.menagerie."+name+"; Menagerie supplies the workspace policies, values and key")
	}
	return declaration, nil
}

func (s *Steps) plain(ctx context.Context, folder string, p devcontainer.Progress, kind bringKind) error {
	if kind.save != nil {
		if err := s.opts.Runtime.CheckHostPrerequisites(ctx, folder, p); err != nil {
			return err
		}
		if err := kind.save(ctx); err != nil {
			return err
		}
	}
	if !kind.rebuild {
		retired, err := s.retiredContainer(ctx, folder)
		if err != nil {
			return err
		}
		if retired {
			fmt.Fprintln(p.Log(""), "the workspace's container has a name its definition no longer declares, so a rebuild that failed left it; replacing it")
			kind.rebuild = true
		}
	}
	if kind.rebuild {
		return s.opts.Runtime.Rebuild(ctx, folder, p)
	}
	return s.opts.Runtime.Start(ctx, folder, p)
}

func (s *Steps) provision(ctx context.Context, folder string, d *Declaration, p devcontainer.Progress, kind bringKind) error {
	if err := s.available(d); err != nil {
		return err
	}
	current, err := s.opts.Data.Selection(ctx, filepath.Base(folder))
	if err != nil {
		return err
	}
	key := current.Key
	if key == nil && s.opts.Keys != nil {
		if s.opts.KeyPolicy == "" {
			return fail(KindUnavailable, "provisioning.xolotl.key-policy is required to name a new workspace key.")
		}
		if !ValidWorkspaceIdentity(filepath.Base(folder)) {
			return fail(KindInvalid, "A provisioned workspace name must be 1 to 63 lowercase letters, digits, _ or -, starting with a letter or digit.")
		}
		key = &Key{Policy: s.opts.KeyPolicy, Name: filepath.Base(folder)}
	}
	identity, err := s.opts.Data.Identity(ctx, filepath.Base(folder), nil, key)
	if err != nil {
		return err
	}
	d.Xolotl = identity.Key
	if d.Xolotl != nil && s.opts.Keys == nil {
		return fail(KindUnavailable, "The workspace has a Xolotl key, and the configuration has no provisioning.xolotl.")
	}
	if err := s.notTaken(ctx, folder, d); err != nil {
		return err
	}
	before, err := s.opts.Runtime.Status(ctx, folder)
	if err != nil {
		return err
	}
	if !kind.rebuild {
		behind, err := s.leftBehind(ctx, folder, d.ContainerName, before)
		if err != nil {
			return err
		}
		if behind {
			fmt.Fprintf(p.Log(""), "the workspace's container does not have the declared name %s, so a rebuild that failed left it; replacing it\n", d.ContainerName)
			kind.rebuild = true
		}
	}
	begin := s.opts.Runtime.BeginStart
	if kind.rebuild {
		begin = s.opts.Runtime.BeginRebuild
	}
	bringUp, err := begin(folder, p)
	if err != nil {
		return err
	}
	if err := s.opts.Runtime.CheckHostPrerequisites(ctx, folder, p); err != nil {
		return err
	}
	// Only a start of a container that exists goes on without Xolotl.
	existing := !kind.rebuild && kind.save == nil && before.State != devcontainer.StateNoContainer
	if err := s.issueKey(ctx, folder, d, p, existing); err != nil {
		return err
	}
	if err := s.bind(ctx, folder, d, p); err != nil {
		return err
	}
	if d.Xolotl != nil {
		if naming, ok := bringUp.(interface{ SetXolotlKey(string) }); ok {
			naming.SetXolotlKey(d.Xolotl.String())
		}
	}
	if kind.save != nil {
		if err := kind.save(ctx); err != nil {
			return err
		}
	}
	if err := bringUp.Build(ctx); err != nil {
		return err
	}
	id, err := bringUp.Create(ctx)
	if err != nil {
		return err
	}
	status, err := s.opts.Runtime.Status(ctx, folder)
	if err != nil {
		return err
	}
	if status.State != devcontainer.StateRunning || status.ContainerID != id {
		p.Phase("wait-for-sockets")
		if err := s.await(ctx, d, p, "its sockets", containerReady(d, id)); err != nil {
			return err
		}
	}
	if err := bringUp.Up(ctx); err != nil {
		return err
	}
	p.Phase("verify")
	err = s.await(ctx, d, p, "its running container and sockets", func(b operator.Binding) (bool, string) {
		ready, failure := containerReady(d, id)(b)
		return ready && containerState(b, id) == "running", failure
	})
	if err != nil {
		return &AfterStartError{Step: "verifying its sockets", Err: err}
	}
	return nil
}

// leftBehind says that a start finds a container that a failed rebuild had to
// replace: the workspace has a container, and it does not have the name the
// definition declares. A start would keep it, and its binding would never
// report it.
func (s *Steps) leftBehind(ctx context.Context, folder, name string, before devcontainer.Status) (bool, error) {
	if before.State == devcontainer.StateNoContainer {
		return false, nil
	}
	owner, found, err := s.opts.Runtime.ContainerHolder(ctx, name)
	if err != nil {
		return false, err
	}
	return !found || owner != folder, nil
}

// retiredContainer says that the workspace's container still has a name
// Menagerie's data holds for it, which a definition with no declaration retires:
// a rebuild that would have removed the block failed, and retire would take
// what that container uses.
func (s *Steps) retiredContainer(ctx context.Context, folder string) (bool, error) {
	held, err := s.opts.Data.Held(ctx, filepath.Base(folder))
	if err != nil {
		return false, err
	}
	for _, name := range held.Names {
		owner, found, err := s.opts.Runtime.ContainerHolder(ctx, name.Name)
		if err != nil {
			return false, err
		}
		if found && owner == folder {
			return true, nil
		}
	}
	return false, nil
}

// available fails a declaration that needs a section configuration lacks.
func (s *Steps) available(d *Declaration) error {
	if s.opts.Bindings == nil {
		return &jobs.Failure{Kind: KindUnavailable, Err: errors.New("the definition declares a Wyrwood binding, and the configuration has no provisioning.wyrwood")}
	}
	if d.Xolotl != nil && s.opts.Keys == nil {
		return &jobs.Failure{Kind: KindUnavailable, Err: errors.New("the definition declares a Xolotl key, and the configuration has no provisioning.xolotl")}
	}
	return nil
}

// notTaken refuses a container name or Xolotl key that Menagerie's data holds
// for another workspace, or a container name a container of another workspace
// holds, so that the delete of one workspace cannot take another's binding or
// key.
func (s *Steps) notTaken(ctx context.Context, folder string, d *Declaration) error {
	workspace := filepath.Base(folder)
	refuse := func(what, holder string) error {
		return &jobs.Failure{Kind: KindInvalid, Err: fmt.Errorf("%s is already taken: %s", what, holder)}
	}
	holder, err := s.opts.Data.NameHolder(ctx, d.ContainerName)
	if err != nil {
		return err
	}
	if holder != "" && holder != workspace {
		return refuse("container name "+d.ContainerName, "Menagerie's data holds it for workspace "+holder)
	}
	if d.Xolotl != nil {
		holder, err := s.opts.Data.KeyHolder(ctx, *d.Xolotl)
		if err != nil {
			return err
		}
		if holder != "" && holder != workspace {
			return refuse("Xolotl key "+d.Xolotl.String(), "Menagerie's data holds it for workspace "+holder)
		}
	}
	owner, found, err := s.opts.Runtime.ContainerHolder(ctx, d.ContainerName)
	if err != nil {
		return err
	}
	if found && owner != folder {
		holder := "a container that belongs to no workspace has that name"
		if owner != "" {
			holder = "the container of " + filepath.Base(owner) + " has that name"
		}
		return refuse("container name "+d.ContainerName, holder)
	}
	return nil
}

// taken turns a name or key that another workspace took meanwhile into the
// refusal read-definition gives it.
func taken(err error) error {
	var other *store.TakenError
	if errors.As(err, &other) {
		return &jobs.Failure{Kind: KindInvalid, Err: fmt.Errorf("%s is already taken: Menagerie's data holds it for workspace %s", other.What, other.Holder)}
	}
	return err
}

// issueKey issues the declared key, after Menagerie's data records it as the
// workspace's. The answer that it exists is done, so a repeat never rotates it,
// and the console shows what a repeat cannot change.
func (s *Steps) issueKey(ctx context.Context, folder string, d *Declaration, p devcontainer.Progress, existing bool) error {
	if d.Xolotl == nil {
		return nil
	}
	p.Phase("issue-key")
	if err := s.opts.Data.HoldKey(ctx, filepath.Base(folder), *d.Xolotl); err != nil {
		return taken(err)
	}
	issued, err := s.opts.Keys.Issue(ctx, d.Xolotl.Policy, d.Xolotl.Name)
	var failure *xolotl.Error
	if existing && errors.As(err, &failure) && failure.Unreachable {
		fmt.Fprintf(p.Log(""), "Xolotl cannot be reached, so key %s was not checked; the existing container still starts\n", d.Xolotl)
		return nil
	}
	if err != nil {
		return err
	}
	out := p.Log("")
	if issued.Created {
		fmt.Fprintf(out, "issued Xolotl key %s\n", d.Xolotl)
	} else {
		fmt.Fprintf(out, "Xolotl key %s already exists and was left as it is\n", d.Xolotl)
	}
	if issued.Disabled {
		fmt.Fprintf(out, "Xolotl key %s is disabled, so it is not usable\n", d.Xolotl)
	}
	if issued.ExpiresAt != nil {
		fmt.Fprintf(out, "Xolotl key %s expires at %s\n", d.Xolotl, issued.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// bind records the container name as the workspace's current one in
// Menagerie's data, converges the binding on its stored selection, and waits
// until its consumer directory exists for the container's bind source.
func (s *Steps) bind(ctx context.Context, folder string, d *Declaration, p devcontainer.Progress) error {
	p.Phase("bind")
	workspace := filepath.Base(folder)
	if err := s.retireOtherAgent(ctx, workspace, d.ContainerName, p); err != nil {
		return err
	}
	if err := s.opts.Data.HoldName(ctx, workspace, d.ContainerName, s.opts.Agent, true); err != nil {
		return taken(err)
	}
	if err := s.applySelection(ctx, folder, d.ContainerName, &d.Wyrwood.AccessGroup, p); err != nil {
		return err
	}
	fmt.Fprintf(p.Log(""), "binding of %s is in place\n", d.ContainerName)
	return s.await(ctx, d, p, "its consumer directory", func(b operator.Binding) (bool, string) {
		return slices.ContainsFunc(b.Status.Consumers, func(c operator.ConsumerStatus) bool { return c.Directory != "" }), ""
	})
}

// retireOtherAgent removes the binding of a name the workspace holds on
// another agent than this host's configured one, as an earlier
// provisioning.wyrwood.agent bound it, before the name is recorded and bound on
// this one. The data keeps the recorded agent until Wyrwood confirms that
// binding gone, so a bind that then fails never loses it, and one Wyrwood
// answers is outside the grant fails the job and stays recorded.
func (s *Steps) retireOtherAgent(ctx context.Context, workspace, name string, p devcontainer.Progress) error {
	held, err := s.opts.Data.Held(ctx, workspace)
	if err != nil {
		return err
	}
	for _, n := range held.Names {
		if n.Name != name || n.Agent == "" || n.Agent == s.opts.Agent {
			continue
		}
		fmt.Fprintf(p.Log(""), "Menagerie's data records the binding of %s on Wyrwood agent %s, and provisioning.wyrwood.agent is %s; removing that binding before binding the name on %s\n", name, n.Agent, s.opts.Agent, s.opts.Agent)
		if err := s.removeBinding(ctx, workspace, n, p); err != nil {
			return err
		}
		fmt.Fprintf(p.Log(""), "binding of %s on %s is gone\n", name, n.Agent)
	}
	return nil
}

// unbind removes a binding and shows what part of the removal did not finish.
func (s *Steps) unbind(ctx context.Context, agent, name string, p devcontainer.Progress) error {
	result, err := s.opts.Bindings.Unbind(ctx, agent, name)
	if err != nil {
		return err
	}
	if result.Warning != "" {
		fmt.Fprintf(p.Log(""), "unbinding %s: %s\n", name, wyrwood.OneLine(result.Warning))
	}
	if result.TokenLive {
		fmt.Fprintf(p.Log(""), "unbinding %s: the container's token may stay valid until it expires\n", name)
	}
	return nil
}

// await reads the binding's status until check says it is done, says why it
// never will be, or the readiness wait passes. Each change of the status goes
// to the console, so that a wrong policy never looks like a hang.
func (s *Steps) await(ctx context.Context, d *Declaration, p devcontainer.Progress, what string, check func(operator.Binding) (bool, string)) error {
	deadline := time.Now().Add(s.opts.Wait)
	notReady := func(state string) error {
		return &jobs.Failure{Kind: KindNotReady, Err: fmt.Errorf("the binding of %s did not report %s within %s: %s", d.ContainerName, what, s.opts.Wait, state)}
	}
	var last string
	for {
		read, cancel := context.WithDeadline(ctx, deadline)
		binding, found, err := s.opts.Bindings.Read(read, d.ContainerName)
		passed := read.Err() != nil
		cancel()
		state := "the controller answered nothing yet"
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err != nil && !passed && !errors.Is(err, operator.ErrUnreachable):
			return err
		case err != nil:
			state = "the controller cannot be reached"
		case !found:
			return &jobs.Failure{Kind: KindNotReady, Err: fmt.Errorf("the binding of %s cannot be read: it is gone, or outside the operator key's grant", d.ContainerName)}
		default:
			state = describe(binding.Status)
			done, failure := check(binding)
			if done {
				return nil
			}
			if failure != "" {
				return &jobs.Failure{Kind: KindNotReady, Err: fmt.Errorf("the binding of %s cannot become ready: %s: %s", d.ContainerName, failure, state)}
			}
		}
		if state != last {
			fmt.Fprintf(p.Log(""), "waiting for %s: %s\n", what, state)
			last = state
		}
		if !time.Now().Before(deadline) {
			return notReady(state)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.opts.Poll):
		}
	}
}

// containerReady checks that the agent reports the container id with the
// declared access group, both sockets and no conformance failure. Comparing
// the id keeps the sockets of a rebuild's old container from counting. It
// reads Wyrwood's readiness, and never the token alone, which reports valid
// while a wrong directory owner leaves no sockets. A failure of the agent's
// report on this container cannot pass by waiting.
func containerReady(d *Declaration, id string) func(operator.Binding) (bool, string) {
	return func(b operator.Binding) (bool, string) {
		index := slices.IndexFunc(b.Status.Consumers, func(c operator.ConsumerStatus) bool { return c.ContainerID == id })
		if index < 0 || containerState(b, id) == "" {
			return false, ""
		}
		consumer := b.Status.Consumers[index]
		if consumer.Failed != "" {
			return false, "the agent reports " + wyrwood.OneLine(consumer.Failed)
		}
		if group := consumer.AccessGroup; group != nil && *group != d.Wyrwood.AccessGroup {
			return false, fmt.Sprintf("the consumer directory has access group %d, and the definition declares %d", *group, d.Wyrwood.AccessGroup)
		}
		if b.Status.Readiness != operator.ReadinessReady || consumer.AccessGroup == nil {
			return false, ""
		}
		for _, socket := range socketNames {
			if !slices.Contains(consumer.Sockets, socket) {
				return false, ""
			}
		}
		return true, ""
	}
}

// containerState is the reported state of the container id, or "" when the
// agent does not report it.
func containerState(b operator.Binding, id string) string {
	for _, c := range b.Status.Containers {
		if c.ID == id {
			return c.State
		}
	}
	return ""
}

// describe keeps a status to one line for the console. The controller's and
// the agent's text is not cleaned before this.
func describe(status operator.BindingStatus) string {
	parts := []string{"readiness " + wyrwood.OneLine(string(status.Readiness))}
	if !status.AgentConnected {
		parts = append(parts, "the agent is not connected")
	}
	if len(status.Reasons) > 0 {
		parts = append(parts, "reasons "+wyrwood.OneLine(strings.Join(status.Reasons, ", ")))
	}
	for _, c := range status.Containers {
		parts = append(parts, fmt.Sprintf("container %s %s", shortID(c.ID), wyrwood.OneLine(c.State)))
	}
	if status.LastError != "" {
		parts = append(parts, "last error "+wyrwood.OneLine(status.LastError))
	}
	if status.Reason != "" {
		parts = append(parts, "reason "+wyrwood.OneLine(status.Reason))
	}
	return strings.Join(parts, "; ")
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// retire ends a job that started the devcontainer: it unbinds each name
// Menagerie's data holds for the workspace that the definition no longer
// declares and revokes each key it no longer declares, all of them when the
// definition has no declaration, and removes each from the data once it is
// gone. A failure fails the job, with the devcontainer running, and leaves the
// rest for the next job.
func (s *Steps) retire(ctx context.Context, folder string, d *Declaration, p devcontainer.Progress) error {
	workspace := filepath.Base(folder)
	held, err := s.opts.Data.Held(ctx, workspace)
	if err != nil {
		return err
	}
	var names []store.Name
	for _, name := range held.Names {
		if d == nil || d.ContainerName != name.Name {
			names = append(names, name)
		}
	}
	var keys []Key
	for _, key := range held.Keys {
		if d == nil || d.Xolotl == nil || *d.Xolotl != key {
			keys = append(keys, key)
		}
	}
	if len(names) == 0 && len(keys) == 0 {
		return nil
	}
	p.Phase("retire")
	for _, name := range names {
		if err := s.removeBinding(ctx, workspace, name, p); err != nil {
			return err
		}
		fmt.Fprintf(p.Log(""), "unbound %s, which the definition no longer declares\n", name.Name)
	}
	for _, key := range keys {
		if err := s.revokeKey(ctx, workspace, key, p); err != nil {
			return err
		}
	}
	return nil
}

// removeBinding unbinds a name the workspace holds, by the agent and the name
// Menagerie's data recorded, never by what a read shows, so a binding the grant
// hides is not taken for a removed one. It removes the name from the data once
// Wyrwood confirms the binding gone. One Wyrwood answers as outside the grant
// keeps its name and fails as KindBindingOutsideGrant.
func (s *Steps) removeBinding(ctx context.Context, workspace string, name store.Name, p devcontainer.Progress) error {
	if err := s.reachesBinding(name); err != nil {
		return err
	}
	err := s.unbind(ctx, name.Agent, name.Name, p)
	if errors.Is(err, operator.ErrOutsideGrant) {
		return &jobs.Failure{Kind: KindBindingOutsideGrant, Err: fmt.Errorf("Wyrwood answers that the binding of %s on agent %s is outside the operator key's grant, so Menagerie cannot remove it; it stays in Menagerie's data for workspace %s until an operator widens the grant or removes the binding in the Wyrwood web interface", name.Name, name.Agent, workspace)}
	}
	if err != nil {
		return err
	}
	return s.opts.Data.ForgetName(ctx, workspace, name.Name)
}

// reachesBinding fails, as KindUnavailable, a name no job can unbind: one
// imported with no provisioning.wyrwood, or any with no such section now.
func (s *Steps) reachesBinding(name store.Name) error {
	if s.opts.Bindings == nil {
		return &jobs.Failure{Kind: KindUnavailable, Err: fmt.Errorf("Menagerie's data holds the binding of %s, and the configuration has no provisioning.wyrwood", name.Name)}
	}
	if name.Agent == "" {
		return &jobs.Failure{Kind: KindUnavailable, Err: fmt.Errorf("the binding of %s was imported with no provisioning.wyrwood, so Menagerie does not know its agent; a start with provisioning.wyrwood records it", name.Name)}
	}
	return nil
}

// revokeKey revokes a key the workspace holds, and removes it from the data
// once Xolotl confirms it gone.
func (s *Steps) revokeKey(ctx context.Context, workspace string, key Key, p devcontainer.Progress) error {
	if s.opts.Keys == nil {
		return &jobs.Failure{Kind: KindUnavailable, Err: fmt.Errorf("Menagerie's data holds Xolotl key %s, and the configuration has no provisioning.xolotl", key)}
	}
	revocation, err := s.opts.Keys.Revoke(ctx, key.Policy, key.Name)
	if err != nil {
		return err
	}
	if revocation == xolotl.Revoked {
		fmt.Fprintf(p.Log(""), "revoked Xolotl key %s\n", key)
	} else {
		fmt.Fprintf(p.Log(""), "Xolotl key %s is already gone\n", key)
	}
	return s.opts.Data.ForgetKey(ctx, workspace, key)
}
