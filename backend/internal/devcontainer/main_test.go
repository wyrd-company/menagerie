package devcontainer

import (
	"fmt"
	"os"
	"testing"
)

// TestMain isolates every test in this package, including the Docker smoke
// tests, from the host's Docker client configuration: registry credentials
// are read from an empty configuration folder unless a test sets its own.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "menagerie-docker-config-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("DOCKER_CONFIG", dir)
	os.Unsetenv("DOCKER_AUTH_CONFIG")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
