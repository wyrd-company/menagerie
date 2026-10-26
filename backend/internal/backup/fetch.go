// ---
// relationships:
//   implements: menagerie
//   references: [backup-force-push-overwrites-branch]
// ---

package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// FetchedRef keeps the commit Fetch fetched until its apply ends.
const FetchedRef plumbing.ReferenceName = "refs/menagerie/apply"

// ErrInvalidApplyBranch means a name is not a branch of the workspace
// repository that can be applied.
var ErrInvalidApplyBranch = errors.New("not a valid branch name")

// ErrBranchNotFound means the workspace repository has no branch of the
// name given.
var ErrBranchNotFound = errors.New("the workspace repository has no such branch")

// ApplyBranch returns refs/heads/<name> for a branch to apply. name is a
// branch's short name: slashes are allowed, a full ref name such as
// refs/heads/<name> is not.
func ApplyBranch(name string) (plumbing.ReferenceName, error) {
	ref := plumbing.NewBranchReferenceName(name)
	if name == "" || name == "HEAD" || strings.HasPrefix(name, "refs/") || ref.Validate() != nil {
		return "", fmt.Errorf("%w: %q", ErrInvalidApplyBranch, name)
	}
	return ref, nil
}

// Fetch fetches branch, with the history it needs, from the workspace
// repository into FetchedRef, replacing what FetchedRef held, and returns
// its commit. It fetches only that branch, no tag, through an
// in-memory remote that is never written to the workspace repository's
// configuration, and changes no other ref. A branch the workspace
// repository does not have is ErrBranchNotFound. A shallow workspace
// repository, which lacks the history an apply walks, is refused with
// *NotApplicableError before anything is fetched.
func (r *Remote) Fetch(ctx context.Context, folder, workspace, branch string, progress io.Writer) (plumbing.Hash, error) {
	name, err := ApplyBranch(branch)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	repo, _, err := open(folder, workspace)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	shallow, err := repo.Storer.Shallow()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if len(shallow) > 0 {
		return plumbing.ZeroHash, &NotApplicableError{Reason: "the workspace repository is shallow; Apply branch needs its whole history"}
	}
	var auth transport.AuthMethod
	if r.Auth != nil {
		var release func()
		auth, release, err = r.Auth(ctx, r.URL)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		defer release()
	}
	remote := git.NewRemote(repo.Storer, &config.RemoteConfig{Name: remoteName, URLs: []string{r.URL}})
	err = remote.FetchContext(ctx, &git.FetchOptions{
		RemoteName: remoteName,
		RefSpecs:   []config.RefSpec{config.RefSpec("+" + name.String() + ":" + FetchedRef.String())},
		Auth:       auth,
		Progress:   progress,
		Force:      true,
	})
	switch {
	case errors.Is(err, git.NoMatchingRefSpecError{}), errors.Is(err, transport.ErrEmptyRemoteRepository):
		return plumbing.ZeroHash, fmt.Errorf("%w: %s", ErrBranchNotFound, branch)
	case err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate):
		return plumbing.ZeroHash, fmt.Errorf("fetch %s: %w", branch, transportFailure(ctx, r.URL, err))
	}
	ref, err := repo.Reference(FetchedRef, false)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("fetch %s: %w", branch, err)
	}
	return ref.Hash(), nil
}

// RemoveFetched deletes FetchedRef. A missing ref is no error.
func RemoveFetched(folder, workspace string) error {
	repo, _, err := open(folder, workspace)
	if err != nil {
		return err
	}
	return repo.Storer.RemoveReference(FetchedRef)
}
