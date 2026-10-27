package devcontainer

import "testing"

func TestComposeOperationsStageInTheConfiguredFolder(t *testing.T) {
	rt := New(nil, Options{ComposeStaging: "/srv/compose-staging"})
	if got := rt.composeOptions(&progress{}, anonymous).UserDataFolder; got != "/srv/compose-staging" {
		t.Errorf("UserDataFolder %q, want the configured staging folder", got)
	}
}
