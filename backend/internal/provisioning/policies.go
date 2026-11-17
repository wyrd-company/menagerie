// ---
// relationships:
//   implements: menagerie
//   references: workspace-api
// ---

package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/wyrwood"
	"github.com/wyrd-company/wyrwood/operator"
)

// PolicyBindings provides revisioned binding reads and changes, and a fresh
// grant for every decision. The name-only Read can omit hidden bindings.
type PolicyBindings interface {
	Bindings
	Get(context.Context, string) (operator.Binding, error)
	Change(context.Context, operator.Binding, wyrwood.Selection) (operator.Binding, error)
	Policies(context.Context) (operator.Grant, []operator.Policy, error)
}

const (
	KindPolicyOutsideGrant = "policy-outside-grant"
	KindBindingChanged     = "binding-changed"
	KindBindingPending     = "binding-pending"
	KindBindingRefused     = "binding-refused"
)

func fail(kind, message string) error { return &jobs.Failure{Kind: kind, Err: errors.New(message)} }

// ApplyPolicies applies a stored selection without changing workspace files or
// starting its container. Existing bindings supply their own access group.
func (s *Steps) ApplyPolicies(ctx context.Context, folder string, p devcontainer.Progress) error {
	p.Phase("bind")
	if err := s.EnsureImported(ctx, folder); err != nil {
		return fail(KindUnavailable, err.Error())
	}
	workspace := filepath.Base(folder)
	held, err := s.opts.Data.Held(ctx, workspace)
	if err != nil {
		return err
	}
	return s.applySelection(ctx, folder, held.Current(), nil, p)
}

func (s *Steps) policyClient() (PolicyBindings, error) {
	client, ok := s.opts.Bindings.(PolicyBindings)
	if !ok {
		return nil, fail(KindUnavailable, "Configure provisioning.wyrwood with a controller that supports binding changes.")
	}
	return client, nil
}

func (s *Steps) desired(ctx context.Context, workspace string) (store.Selection, wyrwood.Selection, error) {
	current, err := s.opts.Data.Selection(ctx, workspace)
	if err != nil {
		return current, wyrwood.Selection{}, err
	}
	if !current.Imported {
		return current, wyrwood.Selection{}, fail(KindUnavailable, "The workspace's policy selection still needs to be imported from Wyrwood.")
	}
	if s.opts.Bindings == nil && len(current.Policies) == 0 {
		return current, wyrwood.Selection{Policies: []string{}, Values: map[string]string{}}, nil
	}
	client, err := s.policyClient()
	if err != nil {
		return current, wyrwood.Selection{}, err
	}
	_, policies, err := client.Policies(ctx)
	if err != nil {
		return current, wyrwood.Selection{}, err
	}
	allowed := []string{}
	valueNames := []string{}
	for _, p := range policies {
		allowed = append(allowed, p.Name)
		if slices.Contains(current.Policies, p.Name) {
			valueNames = append(valueNames, p.ValueNames...)
		}
	}
	if err = store.CheckAllowed(current.Policies, allowed); err != nil {
		return current, wyrwood.Selection{}, &jobs.Failure{Kind: KindPolicyOutsideGrant, Err: err}
	}
	identity, err := s.opts.Data.Identity(ctx, workspace, valueNames, nil)
	if err != nil {
		return current, wyrwood.Selection{}, err
	}
	values := map[string]string{}
	for _, name := range valueNames {
		values[name] = identity.Values[name]
	}
	return current, wyrwood.Selection{Policies: current.Policies, Values: values}, nil
}

func (s *Steps) applySelection(ctx context.Context, folder, name string, group *uint32, p devcontainer.Progress) (err error) {
	workspace := filepath.Base(folder)
	current, wanted, err := s.desired(ctx, workspace)
	defer func() {
		if err != nil {
			kind := "operation-failed"
			var failure *jobs.Failure
			if errors.As(err, &failure) {
				kind = failure.Kind
			}
			message := wyrwood.OneLine(err.Error())
			fmt.Fprintln(p.Log(""), message)
			// The job's context may have ended; saving its observed failure is bounded
			// separately so the workspace still shows why its selection was not applied.
			recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			err = errors.Join(err, s.opts.Data.ApplyFailed(recordCtx, workspace, current.Revision, store.ApplyFailure{Kind: kind, Message: message}))
		}
	}()
	if err != nil {
		return err
	}
	if name == "" {
		return s.opts.Data.Applied(ctx, workspace, current.Revision)
	}
	client, err := s.policyClient()
	if err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, s.opts.Wait)
	defer cancel()
	changed, pending := 0, 0
	for {
		binding, found, e := s.resolve(waitCtx, client, workspace, name)
		if errors.Is(e, operator.ErrNotFound) {
			if changed >= 1 {
				return fail(KindBindingChanged, "The binding of "+name+" disappeared again while Menagerie was applying its selection. Retry the job.")
			}
			changed++
			continue
		}
		if e != nil {
			return bindingFailure(name, e)
		}
		if group != nil {
			wanted.AccessGroup = *group
		} else if found && binding.Settings.AccessGroup != nil {
			wanted.AccessGroup = *binding.Settings.AccessGroup
		} else {
			definition, e := s.opts.Runtime.Definition(folder)
			if e != nil {
				return fail(KindInvalid, "Cannot read the definition for missing binding "+name+": "+e.Error())
			}
			declaration, e := Parse(definition)
			if e != nil {
				return fail(KindInvalid, "Cannot read a declaration for missing binding "+name+": "+e.Error())
			}
			if declaration == nil || declaration.ContainerName != name {
				return fail(KindInvalid, "The missing binding of "+name+" needs an accepted declaration naming that container.")
			}
			wanted.AccessGroup = declaration.Wyrwood.AccessGroup
		}
		if found && matches(binding, wanted) && binding.Applied {
			return s.opts.Data.Applied(ctx, workspace, current.Revision)
		}
		// Refresh the grant immediately before every mutation, including retries.
		_, policies, e := client.Policies(waitCtx)
		if e != nil {
			return e
		}
		allowed := []string{}
		for _, policy := range policies {
			allowed = append(allowed, policy.Name)
		}
		if e = store.CheckAllowed(wanted.Policies, allowed); e != nil {
			return &jobs.Failure{Kind: KindPolicyOutsideGrant, Err: e}
		}
		if found {
			reportChanges(binding, wanted, p)
			binding, e = client.Change(waitCtx, binding, wanted)
		} else {
			binding, e = client.Bind(waitCtx, name, wanted)
		}
		if binding.ID != "" {
			if recordErr := s.opts.Data.BindingID(ctx, workspace, name, binding.ID); recordErr != nil {
				return recordErr
			}
		}
		switch {
		case errors.Is(e, operator.ErrRevisionDiffers), errors.Is(e, operator.ErrBindingDiffers):
			if changed >= 1 {
				return fail(KindBindingChanged, "The binding of "+name+" changed twice while Menagerie was applying its selection. Retry the job.")
			}
			changed++
			// BindingDiffers means a different binding appeared under the name.
			if errors.Is(e, operator.ErrBindingDiffers) {
				if err = s.opts.Data.BindingID(ctx, workspace, name, ""); err != nil {
					return err
				}
			}
			continue
		case errors.Is(e, operator.ErrPending):
			if pending >= 3 {
				return fail(KindBindingPending, "Wyrwood saved the binding of "+name+" but OpenBao has not applied it after three retries. Retry the apply to finish it.")
			}
			pending++
			timer := time.NewTimer(s.opts.Poll)
			select {
			case <-waitCtx.Done():
				timer.Stop()
				return fail(KindBindingPending, "Wyrwood's binding of "+name+" is still pending: "+waitCtx.Err().Error())
			case <-timer.C:
			}
			continue
		case e != nil:
			return bindingFailure(name, e)
		}
		// Success is proved by the next read, never by the write's response alone.
	}
}

func (s *Steps) resolve(ctx context.Context, client PolicyBindings, workspace, name string) (operator.Binding, bool, error) {
	held, err := s.opts.Data.Held(ctx, workspace)
	if err != nil {
		return operator.Binding{}, false, err
	}
	for _, n := range held.Names {
		if n.Name != name {
			continue
		}
		if n.Agent != "" && n.Agent != s.opts.Agent {
			return operator.Binding{}, false, operator.ErrOutsideGrant
		}
		if n.BindingID != "" {
			binding, e := client.Get(ctx, n.BindingID)
			if e == nil {
				if binding.Agent != s.opts.Agent || binding.Key.Kind != operator.KeyName || binding.Key.Value != name {
					return operator.Binding{}, false, operator.ErrOutsideGrant
				}
				return binding, true, nil
			}
			if !errors.Is(e, operator.ErrNotFound) {
				return operator.Binding{}, false, e
			}
			if e = s.opts.Data.BindingID(ctx, workspace, name, ""); e != nil {
				return operator.Binding{}, false, e
			}
			return operator.Binding{}, false, operator.ErrNotFound
		}
	}
	binding, found, err := client.Read(ctx, name)
	if err == nil && found {
		err = s.opts.Data.BindingID(ctx, workspace, name, binding.ID)
	}
	return binding, found, err
}

func matches(binding operator.Binding, wanted wyrwood.Selection) bool {
	actual := slices.Clone(binding.Policies)
	desired := slices.Clone(wanted.Policies)
	slices.Sort(actual)
	slices.Sort(desired)
	return slices.Equal(actual, desired) && maps.Equal(binding.Settings.Values, wanted.Values) && binding.Settings.AccessGroup != nil && *binding.Settings.AccessGroup == wanted.AccessGroup
}

func bindingFailure(name string, err error) error {
	if errors.Is(err, operator.ErrOutsideGrant) {
		return fail(KindBindingOutsideGrant, "Wyrwood answers that the binding of "+name+" is outside the operator key's grant; edit the key's grant in the Wyrwood web interface, then retry the job.")
	}
	// Task 23's operator-key diagnostics stay intact.
	if errors.Is(err, operator.ErrNotApproved) || errors.Is(err, operator.ErrKeySelection) || errors.Is(err, operator.ErrUnreachable) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fail(KindBindingRefused, wyrwood.OneLine(err.Error()))
}

func reportChanges(before operator.Binding, wanted wyrwood.Selection, p devcontainer.Progress) {
	out := p.Log("")
	for _, policy := range wanted.Policies {
		if !slices.Contains(before.Policies, policy) {
			fmt.Fprintln(out, "adding policy", policy)
		}
	}
	for _, policy := range before.Policies {
		if !slices.Contains(wanted.Policies, policy) {
			fmt.Fprintln(out, "removing policy", policy)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(wanted.Values)) {
		value := wanted.Values[name]
		if old, ok := before.Settings.Values[name]; !ok || old != value {
			fmt.Fprintln(out, "setting value", name, "to", value)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(before.Settings.Values)) {
		if _, ok := wanted.Values[name]; !ok {
			fmt.Fprintln(out, "removing value", name)
		}
	}
	if before.Settings.AccessGroup == nil || *before.Settings.AccessGroup != wanted.AccessGroup {
		fmt.Fprintln(out, "setting access group to", wanted.AccessGroup)
	}
	if !before.Applied {
		fmt.Fprintln(out, "finishing Wyrwood's pending assignment")
	}
}

// PolicyNames is the sorted list returned by a fresh policy-list.
func PolicyNames(policies []operator.Policy) []string {
	names := []string{}
	for _, policy := range policies {
		names = append(names, policy.Name)
	}
	slices.Sort(names)
	return names
}

// ValidWorkspaceIdentity is shared by the create request and key issuance.
func ValidWorkspaceIdentity(name string) bool {
	return len(name) > 0 && len(name) <= 63 && xolotlNamePattern.MatchString(name)
}

// legacySelection reads only the migration members, independent of whether
// today's declaration parser accepts the rest of the block.
func legacySelection(def devcontainer.Definition) ([]string, map[string]string, *Key) {
	var raw struct {
		Wyrwood struct {
			Policies []string          `json:"policies"`
			Values   map[string]string `json:"values"`
		} `json:"wyrwood"`
	}
	_ = json.Unmarshal(def.Block, &raw)
	var key *Key
	if k, ok := declaredKey(def.Block); ok {
		key = &k
	}
	return raw.Wyrwood.Policies, raw.Wyrwood.Values, key
}

// EnsureImported takes over a pending selection before an edit's revision is
// checked, or before an apply can write anything to Wyrwood.
func (s *Steps) EnsureImported(ctx context.Context, folder string) error {
	workspace := filepath.Base(folder)
	current, err := s.opts.Data.Selection(ctx, workspace)
	if err != nil || current.Imported {
		return err
	}
	held, err := s.opts.Data.Held(ctx, workspace)
	if err != nil {
		return err
	}
	name := held.Current()
	def, _ := s.opts.Runtime.Definition(folder)
	policies, values, key := legacySelection(def)
	if name != "" {
		client, e := s.policyClient()
		if e != nil {
			return e
		}
		binding, found, e := client.Read(ctx, name)
		if e != nil {
			return e
		}
		if found {
			policies, values = binding.Policies, binding.Settings.Values
			if e = s.opts.Data.BindingID(ctx, workspace, name, binding.ID); e != nil {
				return e
			}
		}
	}
	if current.Key != nil {
		key = current.Key
	}
	return s.opts.Data.ImportSelection(ctx, workspace, policies, values, key)
}

// ImportSelections records import 5 and attempts every pending selection on
// start-up. A controller outage leaves the pending marks for the next request.
func (s *Steps) ImportSelections(ctx context.Context, root Workspaces, logf func(string, ...any)) error {
	_, selections, err := s.opts.Data.Policies(ctx)
	if err != nil {
		return err
	}
	_, err = s.opts.Data.Import(ctx, 5, func(tx *store.Tx) error {
		for _, name := range slices.Sorted(maps.Keys(selections)) {
			folder, e := root.FolderPath(name)
			if e != nil {
				return e
			}
			def, _ := s.opts.Runtime.Definition(folder)
			_, _, key := legacySelection(def)
			held, e := tx.Held(name)
			if e != nil {
				return e
			}
			pending := def.Block != nil && def.ContainerName != "" && held.Current() == def.ContainerName
			if e = tx.PrepareSelectionImport(name, key, pending); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	_, selections, err = s.opts.Data.Policies(ctx)
	if err != nil {
		return err
	}
	for _, name := range slices.Sorted(maps.Keys(selections)) {
		if selections[name].Imported {
			continue
		}
		folder, e := root.FolderPath(name)
		if e != nil {
			return e
		}
		if e = s.EnsureImported(ctx, folder); e != nil {
			logf("menagerie: policy selection of %s still needs importing: %s", name, wyrwood.OneLine(e.Error()))
			continue
		}
		imported, e := s.opts.Data.Selection(ctx, name)
		if e != nil {
			return e
		}
		for _, value := range slices.Sorted(maps.Keys(imported.Values)) {
			if imported.Values[value] != name {
				logf("menagerie: imported value %s=%s of workspace %s differs from its name", value, imported.Values[value], name)
			}
		}
	}
	return nil
}
