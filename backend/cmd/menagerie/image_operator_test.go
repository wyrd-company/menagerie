//go:build pocketid && image

package main

// TestImageOperatorWorkspace runs the built image with a workspace root
// owned by an operator, uid and gid 1000, and creates, uses and upgrades a
// workspace through it. Every file the services, the template hook and
// initializeCommand write belongs to the operator, and a devcontainer
// whose remote user starts as uid 1001 edits generated files as the
// operator, before and after an upgrade and rebuild. The workspace is
// backed up over SSH through an agent socket the operator's group may use,
// as a Wyrwood access group grants.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wyrd-company/menagerie/backend/internal/auth"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

const (
	operatorUID = 1000
	operatorGID = 1000
	// The devcontainer's user starts with another uid, so the remap to the
	// operator is visible.
	devcontainerUID  = 1001
	devcontainerBase = "debian:trixie-slim@sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad"
	// The Wyrwood access group: not the operator's own group, so the agent
	// is reachable only through the supplementary group start-up adds.
	wyrwoodGID = 2001
	// Test-only bound, imageReadyInterval apart, on a create or upgrade job
	// that builds the devcontainer image. Each took under 4 s on the shared
	// host with the Debian base already pulled; a first pull adds its time.
	jobAttempts = 2400
)

// operatorTemplate writes version 1 or 2 of the sample template with a
// devcontainer built from a Debian image whose remote user is uid 1001.
func operatorTemplate(t *testing.T, dir string, version int) {
	t.Helper()
	browserTemplate(t, dir, version)
	writeTo(t, dir, "template/.gitignore", "/markers/\n/notes/\n")
	writeTo(t, dir, "template/.devcontainer/Dockerfile", fmt.Sprintf("FROM %s\nRUN useradd --create-home --uid %d sample\n", devcontainerBase, devcontainerUID))
	writeTo(t, dir, "template/.devcontainer/devcontainer.json", `{
  "name": "sample",
  "build": { "dockerfile": "Dockerfile" },
  "remoteUser": "sample",
  "initializeCommand": "mkdir -p markers && echo initialized > markers/initialize.txt"
}
`)
}

// chownTree gives a test-owned fixture tree to the operator.
func chownTree(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, operatorUID, operatorGID)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// openTo makes each test-owned folder from dir up to stop traversable, so
// the operator reaches the fixtures inside them.
func openTo(t *testing.T, dir, stop string) {
	t.Helper()
	for ; strings.HasPrefix(dir, stop) && dir != stop; dir = filepath.Dir(dir) {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// notOperatorOwned lists the paths under root that the operator does not own.
func notOperatorOwned(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if st := info.Sys().(*syscall.Stat_t); st.Uid != operatorUID || st.Gid != operatorGID {
			out = append(out, fmt.Sprintf("%s (%d:%d)", strings.TrimPrefix(path, root), st.Uid, st.Gid))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type interviewReply struct {
	State    string          `json:"state"`
	Session  json.RawMessage `json:"session"`
	Document struct {
		Context struct {
			Template string `json:"template"`
		} `json:"context"`
		Schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"schema"`
	} `json:"document"`
}

// firstInterview reads the job's event stream until its first interview
// event and returns that reply.
func firstInterview(t *testing.T, c *http.Client, site, jobID string) interviewReply {
	t.Helper()
	resp, err := c.Get(site + "/api/jobs/" + jobID + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	s := bufio.NewScanner(resp.Body)
	s.Buffer(make([]byte, 1<<20), 64<<20)
	event := ""
	for s.Scan() {
		line := s.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "interview":
			var r interviewReply
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &r); err != nil {
				t.Fatal(err)
			}
			return r
		case strings.HasPrefix(line, "data: ") && event == "result":
			t.Fatalf("the job ended before an interview: %s", line)
		}
	}
	t.Fatalf("the event stream ended without an interview: %v", s.Err())
	return interviewReply{}
}

// runInterviewJob starts an interview operation, answers every question it
// asks from answers, waits for the job to end and returns its snapshot.
func runInterviewJob(t *testing.T, c *http.Client, site, operation, workspace, templateID string, answers map[string]any) jobs.Snapshot {
	t.Helper()
	header := http.Header{"Content-Type": {"application/json"}, "Sec-Fetch-Site": {"same-origin"}}
	s := fetch(t, c, http.MethodPost, site+"/api/jobs", strings.NewReader(fmt.Sprintf(`{"operation":%q,"workspace":%q,"templateId":%q}`, operation, workspace, templateID)), header)
	if s.status != http.StatusAccepted {
		t.Fatalf("%s answered %d %s", operation, s.status, s.body)
	}
	var snap jobs.Snapshot
	if err := json.Unmarshal([]byte(s.body), &snap); err != nil {
		t.Fatal(err)
	}
	r := firstInterview(t, c, site, snap.JobID)
	for batches := 0; r.State == "questions"; batches++ {
		if batches == 5 {
			t.Fatal("the interview does not complete")
		}
		given := map[string]any{}
		for id := range r.Document.Schema.Properties {
			v, ok := answers[id]
			if !ok {
				t.Fatalf("the interview asks %q, which the test does not answer", id)
			}
			given[id] = v
		}
		submission, _ := json.Marshal(map[string]any{"template": r.Document.Context.Template, "answers": given})
		a := fetch(t, c, http.MethodPost, site+"/api/jobs/"+snap.JobID+"/answers", strings.NewReader(`{"session":`+string(r.Session)+`,"submission":`+string(submission)+`}`), header)
		if a.status != http.StatusOK {
			t.Fatalf("answer answered %d %s", a.status, a.body)
		}
		r = interviewReply{}
		if err := json.Unmarshal([]byte(a.body), &r); err != nil {
			t.Fatalf("answer reply %s: %v", a.body, err)
		}
	}
	for i := 0; ; i++ {
		s := fetch(t, c, http.MethodGet, site+"/api/jobs/"+snap.JobID, nil, nil)
		if err := json.Unmarshal([]byte(s.body), &snap); err != nil {
			t.Fatalf("job answered %d %s", s.status, s.body)
		}
		if snap.FinishedAt != nil {
			return snap
		}
		if i == jobAttempts {
			t.Fatalf("the %s job did not end: %s", operation, s.body)
		}
		time.Sleep(imageReadyInterval)
	}
}

// devcontainerExec runs a shell script in the workspace's devcontainer as
// its remote user, in the workspace folder, and returns output and code.
func devcontainerExec(t *testing.T, c *imageContainer, folder, script string) (string, int) {
	t.Helper()
	ctx := context.Background()
	list, err := c.docker.ContainerList(ctx, container.ListOptions{Filters: filters.NewArgs(filters.Arg("label", "devcontainer.local_folder="+folder))})
	if err != nil || len(list) != 1 {
		t.Fatalf("devcontainers for %s: %d, %v", folder, len(list), err)
	}
	dest := ""
	for _, m := range list[0].Mounts {
		if m.Source == folder {
			dest = m.Destination
		}
	}
	if dest == "" {
		t.Fatalf("the devcontainer does not mount %s", folder)
	}
	devc := &imageContainer{t: t, docker: c.docker, id: list[0].ID}
	return devc.exec("sample", "sh", "-c", "cd "+dest+" && "+script)
}

func TestImageOperatorWorkspace(t *testing.T) {
	ref := requireEnv(t, "MENAGERIE_IMAGE")
	p := startPocketID(t)
	ctx := context.Background()
	tmp := os.TempDir()

	base := t.TempDir()
	openTo(t, base, tmp)
	rootDir := filepath.Join(base, "workspaces")
	repo := filepath.Join(base, "templates", "sample-kit")
	for _, d := range []string{rootDir, repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "init", "-q", "-b", "main")
	operatorTemplate(t, repo, 1)
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "v1")
	gitRun(t, repo, "tag", "v1")
	operatorTemplate(t, repo, 2)
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "v2")
	gitRun(t, repo, "tag", "v2")
	// The operator owns the workspace root and the templates, as on a host.
	chownTree(t, filepath.Join(base, "templates"))
	chownTree(t, rootDir)

	// The workspace repository and the agent: the agent's folder and socket
	// belong to an access group, as Wyrwood's do.
	host := backuptest.New(t)
	if _, err := git.PlainInit(filepath.Join(host.Dir, "backups.git"), true); err != nil {
		t.Fatal(err)
	}
	openTo(t, host.Dir, tmp)
	if err := os.Chmod(host.KnownHosts, 0o644); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Dir(host.AgentPath)
	for path, mode := range map[string]os.FileMode{agentDir: 0o750, host.AgentPath: 0o660} {
		if err := os.Chown(path, 0, wyrwoodGID); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}

	port := freePort(t)
	site := fmt.Sprintf("http://localhost:%d", port)
	group := p.admin(http.MethodPost, "/api/user-groups", map[string]any{"friendlyName": "Managers", "name": auth.ManagersGroup})
	p.user("sample-manager", "Sample", "Manager", group["id"].(string))
	client := p.admin(http.MethodPost, "/api/oidc/clients", map[string]any{"name": "Menagerie sample", "callbackURLs": []string{site + auth.CallbackPath}, "isPublic": true, "pkceEnabled": true})
	writeTo(t, base, "config.yml", fmt.Sprintf(`listen: 127.0.0.1:%d
workspace-root: %s
workspace-repository: %s
wyrwood-directory: %s
oidc:
  issuer: %s
  client-id: %s
  redirect-url: %s
templates:
  - {id: sample-kit-v1, name: "Sample kit, version 1", address: 'file://%s@v1'}
  - {id: sample-kit-v2, name: "Sample kit, version 2", address: 'file://%s@v2'}
`, port, rootDir, host.URL("backups.git"), agentDir, p.url, client["id"], site+auth.CallbackPath, repo, repo))

	created, err := p.docker.ContainerCreate(ctx,
		&container.Config{
			Image: ref,
			Env: []string{
				"SSH_AUTH_SOCK=" + host.AgentPath,
				"SSH_KNOWN_HOSTS=" + host.KnownHosts,
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
			},
		}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	c := &imageContainer{t: t, docker: p.docker, id: created.ID, site: site}
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), c.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	// The workspace's devcontainers, found by their label, go with it.
	t.Cleanup(func() {
		list, _ := p.docker.ContainerList(context.Background(), container.ListOptions{All: true,
			Filters: filters.NewArgs(filters.Arg("label", "devcontainer.local_folder="+filepath.Join(rootDir, "alpha")))})
		for _, l := range list {
			_ = p.docker.ContainerRemove(context.Background(), l.ID, container.RemoveOptions{Force: true, RemoveVolumes: true})
		}
	})
	if err := p.docker.ContainerStart(ctx, c.id, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	c.waitReady(0)
	if t.Failed() {
		return
	}
	folder := filepath.Join(rootDir, "alpha")
	b := signInBrowser()
	if s, _ := p.signIn(t, b, "sample-manager", site+"/"); s.status != http.StatusOK {
		t.Fatalf("sign-in answered %d", s.status)
	}

	t.Run("services run as the workspace root's owner", func(t *testing.T) {
		c.t = t
		for _, service := range []string{"template-service", "workspace-service"} {
			out := c.mustExec("sh", "-c", "stat -c '%u:%g' /proc/$(/command/s6-svstat -o pid /run/service/"+service+")")
			if out != fmt.Sprintf("%d:%d", operatorUID, operatorGID) {
				t.Errorf("%s runs as %s", service, out)
			}
		}
		if out := c.mustExec("stat", "-c", "%a %u", "/run/menagerie"); out != fmt.Sprintf("700 %d", operatorUID) {
			t.Errorf("socket folder %q", out)
		}
		if out := c.mustExec("getent", "passwd", fmt.Sprint(operatorUID)); !strings.Contains(out, ":/var/lib/menagerie/home:") {
			t.Errorf("passwd entry %q", out)
		}
		want := fmt.Sprintf("menagerie: services run as menagerie (uid %d, gid %d, groups %d,", operatorUID, operatorGID, operatorGID)
		logs := c.logs(time.Time{})
		if !strings.Contains(logs, want) || !strings.Contains(logs, fmt.Sprintf(",%d)", wyrwoodGID)) && !strings.Contains(logs, fmt.Sprintf(",%d,", wyrwoodGID)) {
			t.Errorf("logs lack %q with group %d:\n%s", want, wyrwoodGID, logs)
		}
		for _, service := range []string{"template-service", "workspace-service"} {
			out := c.mustExec("sh", "-c", "grep '^Groups:' /proc/$(/command/s6-svstat -o pid /run/service/"+service+")/status")
			if !strings.Contains(" "+out+" ", fmt.Sprintf(" %d ", wyrwoodGID)) {
				t.Errorf("%s groups %q lack the access group %d", service, out, wyrwoodGID)
			}
		}
	})

	t.Run("create writes operator-owned files a non-root devcontainer user edits", func(t *testing.T) {
		c.t = t
		snap := runInterviewJob(t, b, site, "create", "alpha", "sample-kit-v1",
			map[string]any{"label": "Sample label", "size": "small", "verbose": false, "tags": []string{"alpha"}})
		if snap.State != jobs.Succeeded {
			t.Fatalf("create ended %s: %+v\n%s", snap.State, snap.Error, c.logs(time.Time{}))
		}
		for _, rel := range []string{"result.txt", "hook-marker.txt", ".toha.json", "markers/initialize.txt", ".git/HEAD"} {
			if _, err := os.Stat(filepath.Join(folder, rel)); err != nil {
				t.Errorf("%s: %v", rel, err)
			}
		}
		if others := notOperatorOwned(t, folder); len(others) > 0 {
			t.Errorf("paths the operator does not own: %v", others)
		}
		out, code := devcontainerExec(t, c, folder, "id -u && sed -i 's/^first: template$/first: local/' local.txt && mkdir -p notes && echo note > notes/sample.txt")
		if code != 0 || !strings.HasPrefix(out, fmt.Sprint(operatorUID)) {
			t.Fatalf("the devcontainer user's edit exited %d: %q", code, out)
		}
		t.Logf("the devcontainer's remote user runs as uid %s", out)
		backups, err := git.PlainOpen(filepath.Join(host.Dir, "backups.git"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backups.Reference(plumbing.NewBranchReferenceName("alpha"), false); err != nil {
			t.Errorf("the backup branch: %v", err)
		}
	})

	t.Run("upgrade and rebuild keep the edit, the new file and the ownership", func(t *testing.T) {
		c.t = t
		snap := runInterviewJob(t, b, site, "upgrade", "alpha", "sample-kit-v2", map[string]any{"region": "north"})
		if snap.State != jobs.Succeeded {
			t.Fatalf("upgrade ended %s: %+v\n%s", snap.State, snap.Error, c.logs(time.Time{}))
		}
		local, _ := os.ReadFile(filepath.Join(folder, "local.txt"))
		if !strings.Contains(string(local), "first: local\n") || !strings.Contains(string(local), "fifth: v2\n") {
			t.Errorf("local.txt after upgrade: %q", local)
		}
		if _, err := os.Stat(filepath.Join(folder, "notes", "sample.txt")); err != nil {
			t.Errorf("the new file: %v", err)
		}
		if others := notOperatorOwned(t, folder); len(others) > 0 {
			t.Errorf("paths the operator does not own: %v", others)
		}
		if out, code := devcontainerExec(t, c, folder, "echo again >> result.txt && echo more > notes/more.txt"); code != 0 {
			t.Errorf("the rebuilt devcontainer's user edit exited %d: %q", code, out)
		}
		backups, _ := git.PlainOpen(filepath.Join(host.Dir, "backups.git"))
		ref, err := backups.Reference(plumbing.NewBranchReferenceName("alpha"), false)
		if err != nil {
			t.Fatal(err)
		}
		tip, _ := backups.CommitObject(ref.Hash())
		if tip == nil || tip.NumParents() != 1 {
			t.Errorf("the backup branch's upgrade commit: %v", tip)
		}
	})

	// Toha's data in /var/lib/menagerie belongs to the services' user. Data
	// another uid wrote stops start-up with a message, instead of failing
	// later inside Toha.
	t.Run("data another uid wrote stops start-up", func(t *testing.T) {
		data := filepath.Join(base, "data")
		writeTo(t, data, "toha/cache/stale", "written by root\n")
		other, err := p.docker.ContainerCreate(ctx,
			&container.Config{Image: ref, Labels: map[string]string{"menagerie-test": "image"}},
			&container.HostConfig{
				NetworkMode: "host",
				Binds: []string{
					"/var/run/docker.sock:/var/run/docker.sock",
					tmp + ":" + tmp,
					filepath.Join(base, "config.yml") + ":/etc/menagerie/config.yml:ro",
					data + ":/var/lib/menagerie",
				},
			}, nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		o := &imageContainer{t: t, docker: p.docker, id: other.ID}
		t.Cleanup(func() {
			_ = p.docker.ContainerRemove(context.Background(), o.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
		})
		if err := p.docker.ContainerStart(ctx, o.id, container.StartOptions{}); err != nil {
			t.Fatal(err)
		}
		for i := 0; o.inspect().State.Running; i++ {
			if i == imageReadyAttempts {
				t.Fatal("the container kept running")
			}
			time.Sleep(imageReadyInterval)
		}
		if code := o.inspect().State.ExitCode; code == 0 {
			t.Errorf("the container exited 0")
		}
		want := fmt.Sprintf("belongs to uid 0, but Menagerie runs as uid %d", operatorUID)
		if logs := o.logs(time.Time{}); !strings.Contains(logs, want) {
			t.Errorf("logs lack %q:\n%s", want, logs)
		}
		if _, err := os.Stat(filepath.Join(data, "toha", "cache", "stale")); err != nil {
			t.Errorf("start-up touched the data: %v", err)
		}
	})

	// A configuration the services' user cannot read stops start-up with a
	// message, before either service starts.
	t.Run("a configuration the owner cannot read stops start-up", func(t *testing.T) {
		private := filepath.Join(base, "private-config.yml")
		data, err := os.ReadFile(filepath.Join(base, "config.yml"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(private, data, 0o600); err != nil {
			t.Fatal(err)
		}
		other, err := p.docker.ContainerCreate(ctx,
			&container.Config{Image: ref, Labels: map[string]string{"menagerie-test": "image"}},
			&container.HostConfig{
				NetworkMode: "host",
				Binds: []string{
					"/var/run/docker.sock:/var/run/docker.sock",
					tmp + ":" + tmp,
					private + ":/etc/menagerie/config.yml:ro",
				},
			}, nil, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		o := &imageContainer{t: t, docker: p.docker, id: other.ID}
		t.Cleanup(func() {
			_ = p.docker.ContainerRemove(context.Background(), o.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
		})
		if err := p.docker.ContainerStart(ctx, o.id, container.StartOptions{}); err != nil {
			t.Fatal(err)
		}
		for i := 0; o.inspect().State.Running; i++ {
			if i == imageReadyAttempts {
				t.Fatal("the container kept running")
			}
			time.Sleep(imageReadyInterval)
		}
		want := fmt.Sprintf("/etc/menagerie/config.yml is not readable by uid %d", operatorUID)
		if logs := o.logs(time.Time{}); o.inspect().State.ExitCode == 0 || !strings.Contains(logs, want) {
			t.Errorf("exit %d, logs lack %q:\n%s", o.inspect().State.ExitCode, want, logs)
		}
	})
}
