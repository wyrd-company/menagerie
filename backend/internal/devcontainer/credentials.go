// ---
// relationships:
//   implements: menagerie
//   references: workspace-api
// ---

package devcontainer

import (
	"fmt"
	"os"
	"path/filepath"

	dockerconfig "github.com/docker/cli/cli/config"
)

// CredentialHelperError reports a Docker client configuration that names a
// credential helper. Menagerie runs no credential helper programs.
type CredentialHelperError struct {
	// ConfigFile is the configuration file that names the helper.
	ConfigFile string
}

func (e *CredentialHelperError) Error() string {
	return fmt.Sprintf("%s names a credential helper (credsStore or credHelpers); Menagerie does not run credential helpers: use auths entries or DOCKER_AUTH_CONFIG", e.ConfigFile)
}

// CheckNoCredentialHelpers reads the Docker client configuration that
// devcontainer-builder's registry credentials use ($DOCKER_CONFIG, else
// ~/.docker) and refuses one that names a credential helper. Builds,
// feature fetches and pulls read that configuration again when they need
// credentials, so a helper added to it after this check is not refused.
func CheckNoCredentialHelpers() error {
	dir := os.Getenv(dockerconfig.EnvOverrideConfigDir)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			dir = ".docker"
		} else {
			dir = filepath.Join(home, ".docker")
		}
	}
	file, err := dockerconfig.Load(dir)
	if err != nil {
		return fmt.Errorf("registry credentials: %w", err)
	}
	if file.CredentialsStore != "" || len(file.CredentialHelpers) > 0 {
		return &CredentialHelperError{ConfigFile: file.Filename}
	}
	return nil
}
