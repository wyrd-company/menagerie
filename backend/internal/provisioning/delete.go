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
	"syscall"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/store"
)

// Delete removes a workspace from the host. It refuses while another running
// container mounts the folder, reads what the workspace holds from Menagerie's
// data, never from its definition, stops the devcontainer and runs save, which
// commits and backs the workspace up. A failure there removes nothing. It then
// unbinds, removes the containers, revokes the keys, and removes the socket
// folder and the workspace folder, and last the workspace's data. Each step
// treats what is already gone as done, so a retried delete finds what is left.
// A workspace whose folder is gone, listed as no-folder, is deleted by the same
// steps without the stop, the backup and the folder's removal.
func (s *Steps) Delete(ctx context.Context, folder string, p devcontainer.Progress, save func(context.Context) error) error {
	p.Phase("read-definition")
	others, err := s.opts.Runtime.MountingOutside(ctx, folder, p)
	if err != nil {
		return fmt.Errorf("listing the running containers failed: %w; nothing was removed", err)
	}
	if err := refuseMounted(others, p); err != nil {
		return err
	}
	workspace := filepath.Base(folder)
	held, err := s.opts.Data.Held(ctx, workspace)
	if err != nil {
		return err
	}
	if err := s.reaches(held); err != nil {
		return err
	}
	_, err = os.Lstat(folder)
	gone := errors.Is(err, os.ErrNotExist)
	if err != nil && !gone {
		return fmt.Errorf("reading the workspace folder failed: %w; nothing was removed", err)
	}
	if gone {
		fmt.Fprintln(p.Log(""), "the workspace's folder is gone, so only what Menagerie's data holds for it is removed")
	} else if err := s.saveBeforeRemoval(ctx, folder, p, save); err != nil {
		return err
	}

	if len(held.Names) > 0 {
		p.Phase("unbind")
	}
	for _, name := range held.Names {
		if err := s.removeBinding(ctx, workspace, name, p); err != nil {
			return err
		}
		fmt.Fprintf(p.Log(""), "binding of %s is gone\n", name.Name)
	}
	if err := s.opts.Runtime.RemoveContainers(ctx, folder, p); err != nil {
		return err
	}
	if len(held.Keys) > 0 {
		p.Phase("revoke-key")
	}
	for _, key := range held.Keys {
		if err := s.revokeKey(ctx, workspace, key, p); err != nil {
			return err
		}
	}
	// Every name and key is gone. Once the removal takes the repository, a retry
	// does not back up what is left of the folder, and no job starts its
	// devcontainer. The state ends while the empty folder still stands, so it
	// never outlives the folder it is about. A folder that is gone has none.
	if err := s.opts.Data.SetRemoving(ctx, workspace, folderID(folder)); err != nil {
		return err
	}
	err = s.opts.Runtime.RemoveFolder(ctx, folder, p, func() error { return s.opts.Data.SetRemoving(ctx, workspace, "") })
	if err != nil {
		return err
	}
	return s.opts.Data.RemoveWorkspace(ctx, workspace)
}

// reaches fails a delete, as KindUnavailable and with nothing changed, when the
// workspace holds a name or a key and configuration lacks the section that
// reaches it.
func (s *Steps) reaches(held store.Held) error {
	for _, name := range held.Names {
		if err := s.reachesBinding(name); err != nil {
			var failure *jobs.Failure
			if errors.As(err, &failure) {
				failure.Err = fmt.Errorf("%w; nothing was removed", failure.Err)
			}
			return err
		}
	}
	if len(held.Keys) > 0 && s.opts.Keys == nil {
		return &jobs.Failure{Kind: KindUnavailable, Err: fmt.Errorf("the workspace holds Xolotl key %s, and the configuration has no provisioning.xolotl; nothing was removed", held.Keys[0])}
	}
	return nil
}

// saveBeforeRemoval stops the devcontainer and runs save, unless an earlier
// delete already backed the folder up and began to remove it. A container that
// still mounts the folder after the stop refuses the delete.
func (s *Steps) saveBeforeRemoval(ctx context.Context, folder string, p devcontainer.Progress, save func(context.Context) error) error {
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
	began, err := s.opts.Data.Removing(ctx, filepath.Base(folder))
	if err != nil {
		return err
	}
	if removalBegan(began, folder) {
		fmt.Fprintln(p.Log(""), "an earlier delete backed this workspace up and began to remove its folder, so there is no backup to make")
		return nil
	}
	if err := save(ctx); err != nil {
		fmt.Fprintln(p.Log(""), stoppedNotice)
		return err
	}
	return nil
}

// folderID names a folder as the host's file system knows it, or is "" when it
// cannot be read, which no state matches.
func folderID(folder string) string {
	info, err := os.Lstat(folder)
	if err != nil {
		return ""
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

// removalBegan says that a delete backed this folder up and has taken its
// repository. The recorded folder alone does not say so: a removal that stopped
// before it took the repository leaves a folder that jobs can still change, and
// another folder can stand under this name. Each job that writes to a folder
// needs its repository, so with the repository gone nothing was written after
// the backup.
func removalBegan(removing, folder string) bool {
	if removing == "" || removing != folderID(folder) {
		return false
	}
	_, err := os.Lstat(filepath.Join(folder, ".git"))
	return errors.Is(err, os.ErrNotExist)
}

// refuseRemoving keeps a job from starting a devcontainer on what is left of a
// folder. Any other folder of this name was not touched, so the state ends.
func (s *Steps) refuseRemoving(ctx context.Context, folder string) error {
	workspace := filepath.Base(folder)
	removing, err := s.opts.Data.Removing(ctx, workspace)
	if err != nil || removing == "" {
		return err
	}
	if removalBegan(removing, folder) {
		return &jobs.Failure{Kind: KindDeleteUnfinished, Err: errors.New("a delete began to remove this workspace's folder; delete the workspace again to finish it")}
	}
	return s.opts.Data.SetRemoving(ctx, workspace, "")
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
