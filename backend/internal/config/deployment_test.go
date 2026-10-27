package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
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
	ContainerName string            `yaml:"container_name"`
	User          string            `yaml:"user"`
	CapDrop       []string          `yaml:"cap_drop"`
	CapAdd        []string          `yaml:"cap_add"`
	ReadOnly      bool              `yaml:"read_only"`
	NetworkMode   string            `yaml:"network_mode"`
	Ports         []string          `yaml:"ports"`
	Environment   map[string]string `yaml:"environment"`
	Volumes       []string          `yaml:"volumes"`
	Restart       string            `yaml:"restart"`
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
	mounts, modes := volumes(t, svc)

	if src, ok := mounts[cfg.WorkspaceRoot]; !ok || src != cfg.WorkspaceRoot {
		t.Errorf("workspace-root %s is mounted from %q, not from the same host path", cfg.WorkspaceRoot, src)
	}
	if cfg.WorkspaceSocket == nil {
		t.Error("the example has no workspace-socket")
	} else if src, ok := mounts[cfg.WorkspaceSocket.Folder]; !ok || src != cfg.WorkspaceSocket.Folder {
		t.Errorf("workspace-socket.folder %s is mounted from %q, not from the same host path", cfg.WorkspaceSocket.Folder, src)
	}
	// Wyrwood names a container's consumer directory after the container
	// under its consumer root, /run/wyrwood/containers by default.
	if src, ok := mounts[cfg.WyrwoodDirectory]; !ok || src != "/run/wyrwood/containers/"+svc.ContainerName || svc.ContainerName == "" {
		t.Errorf("wyrwood-directory %s is mounted from %q, not the default consumer directory of container %q", cfg.WyrwoodDirectory, src, svc.ContainerName)
	}
	if got, want := svc.Environment["SSH_AUTH_SOCK"], path.Join(cfg.WyrwoodDirectory, "ssh-agent.sock"); got != want {
		t.Errorf("SSH_AUTH_SOCK %q, want %q", got, want)
	}
	if src := mounts[DefaultPath]; src != "./config.yml" {
		t.Errorf("%s is mounted from %q", DefaultPath, src)
	}
	if src, ok := mounts["/var/run/docker.sock"]; ok {
		t.Errorf("Menagerie mounts the Docker socket from %q; it reaches Docker through the proxy only", src)
	}
	if cfg.ComposeStagingFolder == "" {
		t.Error("the example has no compose-staging-folder")
	} else if src := mounts[cfg.ComposeStagingFolder]; src != cfg.ComposeStagingFolder {
		t.Errorf("compose-staging-folder %s is mounted from %q, not from the same host path", cfg.ComposeStagingFolder, src)
	}
	if svc.Restart != "unless-stopped" {
		t.Errorf("restart %q", svc.Restart)
	}

	// One port: the workspace service's listener, exposed by the image and
	// asked by its health check. The template service's socket folder is the
	// one the image's start-up helper makes private to the services' user.
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
	dir := path.Dir(cfg.TemplateService.Socket)
	if want := fmt.Sprintf("{parent: %q, name: %q, mode: 0o700}", path.Dir(dir), path.Base(dir)); !strings.Contains(repoFile(t, "backend/cmd/menagerie-folders/main.go"), want) {
		t.Errorf("menagerie-folders does not make %s private: no %s", dir, want)
	}
	if !strings.Contains(repoFile(t, "image/rootfs/etc/s6-overlay/scripts/menagerie-prepare"), "/usr/local/bin/menagerie-folders -uid") {
		t.Error("the prepare script does not run menagerie-folders")
	}
	checkProxy(t, cfg, compose.Services, svc, mounts, modes)
}

// volumes maps each of svc's mount targets to its source and its mode.
func volumes(t *testing.T, svc composeService) (map[string]string, map[string]string) {
	t.Helper()
	mounts, modes := map[string]string{}, map[string]string{}
	for _, v := range svc.Volumes {
		parts := strings.Split(interpolate(t, v), ":")
		if len(parts) < 2 {
			t.Fatalf("volume %q", v)
		}
		mounts[parts[1]] = parts[0]
		if len(parts) > 2 {
			modes[parts[1]] = parts[2]
		}
	}
	return mounts, modes
}

// sockguardProfile is the part of deploy/sockguard.yml the deployment
// relies on.
type sockguardProfile struct {
	Listeners []struct {
		Socket          string   `yaml:"socket"`
		SocketMode      string   `yaml:"socket_mode"`
		SocketGID       *int     `yaml:"socket_gid"`
		AllowedProfiles []string `yaml:"allowed_profiles"`
	} `yaml:"listeners"`
	Response struct {
		DenyVerbosity string `yaml:"deny_verbosity"`
	} `yaml:"response"`
	Ownership struct {
		Owner string `yaml:"owner"`
	} `yaml:"ownership"`
	WorkspaceExceptions struct {
		File          string `yaml:"file"`
		WorkspaceRoot string `yaml:"workspace_root"`
	} `yaml:"workspace_exceptions"`
	Clients struct {
		UnixPeerProfiles []struct {
			Profile string `yaml:"profile"`
			UIDs    []int  `yaml:"uids"`
		} `yaml:"unix_peer_profiles"`
		Profiles []struct {
			Name        string `yaml:"name"`
			RequestBody struct {
				ContainerCreate struct {
					AllowedBindMounts []string `yaml:"allowed_bind_mounts"`
				} `yaml:"container_create"`
			} `yaml:"request_body"`
		} `yaml:"profiles"`
	} `yaml:"clients"`
	Rules []struct {
		Action string `yaml:"action"`
	} `yaml:"rules"`
}

// checkProxy checks that the sidecar, deploy/sockguard.yml and Menagerie's
// service agree: one socket, the same bind roots at their host paths, the
// exceptions folder read-only in both, and nothing of Docker in Menagerie
// but the proxy's socket.
func checkProxy(t *testing.T, cfg *Config, services map[string]composeService, svc composeService, mounts, modes map[string]string) {
	t.Helper()
	proxy, ok := services["sockguard"]
	if !ok {
		t.Fatal("deploy/compose.yml has no sockguard service")
	}
	var profile sockguardProfile
	if err := yaml.Unmarshal([]byte(repoFile(t, "deploy/sockguard.yml")), &profile); err != nil {
		t.Fatal(err)
	}
	if len(profile.Listeners) != 1 || len(profile.Clients.Profiles) != 1 || len(profile.Clients.UnixPeerProfiles) != 1 {
		t.Fatalf("the profile has %d listeners, %d profiles and %d peer mappings, want one each", len(profile.Listeners), len(profile.Clients.Profiles), len(profile.Clients.UnixPeerProfiles))
	}
	listener, client := profile.Listeners[0], profile.Clients.Profiles[0]
	socket := strings.TrimPrefix(svc.Environment["DOCKER_HOST"], "unix://")
	if socket != listener.Socket || socket == svc.Environment["DOCKER_HOST"] {
		t.Errorf("DOCKER_HOST %q does not name the proxy's socket %s", svc.Environment["DOCKER_HOST"], listener.Socket)
	}
	dir := path.Dir(listener.Socket)
	pmounts, pmodes := volumes(t, proxy)
	if mounts[dir] == "" || mounts[dir] != pmounts[dir] || modes[dir] != "ro" {
		t.Errorf("the socket folder %s is %q:%s in Menagerie and %q in the proxy, want one volume, read-only in Menagerie", dir, mounts[dir], modes[dir], pmounts[dir])
	}
	// Group-readable, with a group of its own: the workspace service gets
	// the group and the template service, as the same user, does not.
	if listener.SocketMode != "0660" || listener.SocketGID == nil || *listener.SocketGID == 0 || !slices.Equal(listener.AllowedProfiles, []string{client.Name}) {
		t.Errorf("listener mode %s gid %v profiles %v", listener.SocketMode, listener.SocketGID, listener.AllowedProfiles)
	}
	if peers := profile.Clients.UnixPeerProfiles[0]; peers.Profile != client.Name || len(peers.UIDs) != 1 {
		t.Errorf("peer mapping %+v, want one uid for %s", peers, client.Name)
	}
	if src := pmounts["/var/run/docker.sock"]; src != "/var/run/docker.sock" {
		t.Errorf("the proxy mounts the Docker socket from %q", src)
	}
	if src := pmounts["/etc/sockguard/sockguard.yaml"]; src != "./sockguard.yml" || pmodes["/etc/sockguard/sockguard.yaml"] != "ro" {
		t.Errorf("the proxy's configuration is mounted from %q", src)
	}
	exceptions := path.Dir(profile.WorkspaceExceptions.File)
	for name, m := range map[string][2]map[string]string{"Menagerie": {mounts, modes}, "the proxy": {pmounts, pmodes}} {
		if m[0][exceptions] != "./sockguard-exceptions" || m[1][exceptions] != "ro" {
			t.Errorf("%s mounts %s from %q:%s, want the exceptions folder read-only", name, exceptions, m[0][exceptions], m[1][exceptions])
		}
	}
	var example struct {
		Version    int   `json:"version"`
		Exceptions []any `json:"exceptions"`
	}
	if err := json.Unmarshal([]byte(repoFile(t, "deploy/sockguard-exceptions/"+path.Base(profile.WorkspaceExceptions.File))), &example); err != nil || example.Version != 1 || example.Exceptions == nil {
		t.Errorf("the example exceptions file: %+v %v", example, err)
	}
	if profile.WorkspaceExceptions.WorkspaceRoot != cfg.WorkspaceRoot {
		t.Errorf("workspace_exceptions.workspace_root %s, want workspace-root %s", profile.WorkspaceExceptions.WorkspaceRoot, cfg.WorkspaceRoot)
	}
	roots := []string{cfg.WorkspaceRoot, cfg.WorkspaceSocket.Folder, cfg.ComposeStagingFolder}
	if got := client.RequestBody.ContainerCreate.AllowedBindMounts; !slices.Equal(got, roots) {
		t.Errorf("allowed_bind_mounts %v, want exactly %v", got, roots)
	}
	for _, root := range roots {
		if pmounts[root] != root || pmodes[root] != "ro" {
			t.Errorf("the proxy mounts %s from %q:%s, want its host path read-only", root, pmounts[root], pmodes[root])
		}
	}
	if profile.Response.DenyVerbosity != "verbose" || profile.Ownership.Owner == "" {
		t.Errorf("deny_verbosity %q, ownership owner %q", profile.Response.DenyVerbosity, profile.Ownership.Owner)
	}
	if len(profile.Rules) != 1 || profile.Rules[0].Action != "deny" {
		t.Errorf("root rules %+v, want one deny", profile.Rules)
	}
	if proxy.User != "0:0" || !slices.Equal(proxy.CapDrop, []string{"ALL"}) || !slices.Equal(proxy.CapAdd, []string{"CHOWN", "DAC_READ_SEARCH"}) || !proxy.ReadOnly || proxy.NetworkMode != "none" || proxy.Restart != "unless-stopped" {
		t.Errorf("the proxy runs as %+v", proxy)
	}
}
