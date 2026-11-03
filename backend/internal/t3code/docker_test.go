//go:build docker

// The minter against a real Docker Engine: a generic container whose t3 is
// a stub that records how it ran, as the t3code-server Feature lays it out.
package t3code

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	testImage = "alpine:3.22"
	runLabel  = "menagerie.test.t3code"
	secret    = "SYNTHETICSECRET"
)

const goodConfig = "T3CODE_SERVER_USER=nobody\nT3CODE_SERVER_HOME=/home/sample\n"

const goodOutput = `{"id":"0","credential":"SAMPLETOKEN123","label":"menagerie","scopes":[],"expiresAt":"2030-01-02T03:04:05.777Z"}`

// recordingStub records its user, HOME and arguments, then runs body.
func recordingStub(body string) string {
	return "#!/bin/sh\n{ id -un; echo \"$HOME\"; echo \"$*\"; } > /tmp/invocation\n" + body + "\n"
}

type harness struct {
	t      *testing.T
	ctx    context.Context
	client *dockerclient.Client
	run    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	client, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ctx: context.Background(), client: client, run: strings.ReplaceAll(t.Name(), "/", "-") + "-" + time.Now().Format("150405.000000000")}
	t.Cleanup(h.cleanup)
	if _, err := client.ImageInspect(h.ctx, testImage); err != nil {
		reader, err := client.ImagePull(h.ctx, testImage, image.PullOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, reader)
		_ = reader.Close()
	}
	return h
}

// cleanup removes only this test's containers, by its own run label.
func (h *harness) cleanup() {
	list, err := h.client.ContainerList(context.Background(), container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("label", runLabel+"="+h.run))})
	if err != nil {
		h.t.Errorf("cleanup list: %v", err)
	}
	for _, c := range list {
		if err := h.client.ContainerRemove(context.Background(), c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err != nil {
			h.t.Errorf("cleanup remove: %v", err)
		}
	}
	_ = h.client.Close()
}

// container starts a sleeping container holding the given files.
func (h *harness) container(files map[string]string) string {
	h.t.Helper()
	created, err := h.client.ContainerCreate(h.ctx, &container.Config{
		Image:  testImage,
		Cmd:    []string{"sleep", "600"},
		Labels: map[string]string{runLabel: h.run},
	}, nil, nil, nil, "")
	if err != nil {
		h.t.Fatal(err)
	}
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	for name, content := range files {
		if err := w.WriteHeader(&tar.Header{Name: strings.TrimPrefix(name, "/"), Mode: 0o755, Size: int64(len(content))}); err != nil {
			h.t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.CopyToContainer(h.ctx, created.ID, "/", &archive, container.CopyToContainerOptions{}); err != nil {
		h.t.Fatal(err)
	}
	if err := h.client.ContainerStart(h.ctx, created.ID, container.StartOptions{}); err != nil {
		h.t.Fatal(err)
	}
	return created.ID
}

// readFile reads a file of the container through an exec of cat.
func (h *harness) readFile(id, path string) string {
	h.t.Helper()
	created, err := h.client.ContainerExecCreate(h.ctx, id, container.ExecOptions{Cmd: []string{"cat", path}, AttachStdout: true, AttachStderr: true})
	if err != nil {
		h.t.Fatal(err)
	}
	attached, err := h.client.ContainerExecAttach(h.ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	defer attached.Close()
	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, &out, attached.Reader); err != nil {
		h.t.Fatal(err)
	}
	return out.String()
}

func installed(stub string) map[string]string {
	return map[string]string{ConfigPath: goodConfig, Executable: stub, "/tmp/.keep": ""}
}

func TestMintRunsTheFeaturesCommandAsItsServiceUser(t *testing.T) {
	h := newHarness(t)
	id := h.container(installed(recordingStub("echo noise >&2\necho '" + goodOutput + "'")))
	token, err := NewMinter(h.client).Mint(h.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "SAMPLETOKEN123" || token.ExpiresAt != "2030-01-02T03:04:05.777Z" {
		t.Errorf("token = %+v", token)
	}
	want := "nobody\n"
	got := h.readFile(id, "/tmp/invocation")
	if !strings.HasPrefix(got, want) {
		t.Errorf("ran as %q", got)
	}
	if !strings.Contains(got, "auth pairing create --base-dir /home/sample/.t3 --label menagerie --json") {
		t.Errorf("invocation = %q", got)
	}
}

func TestMintFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		want  error
	}{
		"no Feature":                      {map[string]string{"/tmp/.keep": ""}, ErrNotInstalled},
		"no command":                      {map[string]string{ConfigPath: goodConfig}, ErrNotInstalled},
		"unreadable config":               {map[string]string{ConfigPath: "T3CODE_SERVER_USER=sam\\ ple\n", Executable: "#!/bin/sh\n"}, ErrNotInstalled},
		"command fails, printing a token": {installed("#!/bin/sh\necho '{\"credential\":\"" + secret + "\",\"expiresAt\":\"2030-01-02T03:04:05Z\"}'\necho " + secret + " >&2\nexit 1\n"), ErrFailed},
		"command prints no JSON":          {installed("#!/bin/sh\necho " + secret + "\n"), ErrFailed},
		"command prints without end":      {installed("#!/bin/sh\nwhile :; do echo " + secret + "; done\n"), ErrFailed},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			id := h.container(tc.files)
			_, err := NewMinter(h.client).Mint(h.ctx, id)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error holds the output: %v", err)
			}
		})
	}
}

func TestMintOnAContainerThatStoppedOrWasRemoved(t *testing.T) {
	h := newHarness(t)
	id := h.container(installed("#!/bin/sh\necho '" + goodOutput + "'\n"))
	if err := h.client.ContainerKill(h.ctx, id, "KILL"); err != nil {
		t.Fatal(err)
	}
	waited := time.Now()
	for {
		info, err := h.client.ContainerInspect(h.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !info.State.Running {
			break
		}
		if time.Since(waited) > time.Minute {
			t.Fatal("the container still runs")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := NewMinter(h.client).Mint(h.ctx, id); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stopped: err = %v", err)
	}
	if err := h.client.ContainerRemove(h.ctx, id, container.RemoveOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMinter(h.client).Mint(h.ctx, id); !errors.Is(err, ErrNotRunning) {
		t.Errorf("removed: err = %v", err)
	}
}

func TestMintReturnsWhenItsRequestEndsThoughTheCommandHangs(t *testing.T) {
	h := newHarness(t)
	id := h.container(installed("#!/bin/sh\nexec sleep 600\n"))
	ctx, cancel := context.WithCancel(h.ctx)
	result := make(chan error, 1)
	go func() {
		_, err := NewMinter(h.client).Mint(ctx, id)
		result <- err
	}()
	// Mint has reached the command when a process other than the container's
	// sleep exists.
	deadline := time.Now().Add(time.Minute)
	for {
		top, err := h.client.ContainerTop(h.ctx, id, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(top.Processes) > 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
	case <-time.After(time.Minute):
		t.Fatal("Mint did not return after its request ended")
	}
}
