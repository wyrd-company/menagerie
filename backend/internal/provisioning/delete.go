// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, provisioning-declaration, workspace-delete-loses-ignored-files]
// ---

package provisioning

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/xolotl"
)

// Delete removes a workspace from the host. It refuses while another running
// container mounts the folder, reads what the workspace holds, stops the
// devcontainer and runs save, which commits and backs the workspace up. A
// failure there removes nothing. It then unbinds, removes the containers,
// revokes the key, and removes the socket folder and the workspace folder.
// Each step treats what is already gone as done, and the folder goes last, so a
// retried delete can still read its declaration.
func (s *Steps) Delete(ctx context.Context, folder string, p devcontainer.Progress, save func(context.Context) error) error {
	p.Phase("read-definition")
	others, err := s.opts.Runtime.MountingOutside(ctx, folder, p)
	if err != nil {
		return fmt.Errorf("listing the running containers failed: %w; nothing was removed", err)
	}
	if err := refuseMounted(others, p); err != nil {
		return err
	}
	held, err := s.held(ctx, folder, p)
	if err != nil {
		return err
	}
	if len(held.Names) > 0 && s.opts.Bindings == nil {
		return &jobs.Failure{Kind: KindUnavailable, Err: fmt.Errorf("the workspace holds the binding of %s, and the configuration has no provisioning.wyrwood; nothing was removed", held.Names[0])}
	}
	if len(held.Keys) > 0 && s.opts.Keys == nil {
		return &jobs.Failure{Kind: KindUnavailable, Err: fmt.Errorf("the workspace holds Xolotl key %s, and the configuration has no provisioning.xolotl; nothing was removed", held.Keys[0])}
	}

	if err := s.opts.Runtime.Stop(ctx, folder, p); err != nil {
		return err
	}
	// A container that started since the first check, or that the stop left,
	// could still write to the folder.
	still, err := s.opts.Runtime.Mounting(ctx, folder)
	if err != nil {
		return fmt.Errorf("listing the running containers failed: %w; nothing was removed", err)
	}
	if err := refuseMounted(still, p); err != nil {
		fmt.Fprintln(p.Log(""), stoppedNotice)
		return err
	}
	began, err := s.opts.Records.Read(filepath.Base(folder))
	if err != nil {
		return err
	}
	if removalBegan(began, folder) {
		fmt.Fprintln(p.Log(""), "an earlier delete backed this workspace up and began to remove its folder, so there is no backup to make")
	} else if err := save(ctx); err != nil {
		fmt.Fprintln(p.Log(""), stoppedNotice)
		return err
	}

	if len(held.Names) > 0 {
		p.Phase("unbind")
	}
	for _, name := range held.Names {
		if err := s.unbind(ctx, name, p); err != nil {
			return err
		}
		fmt.Fprintf(p.Log(""), "binding of %s is gone\n", name)
	}
	if err := s.opts.Runtime.RemoveContainers(ctx, folder, p); err != nil {
		return err
	}
	if len(held.Keys) > 0 {
		p.Phase("revoke-key")
	}
	for _, key := range held.Keys {
		revocation, err := s.opts.Keys.Revoke(ctx, key.Policy, key.Name)
		if err != nil {
			return err
		}
		switch revocation {
		case xolotl.Revoked:
			fmt.Fprintf(p.Log(""), "revoked Xolotl key %s\n", key)
		default:
			fmt.Fprintf(p.Log(""), "Xolotl key %s is already gone\n", key)
		}
	}
	// Every name and key of the record is done, and a retried delete acts on the
	// declaration again while the folder has one. Once the removal takes the
	// repository, a retry does not back up what is left of the folder, and no job
	// starts its devcontainer.
	if err := s.opts.Records.Write(filepath.Base(folder), Record{Removing: true}); err != nil {
		return err
	}
	if err := s.opts.Runtime.RemoveFolder(ctx, folder, p); err != nil {
		return err
	}
	return s.opts.Records.Write(filepath.Base(folder), Record{})
}

// removalBegan says that a delete backed the folder up and has taken its
// repository. The record alone does not say so: a removal that stopped before
// it took the repository leaves a folder that jobs can still change, and a folder
// of this name can be new. Each job that writes to a folder needs its repository,
// so with the repository gone nothing was written after the backup.
func removalBegan(record Record, folder string) bool {
	if !record.Removing {
		return false
	}
	_, err := os.Lstat(filepath.Join(folder, ".git"))
	return errors.Is(err, os.ErrNotExist)
}

// refuseRemoving keeps a job from starting a devcontainer on what is left of a
// folder. A folder that still has its repository was not touched, so the state
// ends.
func (s *Steps) refuseRemoving(folder string) error {
	name := filepath.Base(folder)
	record, err := s.opts.Records.Read(name)
	if err != nil || !record.Removing {
		return err
	}
	if removalBegan(record, folder) {
		return &jobs.Failure{Kind: KindDeleteUnfinished, Err: errors.New("a delete began to remove this workspace's folder; delete the workspace again to finish it")}
	}
	record.Removing = false
	return s.opts.Records.Write(name, record)
}

// stoppedNotice is the console line of a delete that stopped the devcontainer
// and removed nothing.
const stoppedNotice = "nothing was removed, and the devcontainer is stopped; start it to go on working, or delete it again"

// refuseMounted fails a delete while a running container mounts the folder, and
// the console names each by its short id.
func refuseMounted(ids []string, p devcontainer.Progress) error {
	if len(ids) == 0 {
		return nil
	}
	for _, id := range ids {
		fmt.Fprintf(p.Log(""), "running container %s mounts the workspace folder\n", id)
	}
	return &jobs.Failure{Kind: KindDeleteRefused, Err: errors.New("a running container other than the workspace's own mounts the workspace folder; nothing was removed")}
}

// held is what a delete unbinds and revokes: what the workspace's retirement
// record names and what its definition declares. A block that is not valid still
// names what it names, because an earlier definition may have provisioned it,
// and without the section it needs it names nothing, so that it cannot hold up
// the delete. What another workspace's definition declares, or a declared name
// that a container outside this workspace holds, stays: its create was refused,
// so nothing was issued for it.
func (s *Steps) held(ctx context.Context, folder string, p devcontainer.Progress) (Record, error) {
	record, err := s.opts.Records.Read(filepath.Base(folder))
	if err != nil {
		return Record{}, err
	}
	declared, err := s.declared(ctx, folder, p)
	if err != nil {
		return Record{}, err
	}
	if declared.ContainerName != "" {
		record = record.add(&declared)
	} else if declared.Xolotl != nil && !containsKey(record.Keys, *declared.Xolotl) {
		record.Keys = append(record.Keys, *declared.Xolotl)
	}
	others, err := s.otherHolders(folder, false)
	if err != nil {
		return Record{}, err
	}
	var out Record
	for _, name := range record.Names {
		if holder := others.names[name]; holder != "" {
			fmt.Fprintf(p.Log(""), "the binding of %s stays: %s\n", name, holder)
			continue
		}
		out.Names = append(out.Names, name)
	}
	for _, key := range record.Keys {
		if holder := others.keys[key]; holder != "" {
			fmt.Fprintf(p.Log(""), "Xolotl key %s stays: %s\n", key, holder)
			continue
		}
		out.Keys = append(out.Keys, key)
	}
	return out, nil
}

// declared reads the name and key the definition declares.
func (s *Steps) declared(ctx context.Context, folder string, p devcontainer.Progress) (Declaration, error) {
	def, err := s.opts.Runtime.Definition(folder)
	if err != nil {
		fmt.Fprintf(p.Log(""), "the definition cannot be read, so only the retirement record says what to unbind and revoke: %v\n", err)
		return Declaration{}, nil
	}
	if def.Block == nil {
		return Declaration{}, nil
	}
	var out Declaration
	_, invalid := Parse(def)
	if invalid != nil {
		fmt.Fprintf(p.Log(""), "the declaration is not valid, so only what it names and the configuration can reach is removed: %v\n", invalid)
	}
	if invalid == nil || s.opts.Bindings != nil {
		out.ContainerName = def.ContainerName
	}
	if key, ok := declaredKey(def.Block); ok && (invalid == nil || s.opts.Keys != nil) {
		out.Xolotl = &key
	}
	if out.ContainerName == "" {
		return out, nil
	}
	owner, found, err := s.opts.Runtime.ContainerHolder(ctx, out.ContainerName)
	if err != nil {
		return Declaration{}, err
	}
	if found && owner != folder {
		fmt.Fprintf(p.Log(""), "a container outside this workspace has the declared name %s, so its binding and key stay\n", out.ContainerName)
		return Declaration{}, nil
	}
	return out, nil
}

func containsKey(keys []Key, key Key) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}
