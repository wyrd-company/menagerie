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
	devcontainerUID = 1001
	// The devcontainer's base comes from MCR, which has no anonymous pull
	// limit: devcontainer-builder reads its metadata from the registry on
	// every build. Its vscode user, uid 1000, is removed so that the remap
	// to the operator's uid has a free uid to take.
	devcontainerBase = "mcr.microsoft.com/devcontainers/base:trixie@sha256:6ee2b4097c75d33e9a94d941577a3286e5d629434fba384688b06daeeaaeac99"
	// The Wyrwood access group: not the operator's own group, so the agent
	// is reachable only through the supplementary group start-up adds.
	wyrwoodGID = 2001
	// Test-only bound, imageReadyInterval apart, on a create or upgrade job
	// that builds the devcontainer image. Each took under 4 s on the shared
	// host with the Debian base already pulled; a first pull adds its time.
	jobAttempts = 2400
)

// operatorTemplate writes version 1 or 2 of the sample template with a
// devcontainer built from a Debian-based image whose remote user is uid
// 1001.
func operatorTemplate(t *testing.T, dir string, version int) {
	t.Helper()
	browserTemplate(t, dir, version)
	writeTo(t, dir, "template/.gitignore", "/markers/\n/notes/\n")
	writeTo(t, dir, "template/.devcontainer/Dockerfile", fmt.Sprintf("FROM %s\nRUN userdel --remove vscode && useradd --create-home --uid %d sample\n", devcontainerBase, devcontainerUID))
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
		// Only the workspace service has the Docker socket's group.
		socketGID := c.mustExec("stat", "-L", "-c", "%g", "/var/run/docker.sock")
		if socketGID == fmt.Sprint(operatorGID) || socketGID == fmt.Sprint(wyrwoodGID) {
			t.Fatalf("the Docker socket's group %s is also the owner's or the access group", socketGID)
		}
		for _, service := range []string{"template-service", "workspace-service"} {
			out := c.mustExec("sh", "-c", "grep '^Groups:' /proc/$(/command/s6-svstat -o pid /run/service/"+service+")/status")
			if !strings.Contains(" "+out+" ", fmt.Sprintf(" %d ", wyrwoodGID)) {
				t.Errorf("%s groups %q lack the access group %d", service, out, wyrwoodGID)
			}
			if has := strings.Contains(" "+out+" ", " "+socketGID+" "); has != (service == "workspace-service") {
				t.Errorf("%s groups %q: the Docker socket's group %s present %v", service, out, socketGID, has)
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

	// Start-up checks /var/lib/menagerie before it changes anything: each of
	// Menagerie's folders is a real folder, not a link that would carry a
	// change into a workspace or another folder; the folders belong to the
	// services' user unless they are a new, empty volume; and everything in
	// Toha's data and the home was written by that user. A refusal stops
	// start-up with a message and leaves every folder as it was.
	elsewhere := filepath.Join(base, "elsewhere")
	foreign := filepath.Join(base, "foreign")
	for _, d := range []struct {
		path     string
		uid, gid int
		mode     os.FileMode
	}{{elsewhere, operatorUID, operatorGID, 0o750}, {foreign, 2000, 2000, 0o710}} {
		if err := os.MkdirAll(d.path, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTo(t, d.path, "stale", "written by root\n")
		if err := os.Chown(d.path, d.uid, d.gid); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func(path string) string {
		info, err := os.Lstat(path)
		if err != nil {
			return err.Error()
		}
		st := info.Sys().(*syscall.Stat_t)
		return fmt.Sprintf("%d:%d %o", st.Uid, st.Gid, info.Mode().Perm())
	}
	operatorDir := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chown(path, operatorUID, operatorGID); err != nil {
			t.Fatal(err)
		}
	}
	link := func(t *testing.T, target, path string) {
		t.Helper()
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	for i, c := range []struct {
		name  string
		setup func(t *testing.T, data string)
		want  string
	}{
		{"data another uid wrote stops start-up", func(t *testing.T, data string) {
			operatorDir(t, data)
			operatorDir(t, filepath.Join(data, "toha"))
			writeTo(t, data, "toha/cache/stale", "written by root\n")
		}, fmt.Sprintf("belongs to uid 0, but Menagerie runs as uid %d", operatorUID)},
		{"a data folder another uid owns stops start-up", func(t *testing.T, data string) {
			writeTo(t, data, "other", "not Menagerie's\n")
		}, fmt.Sprintf("/var/lib/menagerie belongs to uid 0 and is not empty, but Menagerie runs as uid %d", operatorUID)},
		{"an empty data folder another uid owns stops start-up", func(t *testing.T, data string) {
			if err := os.MkdirAll(data, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(data, 2000, 2000); err != nil {
				t.Fatal(err)
			}
		}, fmt.Sprintf("/var/lib/menagerie belongs to uid 2000, but Menagerie runs as uid %d", operatorUID)},
		{"a home linked to a workspace stops start-up", func(t *testing.T, data string) {
			operatorDir(t, data)
			link(t, elsewhere, filepath.Join(data, "home"))
		}, "/var/lib/menagerie/home is a symbolic link"},
		{"a Toha folder linked to another uid's folder stops start-up", func(t *testing.T, data string) {
			operatorDir(t, data)
			link(t, foreign, filepath.Join(data, "toha"))
		}, "/var/lib/menagerie/toha is a symbolic link"},
		{"a Toha file instead of a folder stops start-up", func(t *testing.T, data string) {
			operatorDir(t, data)
			writeTo(t, data, "toha", "a file\n")
		}, "/var/lib/menagerie/toha is not a folder"},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := filepath.Join(base, fmt.Sprintf("data-%d", i))
			c.setup(t, data)
			before := map[string]string{}
			for _, p := range []string{data, filepath.Join(data, "toha"), filepath.Join(data, "home"), elsewhere, foreign, filepath.Join(foreign, "stale")} {
				before[p] = stamp(p)
			}
			o := startWithData(t, p, ref, tmp, filepath.Join(base, "config.yml"), data)
			if code := o.waitExit(); code == 0 {
				t.Errorf("start-up exited 0")
			}
			if logs := o.logs(time.Time{}); !strings.Contains(logs, c.want) {
				t.Errorf("logs lack %q:\n%s", c.want, logs)
			}
			for path, was := range before {
				if now := stamp(path); now != was {
					t.Errorf("start-up changed %s from %s to %s", path, was, now)
				}
			}
		})
	}

	t.Run("an empty new data volume is given to the owner", func(t *testing.T) {
		data := filepath.Join(base, "data-new")
		if err := os.MkdirAll(data, 0o755); err != nil {
			t.Fatal(err)
		}
		// It listens on the main container's port, so that one stops first.
		if err := p.docker.ContainerStop(ctx, c.id, container.StopOptions{}); err != nil {
			t.Fatal(err)
		}
		o := startWithData(t, p, ref, tmp, filepath.Join(base, "config.yml"), data)
		for i := 0; ; i++ {
			if out, code := o.exec("", "env", fmt.Sprintf("MENAGERIE_HEALTH_URL=http://127.0.0.1:%d/healthz", port), "menagerie-healthcheck"); code == 0 {
				break
			} else if i == imageReadyAttempts || !o.inspect().State.Running {
				t.Fatalf("not healthy: %q\n%s", out, o.logs(time.Time{}))
			}
			time.Sleep(imageReadyInterval)
		}
		for _, sub := range []string{"", "toha", "home"} {
			if got := stamp(filepath.Join(data, sub)); !strings.HasPrefix(got, fmt.Sprintf("%d:%d ", operatorUID, operatorGID)) {
				t.Errorf("%s/%s is %s", data, sub, got)
			}
		}
		if err := p.docker.ContainerStop(ctx, o.id, container.StopOptions{}); err != nil {
			t.Fatal(err)
		}
	})

	// The configured workspace root and Wyrwood directory may be symbolic
	// links, like any mounted path: the identity is their targets'. Both
	// links belong to root, the root's target to the operator and the
	// Wyrwood directory's target to its access group.
	t.Run("linked workspace root and Wyrwood directory give their targets' identity", func(t *testing.T) {
		rootLink := filepath.Join(base, "workspaces-link")
		wyrwoodLink := filepath.Join(base, "wyrwood-link")
		link(t, rootDir, rootLink)
		link(t, agentDir, wyrwoodLink)
		config, err := os.ReadFile(filepath.Join(base, "config.yml"))
		if err != nil {
			t.Fatal(err)
		}
		linked := strings.Replace(string(config), "workspace-root: "+rootDir+"\n", "workspace-root: "+rootLink+"\n", 1)
		linked = strings.Replace(linked, "wyrwood-directory: "+agentDir+"\n", "wyrwood-directory: "+wyrwoodLink+"\n", 1)
		if linked == string(config) || !strings.Contains(linked, rootLink) || !strings.Contains(linked, wyrwoodLink) {
			t.Fatal("the linked configuration names no link")
		}
		writeTo(t, base, "linked-config.yml", linked)
		o := startWithData(t, p, ref, tmp, filepath.Join(base, "linked-config.yml"), "")
		want := fmt.Sprintf("menagerie: services run as menagerie (uid %d, gid %d, groups %d,", operatorUID, operatorGID, operatorGID)
		for i := 0; ; i++ {
			logs := o.logs(time.Time{})
			if strings.Contains(logs, "menagerie: services run as") {
				if !strings.Contains(logs, want) || !strings.Contains(logs, fmt.Sprintf(",%d)", wyrwoodGID)) && !strings.Contains(logs, fmt.Sprintf(",%d,", wyrwoodGID)) {
					t.Errorf("logs lack %q with group %d:\n%s", want, wyrwoodGID, logs)
				}
				break
			}
			if i == imageReadyAttempts || !o.inspect().State.Running {
				t.Fatalf("start-up named no identity:\n%s", logs)
			}
			time.Sleep(imageReadyInterval)
		}
		for i := 0; ; i++ {
			out, code := o.exec("", "env", fmt.Sprintf("MENAGERIE_HEALTH_URL=http://127.0.0.1:%d/healthz", port), "menagerie-healthcheck")
			if code == 0 {
				break
			}
			if i == imageReadyAttempts || !o.inspect().State.Running {
				t.Fatalf("not healthy: %q\n%s", out, o.logs(time.Time{}))
			}
			time.Sleep(imageReadyInterval)
		}
		for _, service := range []string{"template-service", "workspace-service"} {
			out := o.mustExec("sh", "-c", "stat -c '%u:%g' /proc/$(/command/s6-svstat -o pid /run/service/"+service+")")
			if out != fmt.Sprintf("%d:%d", operatorUID, operatorGID) {
				t.Errorf("%s runs as %s", service, out)
			}
		}
		if err := p.docker.ContainerStop(ctx, o.id, container.StopOptions{}); err != nil {
			t.Fatal(err)
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
		o := startWithData(t, p, ref, tmp, private, "")
		o.waitExit()
		want := fmt.Sprintf("/etc/menagerie/config.yml is not readable by uid %d", operatorUID)
		if logs := o.logs(time.Time{}); o.inspect().State.ExitCode == 0 || !strings.Contains(logs, want) {
			t.Errorf("exit %d, logs lack %q:\n%s", o.inspect().State.ExitCode, want, logs)
		}
	})
}

// startWithData starts the image once, with no restart policy, with the
// given configuration and, unless data is empty, data mounted at
// /var/lib/menagerie. It listens on the main container's port, so a caller
// that lets it start its services stops the main container first.
func startWithData(t *testing.T, p *pocketID, ref, tmp, config, data string) *imageContainer {
	t.Helper()
	ctx := context.Background()
	binds := []string{
		"/var/run/docker.sock:/var/run/docker.sock",
		tmp + ":" + tmp,
		config + ":/etc/menagerie/config.yml:ro",
	}
	if data != "" {
		binds = append(binds, data+":/var/lib/menagerie")
	}
	created, err := p.docker.ContainerCreate(ctx,
		&container.Config{Image: ref, Labels: map[string]string{"menagerie-test": "image"}},
		&container.HostConfig{NetworkMode: "host", Binds: binds}, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	o := &imageContainer{t: t, docker: p.docker, id: created.ID}
	t.Cleanup(func() {
		_ = p.docker.ContainerRemove(context.Background(), o.id, container.RemoveOptions{Force: true, RemoveVolumes: true})
	})
	if err := p.docker.ContainerStart(ctx, o.id, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	return o
}

// waitExit waits for the container to stop and returns its exit code.
func (c *imageContainer) waitExit() int {
	c.t.Helper()
	for i := 0; c.inspect().State.Running; i++ {
		if i == imageReadyAttempts {
			c.t.Fatal("the container kept running")
		}
		time.Sleep(imageReadyInterval)
	}
	return c.inspect().State.ExitCode
}
