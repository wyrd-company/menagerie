// ---
// relationships:
//   implements: configuration
//   references: menagerie
// ---

// Package config reads the Menagerie configuration document.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/distribution/reference"
	"gopkg.in/yaml.v3"

	"github.com/wyrd-company/menagerie/backend/internal/alerts"
	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/githubapp"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/registrycreds"
)

// DefaultPath is where the workspace service looks for its configuration
// when no path is given.
const DefaultPath = "/etc/menagerie/config.yml"

// Default values for optional fields.
const (
	DefaultListen           = ":8080"
	DefaultTemplateSocket   = "/run/menagerie/template.sock"
	DefaultHookRunnerSocket = "/run/menagerie/hook-runner.sock"
	DefaultCandidateFolder  = "/var/lib/menagerie/candidates"
)

// Config is the Menagerie configuration document.
type Config struct {
	// Listen is the HTTP listen address of the workspace service.
	Listen string `yaml:"listen"`
	// WorkspaceRoot is the folder whose direct child folders are the
	// workspaces. It has the same path in the container and on the host.
	WorkspaceRoot string `yaml:"workspace-root"`
	// WorkspaceRepository is the Git remote that holds one backup branch
	// per workspace.
	WorkspaceRepository string `yaml:"workspace-repository"`
	// WyrwoodDirectory is the Wyrwood consumer directory that holds the
	// SSH agent socket.
	WyrwoodDirectory string `yaml:"wyrwood-directory"`
	// Templates is the approved template list.
	Templates []Template `yaml:"templates"`
	// OIDC names the identity provider and client.
	OIDC OIDC `yaml:"oidc"`
	// TemplateService locates the internal template service.
	TemplateService TemplateService `yaml:"template-service"`
	// RegistryCredentials names the OpenBao secrets that hold container
	// registry credentials. Absent, every registry is anonymous.
	RegistryCredentials *RegistryCredentials `yaml:"registry-credentials"`
	// LocalEnv names the variables of Menagerie's own environment that a
	// devcontainer definition may read through ${localEnv:NAME}. Absent,
	// a definition reads none.
	LocalEnv []string `yaml:"local-env"`
	// JobLimits limits the running time of jobs. Absent, no job has a
	// limit.
	JobLimits *JobLimits `yaml:"job-limits"`
	// Alerts names the webhook told about failed and long-running jobs.
	// Absent, no alert is sent.
	Alerts *Alerts `yaml:"alerts"`
	// RunnerImage is the image of the throwaway containers that run
	// initializeCommand and the hooks of templates without their own
	// runner image. Absent, Menagerie's own image.
	RunnerImage string `yaml:"runner-image"`
	// AutoCommit commits and backs up every idle workspace that has
	// changes, on a schedule. Absent, no commit is scheduled.
	AutoCommit *AutoCommit `yaml:"auto-commit"`
	// WorkspaceSocket gives each workspace's devcontainer a socket that
	// commits and backs up its own workspace. Absent, no socket.
	WorkspaceSocket *WorkspaceSocket `yaml:"workspace-socket"`
	// GitHubApp authenticates backups and template fetches over https to
	// repositories on its server with installation tokens. Absent, no
	// token is minted.
	GitHubApp *GitHubApp `yaml:"github-app"`
}

// GitHubApp names the GitHub App whose installation tokens Menagerie uses
// for https repository URLs on its server. The App's private key is a
// Transit key in OpenBao, used through the Wyrwood vault socket.
type GitHubApp struct {
	// AppID is the App's id.
	AppID int64 `yaml:"app-id"`
	// TransitKey is the name of the Transit key that holds the App's
	// private key.
	TransitKey string `yaml:"transit-key"`
	// Installations maps an account name to the App's installation on it.
	// A repository whose owner is not listed has its installation looked
	// up.
	Installations map[string]int64 `yaml:"installations"`
	// Server is the GitHub server's https origin. Absent,
	// githubapp.DefaultServer.
	Server string `yaml:"server"`
}

// AutoCommit is the schedule of automatic commits.
type AutoCommit struct {
	// Every is the time between two sweeps of the workspace root; the
	// first is one interval after start-up.
	Every Duration `yaml:"every"`
}

// DefaultMinInterval is the workspace socket's minimum interval when none
// is given.
const DefaultMinInterval = time.Minute

// WorkspaceSocket configures the per-workspace sockets.
type WorkspaceSocket struct {
	// Folder holds one folder per workspace with its socket and client. It
	// has the same path in the container and on the host, and belongs to
	// the services' user.
	Folder string `yaml:"folder"`
	// MinInterval is the shortest time between two accepted commits of a
	// workspace through its socket: a duration, or 0 for none. Absent,
	// DefaultMinInterval.
	MinInterval Duration `yaml:"min-interval"`
}

// MinIntervalValue is the minimum interval, with its default.
func (w *WorkspaceSocket) MinIntervalValue() time.Duration {
	if w.MinInterval == "" {
		return DefaultMinInterval
	}
	return w.MinInterval.Value()
}

// Duration is a positive time span of whole hours, minutes and seconds,
// such as 2h, 45m or 1h30m. Empty is an absent field: a field given with
// no value is an error (see givenEmpty).
type Duration string

// durationPattern is the form of a Duration: units h, m and s, in that
// order, each at most once.
var durationPattern = regexp.MustCompile(`^([0-9]+h([0-9]+m)?([0-9]+s)?|[0-9]+m([0-9]+s)?|[0-9]+s)$`)

// Value is the duration; zero when empty.
func (d Duration) Value() time.Duration {
	v, _ := time.ParseDuration(string(d))
	return v
}

func (d Duration) validate(field string) error {
	if d == "" {
		return nil
	}
	if !durationPattern.MatchString(string(d)) {
		return fmt.Errorf("%s must be whole hours, minutes and seconds, such as 2h, 45m or 1h30m", field)
	}
	if v, err := time.ParseDuration(string(d)); err != nil || v <= 0 {
		return fmt.Errorf("%s must be a positive duration such as 45m", field)
	}
	return nil
}

// JobLimits are the time limits on a job's running time. Time in
// waiting-for-answers does not count.
type JobLimits struct {
	// Default limits every operation without its own limit.
	Default Duration `yaml:"default"`
	// Operations limits jobs by operation.
	Operations OperationLimits `yaml:"operations"`
	// Phases limits each stay in a phase, keyed by a name in jobs.Phases.
	Phases map[string]Duration `yaml:"phases"`
}

// OperationLimits is one optional limit per operation.
type OperationLimits struct {
	Start   Duration `yaml:"start"`
	Stop    Duration `yaml:"stop"`
	Rebuild Duration `yaml:"rebuild"`
	Create  Duration `yaml:"create"`
	Upgrade Duration `yaml:"upgrade"`
	Commit  Duration `yaml:"commit"`
	// ApplyBranch limits apply-branch jobs.
	ApplyBranch Duration `yaml:"apply-branch"`
}

// ByName maps each operation name to its limit, empty included.
func (o OperationLimits) ByName() map[string]Duration {
	return map[string]Duration{"start": o.Start, "stop": o.Stop, "rebuild": o.Rebuild, "create": o.Create, "upgrade": o.Upgrade, "commit": o.Commit, "apply-branch": o.ApplyBranch}
}

// Alerts names the webhook that receives job alerts.
type Alerts struct {
	// Webhook is the http or https URL alerts are posted to. It can carry
	// a credential and is never logged.
	Webhook WebhookURL `yaml:"webhook"`
	// LongRunning, when given, sends one alert for a job whose running
	// time passes it.
	LongRunning Duration `yaml:"long-running"`
}

// WebhookURL is the alert webhook URL. It can carry a credential, so no
// configuration error names its value.
type WebhookURL string

// UnmarshalYAML accepts a string and refuses any other node with a fixed
// message.
func (w *WebhookURL) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		return fieldError("alerts.webhook must be an http or https URL")
	}
	*w = WebhookURL(n.Value)
	return nil
}

// Template is one approved template.
type Template struct {
	// ID selects the template in API requests.
	ID string `yaml:"id"`
	// Name is shown in the UI.
	Name string `yaml:"name"`
	// Address is the Toha template address.
	Address string `yaml:"address"`
	// RunnerImage is the image of the throwaway containers that run the
	// template's hooks. Absent, the global runner-image.
	RunnerImage string `yaml:"runner-image"`
}

// OIDC names the identity provider and Menagerie's public client.
type OIDC struct {
	// Issuer is the OIDC issuer URL.
	Issuer string `yaml:"issuer"`
	// ClientID is the public client's id. The client has no secret.
	ClientID string `yaml:"client-id"`
	// RedirectURL is the redirect URI registered for the client: the
	// browser-facing origin followed by /auth/callback.
	RedirectURL string `yaml:"redirect-url"`
	// Scopes are the requested scopes; absent, openid, profile and
	// groups.
	Scopes []string `yaml:"scopes"`
}

// TemplateService locates the internal template service and the hook
// runner it calls back.
type TemplateService struct {
	// Socket is the Unix socket the template service listens on.
	Socket string `yaml:"socket"`
	// HookRunnerSocket is the Unix socket the workspace service listens on
	// for the template service's hooks.
	HookRunnerSocket string `yaml:"hook-runner-socket"`
	// CandidateFolder is the template service's temporary folder, where
	// Toha builds an upgrade's candidate and its hooks run. The hook
	// runner accepts an upgrade's hook target only inside it.
	CandidateFolder string `yaml:"candidate-folder"`
}

// RegistryCredentials names, per registry, the secret in a KV version 2
// mount that holds its credentials. OpenBao is read through the vault
// socket in the Wyrwood directory.
type RegistryCredentials struct {
	KVMount    string     `yaml:"kv-mount"`
	Registries []Registry `yaml:"registries"`
}

// Registry is one registry's credential secret.
type Registry struct {
	// Registry is the registry host, such as ghcr.io or docker.io.
	Registry string `yaml:"registry"`
	// Path is the secret's path in the KV mount.
	Path string `yaml:"path"`
}

var (
	templateIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	envNamePattern    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("configuration %s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates a configuration document. Unknown fields are
// errors, so a misspelled key cannot silently fall back to a default. No
// error names the value of a duration or of the alerts webhook URL: an
// anchor can share the webhook's node with any field.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("document is empty")
		}
		return nil, decodeError(data, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected a single YAML document")
	}
	cfg.applyDefaults()
	if err := errors.Join(append(givenEmpty(data), cfg.validate())...); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// durationNodes holds the node given for each duration field, with merge
// keys applied by the decoder. A node of kind zero is a field not given.
type durationNodes struct {
	JobLimits *struct {
		Default    yaml.Node            `yaml:"default"`
		Operations map[string]yaml.Node `yaml:"operations"`
		Phases     map[string]yaml.Node `yaml:"phases"`
	} `yaml:"job-limits"`
	Alerts *struct {
		LongRunning yaml.Node `yaml:"long-running"`
	} `yaml:"alerts"`
	AutoCommit *struct {
		Every yaml.Node `yaml:"every"`
	} `yaml:"auto-commit"`
	WorkspaceSocket *struct {
		MinInterval yaml.Node `yaml:"min-interval"`
	} `yaml:"workspace-socket"`
}

// givenEmpty reports the duration fields given with no value, such as
// "rebuild:", `default: ""` or an alias of an empty value. They decode as
// if absent, which would drop a limit or fall back to another without a
// word, so the nodes the decoder chose for them tell them apart.
func givenEmpty(data []byte) []error {
	var doc durationNodes
	if yaml.Unmarshal(data, &doc) != nil {
		return nil
	}
	var errs []error
	check := func(field string, n *yaml.Node) {
		n = resolved(n)
		if n.Kind == yaml.ScalarNode && (n.ShortTag() == "!!null" || n.Value == "") {
			errs = append(errs, fmt.Errorf("%s must be a duration", field))
		}
	}
	if l := doc.JobLimits; l != nil {
		check("job-limits.default", &l.Default)
		for _, group := range []struct {
			name  string
			nodes map[string]yaml.Node
		}{{"operations", l.Operations}, {"phases", l.Phases}} {
			for _, name := range slices.Sorted(maps.Keys(group.nodes)) {
				n := group.nodes[name]
				if !keyPattern.MatchString(name) {
					continue
				}
				check("job-limits."+group.name+"."+name, &n)
			}
		}
	}
	if a := doc.Alerts; a != nil {
		check("alerts.long-running", &a.LongRunning)
	}
	if a := doc.AutoCommit; a != nil {
		check("auto-commit.every", &a.Every)
	}
	if w := doc.WorkspaceSocket; w != nil {
		check("workspace-socket.min-interval", &w.MinInterval)
	}
	return errs
}

// within reports whether path is root or inside it, lexically.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || filepath.IsLocal(rel))
}

// resolved follows aliases to the node they name.
func resolved(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.TemplateService.Socket == "" {
		c.TemplateService.Socket = DefaultTemplateSocket
	}
	if c.TemplateService.HookRunnerSocket == "" {
		c.TemplateService.HookRunnerSocket = DefaultHookRunnerSocket
	}
	if c.TemplateService.CandidateFolder == "" {
		c.TemplateService.CandidateFolder = DefaultCandidateFolder
	}
	if c.WorkspaceRoot != "" {
		c.WorkspaceRoot = filepath.Clean(c.WorkspaceRoot)
	}
	if c.GitHubApp != nil && c.GitHubApp.Server == "" {
		c.GitHubApp.Server = githubapp.DefaultServer
	}
}

func (c *Config) validate() error {
	var errs []error
	requireAbsolute := func(field, value string) {
		switch {
		case value == "":
			errs = append(errs, fmt.Errorf("%s is required", field))
		case !filepath.IsAbs(value):
			errs = append(errs, fmt.Errorf("%s must be an absolute path", field))
		}
	}
	requireAbsolute("workspace-root", c.WorkspaceRoot)
	requireAbsolute("wyrwood-directory", c.WyrwoodDirectory)
	if !filepath.IsAbs(c.TemplateService.Socket) {
		errs = append(errs, errors.New("template-service.socket must be an absolute path"))
	}
	if !filepath.IsAbs(c.TemplateService.HookRunnerSocket) {
		errs = append(errs, errors.New("template-service.hook-runner-socket must be an absolute path"))
	} else if filepath.Clean(c.TemplateService.HookRunnerSocket) == filepath.Clean(c.TemplateService.Socket) {
		errs = append(errs, errors.New("template-service.hook-runner-socket must differ from template-service.socket"))
	}
	if candidates := c.TemplateService.CandidateFolder; !filepath.IsAbs(candidates) || filepath.Clean(candidates) != candidates || candidates == "/" {
		errs = append(errs, errors.New("template-service.candidate-folder must be an absolute clean folder other than /"))
	} else if filepath.IsAbs(c.WorkspaceRoot) && (within(c.WorkspaceRoot, candidates) || within(candidates, c.WorkspaceRoot)) {
		errs = append(errs, errors.New("template-service.candidate-folder must be neither inside workspace-root nor contain it"))
	}
	if err := validImage(c.RunnerImage); err != nil {
		errs = append(errs, fmt.Errorf("runner-image %w", err))
	}
	if c.WorkspaceRoot == "/" {
		errs = append(errs, errors.New("workspace-root must not be the filesystem root"))
	}
	if c.WorkspaceRepository == "" {
		errs = append(errs, errors.New("workspace-repository is required"))
	}
	if c.OIDC.Issuer == "" {
		errs = append(errs, errors.New("oidc.issuer is required"))
	} else if err := auth.ValidateIssuer(c.OIDC.Issuer); err != nil {
		errs = append(errs, fmt.Errorf("oidc.issuer %w", err))
	}
	if c.OIDC.ClientID == "" {
		errs = append(errs, errors.New("oidc.client-id is required"))
	}
	if c.OIDC.RedirectURL == "" {
		errs = append(errs, errors.New("oidc.redirect-url is required"))
	} else if err := auth.ValidateRedirectURL(c.OIDC.RedirectURL); err != nil {
		errs = append(errs, fmt.Errorf("oidc.redirect-url %w", err))
	}
	if c.OIDC.Scopes != nil && len(c.OIDC.Scopes) == 0 {
		errs = append(errs, errors.New("oidc.scopes must not be empty when given"))
	} else if err := auth.ValidateScopes(c.OIDC.Scopes); err != nil {
		errs = append(errs, fmt.Errorf("oidc.scopes %w", err))
	}
	seen := map[string]bool{}
	for i, t := range c.Templates {
		switch {
		case !templateIDPattern.MatchString(t.ID):
			errs = append(errs, fmt.Errorf("templates[%d].id must be lower-case letters and digits separated by single dashes", i))
		case seen[t.ID]:
			errs = append(errs, fmt.Errorf("templates[%d].id %q is not unique", i, t.ID))
		}
		seen[t.ID] = true
		if t.Name == "" {
			errs = append(errs, fmt.Errorf("templates[%d].name is required", i))
		}
		if t.Address == "" {
			errs = append(errs, fmt.Errorf("templates[%d].address is required", i))
		}
		if err := validImage(t.RunnerImage); err != nil {
			errs = append(errs, fmt.Errorf("templates[%d].runner-image %w", i, err))
		}
	}
	if c.RegistryCredentials != nil {
		errs = append(errs, c.RegistryCredentials.validate()...)
	}
	if c.JobLimits != nil {
		errs = append(errs, c.JobLimits.validate()...)
	}
	if c.Alerts != nil {
		errs = append(errs, c.Alerts.validate()...)
	}
	if c.AutoCommit != nil {
		if c.AutoCommit.Every == "" {
			errs = append(errs, errors.New("auto-commit.every is required"))
		} else if err := c.AutoCommit.Every.validate("auto-commit.every"); err != nil {
			errs = append(errs, err)
		}
	}
	if c.WorkspaceSocket != nil {
		errs = append(errs, c.WorkspaceSocket.validate(c)...)
	}
	if c.GitHubApp != nil {
		errs = append(errs, c.GitHubApp.validate()...)
	}
	names := map[string]int{}
	for i, name := range c.LocalEnv {
		if !envNamePattern.MatchString(name) {
			errs = append(errs, fmt.Errorf("local-env[%d] must be a variable name: letters, digits and underscores, not starting with a digit", i))
		} else if first, dup := names[name]; dup {
			errs = append(errs, fmt.Errorf("local-env[%d] names %s, as local-env[%d] does", i, name, first))
		} else {
			names[name] = i
		}
	}
	return errors.Join(errs...)
}

func (l *JobLimits) validate() []error {
	var errs []error
	if err := l.Default.validate("job-limits.default"); err != nil {
		errs = append(errs, err)
	}
	ops := l.Operations.ByName()
	for _, name := range slices.Sorted(maps.Keys(ops)) {
		if err := ops[name].validate("job-limits.operations." + name); err != nil {
			errs = append(errs, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(l.Phases)) {
		if !slices.Contains(jobs.Phases, name) {
			key := "job-limits.phases." + name
			if !keyPattern.MatchString(name) {
				key = "a key of job-limits.phases"
			}
			errs = append(errs, fmt.Errorf("%s is not a phase; phases are %s", key, strings.Join(jobs.Phases, ", ")))
			continue
		}
		if err := l.Phases[name].validate("job-limits.phases." + name); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

func (w *WorkspaceSocket) validate(c *Config) []error {
	var errs []error
	switch folder := w.Folder; {
	case folder == "":
		errs = append(errs, errors.New("workspace-socket.folder is required"))
	case !filepath.IsAbs(folder) || filepath.Clean(folder) != folder || folder == "/":
		errs = append(errs, errors.New("workspace-socket.folder must be an absolute clean folder other than /"))
	case strings.Contains(folder, ","):
		// A Compose mount is comma-separated fields.
		errs = append(errs, errors.New("workspace-socket.folder must not hold a comma"))
	default:
		for _, other := range []struct{ field, path string }{
			{"workspace-root", c.WorkspaceRoot},
			{"wyrwood-directory", c.WyrwoodDirectory},
			{"template-service.candidate-folder", c.TemplateService.CandidateFolder},
		} {
			if filepath.IsAbs(other.path) && (within(other.path, folder) || within(folder, other.path)) {
				errs = append(errs, fmt.Errorf("workspace-socket.folder must be neither inside %s nor contain it", other.field))
			}
		}
	}
	if w.MinInterval != "0" {
		if err := w.MinInterval.validate("workspace-socket.min-interval"); err != nil {
			errs = append(errs, fmt.Errorf("%w, or 0 for none", err))
		}
	}
	return errs
}

// validate checks the GitHub App block. No error names a value.
func (g *GitHubApp) validate() []error {
	var errs []error
	if g.AppID <= 0 {
		errs = append(errs, errors.New("github-app.app-id must be a positive integer"))
	}
	if g.TransitKey == "" {
		errs = append(errs, errors.New("github-app.transit-key is required"))
	} else if !githubapp.ValidTransitKey(g.TransitKey) {
		errs = append(errs, errors.New("github-app.transit-key must be one Transit key name: letters, digits, '_', '.' and '-', starting with a letter or digit"))
	}
	if err := githubapp.ValidServer(g.Server); err != nil {
		errs = append(errs, fmt.Errorf("github-app.server %w", err))
	}
	owners := map[string]bool{}
	var badOwner, badID, twice bool
	for owner, id := range g.Installations {
		badOwner = badOwner || !githubapp.ValidOwner(owner)
		badID = badID || id <= 0
		twice = twice || owners[strings.ToLower(owner)]
		owners[strings.ToLower(owner)] = true
	}
	if badOwner {
		errs = append(errs, errors.New("github-app.installations has a key that is not a GitHub account name"))
	}
	if badID {
		errs = append(errs, errors.New("github-app.installations has an installation id that is not a positive integer"))
	}
	if twice {
		errs = append(errs, errors.New("github-app.installations names one account twice; account names are compared in lower case"))
	}
	return errs
}

func (a *Alerts) validate() []error {
	var errs []error
	if a.Webhook == "" {
		errs = append(errs, errors.New("alerts.webhook is required"))
	} else if err := alerts.ValidURL(string(a.Webhook)); err != nil {
		errs = append(errs, fmt.Errorf("alerts.webhook %w", err))
	}
	if err := a.LongRunning.validate("alerts.long-running"); err != nil {
		errs = append(errs, err)
	}
	return errs
}

func (r *RegistryCredentials) validate() []error {
	var errs []error
	if err := validSecretPath(r.KVMount); err != nil {
		errs = append(errs, fmt.Errorf("registry-credentials.kv-mount %w", err))
	}
	seen := map[string]int{}
	for i, reg := range r.Registries {
		name, err := registrycreds.NormalizeRegistry(reg.Registry)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("registry-credentials.registries[%d].registry: %w", i, err))
		default:
			if first, dup := seen[name]; dup {
				errs = append(errs, fmt.Errorf("registry-credentials.registries[%d].registry names %s, as registries[%d] does", i, name, first))
			} else {
				seen[name] = i
			}
		}
		if err := validSecretPath(reg.Path); err != nil {
			errs = append(errs, fmt.Errorf("registry-credentials.registries[%d].path %w", i, err))
		}
	}
	return errs
}

// validImage accepts an absent image or a Docker image reference, such as
// alpine:3.22 or ghcr.io/example/runner@sha256:...
func validImage(ref string) error {
	if ref == "" {
		return nil
	}
	if _, err := reference.ParseNormalizedNamed(ref); err != nil {
		return errors.New("must be an image reference such as alpine:3.22")
	}
	return nil
}

// validSecretPath accepts slash-separated OpenBao path segments without
// empty, "." or ".." segments.
func validSecretPath(p string) error {
	if p == "" {
		return errors.New("is required")
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.TrimSpace(seg) != seg {
			return fmt.Errorf("%q must be slash-separated segments without empty, \".\" or \"..\" segments or surrounding spaces", p)
		}
	}
	return nil
}
