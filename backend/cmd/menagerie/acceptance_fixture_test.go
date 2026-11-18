//go:build pocketid && browser && image && acceptance

package main

// TestAcceptanceFixture holds the built image, MENAGERIE_IMAGE, open for the
// web UI's acceptance run (web/test/integrated/acceptance.sh). It is the
// integrated run's image topology: the same control server and browser
// story, against Menagerie as an operator deploys it.
//
//   - the image on the Docker Engine named by DOCKER_HOST, which mounts its
//     own socket, the workspace root at the same path, owned by an operator
//     (uid 1000), and its Wyrwood consumer directory
//   - a real Wyrwood agent binding the container (wyrwood_test.go): Toha
//     fetches the templates over SSH and Menagerie pushes the backups over
//     SSH, both signed by Wyrwood's Transit key
//   - a disposable Pocket ID with a manager and a non-manager
//   - the sample template tagged 1.0.0, which the story registers in the
//     interface and whose next version the control server releases
//   - a configuration whose templates key, as an earlier release's, the
//     first start imports: a kept kit at 1.0.0 whose repository also has
//     2.0.0, a template whose hook waits for a release file, so a job can be
//     held at a real boundary, an entry pinned to a branch and one whose
//     address holds a password, which the import skips by id
//   - with MENAGERIE_XOLOTL_IMAGE, the host preparation for provisioning and
//     a template that declares it (acceptance_provisioning_test.go)
//
// The test runs in scripts/isolated-docker.sh's runner, beside the Engine;
// the Engine publishes the site, Pocket ID and the control server on the
// host's loopback, so a browser on the host uses the same URLs. It ends
// when the control server receives POST /shutdown.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	git "github.com/go-git/go-git/v5"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
)

// acceptanceName is the Menagerie container Wyrwood binds by name.
const acceptanceName = "sample-acceptance"

func envPort(t *testing.T, name string) int {
	t.Helper()
	port, err := strconv.Atoi(requireEnv(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return port
}

// heldTemplate turns version 1 of the sample template into one whose hook
// waits until heldRelease exists in the workspace folder.
func heldTemplate(t *testing.T, dir string) {
	t.Helper()
	browserTemplate(t, dir, 1)
	path := filepath.Join(dir, "template.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	held := strings.Replace(string(body), "'echo ran > hook-marker.txt'",
		fmt.Sprintf("'echo ran > hook-marker.txt; while [ ! -e %[1]s ]; do sleep 0.2; done; rm %[1]s'", heldRelease), 1)
	if held == string(body) {
		t.Fatal("the sample template has no hook to hold")
	}
	writeTo(t, dir, "template.yml", held)
}

func TestAcceptanceFixture(t *testing.T) {
	ref := requireEnv(t, "MENAGERIE_IMAGE")
	info := requireEnv(t, "MENAGERIE_BROWSER_INFO")
	port := envPort(t, "MENAGERIE_BROWSER_PORT")
	controlPort := envPort(t, "MENAGERIE_BROWSER_CONTROL_PORT")
	pocketPort := envPort(t, "MENAGERIE_POCKETID_PORT")
	ctx := context.Background()
	tmp := os.TempDir()

	// Published from the Engine's host, so on every address there.
	p := startPocketIDAt(t, "0.0.0.0", pocketPort)
	w := startWyrwood(t, p)
	if _, err := w.bao.Logical().Write("transit/keys/sample-git", map[string]any{"type": "ed25519"}); err != nil {
		t.Fatal(err)
	}

	base := t.TempDir()
	openTo(t, base, tmp)
	provisioned := prepareProvisionedHost(t, w, p, base)

	// The templates and the workspace repository on an SSH Git host that
	// admits Wyrwood's Transit key.
	host := backuptest.New(t)
	host.Authorize(transitKey(t, w, "sample-git"))
	if err := os.Chmod(host.KnownHosts, 0o644); err != nil {
		t.Fatal(err)
	}
	// Each template is a repository of its own on the SSH host, published
	// from a working copy that the control server can release from.
	publish := func(name string, versions ...func(dir string)) string {
		work := filepath.Join(base, "template-work", name)
		if err := os.MkdirAll(work, 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(t, work, "init", "-q", "-b", "main")
		for i, write := range versions {
			write(work)
			gitRun(t, work, "add", "-A")
			gitRun(t, work, "commit", "-q", "-m", fmt.Sprintf("v%d", i+1))
			gitRun(t, work, "tag", fmt.Sprintf("%d.0.0", i+1))
		}
		gitRun(t, host.Dir, "clone", "-q", "--bare", work, filepath.Join(host.Dir, name+".git"))
		return work
	}
	version := func(v int) func(string) { return func(dir string) { browserTemplate(t, dir, v) } }
	sampleWork := publish("sample-kit", version(1))
	publish("kept-kit", version(1), version(2))
	publish("held-kit", func(dir string) { heldTemplate(t, dir) })
	if provisioned != nil {
		publish("workspace-kit", func(dir string) { browserTemplate(t, dir, 1); provisionedTemplate(t, dir, 1) },
			func(dir string) { browserTemplate(t, dir, 2); provisionedTemplate(t, dir, 2) })
	}
	newVersion := func(name, tag string) error {
		if name != "sample-kit" {
			return fmt.Errorf("only the sample kit has a next version")
		}
		browserTemplate(t, sampleWork, 2)
		gitRun(t, sampleWork, "add", "-A")
		gitRun(t, sampleWork, "commit", "-q", "-m", "v2")
		gitRun(t, sampleWork, "tag", tag)
		gitRun(t, sampleWork, "push", "-q", filepath.Join(host.Dir, "sample-kit.git"), "main", tag)
		return nil
	}
	bare := filepath.Join(host.Dir, "backups.git")
	if _, err := git.PlainInit(bare, true); err != nil {
		t.Fatal(err)
	}

	rootDir := filepath.Join(base, "workspaces")
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		t.Fatal(err)
	}
	chownTree(t, rootDir)

	site := fmt.Sprintf("http://localhost:%d", port)
	group := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Managers", "name": auth.ManagersGroup})
	other := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Other", "name": "other"})
	p.user("sample-manager", "Sample", "Manager", group["id"].(string))
	p.user("sample-other", "Sample", "Other", other["id"].(string))
	client := p.admin(http.MethodPost, "/api/oidc/clients", map[string]any{"name": "Menagerie sample", "callbackURLs": []string{site + auth.CallbackPath}, "isPublic": true, "pkceEnabled": true})
	provisionedTemplates, provisioning, menagerieKeys, menageriePolicy := "", "", []string{"sample-git"}, ""
	if provisioned != nil {
		provisionedTemplates = fmt.Sprintf("  - {id: workspace-kit, name: \"Workspace kit\", address: '%s@1.0.0'}\n", host.URL("workspace-kit.git"))
		provisioning = provisioned.config
		menagerieKeys = append(menagerieKeys, provisioned.transitKeys...)
		menageriePolicy = provisioned.policy
	}
	writeTo(t, base, "config.yml", fmt.Sprintf(`listen: 0.0.0.0:%d
workspace-root: %s
workspace-repository: %s
wyrwood-directory: /run/wyrwood
oidc:
  issuer: %s
  client-id: %s
  redirect-url: %s
templates:
  - {id: kept-kit, name: "Kept kit", address: '%s@1.0.0'}
  - {id: sample-kit-held, name: "Sample kit, held hook", address: '%s@1.0.0'}
  - {id: legacy-branch, name: "Legacy branch", address: '%s@main'}
  - {id: legacy-password, name: "Legacy password", address: 'https://sample-user:sample-password@git.example.invalid/example-org/kit@1.0.0'}
%s%s`, port, rootDir, host.URL("backups.git"), p.url, client["id"], site+auth.CallbackPath, host.URL("kept-kit.git"), host.URL("held-kit.git"), host.URL("legacy-kit.git"), provisionedTemplates, provisioning))

	created, err := p.docker.ContainerCreate(ctx,
		&container.Config{
			Image: ref,
			Env: []string{
				"SSH_AUTH_SOCK=/run/wyrwood/ssh-agent.sock",
				fmt.Sprintf("MENAGERIE_HEALTH_URL=http://127.0.0.1:%d/healthz", port),
			},
			Labels: map[string]string{"menagerie-test": "image"},
		},
		&container.HostConfig{
			NetworkMode:   "host",
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyUnlessStopped},
			Binds: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				tmp + ":" + tmp,
				filepath.Join(base, "config.yml") + ":/etc/menagerie/config.yml:ro",
				host.KnownHosts + ":/etc/ssh/ssh_known_hosts:ro",
				wyrwoodConsumers + "/" + acceptanceName + ":/run/wyrwood",
			},
		}, nil, nil, acceptanceName)
	if err != nil {
		t.Fatal(err)
	}
	c := &imageContainer{t: t, docker: p.docker, id: created.ID, site: site}
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), c.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	// Every devcontainer of a folder under the root goes with the run.
	t.Cleanup(func() {
		entries, _ := os.ReadDir(rootDir)
		for _, e := range entries {
			removeDevcontainers(p, filepath.Join(rootDir, e.Name()))
		}
	})
	// The control server's outsider container goes too.
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), "sample-outsider", container.RemoveOptions{Force: true})
	})

	w.savePolicy("sample-menagerie", `path "transit/keys/sample-git" { capabilities = ["read"] }
path "transit/sign/sample-git" { capabilities = ["update"] }
`+menageriePolicy, true, menagerieKeys, "/run/wyrwood")
	w.bind(acceptanceName, []string{"sample-menagerie"}, "1h", "8h", wyrwoodGID)
	waitFor(t, "a token for Menagerie", func() bool { return w.token(acceptanceName) == "valid" })
	if err := p.docker.ContainerStart(ctx, c.id, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	c.waitReady(0)
	if t.Failed() {
		return
	}

	healthy := func() bool {
		resp, err := http.Get(site + "/healthz")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}
	restart := func() error {
		if err := p.docker.ContainerRestart(context.Background(), c.id, container.StopOptions{}); err != nil {
			return err
		}
		for i := 0; !healthy(); i++ {
			if i == imageReadyAttempts {
				return fmt.Errorf("Menagerie did not answer after the restart")
			}
			time.Sleep(imageReadyInterval)
		}
		return nil
	}

	done := make(chan struct{})
	control := controlServer(t, controlFixture{p: p, rootDir: rootDir, bare: bare, docker: p.docker, done: done,
		restart: restart, release: true, newVersion: newVersion})
	// Read-only evidence from the controller's actual web page, independent of
	// Menagerie's appliedRevision, for the policy acceptance test.
	controls := http.NewServeMux()
	controls.Handle("/", control)
	controls.HandleFunc("GET /binding", func(writer http.ResponseWriter, r *http.Request) {
		var page struct {
			Rows []struct {
				KeyValue string
				Policies []string
				ID       string
			}
		}
		pageData(t, fetch(t, w.operator, http.MethodGet, w.site+"/bindings", nil, nil), &page)
		for _, row := range page.Rows {
			if row.KeyValue == r.URL.Query().Get("name") {
				writeJSON(writer, row)
				return
			}
		}
		http.Error(writer, "binding absent", 404)
	})
	csrv := &http.Server{Addr: fmt.Sprintf("0.0.0.0:%d", controlPort), Handler: controls}
	go func() { _ = csrv.ListenAndServe() }()
	t.Cleanup(func() { _ = csrv.Shutdown(context.Background()) })

	data, _ := json.Marshal(map[string]string{
		"mode":     "image",
		"site":     site,
		"pocket":   p.url,
		"control":  fmt.Sprintf("http://127.0.0.1:%d", controlPort),
		"root":     rootDir,
		"template": host.URL("sample-kit.git"),
	})
	if err := os.WriteFile(info, data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("fixture ready: %s", data)
	// The first start imported the templates key once, skipping the entries
	// it cannot take by their ids alone, and warns that the key is no
	// longer read.
	started := c.logs(time.Time{})
	for _, want := range []string{"legacy-branch", "legacy-password", "imported template kept-kit", "templates key is no longer read"} {
		if !strings.Contains(started, want) {
			t.Errorf("the start's log does not name %q:\n%s", want, started)
		}
	}
	if strings.Contains(started, "sample-password") {
		t.Errorf("the start's log quotes an address with a password:\n%s", started)
	}
	<-done
	t.Logf("Menagerie logs:\n%s", c.logs(time.Time{}))
}
