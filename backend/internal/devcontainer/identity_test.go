package devcontainer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	dockerclient "github.com/docker/docker/client"
	"github.com/wyrd-company/devcontainer-builder/pkg/spec"

	"github.com/wyrd-company/menagerie/backend/internal/dockerproxy"
)

func TestRunArgLabelsReadsEveryLabelForm(t *testing.T) {
	got := runArgLabels([]string{"--init", "-l", "a=1", "--label", "b", "--label=c=3", "-l=d=4", "--hostname", "box", "-l"})
	if want := []string{"a", "b", "c", "d"}; !slices.Equal(got, want) {
		t.Fatalf("labels %q, want %q", got, want)
	}
}

func TestIdentityLabels(t *testing.T) {
	for key, want := range map[string]bool{
		"devcontainer.local_folder":   true,
		"devcontainer.config_file":    true,
		"menagerie.throwaway":         true,
		"menagerie.anything":          true,
		"com.docker.compose.project":  true,
		"com.sockguard.owner":         true,
		"devcontainer.metadata":       false,
		"org.opencontainers.image.os": false,
		"tier":                        false,
		"menagerie":                   false,
	} {
		if got := identityLabel(key); got != want {
			t.Errorf("identityLabel(%q) = %t, want %t", key, got, want)
		}
	}
}

// TestADefinitionThatForgesAnIdentityLabelIsRefused refuses runArgs that
// set an identity label, naming the key and never the value.
func TestADefinitionThatForgesAnIdentityLabelIsRefused(t *testing.T) {
	for _, args := range []string{
		`["--label", "devcontainer.local_folder=/srv/other-value"]`,
		`["-l", "devcontainer.config_file=/srv/other-value"]`,
		`["--label=menagerie.throwaway.workspace=/srv/other-value"]`,
		`["-l=com.docker.compose.project=other-value"]`,
	} {
		t.Run(args, func(t *testing.T) {
			dc, folder := writeDefinition(t, `{"image": "alpine:3.22", "runArgs": `+args+`}`)
			err := checkIdentityLabels(context.Background(), dc, folder, nil)
			var ie *IdentityLabelError
			if !errors.As(err, &ie) || ie.Where != "runArgs" {
				t.Fatalf("error %v, want an IdentityLabelError in runArgs", err)
			}
			if strings.Contains(err.Error(), "other-value") {
				t.Errorf("error names the value: %v", err)
			}
		})
	}
	dc, folder := writeDefinition(t, `{"image": "alpine:3.22", "runArgs": ["--label", "tier=dev", "--hostname", "devcontainer.local_folder"]}`)
	if err := checkIdentityLabels(context.Background(), dc, folder, nil); err != nil {
		t.Fatalf("plain labels refused: %v", err)
	}
}

// writeCompose writes a Compose definition and its files into a new
// workspace folder.
func writeCompose(t *testing.T, files map[string]string) (string, error) {
	t.Helper()
	folder := filepath.Join(t.TempDir(), "sample-space")
	dir := filepath.Join(folder, ".devcontainer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	files["devcontainer.json"] = `{"dockerComposeFile": ["compose.yml"], "service": "app", "workspaceFolder": "/w"}`
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dc, folder := loadFolder(t, folder)
	return folder, checkIdentityLabels(context.Background(), dc, folder, map[string]string{"SAMPLE_KEY": "menagerie.forged"})
}

func TestAComposeServiceThatForgesAnIdentityLabelIsRefused(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"map": {"compose.yml": `services:
  app: {image: alpine:3.22}
  side:
    image: alpine:3.22
    labels: {devcontainer.local_folder: /srv/other-value}
`},
		"list": {"compose.yml": `services:
  app:
    image: alpine:3.22
    labels: ["com.docker.compose.project=other-value"]
`},
		"label file": {"compose.yml": `services:
  app:
    image: alpine:3.22
    label_file: ./labels.env
`, "labels.env": "menagerie.throwaway=other-value\n"},
		"interpolated": {"compose.yml": `services:
  app:
    image: alpine:3.22
    labels: ["${SAMPLE_KEY}=other-value"]
`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := writeCompose(t, files)
			var ie *IdentityLabelError
			if !errors.As(err, &ie) || !strings.HasPrefix(ie.Where, "the labels of Compose service ") {
				t.Fatalf("error %v, want an IdentityLabelError in a service", err)
			}
			if strings.Contains(err.Error(), "other-value") {
				t.Errorf("error names the value: %v", err)
			}
		})
	}
	if _, err := writeCompose(t, map[string]string{"compose.yml": "services:\n  app:\n    image: alpine:3.22\n    labels: {tier: dev}\n"}); err != nil {
		t.Fatalf("plain labels refused: %v", err)
	}
}

func loadFolder(t *testing.T, folder string) (*spec.DevContainer, string) {
	t.Helper()
	dc, err := spec.LoadFromWorkspace(folder)
	if err != nil {
		t.Fatal(err)
	}
	return dc, folder
}

// absentClient is a client for an unreachable daemon: any request fails
// with a connection error.
func absentClient(t *testing.T, headers map[string]string) *dockerclient.Client {
	t.Helper()
	client, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("unix://"+filepath.Join(t.TempDir(), "absent.sock")), dockerclient.WithHTTPHeaders(headers))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// TestAForgedIdentityLabelStopsStartBeforeAnyRequest starts and rebuilds a
// definition that forges an identity label: the refusal comes before
// initializeCommand and before any Docker request.
func TestAForgedIdentityLabelStopsStartBeforeAnyRequest(t *testing.T) {
	_, folder := writeDefinition(t, `{"image": "alpine:3.22", "initializeCommand": "touch init-ran", "runArgs": ["--label", "devcontainer.local_folder=/srv/other"]}`)
	l := &localContainers{}
	rt := New(absentClient(t, nil), Options{Containers: l, RunnerImage: func(context.Context) (string, error) { return "runner:sample", nil }})
	for _, op := range []func(context.Context, string, Progress) error{rt.Start, rt.Rebuild} {
		err := op(context.Background(), folder, &progress{})
		var ie *IdentityLabelError
		if !errors.As(err, &ie) || ie.Key != labelLocalFolder {
			t.Fatalf("error %v, want an IdentityLabelError", err)
		}
	}
	if _, err := os.Stat(filepath.Join(folder, "init-ran")); err == nil {
		t.Fatal("initializeCommand ran for a refused definition")
	}
}

// TestEachOperationNamesItsWorkspace gives Start, Rebuild and Stop each a
// client of their own, made with the workspace's identity in
// dockerproxy.HeaderWorkspace, and closes it.
func TestEachOperationNamesItsWorkspace(t *testing.T) {
	_, folder := writeDefinition(t, `{"image": "alpine:3.22"}`)
	want, err := dockerproxy.Workspace(folder)
	if err != nil {
		t.Fatal(err)
	}
	var made []map[string]string
	rt := New(absentClient(t, nil), Options{Client: func(headers map[string]string) (*dockerclient.Client, error) {
		made = append(made, headers)
		return absentClient(t, headers), nil
	}})
	for _, op := range []func(context.Context, string, Progress) error{rt.Start, rt.Rebuild, rt.Stop} {
		if err := op(context.Background(), folder, &progress{}); err == nil {
			t.Fatal("an operation on an absent daemon succeeded")
		}
	}
	if len(made) != 3 {
		t.Fatalf("%d clients made, want 3", len(made))
	}
	for _, h := range made {
		if len(h) != 1 || h[dockerproxy.HeaderWorkspace] != want {
			t.Errorf("client headers %v, want %s=%s", h, dockerproxy.HeaderWorkspace, want)
		}
	}
	rt = New(absentClient(t, nil), Options{Client: func(map[string]string) (*dockerclient.Client, error) {
		return nil, errors.New("no client")
	}})
	if err := rt.Start(context.Background(), folder, &progress{}); err == nil || err.Error() != "no Docker client could be made for the workspace" {
		t.Fatalf("start without a client: %v", err)
	}
}
