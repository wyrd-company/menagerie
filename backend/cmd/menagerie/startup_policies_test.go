package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/provisioning"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
	"github.com/wyrd-company/wyrwood/operator"
)

type startupDefinition struct{ provisioning.Runtime }

func (startupDefinition) Definition(folder string) (devcontainer.Definition, error) {
	return devcontainer.Definition{ContainerName: "container-" + filepath.Base(folder), Block: []byte(`{"wyrwood":{"accessGroup":1000}}`)}, nil
}

type unavailableImport struct {
	provisioning.PolicyBindings
	entered chan string
}

func (b unavailableImport) Read(ctx context.Context, name string) (operator.Binding, bool, error) {
	b.entered <- name
	<-ctx.Done() // An unreachable controller which doesn't answer, rather than refusing.
	return operator.Binding{}, false, ctx.Err()
}

func TestServiceListensWhilePolicyImportCannotReachWyrwood(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir, data := t.TempDir(), testData(t)
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := data.HoldName(ctx, name, "container-"+name, "host", true); err != nil {
			t.Fatal(err)
		}
	}
	blocked := unavailableImport{entered: make(chan string, 3)}
	steps := provisioning.New(provisioning.Options{Clients: provisioning.Clients{Bindings: blocked, Agent: "host"}, Data: data, Runtime: startupDefinition{}})
	// The first-start snapshot must already be frozen when requests can arrive.
	if err := steps.PrepareSelectionImports(ctx, root, t.Logf); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{WorkspaceRoot: dir, TemplateService: config.TemplateService{Socket: filepath.Join(dir, "absent.sock")}}
	manager := jobs.NewManager(ctx, jobs.Options{})
	defer manager.Wait()
	defer cancel()
	handler := newServer(cfg, root, data, backupAuth(cfg, nil), devcontainer.New(nil, devcontainer.Options{}), provisioning.Clients{}, manager, nil, nil, nil)
	policies := api.New(api.Options{Root: root, Data: data, Jobs: manager, PolicyApplication: steps})
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.Addr().String()
	_ = reservation.Close()
	server := &http.Server{Addr: address, Handler: handler}
	finished := make(chan error, 1)
	go func() {
		finished <- serveWorkspace(ctx, server, nil, func(ctx context.Context) error {
			if err := steps.ImportSelections(ctx, root, func(string, ...any) {}); err != nil {
				return err
			}
			return policies.ApplyPending(ctx)
		})
	}()
	defer func() {
		cancel()
		select {
		case err := <-finished:
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Error(err)
			}
		case <-time.After(2 * time.Second):
			t.Error("server or policy import did not stop")
		}
	}()
	select {
	case name := <-blocked.entered:
		if name != "container-alpha" {
			t.Fatal("first import", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("import did not reach unavailable controller")
	}
	// Own the test's connections. The default transport may leave a spare
	// connection in StateNew, which Shutdown gives five seconds to send a
	// request. That is independent of the startup-availability assertion.
	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Timeout: time.Second, Transport: transport}
	defer client.CloseIdleConnections()
	for _, path := range []string{"/healthz", "/api/jobs"} {
		response, err := client.Get("http://" + address + path)
		if err != nil {
			t.Fatalf("service unavailable while first import is blocked: %v", err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, response.StatusCode)
		}
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		selection, err := data.Selection(ctx, name)
		if err != nil || selection.Imported || selection.AppliedRevision != nil {
			t.Fatalf("%s lost pending state: %+v %v", name, selection, err)
		}
	}
}
