// ---
// relationships:
//   implements: [menagerie, choose-a-backup-repository-per-workspace]
// ---

package backup

import (
	"context"
	"errors"
	"fmt"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"
)

// BranchCommit reads the commit the repository's branch of the workspace's
// name is at, from the refs the repository advertises, and fetches
// nothing. found is false when the repository has no such branch or no ref
// at all.
func (r *Remote) BranchCommit(ctx context.Context, workspace string) (commit plumbing.Hash, found bool, err error) {
	branch, err := Branch(workspace)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	s, err := r.open(ctx, nil)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	defer s.close()
	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: remoteName, URLs: []string{r.URL}})
	refs, err := remote.ListContext(s.ctx, &git.ListOptions{Auth: s.auth})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return plumbing.ZeroHash, false, nil
	}
	if err != nil {
		return plumbing.ZeroHash, false, fmt.Errorf("list branches: %w", transportFailure(s.ctx, r.URL, err))
	}
	for _, ref := range refs {
		if ref.Name() == branch && ref.Type() == plumbing.HashReference {
			return ref.Hash(), true, nil
		}
	}
	return plumbing.ZeroHash, false, nil
}

// InHistory says whether commit is the workspace branch's commit or one of
// its ancestors in the workspace's own repository. A commit the repository
// does not hold is not.
func InHistory(folder, workspace string, commit plumbing.Hash) (bool, error) {
	repo, branch, err := open(folder, workspace)
	if err != nil {
		return false, err
	}
	head, err := repo.Reference(branch, false)
	if err != nil {
		return false, fmt.Errorf("workspace branch %s has no commit: %w", branch.Short(), err)
	}
	if head.Hash() == commit {
		return true, nil
	}
	candidate, err := repo.CommitObject(commit)
	if errors.Is(err, plumbing.ErrObjectNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tip, err := repo.CommitObject(head.Hash())
	if err != nil {
		return false, err
	}
	return candidate.IsAncestor(tip)
}
