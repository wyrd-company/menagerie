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
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	dockerclient "github.com/docker/docker/client"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/registrycreds"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/workflows"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
	"github.com/wyrd-company/menagerie/web"
)

func main() {
	configPath := flag.String("config", config.DefaultPath, "configuration file")
	printRoot := flag.Bool("print-workspace-root", false, "print the configured workspace root and exit")
	printWyrwood := flag.Bool("print-wyrwood-directory", false, "print the configured Wyrwood consumer directory and exit")
	flag.Parse()
	if *printRoot || *printWyrwood {
		if err := printPaths(os.Stdout, *configPath, *printRoot, *printWyrwood); err != nil {
			fmt.Fprintln(os.Stderr, "menagerie:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*configPath); err != nil {
		fmt.Fprintln(os.Stderr, "menagerie:", err)
		os.Exit(1)
	}
}

// printPaths writes the configured paths the image's start-up needs, one
// per line, from a configuration that loads and validates. It starts no
// service and opens no connection.
func printPaths(w io.Writer, configPath string, root, wyrwood bool) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if root {
		fmt.Fprintln(w, filepath.Clean(cfg.WorkspaceRoot))
	}
	if wyrwood {
		fmt.Fprintln(w, filepath.Clean(cfg.WyrwoodDirectory))
	}
	return nil
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

	credentials, err := registryCredentials(cfg)
	if err != nil {
		return err
	}
	runtime := devcontainer.New(docker, devcontainer.Options{Credentials: credentials})
	manager := jobs.NewManager(ctx)
	handler, err := newHandler(cfg, root, runtime, manager)
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:        cfg.Listen,
		Handler:     handler,
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

// newHandler is the workspace service's whole HTTP surface: the workspace
// API behind manager sign-in.
func newHandler(cfg *config.Config, root *workspace.Root, runtime *devcontainer.Runtime, manager *jobs.Manager) (http.Handler, error) {
	return signedIn(newServer(cfg, root, runtime, manager), cfg.OIDC)
}

// signedIn serves server behind manager sign-in: the sign-in routes are
// added to it, and every other route requires a manager session.
func signedIn(server *api.Server, c config.OIDC) (http.Handler, error) {
	authn, err := auth.New(auth.Config{Issuer: c.Issuer, ClientID: c.ClientID, RedirectURL: c.RedirectURL, Scopes: c.Scopes})
	if err != nil {
		return nil, fmt.Errorf("oidc: %w", err)
	}
	authn.Register(server)
	return authn.Wrap(server), nil
}

// newServer composes the workspace API: the container operations, create
// and upgrade with their interviews, and the web UI embedded at build time.
func newServer(cfg *config.Config, root *workspace.Root, runtime *devcontainer.Runtime, manager *jobs.Manager) *api.Server {
	approved := make([]api.Template, 0, len(cfg.Templates))
	ids := make(map[string]bool, len(cfg.Templates))
	for _, t := range cfg.Templates {
		approved = append(approved, api.Template{ID: t.ID, Name: t.Name})
		ids[t.ID] = true
	}
	flows := workflows.New(workflows.Options{
		Root:      root,
		Templates: templates.New(cfg.TemplateService.Socket),
		Approved:  ids,
		Remote:    backupRemote(cfg),
		Runtime:   runtime,
	})
	operations := api.ContainerOperations(root, runtime)
	for name, op := range flows.Operations() {
		operations[name] = op
	}
	return api.New(api.Options{
		Root:       root,
		Status:     runtime,
		Jobs:       manager,
		Operations: operations,
		Interviews: flows,
		Templates:  approved,
		UI:         web.FS,
	})
}

// backupRemote is the workspace repository, reached through the SSH agent
// socket in the Wyrwood consumer directory.
func backupRemote(cfg *config.Config) *backup.Remote {
	return &backup.Remote{
		URL:  cfg.WorkspaceRepository,
		Auth: backup.AgentAuth(filepath.Join(cfg.WyrwoodDirectory, backup.AgentSocketName)),
	}
}

// registryCredentials returns the per-operation registry credential source:
// OpenBao read through the Wyrwood vault socket for the configured
// registries, anonymous for every other registry.
func registryCredentials(cfg *config.Config) (func(context.Context) devcontainer.RegistryAuth, error) {
	rc := cfg.RegistryCredentials
	if rc == nil {
		return nil, nil
	}
	source, err := registrycreds.NewOpenBao(registrycreds.SocketAddress(cfg.WyrwoodDirectory), rc.KVMount)
	if err != nil {
		return nil, err
	}
	entries := make([]registrycreds.Entry, 0, len(rc.Registries))
	for _, r := range rc.Registries {
		entries = append(entries, registrycreds.Entry{Registry: r.Registry, Path: r.Path})
	}
	registries, err := registrycreds.New(source, entries)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) devcontainer.RegistryAuth { return registries.Operation(ctx) }, nil
}
