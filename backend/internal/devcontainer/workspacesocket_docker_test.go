//go:build docker

// The workspace socket's done-when tests: a devcontainer commits and backs
// up its own workspace through workspace-commit and its mounted socket, and
// the schedule commits a workspace left dirty. They need the client built
// for the daemon's platform, named by MENAGERIE_TEST_WORKSPACE_COMMIT
// (task test:docker builds it).
package devcontainer_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/pkg/stdcopy"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/commits"
	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
	"github.com/wyrd-company/menagerie/backend/internal/workspacesocket"
)

// socketProbe is an initializeCommand that fails when its throwaway
// container can reach the workspace socket or is told where it is.
const socketProbe = `found=$(for p in /run/menagerie "$SOCKETS"; do [ -e "$p" ] && echo "reachable $p"; done
env | grep MENAGERIE_WORKSPACE_SOCKET)
[ -z "$found" ] || { echo "$found" > init-found; exit 1; }
echo clean > init-clean`

type socketFixture struct {
	h       *harness
	root    *workspace.Root
	remote  string
	jobs    *jobs.Manager
	commits *commits.Commits
	sockets *workspacesocket.Sockets
	folder  string
	client  string
	rt      *devcontainer.Runtime
}

func newSocketFixture(t *testing.T) *socketFixture {
	t.Helper()
	client := os.Getenv("MENAGERIE_TEST_WORKSPACE_COMMIT")
	if client == "" {
		t.Fatal("MENAGERIE_TEST_WORKSPACE_COMMIT must name the workspace-commit client built for the daemon")
	}
	h := newHarness(t)
	root, err := workspace.NewRoot(h.root)
	if err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(t.TempDir(), "workspaces.git")
	if _, err := git.PlainInit(remote, true); err != nil {
		t.Fatal(err)
	}
	// A Unix socket address has about 100 bytes: the folder is short.
	folder, err := os.MkdirTemp("", "ws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(folder) })
	f := &socketFixture{h: h, root: root, remote: remote, jobs: jobs.NewManager(context.Background(), jobs.Options{}), folder: folder, client: client}
	f.commits = commits.New(commits.Options{Root: root, Remote: &backup.Remote{URL: remote}, Jobs: f.jobs})
	f.open(t)
	f.rt = devcontainer.New(h.client, h.options(devcontainer.Options{
		Environ:  func() []string { return []string{"SOCKETS=" + folder} },
		LocalEnv: []string{"SOCKETS"},
		WorkspaceSocket: func(folder string) (string, error) {
			return f.sockets.Ensure(filepath.Base(folder))
		},
	}))
	t.Cleanup(f.jobs.Wait)
	return f
}

// open opens the socket folder, as Menagerie does at start-up.
func (f *socketFixture) open(t *testing.T) {
	t.Helper()
	sockets, err := workspacesocket.Open(workspacesocket.Options{Folder: f.folder, Client: f.client, Commits: f.commits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sockets.Close)
	f.sockets = sockets
}

// workspace creates a workspace with a committed definition.
func (f *socketFixture) workspace(t *testing.T, name string, files map[string]string) string {
	t.Helper()
	folder, _ := f.root.FolderPath(name)
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := backup.Init(folder, name); err != nil {
		t.Fatal(err)
	}
	files[".toha.json"] = "{}\n"
	f.h.write(folder, files)
	if _, err := backup.CommitRoot(folder, name, backup.Output{Record: ".toha.json"}, "root", io.Discard); err != nil {
		t.Fatal(err)
	}
	return folder
}

// run runs cmd in container id and returns its output and exit code.
func (f *socketFixture) run(t *testing.T, id string, cmd ...string) (string, int) {
	t.Helper()
	ctx := f.h.ctx
	created, err := f.h.client.ContainerExecCreate(ctx, id, container.ExecOptions{Cmd: cmd, AttachStdout: true, AttachStderr: true})
	if err != nil {
		t.Fatal(err)
	}
	att, err := f.h.client.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, err = stdcopy.StdCopy(&out, &out, att.Reader)
	att.Close()
	if err != nil {
		t.Fatal(err)
	}
	info, err := f.h.client.ContainerExecInspect(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out.String()), info.ExitCode
}

// backedUp returns path's content on the workspace's backup branch.
func (f *socketFixture) backedUp(t *testing.T, name, path string) string {
	t.Helper()
	repo, err := git.PlainOpen(f.remote)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(name), false)
	if err != nil {
		t.Fatalf("backup branch %s: %v", name, err)
	}
	c, err := repo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	file, err := c.File(path)
	if err != nil {
		t.Fatalf("%s is not on the backup branch: %v", path, err)
	}
	body, err := file.Contents()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// socketMount returns the container's mount at the socket target.
func socketMount(t *testing.T, info container.InspectResponse) container.MountPoint {
	t.Helper()
	for _, m := range info.Mounts {
		if m.Destination == devcontainer.WorkspaceSocketTarget {
			return m
		}
	}
	t.Fatalf("no mount at %s: %+v", devcontainer.WorkspaceSocketTarget, info.Mounts)
	return container.MountPoint{}
}

func TestADevcontainerCommitsAndBacksUpThroughItsSocket(t *testing.T) {
	f := newSocketFixture(t)
	folder := f.workspace(t, "socket-space", map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + baseImage + `", "initializeCommand": ["sh", "-c", ` + quoteJSON(socketProbe) + `]}`,
	})
	f.h.mustRun(f.rt.Start, folder)
	if _, err := os.Stat(filepath.Join(folder, "init-clean")); err != nil {
		found, _ := os.ReadFile(filepath.Join(folder, "init-found"))
		t.Fatalf("initializeCommand reached the socket: %s", found)
	}
	id := f.h.status(folder).ContainerID
	info := f.h.inspect(id)
	m := socketMount(t, info)
	if m.Type != mount.TypeBind || m.RW || m.Source != filepath.Join(f.folder, "socket-space") {
		t.Fatalf("socket mount %+v", m)
	}
	if got := envValue(info, devcontainer.WorkspaceSocketEnv); got != devcontainer.WorkspaceSocketPath {
		t.Fatalf("%s=%q", devcontainer.WorkspaceSocketEnv, got)
	}
	if out, code := f.run(t, id, "sh", "-c", "touch /run/menagerie/written"); code == 0 {
		t.Fatalf("the socket folder is writable from the container: %s", out)
	}

	out, code := f.run(t, id, "sh", "-c", "echo from the container > /workspaces/socket-space/added.txt && /run/menagerie/bin/workspace-commit -m 'feat: add a file from the container'")
	if code != 0 || !strings.Contains(out, "committed ") || !strings.Contains(out, "backed up: pushed") {
		t.Fatalf("workspace-commit exit %d: %s", code, out)
	}
	if got := f.backedUp(t, "socket-space", "added.txt"); got != "from the container\n" {
		t.Fatalf("backed up %q", got)
	}

	// A socket re-created by a restarted Menagerie is reached through the
	// same folder mount.
	f.sockets.Close()
	f.open(t)
	if _, err := f.sockets.Ensure("socket-space"); err != nil {
		t.Fatal(err)
	}
	out, code = f.run(t, id, "sh", "-c", "echo again > /workspaces/socket-space/again.txt && /run/menagerie/bin/workspace-commit 'chore: after a restart'")
	if code != 0 || f.backedUp(t, "socket-space", "again.txt") != "again\n" {
		t.Fatalf("after a restart: exit %d: %s", code, out)
	}

	// A rebuilt container gets the mount again.
	f.h.mustRun(f.rt.Rebuild, folder)
	rebuilt := f.h.status(folder).ContainerID
	if rebuilt == id {
		t.Fatal("rebuild kept the container")
	}
	if m := socketMount(t, f.h.inspect(rebuilt)); m.RW {
		t.Fatalf("rebuilt mount %+v", m)
	}
	out, code = f.run(t, rebuilt, "/run/menagerie/bin/workspace-commit", "nothing new")
	if code != 0 || !strings.Contains(out, "nothing to commit") {
		t.Fatalf("after a rebuild: exit %d: %s", code, out)
	}
}

func TestTheScheduleCommitsAWorkspaceLeftDirty(t *testing.T) {
	f := newSocketFixture(t)
	folder := f.workspace(t, "dirty-space", map[string]string{
		".devcontainer/devcontainer.json": `{"image": "` + baseImage + `"}`,
	})
	f.workspace(t, "clean-space", map[string]string{".devcontainer/devcontainer.json": `{"image": "` + baseImage + `"}`})
	f.h.mustRun(f.rt.Start, folder)
	id := f.h.status(folder).ContainerID
	if out, code := f.run(t, id, "sh", "-c", "echo left behind > /workspaces/dirty-space/left.txt"); code != 0 {
		t.Fatalf("write: %s", out)
	}

	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.commits.Schedule(ctx, ticks, t.Logf)
		close(done)
	}()
	ticks <- time.Now()
	cancel()
	<-done
	list := f.jobs.List()
	if len(list) != 1 || list[0].Workspace != "dirty-space" || list[0].Operation != "commit" {
		t.Fatalf("jobs %+v", list)
	}
	f.jobs.Wait()
	job, _ := f.jobs.Get(list[0].JobID)
	if s := job.Snapshot(); s.State != jobs.Succeeded {
		t.Fatalf("job %+v", s)
	}
	if got := f.backedUp(t, "dirty-space", "left.txt"); got != "left behind\n" {
		t.Fatalf("backed up %q", got)
	}
	repo, _ := git.PlainOpen(f.remote)
	if _, err := repo.Reference(plumbing.NewBranchReferenceName("clean-space"), false); err == nil {
		t.Fatal("the clean workspace was backed up")
	}
}

func TestAComposePrimaryGetsTheSocket(t *testing.T) {
	f := newSocketFixture(t)
	folder := f.workspace(t, "compose-socket", map[string]string{
		".devcontainer/compose.yml": composeFile,
		// The definition's own name for the socket wins.
		".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/data", "containerEnv": {"MENAGERIE_WORKSPACE_SOCKET": "/run/menagerie/./workspace.sock"}}`,
	})
	f.h.mustRun(f.rt.Start, folder)
	id := f.h.status(folder).ContainerID
	info := f.h.inspect(id)
	if m := socketMount(t, info); m.RW || m.Source != filepath.Join(f.folder, "compose-socket") {
		t.Fatalf("socket mount %+v", m)
	}
	if got := envValue(info, devcontainer.WorkspaceSocketEnv); got != "/run/menagerie/./workspace.sock" {
		t.Fatalf("%s=%q", devcontainer.WorkspaceSocketEnv, got)
	}
	project := info.Config.Labels["com.docker.compose.project"]
	sidecar := f.h.inspect(f.h.service(project, "sidecar"))
	for _, m := range sidecar.Mounts {
		if m.Destination == devcontainer.WorkspaceSocketTarget {
			t.Fatalf("the sidecar has the socket: %+v", m)
		}
	}
	f.h.write(folder, map[string]string{"host-side.txt": "written on the host\n"})
	out, code := f.run(t, id, "/run/menagerie/bin/workspace-commit", "-m", "chore: from compose")
	if code != 0 || f.backedUp(t, "compose-socket", "host-side.txt") != "written on the host\n" {
		t.Fatalf("exit %d: %s", code, out)
	}
}

// capture is a progress whose console the test reads.
type capture struct {
	logProgress
	out strings.Builder
}

func (c *capture) Log(prefix string) io.Writer {
	return writerFunc(func(b []byte) (int, error) {
		c.out.WriteString(prefix + string(b))
		return c.logProgress.Log(prefix).Write(b)
	})
}

// hasSocket reports whether container id has the socket mount and names
// the socket in its environment.
func (f *socketFixture) hasSocket(t *testing.T, id string) bool {
	t.Helper()
	info := f.h.inspect(id)
	mounted := false
	for _, m := range info.Mounts {
		mounted = mounted || m.Destination == devcontainer.WorkspaceSocketTarget
	}
	named := envValue(info, devcontainer.WorkspaceSocketEnv) != ""
	if mounted != named {
		t.Fatalf("mount %t but variable %t", mounted, named)
	}
	return mounted
}

// TestAStartKeepsAContainerCreatedWithoutTheSocket starts a workspace
// whose container was created before the socket was configured: the start
// reuses that container as it is, and the socket attaches at the rebuild.
// A container created with the socket keeps it on a start.
func TestAStartKeepsAContainerCreatedWithoutTheSocket(t *testing.T) {
	for name, definition := range map[string]map[string]string{
		"compose": {
			".devcontainer/compose.yml":       composeFile,
			".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/data"}`,
		},
		"single": {".devcontainer/devcontainer.json": `{"image": "` + baseImage + `"}`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSocketFixture(t)
			folder := f.workspace(t, "kept-"+name, definition)
			// Started before the socket was configured.
			f.h.mustRun(f.h.rt.Start, folder)
			before := f.h.status(folder).ContainerID
			if f.hasSocket(t, before) {
				t.Fatal("a container started without a socket has one")
			}

			start := &capture{logProgress: logProgress{t: t}}
			if err := f.rt.Start(f.h.ctx, folder, start); err != nil {
				t.Fatal(err)
			}
			if after := f.h.status(folder).ContainerID; after != before {
				t.Fatalf("the start recreated the container: %s, then %s", before, after)
			}
			if f.hasSocket(t, before) {
				t.Fatal("the kept container has the socket")
			}
			if !strings.Contains(start.out.String(), "the workspace socket attaches at the next rebuild") || strings.Contains(start.out.String(), f.folder) {
				t.Fatalf("console %q", start.out.String())
			}

			f.h.mustRun(f.rt.Rebuild, folder)
			rebuilt := f.h.status(folder).ContainerID
			if rebuilt == before || !f.hasSocket(t, rebuilt) {
				t.Fatalf("the rebuild %s has no socket", rebuilt)
			}
			again := &capture{logProgress: logProgress{t: t}}
			if err := f.rt.Start(f.h.ctx, folder, again); err != nil {
				t.Fatal(err)
			}
			if id := f.h.status(folder).ContainerID; id != rebuilt || !f.hasSocket(t, id) {
				t.Fatalf("a start changed the container with the socket: %s, then %s", rebuilt, id)
			}
			if strings.Contains(again.out.String(), "attaches at the next rebuild") {
				t.Fatalf("console %q", again.out.String())
			}
			f.h.write(folder, map[string]string{"after-rebuild.txt": "written on the host\n"})
			if out, code := f.run(t, rebuilt, "/run/menagerie/bin/workspace-commit", "chore: after the rebuild"); code != 0 {
				t.Fatalf("exit %d: %s", code, out)
			}
			if got := f.backedUp(t, "kept-"+name, "after-rebuild.txt"); got != "written on the host\n" {
				t.Fatalf("backed up %q", got)
			}
		})
	}
}

// TestAStartThatRecreatesForAnotherReasonSaysTheSocketWaits changes the
// definition of a Compose workspace whose container has no socket: the
// start recreates the primary for that change, still without the socket,
// and the console says the socket attaches at the next rebuild.
func TestAStartThatRecreatesForAnotherReasonSaysTheSocketWaits(t *testing.T) {
	f := newSocketFixture(t)
	folder := f.workspace(t, "changed-compose", map[string]string{
		".devcontainer/compose.yml":       composeFile,
		".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/data"}`,
	})
	f.h.mustRun(f.h.rt.Start, folder)
	before := f.h.status(folder).ContainerID
	f.h.write(folder, map[string]string{
		".devcontainer/devcontainer.json": `{"dockerComposeFile": "compose.yml", "service": "app", "workspaceFolder": "/data", "containerEnv": {"SAMPLE_SETTING": "changed"}}`,
	})
	start := &capture{logProgress: logProgress{t: t}}
	if err := f.rt.Start(f.h.ctx, folder, start); err != nil {
		t.Fatal(err)
	}
	after := f.h.status(folder).ContainerID
	if after == before {
		t.Fatal("the changed definition did not recreate the primary; the test checks nothing")
	}
	if f.hasSocket(t, after) {
		t.Fatal("the recreated primary has the socket")
	}
	if !strings.Contains(start.out.String(), "this start created the container again, without the workspace socket") || strings.Contains(start.out.String(), f.folder) {
		t.Fatalf("console %q", start.out.String())
	}
	f.h.mustRun(f.rt.Rebuild, folder)
	if rebuilt := f.h.status(folder).ContainerID; !f.hasSocket(t, rebuilt) {
		t.Fatalf("the rebuild %s has no socket", rebuilt)
	}
}

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
