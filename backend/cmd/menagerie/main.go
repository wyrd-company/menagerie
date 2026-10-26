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
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	dockerclient "github.com/docker/docker/client"

	"github.com/wyrd-company/menagerie/backend/internal/alerts"
	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/config"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/registrycreds"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
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

	opts, err := runtimeOptions(cfg)
	if err != nil {
		return err
	}
	parts := throwawayContainers(ctx, cfg, root, docker, &opts)
	runtime := devcontainer.New(docker, opts)
	hookErrors, stopHooks, err := serveHooks(ctx, cfg.TemplateService.HookRunnerSocket, parts.hooks)
	if err != nil {
		return err
	}
	defer stopHooks()
	jobOpts, webhook, err := jobOptions(cfg)
	if err != nil {
		return err
	}
	if webhook != nil {
		// Queued alerts get the delivery time limit, no more.
		defer webhook.Close()
	}
	manager := jobs.NewManager(ctx, jobOpts)
	handler, err := newHandler(cfg, root, runtime, manager, parts.hooks, parts.images)
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
	case err := <-hookErrors:
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

// containers are the parts that run template hooks and initializeCommand
// in throwaway containers.
type containers struct {
	docker *throwaway.Docker
	images *throwaway.Images
	hooks  *throwaway.HookRunner
}

// throwawayContainers composes the throwaway containers on docker, as the
// services' user, with the configured runner images, removes the ones a
// previous run left, and gives the devcontainer runtime options their
// initializeCommand runner.
func throwawayContainers(ctx context.Context, cfg *config.Config, root *workspace.Root, docker *dockerclient.Client, opts *devcontainer.Options) containers {
	images := runnerImages(cfg, docker)
	runner := throwaway.NewDocker(docker, throwaway.DockerOptions{
		Root:        root.Path(),
		User:        throwaway.ServiceUser(),
		Credentials: pullCredentials(opts.Credentials),
	})
	if n, err := runner.RemoveLeftovers(ctx); err != nil {
		log.Printf("menagerie: removing leftover throwaway containers: %v", err)
	} else if n > 0 {
		log.Printf("menagerie: removed %d leftover throwaway containers", n)
	}
	opts.Containers, opts.RunnerImage = runner, images.ForWorkspace
	return containers{docker: runner, images: images, hooks: throwaway.NewHookRunner(runner)}
}

// serveHooks serves hooks on the hook runner socket at path until stop,
// which also removes the socket. errs receives a serving failure.
func serveHooks(ctx context.Context, path string, hooks *throwaway.HookRunner) (errs <-chan error, stop func(), err error) {
	listener, err := throwaway.ListenHookRunner(path)
	if err != nil {
		return nil, nil, fmt.Errorf("hook runner socket: %w", err)
	}
	server := &http.Server{Handler: hooks, BaseContext: func(net.Listener) context.Context { return ctx }}
	failed := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
			failed <- fmt.Errorf("hook runner socket: %w", err)
		}
	}()
	return failed, func() {
		_ = server.Close()
		_ = os.Remove(path)
	}, nil
}

// newHandler is the workspace service's whole HTTP surface: the workspace
// API behind manager sign-in.
func newHandler(cfg *config.Config, root *workspace.Root, runtime *devcontainer.Runtime, manager *jobs.Manager, hooks *throwaway.HookRunner, images *throwaway.Images) (http.Handler, error) {
	return signedIn(newServer(cfg, root, runtime, manager, hooks, images), cfg.OIDC)
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
// Template hooks run through hooks in the template's runner image; nil
// hooks refuse every hook.
func newServer(cfg *config.Config, root *workspace.Root, runtime *devcontainer.Runtime, manager *jobs.Manager, hooks *throwaway.HookRunner, images *throwaway.Images) *api.Server {
	approved := make([]api.Template, 0, len(cfg.Templates))
	ids := make(map[string]bool, len(cfg.Templates))
	for _, t := range cfg.Templates {
		approved = append(approved, api.Template{ID: t.ID, Name: t.Name})
		ids[t.ID] = true
	}
	var image func(context.Context, string) (string, error)
	if images != nil {
		image = images.ForTemplate
	}
	flows := workflows.New(workflows.Options{
		Root:        root,
		Templates:   templates.New(cfg.TemplateService.Socket),
		Approved:    ids,
		Remote:      backupRemote(cfg),
		Runtime:     runtime,
		Hooks:       hooks,
		RunnerImage: image,
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

// jobOptions configures the jobs manager: the configured time limits, and
// the alert webhook when one is configured. Console links in alerts start
// with the browser origin of the OIDC redirect URL.
func jobOptions(cfg *config.Config) (jobs.Options, *alerts.Webhook, error) {
	var opts jobs.Options
	if l := cfg.JobLimits; l != nil {
		opts.Limits = jobs.Limits{Default: l.Default.Value(), Operations: map[string]time.Duration{}, Phases: map[string]time.Duration{}}
		for name, d := range l.Operations.ByName() {
			if d != "" {
				opts.Limits.Operations[name] = d.Value()
			}
		}
		for name, d := range l.Phases {
			opts.Limits.Phases[name] = d.Value()
		}
	}
	if cfg.Alerts == nil {
		return opts, nil, nil
	}
	redirect, err := url.Parse(cfg.OIDC.RedirectURL)
	if err != nil {
		return opts, nil, fmt.Errorf("oidc.redirect-url: %w", err)
	}
	webhook, err := alerts.New(alerts.Options{URL: string(cfg.Alerts.Webhook), Origin: redirect.Scheme + "://" + redirect.Host})
	if err != nil {
		return opts, nil, err
	}
	opts.Alerts, opts.LongRunning = webhook, cfg.Alerts.LongRunning.Value()
	return opts, webhook, nil
}

// runnerImages chooses the throwaway containers' images: each template's
// runner-image, else the global one, else Menagerie's own image.
func runnerImages(cfg *config.Config, docker throwaway.Inspector) *throwaway.Images {
	byTemplate := map[string]string{}
	for _, t := range cfg.Templates {
		if t.RunnerImage != "" {
			byTemplate[t.ID] = t.RunnerImage
		}
	}
	return &throwaway.Images{Default: cfg.RunnerImage, Templates: byTemplate, Docker: docker}
}

// pullCredentials gives runner image pulls the credential source of the
// devcontainer operations.
func pullCredentials(source func(context.Context) devcontainer.RegistryAuth) func(context.Context) throwaway.RegistryAuth {
	if source == nil {
		return nil
	}
	return func(ctx context.Context) throwaway.RegistryAuth {
		if auth := source(ctx); auth != nil {
			return auth
		}
		return nil
	}
}

// runtimeOptions configures the devcontainer runtime: registry
// credentials, and the local-env allowlist over Menagerie's environment.
func runtimeOptions(cfg *config.Config) (devcontainer.Options, error) {
	credentials, err := registryCredentials(cfg)
	if err != nil {
		return devcontainer.Options{}, err
	}
	return devcontainer.Options{Credentials: credentials, LocalEnv: cfg.LocalEnv}, nil
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
