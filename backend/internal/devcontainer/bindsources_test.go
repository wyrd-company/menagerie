package devcontainer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	dockerclient "github.com/docker/docker/client"
	"github.com/wyrd-company/devcontainer-builder/pkg/spec"
)

// bufferProgress keeps a job's console.
type bufferProgress struct {
	bytes.Buffer
	phases []string
}

func (p *bufferProgress) Phase(name string)      { p.phases = append(p.phases, name) }
func (p *bufferProgress) Log(_ string) io.Writer { return &p.Buffer }

// definitionFixture writes a workspace with the definition body under a new
// root, and loads it.
func definitionFixture(t *testing.T, body string) (string, *spec.DevContainer) {
	t.Helper()
	folder := filepath.Join(t.TempDir(), "sample")
	if err := os.MkdirAll(filepath.Join(folder, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, ".devcontainer", "devcontainer.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	dc, err := spec.LoadFromWorkspace(folder)
	if err != nil {
		t.Fatal(err)
	}
	return folder, dc
}

func TestBindSourcesReadsEveryFormOfMount(t *testing.T) {
	folder, dc := definitionFixture(t, `{
		"image": "sample",
		"workspaceMount": "type=bind,source=${localWorkspaceFolder},target=/work",
		"mounts": [
			"type=bind,source=${localWorkspaceFolder}/string,target=/a",
			"source=/no/type,target=/b",
			" src = /spaced , dst = /c , readonly ",
			"type=volume,source=named,target=/d",
			"type=bind,source=/no/target",
			"type=bind,source=relative,target=/e",
			{"type": "bind", "source": "${localWorkspaceFolder}/object", "target": "/f"},
			{"type": "volume", "source": "named", "target": "/g"},
			"type=bind,source=/env/${localEnv:SAMPLE},target=/h",
			"type=bind,source=/no/type,target=/again"
		]
	}`)
	got := bindSources(context.Background(), dc, folder, map[string]string{"SAMPLE": "value"})
	want := []string{folder, folder + "/string", "/no/type", "/spaced", folder + "/object", "/env/value"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources\n got %q\nwant %q", got, want)
	}
}

func TestBindSourcesReadsAComposeProjectsVolumes(t *testing.T) {
	folder, _ := definitionFixture(t, `{"image": "sample"}`)
	definition := `{
		"dockerComposeFile": "compose.yml",
		"service": "app",
		"workspaceMount": "type=bind,source=/ignored/in/compose,target=/work",
		"mounts": ["type=bind,source=/from/mounts,target=/a"],
		"runArgs": ["--network", "ignored-in-compose"]
	}`
	if err := os.WriteFile(filepath.Join(folder, ".devcontainer", "devcontainer.json"), []byte(definition), 0o644); err != nil {
		t.Fatal(err)
	}
	compose := `services:
  app:
    image: sample
    volumes:
      - type: bind
        source: ../long
        target: /long
      - named:/named
  other:
    image: sample
    volumes:
      - ../short:/short
volumes:
  named: {}
`
	if err := os.WriteFile(filepath.Join(folder, ".devcontainer", "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	dc, err := spec.LoadFromWorkspace(folder)
	if err != nil {
		t.Fatal(err)
	}
	got := bindSources(context.Background(), dc, folder, map[string]string{})
	want := map[string]bool{"/from/mounts": true, folder + "/long": true, folder + "/short": true}
	if len(got) != len(want) {
		t.Fatalf("sources %q", got)
	}
	for _, source := range got {
		if !want[source] {
			t.Fatalf("sources %q", got)
		}
	}
	if n := runNetwork(dc, SubstitutionContext(dc, folder, nil)); n != "" {
		t.Fatalf("network %q in Compose", n)
	}
}

func TestInsideFolder(t *testing.T) {
	cases := []struct {
		source, rel string
		inside      bool
	}{
		{"/root/sample", ".", true},
		{"/root/sample/a/b", "a/b", true},
		{"/root/sample//a/./b/", "a/b", true},
		{"/root/samples/a", "", false},
		{"/root", "", false},
		{"/elsewhere", "", false},
		{"/root/sample/../other", "", false},
		{"/root/sample/link/../a", "", false},
	}
	for _, c := range cases {
		rel, inside := insideFolder(c.source, "/root/sample")
		if rel != c.rel || inside != c.inside {
			t.Errorf("%s: %q %t, want %q %t", c.source, rel, inside, c.rel, c.inside)
		}
	}
}

func TestRunArgsReadAsDevcontainerBuilderReadsThem(t *testing.T) {
	cases := []struct {
		name          string
		args          []string
		cname, cnet   string
		networkExpect string
	}{
		{"spaced", []string{"--name", "n", "--network", "homelab-sample"}, "n", "homelab-sample", "homelab-sample"},
		{"equals", []string{"--name=n", "--net=homelab-sample"}, "n", "homelab-sample", "homelab-sample"},
		{"the last counts", []string{"--network", "one", "--name", "a", "--network=two", "--name=b"}, "b", "two", "two"},
		{"after a combined short flag", []string{"-it", "--network", "after"}, "", "after", "after"},
		{"another flag's value", []string{"--label", "--network=not-a-network", "-e", "--name=not-a-name"}, "", "", ""},
		{"host", []string{"--network=host"}, "", "host", ""},
		{"none", []string{"--network=none"}, "", "none", ""},
		{"bridge", []string{"--network=bridge"}, "", "bridge", ""},
		{"default", []string{"--network=default"}, "", "default", ""},
		{"another container", []string{"--network=container:other"}, "", "container:other", ""},
		{"stops at a refused positional", []string{"positional=value", "--network=after"}, "", "", ""},
		{"stops at a refused separator", []string{"--", "--network=after"}, "", "", ""},
		{"an empty argument", []string{"", "--network=after"}, "", "after", "after"},
		{"a flag without its value", []string{"--network"}, "", "", ""},
		{"substituted", []string{"--network=${localEnv:SAMPLE}"}, "", "from-env", "from-env"},
	}
	for _, flag := range []string{"--init", "--privileged", "--read-only", "-P", "--publish-all", "-d", "--detach", "-i", "--interactive", "-t", "--tty", "--rm", "--sig-proxy", "--oom-kill-disable"} {
		cases = append(cases, struct {
			name          string
			args          []string
			cname, cnet   string
			networkExpect string
		}{"after " + flag, []string{flag, "--network", "after"}, "", "after", "after"})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dc := &spec.DevContainer{Image: "sample", RunArgs: c.args}
			sub := SubstitutionContext(dc, "/root/sample", map[string]string{"SAMPLE": "from-env"})
			got := runArgValues(dc, sub)
			if got.name != c.cname || got.network != c.cnet {
				t.Fatalf("read %+v, want %q %q", got, c.cname, c.cnet)
			}
			if n := runNetwork(dc, sub); n != c.networkExpect {
				t.Fatalf("network %q, want %q", n, c.networkExpect)
			}
		})
	}
}

func TestCreateBindSources(t *testing.T) {
	outside := t.TempDir()
	folder, dc := definitionFixture(t, fmt.Sprintf(`{
		"image": "sample",
		"mounts": [
			"type=bind,source=${localWorkspaceFolder},target=/a",
			"type=bind,source=${localWorkspaceFolder}/repos,target=/b",
			"type=bind,source=${localWorkspaceFolder}/deep/er/folder,target=/c",
			"type=bind,source=${localWorkspaceFolder}/present/file,target=/d",
			"type=bind,source=${localWorkspaceFolder}/escape/created,target=/e",
			"type=bind,source=%[1]s/not-created,target=/f",
			"type=bind,source=${localWorkspaceFolder}/../not-created,target=/g",
			"type=volume,source=${localWorkspaceFolder}/volume-name,target=/h"
		]
	}`, outside))
	if err := os.MkdirAll(filepath.Join(folder, "present"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "present", "file"), []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(folder, "escape")); err != nil {
		t.Fatal(err)
	}
	p := &bufferProgress{}
	if err := createBindSources(context.Background(), dc, folder, map[string]string{}, p); err != nil {
		t.Fatal(err)
	}
	var rootOwner syscall.Stat_t
	if err := syscall.Stat(filepath.Dir(folder), &rootOwner); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"repos", "deep", "deep/er", "deep/er/folder"} {
		var st syscall.Stat_t
		if err := syscall.Stat(filepath.Join(folder, rel), &st); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if st.Mode&syscall.S_IFDIR == 0 || st.Uid != rootOwner.Uid || st.Gid != rootOwner.Gid {
			t.Errorf("%s: mode %o owner %d:%d, want a folder of %d:%d", rel, st.Mode, st.Uid, st.Gid, rootOwner.Uid, rootOwner.Gid)
		}
	}
	if data, err := os.ReadFile(filepath.Join(folder, "present", "file")); err != nil || string(data) != "kept" {
		t.Errorf("an existing file source changed: %q %v", data, err)
	}
	for _, absent := range []string{filepath.Join(outside, "created"), filepath.Join(outside, "not-created"), filepath.Join(filepath.Dir(folder), "not-created"), filepath.Join(folder, "volume-name")} {
		if _, err := os.Lstat(absent); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s was created: %v", absent, err)
		}
	}
	console := p.String()
	if !strings.Contains(console, "created bind source folder repos\n") || !strings.Contains(console, "bind source escape/created was not checked") {
		t.Errorf("console %q", console)
	}
	if strings.Contains(console, "bind source folder present") {
		t.Errorf("console names an existing source: %q", console)
	}
}

func TestABindSourceThatCannotBeCreatedFailsTheStart(t *testing.T) {
	folder, dc := definitionFixture(t, `{"image": "sample", "mounts": ["type=bind,source=${localWorkspaceFolder}/dangling,target=/a"]}`)
	if err := os.Symlink("absent", filepath.Join(folder, "dangling")); err != nil {
		t.Fatal(err)
	}
	err := createBindSources(context.Background(), dc, folder, map[string]string{}, &bufferProgress{})
	if err == nil || !strings.Contains(err.Error(), "create bind source folder dangling") {
		t.Fatalf("error %v", err)
	}
}

// fakeBindSources answers for the host's bind sources.
type fakeBindSources struct {
	present map[string]bool
	asked   []string
	err     error
}

func (f *fakeBindSources) BindSourceExists(_ context.Context, _, image, source string, _ io.Writer) (bool, error) {
	f.asked = append(f.asked, image+" "+source)
	return f.present[source], f.err
}

// networkEngine answers NetworkInspect: present names exist, and status,
// when set, is every other answer.
func networkEngine(t *testing.T, present map[string]bool, status int) *dockerclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		w.Header().Set("Content-Type", "application/json")
		switch {
		case status != 0:
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"message":"refused"}`))
		case present[name]:
			_, _ = fmt.Fprintf(w, `{"Name":%q}`, name)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"no such network"}`))
		}
	}))
	t.Cleanup(srv.Close)
	client, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("tcp://"+srv.Listener.Addr().String()), dockerclient.WithVersion("1.45"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func checkRuntime(t *testing.T, client *dockerclient.Client, sources *fakeBindSources) *Runtime {
	t.Helper()
	opts := Options{Environ: func() []string { return nil }, RunnerImage: func(context.Context) (string, error) { return "runner:sample", nil }}
	if sources != nil {
		opts.BindSources = sources
	}
	return New(client, opts)
}

func TestTheCheckReportsEverythingMissingInOneMessage(t *testing.T) {
	folder, _ := definitionFixture(t, `{
		"image": "sample",
		"runArgs": ["--name", "sample-box", "--network", "absent-net"],
		"mounts": [
			"type=bind,source=/absent/one,target=/a",
			"type=bind,source=/present/two,target=/b",
			"type=bind,source=/absent/three,target=/c",
			"type=bind,source=/run/consumers/sample-box,target=/d",
			"type=bind,source=${localWorkspaceFolder}/inside,target=/e"
		]
	}`)
	sources := &fakeBindSources{present: map[string]bool{"/present/two": true}}
	p := &bufferProgress{}
	err := checkRuntime(t, networkEngine(t, nil, 0), sources).CheckHostPrerequisites(context.Background(), folder, p)
	var missing *HostPrerequisitesError
	if !errors.As(err, &missing) {
		t.Fatalf("error %v", err)
	}
	// Without a declaration, the source named after the container is a
	// source like any other.
	if want := []string{"/absent/one", "/absent/three", "/run/consumers/sample-box"}; !reflect.DeepEqual(missing.BindSources, want) {
		t.Errorf("bind sources %q", missing.BindSources)
	}
	if !reflect.DeepEqual(missing.Networks, []string{"absent-net"}) {
		t.Errorf("networks %q", missing.Networks)
	}
	want := "the host is missing what this workspace needs: bind sources /absent/one, /absent/three, /run/consumers/sample-box; networks absent-net\n"
	if p.String() != want {
		t.Errorf("console %q", p.String())
	}
	if !reflect.DeepEqual(p.phases, []string{"host-prerequisites"}) {
		t.Errorf("phases %v", p.phases)
	}
	for _, asked := range sources.asked {
		if strings.Contains(asked, "inside") {
			t.Errorf("asked the Engine about a source inside the folder: %q", sources.asked)
		}
	}
	if _, err := os.Lstat(filepath.Join(folder, "inside")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the check changed the folder: %v", err)
	}
}

func TestADeclarationExcludesItsConsumerDirectoryOnly(t *testing.T) {
	folder, _ := definitionFixture(t, `{
		"image": "sample",
		"runArgs": ["--name", "sample-box"],
		"customizations": {"menagerie": {"wyrwood": {"accessGroup": 1000, "policies": ["sample"]}}},
		"mounts": [
			"type=bind,source=/run/consumers/sample-box,target=/a",
			"type=bind,source=/run/consumers/other-box,target=/b"
		]
	}`)
	sources := &fakeBindSources{}
	err := checkRuntime(t, networkEngine(t, nil, 0), sources).CheckHostPrerequisites(context.Background(), folder, &bufferProgress{})
	var missing *HostPrerequisitesError
	if !errors.As(err, &missing) || !reflect.DeepEqual(missing.BindSources, []string{"/run/consumers/other-box"}) {
		t.Fatalf("error %v", err)
	}
	if want := []string{"runner:sample /run/consumers/other-box"}; !reflect.DeepEqual(sources.asked, want) {
		t.Errorf("asked %q", sources.asked)
	}
}

func TestADefinitionWithAllItNeedsPasses(t *testing.T) {
	folder, _ := definitionFixture(t, `{
		"image": "sample",
		"runArgs": ["--network=present-net"],
		"mounts": ["type=bind,source=/present,target=/a"]
	}`)
	sources := &fakeBindSources{present: map[string]bool{"/present": true}}
	p := &bufferProgress{}
	if err := checkRuntime(t, networkEngine(t, map[string]bool{"present-net": true}, 0), sources).CheckHostPrerequisites(context.Background(), folder, p); err != nil {
		t.Fatal(err)
	}
	if p.Len() != 0 {
		t.Errorf("console %q", p.String())
	}
}

func TestADefinitionWithoutOutsideSourcesNeedsNoRunner(t *testing.T) {
	folder, _ := definitionFixture(t, `{"image": "sample", "mounts": ["type=bind,source=${localWorkspaceFolder}/inside,target=/a"]}`)
	if err := checkRuntime(t, networkEngine(t, nil, 0), nil).CheckHostPrerequisites(context.Background(), folder, &bufferProgress{}); err != nil {
		t.Fatal(err)
	}
}

func TestACheckThatCannotAskFails(t *testing.T) {
	t.Run("the Engine refuses the network read", func(t *testing.T) {
		folder, _ := definitionFixture(t, `{"image": "sample", "runArgs": ["--network=some-net"]}`)
		err := checkRuntime(t, networkEngine(t, nil, http.StatusInternalServerError), nil).CheckHostPrerequisites(context.Background(), folder, &bufferProgress{})
		var missing *HostPrerequisitesError
		if err == nil || errors.As(err, &missing) {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("the bind source check fails", func(t *testing.T) {
		folder, _ := definitionFixture(t, `{"image": "sample", "mounts": ["type=bind,source=/some,target=/a"]}`)
		broken := errors.New("broken")
		err := checkRuntime(t, networkEngine(t, nil, 0), &fakeBindSources{err: broken}).CheckHostPrerequisites(context.Background(), folder, &bufferProgress{})
		if !errors.Is(err, broken) {
			t.Fatalf("error %v", err)
		}
	})
	t.Run("no runner is configured", func(t *testing.T) {
		folder, _ := definitionFixture(t, `{"image": "sample", "mounts": ["type=bind,source=/some,target=/a"]}`)
		err := checkRuntime(t, networkEngine(t, nil, 0), nil).CheckHostPrerequisites(context.Background(), folder, &bufferProgress{})
		if err == nil || !strings.Contains(err.Error(), "no container runner") {
			t.Fatalf("error %v", err)
		}
	})
}

func TestAnUnreadableDefinitionIsLeftToTheStart(t *testing.T) {
	if err := checkRuntime(t, networkEngine(t, nil, 0), nil).CheckHostPrerequisites(context.Background(), t.TempDir(), &bufferProgress{}); err != nil {
		t.Fatal(err)
	}
}
