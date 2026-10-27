package devcontainer

import (
	"os"
	"path/filepath"
	"testing"
)

// TestComposeOperationsStageInTheWorkspacesFolder gives each workspace
// its own staging folder, named after it, under the configured one.
func TestComposeOperationsStageInTheWorkspacesFolder(t *testing.T) {
	rt := New(nil, Options{ComposeStaging: "/srv/compose-staging"})
	if got := rt.composeOptions(&progress{}, anonymous, "/srv/workspaces/alpha").UserDataFolder; got != "/srv/compose-staging/alpha" {
		t.Errorf("UserDataFolder %q, want the workspace's folder in the staging folder", got)
	}
	if got := New(nil, Options{}).composeOptions(&progress{}, anonymous, "/srv/workspaces/alpha").UserDataFolder; got != "" {
		t.Errorf("UserDataFolder %q without a staging folder, want devcontainer-builder's default", got)
	}
}

// TestTheStagingFolderIsCreatedPrivate creates the workspace's staging
// folder 0700 before Up, keeps an existing one, and refuses a link or a
// file in its place.
func TestTheStagingFolderIsCreatedPrivate(t *testing.T) {
	staging := t.TempDir()
	rt := New(nil, Options{ComposeStaging: staging})
	if err := rt.ensureStaging("/srv/workspaces/alpha"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(staging, "alpha"))
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("staging folder %v %v, want a 0700 folder", info, err)
	}
	if err := rt.ensureStaging("/srv/workspaces/alpha"); err != nil {
		t.Fatalf("an existing staging folder: %v", err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(staging, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"linked", "file"} {
		if err := rt.ensureStaging("/srv/workspaces/" + name); err != errStaging {
			t.Errorf("%s: %v, want errStaging", name, err)
		}
	}
	if err := New(nil, Options{ComposeStaging: filepath.Join(staging, "absent")}).ensureStaging("/srv/workspaces/alpha"); err != errStaging {
		t.Errorf("a missing staging root: %v, want errStaging", err)
	}
}
