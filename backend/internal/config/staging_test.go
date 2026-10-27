package config

import (
	"strings"
	"testing"
)

func TestParseComposeStagingFolder(t *testing.T) {
	cfg, err := Parse([]byte(valid))
	if err != nil || cfg.ComposeStagingFolder != "" {
		t.Fatalf("absent: %q %v", cfg.ComposeStagingFolder, err)
	}
	cfg, err = Parse([]byte(valid + "compose-staging-folder: /srv/compose-staging\nworkspace-socket:\n  folder: /srv/sockets\n"))
	if err != nil || cfg.ComposeStagingFolder != "/srv/compose-staging" {
		t.Fatalf("given: %q %v", cfg.ComposeStagingFolder, err)
	}
}

// TestComposeStagingFolderErrorsNameNoValue checks each rule of
// compose-staging-folder; no error quotes the value given.
func TestComposeStagingFolderErrorsNameNoValue(t *testing.T) {
	const marker = "v4lue-marker"
	staging := func(folder string) string { return valid + "compose-staging-folder: " + folder + "\n" }
	cases := []struct{ name, doc, want string }{
		{"relative", staging(marker), "compose-staging-folder must be an absolute clean folder other than /"},
		{"unclean", staging("/srv/" + marker + "/"), "compose-staging-folder must be an absolute clean folder other than /"},
		{"root", staging("/"), "compose-staging-folder must be an absolute clean folder other than /"},
		{"in workspace root", staging("/srv/workspaces/" + marker), "compose-staging-folder must be neither inside workspace-root nor contain it"},
		{"holds workspace root", staging("/srv"), "compose-staging-folder must be neither inside workspace-root nor contain it"},
		{"in wyrwood", staging("/run/wyrwood/containers/example/" + marker), "compose-staging-folder must be neither inside wyrwood-directory nor contain it"},
		{"candidates", staging("/var/lib/menagerie/candidates/" + marker), "compose-staging-folder must be neither inside template-service.candidate-folder nor contain it"},
		{"socket folder", staging("/opt/sockets/"+marker) + "workspace-socket:\n  folder: /opt/sockets\n", "compose-staging-folder must be neither inside workspace-socket.folder nor contain it"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.doc))
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), marker) {
				t.Fatalf("got %v, want %q without the value", err, tc.want)
			}
		})
	}
}
