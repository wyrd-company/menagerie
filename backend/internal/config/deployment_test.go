package config

import (
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// composeService is the part of deploy/compose.yml the deployment relies on.
type composeService struct {
	Ports       []string          `yaml:"ports"`
	Environment map[string]string `yaml:"environment"`
	Volumes     []string          `yaml:"volumes"`
	Restart     string            `yaml:"restart"`
}

// interpolate applies Compose's ${NAME:-default} and ${NAME:?message}
// forms with every variable unset, the way the example runs by default.
func interpolate(t *testing.T, s string) string {
	t.Helper()
	for {
		start := strings.Index(s, "${")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], "}")
		expr := s[start+2 : start+end]
		value := "<required>"
		if _, def, ok := strings.Cut(expr, ":-"); ok {
			value = def
		}
		s = s[:start] + value + s[start+end+1:]
	}
}

// TestDeploymentExampleAgreesWithTheImage loads deploy/config.yml with the
// workspace service's own loader and checks that deploy/compose.yml, the
// Dockerfile and the image's scripts name the same paths and port.
func TestDeploymentExampleAgreesWithTheImage(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "..", "deploy", "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Services map[string]composeService `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(repoFile(t, "deploy/compose.yml")), &compose); err != nil {
		t.Fatal(err)
	}
	svc, ok := compose.Services["menagerie"]
	if !ok {
		t.Fatal("deploy/compose.yml has no menagerie service")
	}
	mounts := map[string]string{}
	for _, v := range svc.Volumes {
		parts := strings.Split(interpolate(t, v), ":")
		if len(parts) < 2 {
			t.Fatalf("volume %q", v)
		}
		mounts[parts[1]] = parts[0]
	}

	if src, ok := mounts[cfg.WorkspaceRoot]; !ok || src != cfg.WorkspaceRoot {
		t.Errorf("workspace-root %s is mounted from %q, not from the same host path", cfg.WorkspaceRoot, src)
	}
	if _, ok := mounts[cfg.WyrwoodDirectory]; !ok {
		t.Errorf("wyrwood-directory %s is not mounted", cfg.WyrwoodDirectory)
	}
	if got, want := svc.Environment["SSH_AUTH_SOCK"], path.Join(cfg.WyrwoodDirectory, "ssh-agent.sock"); got != want {
		t.Errorf("SSH_AUTH_SOCK %q, want %q", got, want)
	}
	if src := mounts[DefaultPath]; src != "./config.yml" {
		t.Errorf("%s is mounted from %q", DefaultPath, src)
	}
	if src := mounts["/var/run/docker.sock"]; src != "/var/run/docker.sock" {
		t.Errorf("the Docker socket is mounted from %q", src)
	}
	if svc.Restart != "unless-stopped" {
		t.Errorf("restart %q", svc.Restart)
	}

	// One port: the workspace service's listener, exposed by the image and
	// asked by its health check. The template service's socket folder is the
	// one the image's prepare script makes private.
	port := strings.TrimPrefix(cfg.Listen, ":")
	if cfg.Listen != DefaultListen || len(svc.Ports) != 1 || !strings.HasSuffix(svc.Ports[0], ":"+port) {
		t.Errorf("listen %q, compose ports %v", cfg.Listen, svc.Ports)
	}
	if !strings.Contains(repoFile(t, "Dockerfile"), "\nEXPOSE "+port+"\n") {
		t.Errorf("the Dockerfile does not expose %s", port)
	}
	if !strings.Contains(repoFile(t, "image/rootfs/usr/local/bin/menagerie-healthcheck"), "http://127.0.0.1:"+port+"/healthz") {
		t.Errorf("the health check does not ask port %s", port)
	}
	if !strings.Contains(repoFile(t, "image/rootfs/etc/s6-overlay/scripts/menagerie-prepare"), "-m 0700 -o root -g root "+path.Dir(cfg.TemplateService.Socket)+"\n") {
		t.Errorf("the prepare script does not make %s private", path.Dir(cfg.TemplateService.Socket))
	}
}
