// ---
// relationships:
//   implements: menagerie
//   references: [menagerie-data-loss, provisioning-declaration]
// ---

package provisioning

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// The first-start imports this package runs. Each has a mark in Menagerie's
// data and runs at the first start whose schema has it, so a start that finds
// no database runs every one.
const (
	// ImportWorkspaces gives each workspace folder its workspace.
	ImportWorkspaces = 1
	// ImportProvisioned takes over the container names and Xolotl keys that a
	// host of an earlier release keeps in its retirement records, its
	// definitions and its containers.
	ImportProvisioned = 2
)

// ImportRuntime is what the import reads of the devcontainer runtime.
// *devcontainer.Runtime satisfies it.
type ImportRuntime interface {
	Definition(folder string) (devcontainer.Definition, error)
	ContainerHolder(ctx context.Context, name string) (folder string, found bool, err error)
	ContainerNames(ctx context.Context, folder string) ([]string, error)
}

// Workspaces lists the workspace folders under the root. *workspace.Root
// satisfies it.
type Workspaces interface {
	Names() ([]string, error)
	FolderPath(name string) (string, error)
}

// ImportOptions compose Import.
type ImportOptions struct {
	Data       *store.Store
	Runtime    ImportRuntime
	Workspaces Workspaces
	// Records are the retirement records of an earlier release.
	Records Records
	// Agent is provisioning.wyrwood.agent, or "" without that section.
	Agent string
	// Logf writes one line to the start log.
	Logf func(format string, args ...any)
}

// Import runs, before the workspace service serves, the imports of the first
// start that this package owns, each in one transaction with its mark, and
// then records Agent on each container name imported without one. A start with
// no database runs every import, so a lost data volume recovers what the
// workspaces, their definitions, their containers and the records still hold.
func Import(ctx context.Context, o ImportOptions) error {
	folders, err := o.Workspaces.Names()
	if err != nil {
		return err
	}
	ran, err := o.Data.Import(ctx, ImportWorkspaces, func(tx *store.Tx) error {
		for _, name := range folders {
			if err := tx.AddWorkspace(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("import the workspaces: %w", err)
	}
	if ran {
		o.Logf("menagerie: imported %d workspaces into Menagerie's data", len(folders))
	}

	recorded, err := o.Records.Workspaces()
	if err != nil {
		return err
	}
	var plan *importPlan
	ran, err = o.Data.Import(ctx, ImportProvisioned, func(tx *store.Tx) error {
		plan, err = o.plan(ctx, folders, recorded)
		if err != nil {
			return err
		}
		return plan.write(tx, o.Agent)
	})
	if err != nil {
		return fmt.Errorf("import the provisioning records: %w", err)
	}
	if ran {
		o.Logf("menagerie: imported %d container names and %d Xolotl keys of earlier provisioning into Menagerie's data", len(plan.names), len(plan.keys))
		if o.Agent == "" && len(plan.names) > 0 {
			o.Logf("menagerie: the imported container names have no Wyrwood agent, because the configuration has no provisioning.wyrwood; a job that would bind or unbind one fails until a start with that section records its agent")
		}
		for _, name := range recorded {
			if err := o.Records.Remove(name); err != nil {
				o.Logf("menagerie: %v", err)
			}
		}
	}

	if o.Agent != "" {
		n, err := o.Data.RecordAgent(ctx, o.Agent)
		if err != nil {
			return err
		}
		if n > 0 {
			o.Logf("menagerie: recorded Wyrwood agent %s on %d imported container names", o.Agent, n)
		}
	}
	return nil
}

// claim is a workspace that would hold a name or a key, and whether its
// declaration names it, its record or its containers do, or both.
type claim struct {
	workspace string
	declared  bool
	other     bool
}

// heldOutside is a declared name that a container of another folder holds.
type heldOutside struct {
	workspace, name, owner string
}

// importPlan is what import 2 writes.
type importPlan struct {
	workspaces []string
	// folders are the workspaces' folder paths.
	folders  map[string]string
	removing map[string]string
	names    map[string][]claim
	keys     map[Key][]claim
	outside  []heldOutside
	logf     func(format string, args ...any)
	holder   func(name string) (string, bool, error)
}

func addClaim[K comparable](claims map[K][]claim, key K, workspace string, declared bool) {
	for i, c := range claims[key] {
		if c.workspace == workspace {
			claims[key][i].declared = c.declared || declared
			claims[key][i].other = c.other || !declared
			return
		}
	}
	claims[key] = append(claims[key], claim{workspace: workspace, declared: declared, other: !declared})
}

// plan reads what each workspace holds: the names and keys its retirement
// record names, the name and the key its declaration names, a block that is not
// valid included, and, when its definition cannot be read and Agent is set, the
// names of its containers. A declared name that a container outside the
// workspace holds is not taken.
func (o ImportOptions) plan(ctx context.Context, folders, recorded []string) (*importPlan, error) {
	p := &importPlan{
		folders: map[string]string{}, removing: map[string]string{},
		names: map[string][]claim{}, keys: map[Key][]claim{}, logf: o.Logf,
		holder: func(name string) (string, bool, error) { return o.Runtime.ContainerHolder(ctx, name) },
	}
	all := slices.Sorted(slices.Values(slices.Concat(folders, recorded)))
	for _, name := range slices.Compact(all) {
		folder, err := o.Workspaces.FolderPath(name)
		if err != nil {
			o.Logf("menagerie: the retirement record %s.json names no workspace, and is not imported", name)
			continue
		}
		p.workspaces = append(p.workspaces, name)
		p.folders[name] = folder
		record, err := o.Records.Read(name)
		if err != nil {
			return nil, err
		}
		for _, held := range record.Names {
			addClaim(p.names, held, name, false)
		}
		for _, key := range record.Keys {
			addClaim(p.keys, key, name, false)
		}
		if record.Removing != "" {
			p.removing[name] = record.Removing
		}
		if !slices.Contains(folders, name) {
			continue
		}
		if err := o.declared(ctx, p, name, folder); err != nil {
			return nil, err
		}
	}
	p.dropHeldOutside()
	return p, nil
}

// dropHeldOutside keeps a declared name that a container of another folder
// holds from the workspace that declares it. Only when that container's
// workspace declares the name too does the claim stand, so that arbitration
// gives it to the container's workspace and the start log names both.
func (p *importPlan) dropHeldOutside() {
	for _, o := range p.outside {
		claims := p.names[o.name]
		if slices.ContainsFunc(claims, func(c claim) bool { return c.declared && p.folders[c.workspace] == o.owner }) {
			continue
		}
		p.logf("menagerie: %s declares container name %s, which a container outside it holds, so the declaration does not take the name", o.workspace, o.name)
		i := slices.IndexFunc(claims, func(c claim) bool { return c.workspace == o.workspace })
		if claims[i].other {
			claims[i].declared = false
			continue
		}
		p.names[o.name] = slices.Delete(claims, i, i+1)
		if len(p.names[o.name]) == 0 {
			delete(p.names, o.name)
		}
	}
}

// declared adds what a workspace's definition declares, or, when it cannot be
// read, what its containers are called.
func (o ImportOptions) declared(ctx context.Context, p *importPlan, name, folder string) error {
	def, err := o.Runtime.Definition(folder)
	if err != nil {
		if o.Agent == "" {
			return nil
		}
		containers, err := o.Runtime.ContainerNames(ctx, folder)
		if err != nil {
			return err
		}
		for _, container := range containers {
			addClaim(p.names, container, name, false)
		}
		if len(containers) > 0 {
			o.Logf("menagerie: the definition of %s cannot be read, so its containers' names %s are imported for retirement", name, strings.Join(containers, ", "))
		}
		return nil
	}
	if def.Block == nil {
		return nil
	}
	if def.ContainerName != "" {
		owner, found, err := o.Runtime.ContainerHolder(ctx, def.ContainerName)
		if err != nil {
			return err
		}
		if found && owner != folder {
			p.outside = append(p.outside, heldOutside{workspace: name, name: def.ContainerName, owner: owner})
		}
		addClaim(p.names, def.ContainerName, name, true)
	}
	if key, ok := declaredKey(def.Block); ok {
		addClaim(p.keys, key, name, true)
	}
	return nil
}

// write records the plan: each workspace, the folder a delete began to remove,
// and each name and key with the workspace that wins it. The name a winner's
// declaration names is its current one.
func (p *importPlan) write(tx *store.Tx, agent string) error {
	for _, name := range p.workspaces {
		if err := tx.AddWorkspace(name); err != nil {
			return err
		}
		if removing := p.removing[name]; removing != "" {
			if err := tx.SetRemoving(name, removing); err != nil {
				return err
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(p.names)) {
		won, err := p.arbitrate("container name "+name, p.names[name], func(candidates []claim) (string, error) {
			owner, found, err := p.holder(name)
			if err != nil || !found {
				return "", err
			}
			for _, c := range candidates {
				if p.folders[c.workspace] == owner {
					return c.workspace, nil
				}
			}
			return "", nil
		})
		if err != nil {
			return err
		}
		if err := tx.HoldName(won.workspace, name, agent, won.declared); err != nil {
			return err
		}
	}
	keys := slices.SortedFunc(maps.Keys(p.keys), func(a, b Key) int { return strings.Compare(a.String(), b.String()) })
	for _, key := range keys {
		won, err := p.arbitrate("Xolotl key "+key.String(), p.keys[key], nil)
		if err != nil {
			return err
		}
		if err := tx.HoldKey(won.workspace, key); err != nil {
			return err
		}
	}
	return nil
}

// arbitrate gives a name or key that several workspaces would hold to one: the
// one whose declaration names it; when both or neither declare it, the one
// whose container holds it, which container finds, else the first in name
// order. The start log names them all.
func (p *importPlan) arbitrate(what string, claims []claim, container func([]claim) (string, error)) (claim, error) {
	if len(claims) == 1 {
		return claims[0], nil
	}
	slices.SortFunc(claims, func(a, b claim) int { return strings.Compare(a.workspace, b.workspace) })
	candidates := slices.DeleteFunc(slices.Clone(claims), func(c claim) bool { return !c.declared })
	if len(candidates) == 0 {
		candidates = claims
	}
	won, why := candidates[0], "it is first in name order"
	if len(candidates) == 1 {
		why = "its declaration names it"
	} else if container != nil {
		holder, err := container(candidates)
		if err != nil {
			return claim{}, err
		}
		if i := slices.IndexFunc(candidates, func(c claim) bool { return c.workspace == holder }); holder != "" && i >= 0 {
			won, why = candidates[i], "its container holds the name"
		}
	}
	var names []string
	for _, c := range claims {
		names = append(names, c.workspace)
	}
	p.logf("menagerie: %s would belong to %s; it goes to %s, because %s, and a provisioning job of the others that declares it fails until it is gone", what, strings.Join(names, " and "), won.workspace, why)
	return won, nil
}
