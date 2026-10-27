// ---
// relationships:
//   implements: menagerie
//   references: [workspace, menagerie-has-host-root]
// ---

package devcontainer

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/wyrd-company/devcontainer-builder/pkg/host/dockercompose"
	"github.com/wyrd-company/devcontainer-builder/pkg/spec"
)

// identityKeys are the labels that tell which workspace, Compose project or
// Menagerie command a container belongs to, and identityPrefixes the label
// families that do. Menagerie, devcontainer-builder, Compose and the Docker
// proxy set them; a definition that set them could pass its container off
// as another workspace's.
var (
	identityKeys     = []string{labelLocalFolder, labelConfigFile}
	identityPrefixes = []string{"menagerie.", "com.docker.compose.", "com.sockguard."}
)

// IdentityLabelError reports a definition that sets an identity label. It
// names the label's key and where it is set, never its value.
type IdentityLabelError struct {
	// Key is the label's key.
	Key string
	// Where is "runArgs" or the Compose service that sets it.
	Where string
}

func (e *IdentityLabelError) Error() string {
	return fmt.Sprintf("the definition sets the label %q in %s; Menagerie and the tools it uses set that label themselves, so the workspace is not started", e.Key, e.Where)
}

// identityLabel reports whether key is an identity label.
func identityLabel(key string) bool {
	if slices.Contains(identityKeys, key) {
		return true
	}
	for _, prefix := range identityPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// checkIdentityLabels refuses a definition whose runArgs, or whose Compose
// project's services, set an identity label. The Compose project is loaded
// as devcontainer-builder loads it, with its merges, includes, label files
// and interpolation applied.
func checkIdentityLabels(ctx context.Context, dc *spec.DevContainer, folder string, env map[string]string) error {
	if dc.Mode() == spec.ModeCompose {
		if env == nil {
			env = map[string]string{}
		}
		proj, err := dockercompose.LoadProject(ctx, dockercompose.LoadInputs{Spec: dc, Workspace: folder, LocalEnv: env})
		if err != nil {
			return err
		}
		for _, name := range slices.Sorted(maps.Keys(proj.Services)) {
			for _, key := range slices.Sorted(maps.Keys(proj.Services[name].Labels)) {
				if identityLabel(key) {
					return &IdentityLabelError{Key: key, Where: "the labels of Compose service " + name}
				}
			}
		}
		return nil
	}
	for _, key := range runArgLabels(dc.RunArgs) {
		if identityLabel(key) {
			return &IdentityLabelError{Key: key, Where: "runArgs"}
		}
	}
	return nil
}

// runArgLabels returns the keys of the labels runArgs set: the value after
// -l or --label, or after their "=". Any element that reads as a label
// flag counts, even where devcontainer-builder would read it as another
// flag's value, so no label it applies is missed.
func runArgLabels(args []string) []string {
	var keys []string
	for i, a := range args {
		var value string
		switch {
		case a == "-l" || a == "--label":
			if i+1 >= len(args) {
				continue
			}
			value = args[i+1]
		case strings.HasPrefix(a, "-l=") || strings.HasPrefix(a, "--label="):
			_, value, _ = strings.Cut(a, "=")
		default:
			continue
		}
		key, _, _ := strings.Cut(value, "=")
		keys = append(keys, key)
	}
	return keys
}
