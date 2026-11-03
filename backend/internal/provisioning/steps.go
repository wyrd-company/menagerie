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
// repeat, so a start retries a job that failed.
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
	RemoveFolder(ctx context.Context, folder string, p devcontainer.Progress) error
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

// Bindings manages Wyrwood bindings. *wyrwood.Client satisfies it.
type Bindings interface {
	Bind(ctx context.Context, name string, selection wyrwood.Selection) (operator.Binding, error)
	Unbind(ctx context.Context, name string) (operator.UnbindResult, error)
	Read(ctx context.Context, name string) (operator.Binding, bool, error)
}

// Keys issues and revokes Xolotl client keys. *xolotl.Client satisfies it.
type Keys interface {
	Issue(ctx context.Context, policy, name string) (xolotl.Issued, error)
	Revoke(ctx context.Context, policy, name string) (xolotl.Revocation, error)
}

// Workspaces lists the workspaces under the root. *workspace.Root satisfies
// it.
type Workspaces interface {
	Names() ([]string, error)
	FolderPath(name string) (string, error)
}

// Clients are the host's provisioning services. A nil one is a section that
// configuration lacks.
type Clients struct {
	Bindings Bindings
	Keys     Keys
	// Wait bounds each wait on a binding's status; zero is
	// DefaultReadinessWait.
	Wait time.Duration
}

// Options compose Steps.
type Options struct {
	Clients
	Runtime    Runtime
	Workspaces Workspaces
	Records    Records
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
	declaration, err := s.declaration(folder, p)
	if err != nil {
		return err
	}
	if declaration != nil {
		err = s.provision(ctx, folder, declaration, p, kind)
	} else {
		err = s.plain(ctx, folder, p, kind)
	}
	if err != nil {
		return err
	}
	if err := s.consume(ctx, folder, declaration, p); err != nil {
		return &AfterStartError{Step: "retiring what the definition no longer declares", Err: err}
	}
	return nil
}

// AfterStartError says that a job started the devcontainer and a later step
// failed: verify, or consuming the retirement record, which keeps the rest for
// the next job. A job's message must not say that the devcontainer did not
// start.
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
	if err := s.issueKey(ctx, d, p, existing); err != nil {
		return err
	}
	if err := s.bind(ctx, folder, d, p); err != nil {
		return err
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

// retiredContainer says that the workspace's container still has a name its
// record retires: a rebuild that would have removed the block failed, and
// consuming the record would take what that container uses.
func (s *Steps) retiredContainer(ctx context.Context, folder string) (bool, error) {
	record, err := s.opts.Records.Read(filepath.Base(folder))
	if err != nil {
		return false, err
	}
	for _, name := range record.Names {
		owner, found, err := s.opts.Runtime.ContainerHolder(ctx, name)
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

// notTaken refuses a container name or Xolotl key that another workspace's
// definition declares, its retirement record names, or a container of another
// workspace holds, so that the delete of one workspace cannot revoke another's
// key.
func (s *Steps) notTaken(ctx context.Context, folder string, d *Declaration) error {
	others, err := s.otherHolders(folder, true)
	if err != nil {
		return err
	}
	refuse := func(what, holder string) error {
		return &jobs.Failure{Kind: KindInvalid, Err: fmt.Errorf("%s is already taken: %s", what, holder)}
	}
	if holder, taken := others.names[d.ContainerName]; taken {
		return refuse("container name "+d.ContainerName, holder)
	}
	if d.Xolotl != nil {
		if holder, taken := others.keys[*d.Xolotl]; taken {
			return refuse("Xolotl key "+d.Xolotl.String(), holder)
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

// holders says which workspace holds each name and key.
type holders struct {
	names map[string]string
	keys  map[Key]string
}

// otherHolders reads what every other workspace declares and, with records,
// keeps in its retirement record. A definition that cannot be read holds
// nothing, and a block that is not valid still holds what it names.
func (s *Steps) otherHolders(folder string, records bool) (holders, error) {
	out := holders{names: map[string]string{}, keys: map[Key]string{}}
	if s.opts.Workspaces == nil {
		return out, nil
	}
	names, err := s.opts.Workspaces.Names()
	if err != nil {
		return out, err
	}
	for _, name := range names {
		other, err := s.opts.Workspaces.FolderPath(name)
		if err != nil || other == folder {
			continue
		}
		if def, err := s.opts.Runtime.Definition(other); err == nil && def.Block != nil {
			if def.ContainerName != "" {
				out.names[def.ContainerName] = "the definition of " + name + " declares it"
			}
			if key, ok := declaredKey(def.Block); ok {
				out.keys[key] = "the definition of " + name + " declares it"
			}
		}
		if !records {
			continue
		}
		record, err := s.opts.Records.Read(name)
		if err != nil {
			return out, err
		}
		for _, held := range record.Names {
			out.names[held] = "the retirement record of " + name + " names it"
		}
		for _, key := range record.Keys {
			out.keys[key] = "the retirement record of " + name + " names it"
		}
	}
	return out, nil
}

// issueKey issues the declared key. The answer that it exists is done, so a
// repeat never rotates it, and the console shows what a repeat cannot change.
func (s *Steps) issueKey(ctx context.Context, d *Declaration, p devcontainer.Progress, existing bool) error {
	if d.Xolotl == nil {
		return nil
	}
	p.Phase("issue-key")
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

// bind makes the binding the definition declares, and waits until it reports
// its consumer directory, because a bind source must exist for a create. An
// equal binding is not written, so a repeat revokes no token. The definition
// is the source of the three values Wyrwood will not change, so a binding that
// differs is replaced while its container is stopped.
func (s *Steps) bind(ctx context.Context, folder string, d *Declaration, p devcontainer.Progress) error {
	p.Phase("bind")
	selection := wyrwood.Selection{Policies: d.Wyrwood.Policies, AccessGroup: d.Wyrwood.AccessGroup, Values: d.Wyrwood.Values}
	_, err := s.opts.Bindings.Bind(ctx, d.ContainerName, selection)
	if errors.Is(err, operator.ErrBindingDiffers) {
		err = s.rebind(ctx, folder, d, selection, p)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(p.Log(""), "binding of %s is in place\n", d.ContainerName)
	return s.await(ctx, d, p, "its consumer directory", func(b operator.Binding) (bool, string) {
		return slices.ContainsFunc(b.Status.Consumers, func(c operator.ConsumerStatus) bool { return c.Directory != "" }), ""
	})
}

func (s *Steps) rebind(ctx context.Context, folder string, d *Declaration, selection wyrwood.Selection, p devcontainer.Progress) error {
	current, found, err := s.opts.Bindings.Read(ctx, d.ContainerName)
	if err != nil {
		return err
	}
	if !found {
		return &jobs.Failure{Kind: KindNotReady, Err: fmt.Errorf("the binding of %s differs from the definition, and it cannot be read", d.ContainerName)}
	}
	fmt.Fprintf(p.Log(""), "the binding of %s differs from the definition in %s; stopping the container and binding it again\n", d.ContainerName, strings.Join(differences(current, selection), ", "))
	if err := s.opts.Runtime.Stop(ctx, folder, p); err != nil {
		return err
	}
	p.Phase("bind")
	if err := s.unbind(ctx, d.ContainerName, p); err != nil {
		return err
	}
	_, err = s.opts.Bindings.Bind(ctx, d.ContainerName, selection)
	return err
}

// differences names the fields of a binding that a selection changes.
func differences(current operator.Binding, selection wyrwood.Selection) []string {
	var fields []string
	policies, wanted := slices.Sorted(slices.Values(current.Policies)), slices.Sorted(slices.Values(selection.Policies))
	if !slices.Equal(policies, wanted) {
		fields = append(fields, "policies")
	}
	if group := current.Settings.AccessGroup; group == nil || *group != selection.AccessGroup {
		fields = append(fields, "access group")
	}
	if !sameValues(current.Settings.Values, selection.Values) {
		fields = append(fields, "values")
	}
	if len(fields) == 0 {
		return []string{"its settings"}
	}
	return fields
}

func sameValues(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for name, value := range a {
		if other, ok := b[name]; !ok || other != value {
			return false
		}
	}
	return true
}

// unbind removes a binding and shows what part of the removal did not finish.
func (s *Steps) unbind(ctx context.Context, name string, p devcontainer.Progress) error {
	result, err := s.opts.Bindings.Unbind(ctx, name)
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

// Retire adds what the definition declares to the workspace's retirement
// record, before a job changes the definition. A block that cannot be read
// has given nothing that the job could retire.
func (s *Steps) Retire(folder string, p devcontainer.Progress) error {
	def, err := s.opts.Runtime.Definition(folder)
	if err != nil || def.Block == nil {
		return nil
	}
	d, err := Parse(def)
	if err != nil {
		fmt.Fprintf(p.Log(""), "nothing to retire: %v\n", err)
		return nil
	}
	name := filepath.Base(folder)
	record, err := s.opts.Records.Read(name)
	if err != nil {
		return err
	}
	return s.opts.Records.Write(name, record.add(d))
}

// consume ends a job that started the devcontainer: it unbinds each name and
// revokes each key of the workspace's record that no definition declares any
// more, and removes each entry once it is gone. An entry this or another
// workspace declares is removed without acting, so one workspace's retirement
// never takes what another uses. A failure fails the job, with the devcontainer
// running, and leaves the rest for the next job.
func (s *Steps) consume(ctx context.Context, folder string, d *Declaration, p devcontainer.Progress) error {
	name := filepath.Base(folder)
	record, err := s.opts.Records.Read(name)
	if err != nil || record.empty() {
		return err
	}
	p.Phase("retire")
	// Another workspace's record entry is its own to retire; only a
	// declaration keeps a name in use.
	others, err := s.otherHolders(folder, false)
	if err != nil {
		return err
	}
	for len(record.Names) > 0 {
		retired := record.Names[0]
		declared := (d != nil && d.ContainerName == retired) || others.names[retired] != ""
		if !declared {
			if s.opts.Bindings == nil {
				return &jobs.Failure{Kind: KindUnavailable, Err: fmt.Errorf("the retirement record of %s names the binding of %s, and the configuration has no provisioning.wyrwood", name, retired)}
			}
			if err := s.unbind(ctx, retired, p); err != nil {
				return err
			}
			fmt.Fprintf(p.Log(""), "unbound %s, which the definition no longer declares\n", retired)
		}
		record.Names = record.Names[1:]
		if err := s.opts.Records.Write(name, record); err != nil {
			return err
		}
	}
	for len(record.Keys) > 0 {
		retired := record.Keys[0]
		declared := (d != nil && d.Xolotl != nil && *d.Xolotl == retired) || others.keys[retired] != ""
		if !declared {
			if s.opts.Keys == nil {
				return &jobs.Failure{Kind: KindUnavailable, Err: fmt.Errorf("the retirement record of %s names Xolotl key %s, and the configuration has no provisioning.xolotl", name, retired)}
			}
			if _, err := s.opts.Keys.Revoke(ctx, retired.Policy, retired.Name); err != nil {
				return err
			}
			fmt.Fprintf(p.Log(""), "revoked Xolotl key %s, which the definition no longer declares\n", retired)
		}
		record.Keys = record.Keys[1:]
		if err := s.opts.Records.Write(name, record); err != nil {
			return err
		}
	}
	return nil
}
