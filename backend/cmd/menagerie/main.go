// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

// Command menagerie is the workspace service: it serves the workspace API
// and drives workspace devcontainers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	dockerclient "github.com/docker/docker/client"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

func main() {
	configPath := flag.String("config", config.DefaultPath, "configuration file")
	flag.Parse()
	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "menagerie:", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	root, err := workspace.NewRoot(cfg.WorkspaceRoot)
	if err != nil {
		return err
	}
	docker, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("docker client: %w", err)
	}
	defer docker.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runtime := devcontainer.New(docker, devcontainer.Options{})
	manager := jobs.NewManager(ctx)
	templates := make([]api.Template, 0, len(cfg.Templates))
	for _, t := range cfg.Templates {
		templates = append(templates, api.Template{ID: t.ID, Name: t.Name})
	}
	server := api.New(api.Options{
		Root:       root,
		Status:     runtime,
		Jobs:       manager,
		Operations: api.ContainerOperations(root, runtime),
		Templates:  templates,
	})

	httpServer := &http.Server{
		Addr:        cfg.Listen,
		Handler:     server,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- httpServer.ListenAndServe() }()
	log.Printf("menagerie: listening on %s, workspace root %s", cfg.Listen, root.Path())
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Event streams end with the base context, so shutdown completes once
	// in-flight requests return. Running jobs end with the process; their
	// history is in memory only.
	if err := httpServer.Shutdown(context.Background()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
