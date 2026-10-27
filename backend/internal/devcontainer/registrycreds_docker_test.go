//go:build docker

package devcontainer_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/image"
	"github.com/wyrd-company/devcontainer-builder/pkg/host/dockercompose"
	"github.com/wyrd-company/devcontainer-builder/pkg/registryauth"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/registrycreds"
)

func pullOptions() image.PullOptions { return image.PullOptions{} }

// captureProgress keeps every output line for secret checks.
type captureProgress struct {
	t   *testing.T
	mu  sync.Mutex
	buf bytes.Buffer
}

func (p *captureProgress) Phase(string) {}
func (p *captureProgress) Log(prefix string) io.Writer {
	return writerFunc(func(b []byte) (int, error) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.buf.WriteString(prefix)
		p.buf.Write(b)
		p.t.Logf("%s%s", prefix, strings.TrimRight(string(b), "\n"))
		return len(b), nil
	})
}

// fixedFetcher answers every path with the test registry's credentials,
// or fails as OpenBao would for a missing secret.
type fixedFetcher struct{ fail bool }

func (f fixedFetcher) Fetch(context.Context, string) (registryauth.Credentials, error) {
	if f.fail {
		return registryauth.Credentials{}, &registrycreds.Error{Reason: "the secret does not exist"}
	}
	return registryauth.Credentials{Username: registryUser, Password: registryPassword}, nil
}

func credentialRuntime(t *testing.T, h *harness, reg *testRegistry, fetcher registrycreds.Fetcher) *devcontainer.Runtime {
	t.Helper()
	registries, err := registrycreds.New(fetcher, []registrycreds.Entry{{Registry: reg.addr, Path: "menagerie/registries/test"}})
	if err != nil {
		t.Fatal(err)
	}
	return devcontainer.New(h.client, h.options(devcontainer.Options{Credentials: func(ctx context.Context) devcontainer.RegistryAuth {
		return registries.Operation(ctx)
	}}))
}

// helperTrap makes the Docker client configuration name a credential
// helper that would answer with working credentials and record that it
// ran. Menagerie must never run it.
func helperTrap(t *testing.T, reg *testRegistry) string {
	t.Helper()
	bin := t.TempDir()
	marker := filepath.Join(bin, "helper-ran")
	script := "#!/bin/sh\ntouch " + marker + "\nprintf '{\"ServerURL\":\"%s\",\"Username\":\"" + registryUser + "\",\"Secret\":\"" + registryPassword + "\"}' \"$(cat)\"\n"
	if err := os.WriteFile(filepath.Join(bin, "docker-credential-menagerie-trap"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := t.TempDir()
	if err := os.WriteFile(filepath.Join(cfg, "config.json"), []byte(`{"credsStore":"menagerie-trap","credHelpers":{"`+reg.addr+`":"menagerie-trap"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", cfg)
	return marker
}

func assertNoSecret(t *testing.T, texts ...string) {
	t.Helper()
	encoded := base64.StdEncoding.EncodeToString([]byte(registryUser + ":" + registryPassword))
	for _, s := range texts {
		for _, secret := range []string{registryPassword, encoded} {
			if strings.Contains(s, secret) {
				t.Fatal("output holds the registry credential")
			}
		}
	}
}

// TestRegistryCredentialsReachEveryPath pulls, builds, fetches a feature
// and brings up Compose services from a registry that requires
// authentication, with credentials only from the injected provider, while
// the Docker client configuration names a working credential helper.
func TestRegistryCredentialsReachEveryPath(t *testing.T) {
	h := newHarness(t)
	ensureImage(t, h, featureImage)
	reg := startRegistry(t)
	// Each case reads its own repository, so no case finds an image another
	// case already pulled into the daemon.
	pullRef := reg.pushImage(t, h, featureImage, "menagerie-test/pull", "1")
	buildRef := reg.pushImage(t, h, featureImage, "menagerie-test/build", "1")
	composeRef := reg.pushImage(t, h, featureImage, "menagerie-test/compose", "1")
	serviceRef := reg.pushImage(t, h, featureImage, "menagerie-test/service", "1")
	feature := reg.pushFeature(t, "menagerie-test/features/marker", "1", map[string]string{
		"devcontainer-feature.json": `{"id": "marker", "version": "1.0.0", "name": "Marker"}`,
		"install.sh":                "#!/bin/sh\necho private-feature > /private-feature-marker\n",
	})
	marker := helperTrap(t, reg)
	rt := credentialRuntime(t, h, reg, fixedFetcher{})

	cases := []struct {
		name, repo string
		files      map[string]string
		check      func(t *testing.T, folder string)
	}{
		{"engine pull", "menagerie-test/pull", map[string]string{
			".devcontainer/devcontainer.json": `{"image": "` + pullRef + `"}`,
		}, func(t *testing.T, folder string) {
			if got := h.inspect(h.status(folder).ContainerID).Config.Image; got != pullRef {
				t.Fatalf("image %s", got)
			}
		}},
		{"buildkit base image", "menagerie-test/build", map[string]string{
			".devcontainer/devcontainer.json": `{"build": {"dockerfile": "Dockerfile"}}`,
			".devcontainer/Dockerfile":        "FROM " + buildRef + "\nRUN echo built > /built\n",
		}, func(t *testing.T, folder string) {
			if got := h.exec(h.status(folder).ContainerID, "cat", "/built"); got != "built" {
				t.Fatalf("build output %q", got)
			}
		}},
		{"feature", "menagerie-test/features/marker", map[string]string{
			".devcontainer/devcontainer.json": `{"image": "` + featureImage + `", "features": {"` + feature + `": {}}}`,
		}, func(t *testing.T, folder string) {
			if got := h.exec(h.status(folder).ContainerID, "cat", "/private-feature-marker"); got != "private-feature" {
				t.Fatalf("feature output %q", got)
			}
		}},
		{"compose pull", "menagerie-test/compose", map[string]string{
			".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/"}`,
			".devcontainer/compose.yml": "services:\n  app:\n    image: " + composeRef + "\n    command: [\"sleep\", \"infinity\"]\n" +
				"  sidecar:\n    build:\n      context: .\n      dockerfile: Dockerfile.sidecar\n    command: [\"sleep\", \"infinity\"]\n",
			".devcontainer/Dockerfile.sidecar": "FROM " + serviceRef + "\nRUN echo sidecar > /sidecar\n",
		}, func(t *testing.T, folder string) {
			if reg.pullCount("menagerie-test/service") == 0 {
				t.Fatal("no authenticated read of the service build's base image")
			}
			project := h.inspect(h.status(folder).ContainerID).Config.Labels[dockercompose.LabelProject]
			sidecar := h.service(project, "sidecar")
			h.images = append(h.images, h.inspect(sidecar).Config.Image)
			if got := h.exec(sidecar, "cat", "/sidecar"); got != "sidecar" {
				t.Fatalf("sidecar output %q", got)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := reg.pullCount(tc.repo)
			folder := h.workspace(strings.ReplaceAll(tc.name, " ", "-"), tc.files)
			p := &captureProgress{t: t}
			if err := rt.Start(h.ctx, folder, p); err != nil {
				assertNoSecret(t, err.Error(), p.buf.String())
				t.Fatal(err)
			}
			tc.check(t, folder)
			if reg.pullCount(tc.repo) == before {
				t.Fatalf("no authenticated read of %s", tc.repo)
			}
			assertNoSecret(t, p.buf.String())
		})
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the Docker configuration's credential helper ran")
	}
}

// TestRegistryCredentialsFailClosed shows the registry refuses anonymous
// access, that an anonymous runtime does not fall back to the helper the
// Docker configuration names, and that an unreadable configured secret
// fails pulls and builds with the typed error before any registry read.
func TestRegistryCredentialsFailClosed(t *testing.T) {
	h := newHarness(t)
	ensureImage(t, h, featureImage)
	reg := startRegistry(t)
	private := reg.pushImage(t, h, featureImage, "menagerie-test/busybox", "1")
	marker := helperTrap(t, reg)
	pull := map[string]string{".devcontainer/devcontainer.json": `{"image": "` + private + `"}`}
	build := map[string]string{
		".devcontainer/devcontainer.json": `{"build": {"dockerfile": "Dockerfile"}}`,
		".devcontainer/Dockerfile":        "FROM " + private + "\nRUN true\n",
	}

	anonymous := devcontainer.New(h.client, h.options(devcontainer.Options{}))
	for name, files := range map[string]map[string]string{"anonymous-pull": pull, "anonymous-build": build} {
		folder := h.workspace(name, files)
		unauthorized := reg.unauthorized.Load()
		err := anonymous.Start(h.ctx, folder, &captureProgress{t: t})
		var credErr *registrycreds.Error
		if err == nil || errors.As(err, &credErr) {
			t.Fatalf("%s: got %v", name, err)
		}
		if reg.unauthorized.Load() == unauthorized {
			t.Fatalf("%s: the registry saw no anonymous request", name)
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the Docker configuration's credential helper ran")
	}

	failing := credentialRuntime(t, h, reg, fixedFetcher{fail: true})
	for name, files := range map[string]map[string]string{"failing-pull": pull, "failing-build": build} {
		folder := h.workspace(name, files)
		authorized := reg.authorized.Load()
		p := &captureProgress{t: t}
		err := failing.Start(h.ctx, folder, p)
		var credErr *registrycreds.Error
		if !errors.As(err, &credErr) || credErr.Registry != reg.addr || credErr.Reason != "the secret does not exist" {
			t.Fatalf("%s: got %v", name, err)
		}
		if reg.authorized.Load() != authorized {
			t.Fatalf("%s: the registry served an authenticated request", name)
		}
		if s := h.status(folder); s.State != devcontainer.StateNoContainer {
			t.Fatalf("%s: container created: %+v", name, s)
		}
		assertNoSecret(t, err.Error(), p.buf.String())
	}
}
