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
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// DefaultPath is where the workspace service looks for its configuration
// when no path is given.
const DefaultPath = "/etc/menagerie/config.yml"

// Default values for optional fields.
const (
	DefaultListen         = ":8080"
	DefaultTemplateSocket = "/run/menagerie/template.sock"
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
}

// Template is one approved template.
type Template struct {
	// ID selects the template in API requests.
	ID string `yaml:"id"`
	// Name is shown in the UI.
	Name string `yaml:"name"`
	// Address is the Toha template address.
	Address string `yaml:"address"`
}

// OIDC names the identity provider and client.
type OIDC struct {
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"client-id"`
}

// TemplateService locates the internal template service.
type TemplateService struct {
	// Socket is the Unix socket the template service listens on.
	Socket string `yaml:"socket"`
}

var templateIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

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
// errors, so a misspelled key cannot silently fall back to a default.
func Parse(data []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("document is empty")
		}
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("expected a single YAML document")
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.TemplateService.Socket == "" {
		c.TemplateService.Socket = DefaultTemplateSocket
	}
	if c.WorkspaceRoot != "" {
		c.WorkspaceRoot = filepath.Clean(c.WorkspaceRoot)
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
	if c.WorkspaceRoot == "/" {
		errs = append(errs, errors.New("workspace-root must not be the filesystem root"))
	}
	if c.WorkspaceRepository == "" {
		errs = append(errs, errors.New("workspace-repository is required"))
	}
	if c.OIDC.Issuer == "" {
		errs = append(errs, errors.New("oidc.issuer is required"))
	}
	if c.OIDC.ClientID == "" {
		errs = append(errs, errors.New("oidc.client-id is required"))
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
	}
	return errors.Join(errs...)
}
