// ---
// relationships:
//   implements: [menagerie, choose-a-backup-repository-per-workspace]
// ---

package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/memory"
)

// ProbePrefix starts the ref a write check pushes and deletes. It is not a
// branch, so it never shows among a repository's branches.
const ProbePrefix = "refs/menagerie/write-check-"

// probeLocal holds the write check's commit in its in-memory repository.
const probeLocal plumbing.ReferenceName = "refs/heads/probe"

// CheckWrite proves that Menagerie can write to the repository: it pushes a
// ref under ProbePrefix holding one empty commit made in memory, then
// deletes it. It reads no workspace and changes no branch; an empty
// repository stays empty. A failure is the push's, with the fixed reasons
// of every push.
func (r *Remote) CheckWrite(ctx context.Context, progress io.Writer) error {
	repo, err := git.Init(memory.NewStorage(), nil)
	if err != nil {
		return err
	}
	commit, err := emptyCommit(repo)
	if err != nil {
		return err
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(probeLocal, commit)); err != nil {
		return err
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	probe := plumbing.ReferenceName(ProbePrefix + hex.EncodeToString(suffix))
	push := func(spec config.RefSpec) error {
		s, err := r.open(ctx, progress)
		if err != nil {
			return err
		}
		defer s.close()
		remote := git.NewRemote(repo.Storer, &config.RemoteConfig{Name: remoteName, URLs: []string{r.URL}})
		err = remote.PushContext(s.ctx, &git.PushOptions{RemoteName: remoteName, RefSpecs: []config.RefSpec{spec}, Auth: s.auth, Progress: s.progress})
		if err != nil {
			return transportFailure(s.ctx, r.URL, err)
		}
		return nil
	}
	if err := push(config.RefSpec(probeLocal.String() + ":" + probe.String())); err != nil {
		return fmt.Errorf("write check: %w", err)
	}
	if err := push(config.RefSpec(":" + probe.String())); err != nil {
		return fmt.Errorf("write check: the check's ref %s was written and could not be removed: %w", probe, err)
	}
	return nil
}

// emptyCommit stores a commit of the empty tree in repo.
func emptyCommit(repo *git.Repository) (plumbing.Hash, error) {
	tree := repo.Storer.NewEncodedObject()
	if err := (&object.Tree{}).Encode(tree); err != nil {
		return plumbing.ZeroHash, err
	}
	treeHash, err := repo.Storer.SetEncodedObject(tree)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	sig := *signature()
	commit := repo.Storer.NewEncodedObject()
	if err := (&object.Commit{Author: sig, Committer: sig, Message: "Menagerie write check\n", TreeHash: treeHash}).Encode(commit); err != nil {
		return plumbing.ZeroHash, err
	}
	return repo.Storer.SetEncodedObject(commit)
}
