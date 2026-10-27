//go:build templateservice

package workflows

// Create and upgrade against the real template service binary, named by
// MENAGERIE_TEMPLATE_SERVICE, with backups pushed through an isolated SSH
// Git host and agent. Git subprocesses here only prepare the test's
// template repositories; the workflows themselves use go-git.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/wyrd-company/menagerie/backend/internal/api"
	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/throwaway"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

func gitCLI(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Example User", "GIT_AUTHOR_EMAIL=user@example.invalid",
		"GIT_COMMITTER_NAME=Example User", "GIT_COMMITTER_EMAIL=user@example.invalid",
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// definition is the workspace devcontainer the sample template generates:
// an image without features, so no build runs, and an initializeCommand
// that leaves an ignored marker.
const definition = `{
  "name": "sample",
  "image": "busybox:1.37",
  "initializeCommand": "mkdir -p markers && echo initialized > markers/initialize.txt"
}
`

// sampleTemplate writes version 1 or 2 of a generic template. Version 2
// adds one question. Its hook writes its version into a tracked marker,
// leaves an ignored marker named by its version where it ran, and clones a
// nested repository that the template's .gitignore excludes.
func sampleTemplate(t *testing.T, dir string, version int) {
	extra, extraLine := "", ""
	if version == 2 {
		extra = "  - {id: extra, type: text, prompt: Extra, required: true}\n"
		extraLine = "extra: {{ extra }}\n"
	}
	writeFile(t, dir, "template.yml", "name: sample\ninterview:\n  - {id: label, type: text, prompt: Label, required: true}\n"+extra+
		fmt.Sprintf("hooks:\n  - run: [sh, -c, 'echo v%d > hook-marker.txt && mkdir -p markers && echo ran > markers/hook-v%d.txt && git init -q clones/tool && echo cloned > clones/tool/README']\n", version, version))
	writeFile(t, dir, "template/result.txt", "label: {{ label }}\n"+extraLine)
	writeFile(t, dir, "template/local.txt", fmt.Sprintf("first: template\nsecond: stable\nthird: stable\nfourth: stable\nfifth: v%d\n", version))
	writeFile(t, dir, "template/.gitignore", "/clones/\n/markers/\n*.local\n")
	writeFile(t, dir, "template/.devcontainer/devcontainer.json", definition)
}

// capturedSecret is what the leaks template's first hook captures and its
// second hook takes as an argument before it fails. The template holds it
// only in lower case.
const capturedSecret = "CAPTURED-SAMPLE-VALUE"

// capturing is a template whose first hook captures capturedSecret, as
// prefix followed by the value, and whose second hook uses it as given.
// The template holds the value only in lower case.
func capturing(prefix, second string) string {
	return "name: captures\ninterview:\n  - {id: label, type: text, prompt: Label, required: true}\nhooks:\n" +
		"  - id: token\n    run: [sh, -c, 'v=" + prefix + "$(echo " + strings.ToLower(capturedSecret) + " | tr a-z A-Z); " + "echo \"$v\"']\n    capture: [stdout]\n" +
		second
}

// leakTemplates carry a captured value into every way a later hook's
// working directory can fail: invalid when it renders, missing, and linked
// to the templates folder, which exists for this process but not in a
// throwaway container. Toha reads hook results nowhere else that can fail
// before the hook runs; a failing hook is the leaks template.
var leakTemplates = map[string]string{
	"leaks-deferred": capturing("/", "  - run: ['true']\n    cwd: '{{ token.stdout }}'\n"),
	"leaks-linked": strings.Replace(capturing("", "  - run: ['true']\n    cwd: '{{ token.stdout }}'\n"),
		`echo \"$v\"`, `ln -s ../../templates \"$v\"; echo \"$v\"`, 1),
	"leaks-missing": capturing("", "  - run: ['true']\n    cwd: '{{ token.stdout }}'\n"),
}

// leakKinds are the template service error kinds the leak templates fail
// with.
var leakKinds = map[string]string{
	"leaks-deferred": "(apply-failed)",
	"leaks-linked":   "(hook-io-failed)",
	"leaks-missing":  "(hook-io-failed)",
}

// accept is a workspace root served by the real template service and the
// workflows composed as the workspace service composes them.
type accept struct {
	root, repo, remote string
	host               *backuptest.Host
	manager            *jobs.Manager
	http               *httptest.Server
	runtime            Runtime
	flows              *Workflows
}

// localContainers stands in for throwaway containers without Docker: it
// runs each hook as a local process in its working directory, with its
// environment and PATH.
type localContainers struct{}

func (localContainers) Run(ctx context.Context, c throwaway.Command) (int, error) {
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = c.Dir, append(c.Env, "PATH="+os.Getenv("PATH")), c.Stdout, c.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

func newAccept(t *testing.T, runtime Runtime) *accept {
	t.Helper()
	return newAcceptWith(t, runtime, acceptOptions{containers: localContainers{}, image: "runner:local"})
}

// acceptOptions run the template's hooks through containers in image,
// under limits. templates adds approved templates by id; credentials,
// when set, serves the hook runner socket's credential route; env is added
// to the template service's environment.
type acceptOptions struct {
	containers  throwaway.Containers
	image       string
	limits      jobs.Limits
	templates   map[string]string
	credentials http.Handler
	env         []string
}

func newAcceptWith(t *testing.T, runtime Runtime, opts acceptOptions) *accept {
	t.Helper()
	bin := os.Getenv("MENAGERIE_TEMPLATE_SERVICE")
	if bin == "" {
		t.Fatal("MENAGERIE_TEMPLATE_SERVICE must name the template service binary")
	}
	base := t.TempDir()
	a := &accept{root: filepath.Join(base, "workspaces"), repo: filepath.Join(base, "templates", "sample"), runtime: runtime}
	for _, d := range []string{a.root, a.repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitCLI(t, a.repo, "init", "-q", "-b", "main")
	sampleTemplate(t, a.repo, 1)
	gitCLI(t, a.repo, "add", "-A")
	gitCLI(t, a.repo, "commit", "-q", "-m", "v1")
	gitCLI(t, a.repo, "tag", "v1")
	sampleTemplate(t, a.repo, 2)
	gitCLI(t, a.repo, "add", "-A")
	gitCLI(t, a.repo, "commit", "-q", "-m", "v2")
	gitCLI(t, a.repo, "tag", "v2")
	fixtures := map[string]string{
		"broken": "name: broken\ninterview:\n  - {id: label, type: text, prompt: Label, required: true}\nhooks:\n  - run: [sh, -c, 'exit 3']\n",
		"hangs":  "name: hangs\ninterview:\n  - {id: label, type: text, prompt: Label, required: true}\nhooks:\n  - run: [sh, -c, 'while :; do date >> ticks.txt; sleep 0.1; done']\n",
		"leaks":  "name: leaks\ninterview:\n  - {id: label, type: text, prompt: Label, required: true}\nhooks:\n  - id: token\n    run: [sh, -c, 'echo " + strings.ToLower(capturedSecret) + " | tr a-z A-Z']\n    capture: [stdout]\n  - run: [sh, -c, 'exit 4', '{{ token.stdout }}']\n",
		"flows":  "name: flows\ninterview:\n  - {id: label, type: text, prompt: Label, required: true}\n  - flow: stop\n    when: label == 'stop'\n  - {id: second, type: text, prompt: Second, required: true}\n",
	}
	for name, template := range leakTemplates {
		fixtures[name] = template
	}
	fixtures["named"] = "name: named\ninterview:\n  - {id: label, type: text, prompt: Label, required: true}\n"
	for name, template := range fixtures {
		dir := filepath.Join(base, "templates", name)
		writeFile(t, dir, "template.yml", template)
		writeFile(t, dir, "template/out.txt", "label: {{ label }}\n")
		if name == "named" {
			// A file named by the answer, so a plan can refuse its path.
			writeFile(t, dir, "template/{{ label }}.txt", "named\n")
		}
		gitCLI(t, dir, "init", "-q", "-b", "main")
		gitCLI(t, dir, "add", "-A")
		gitCLI(t, dir, "commit", "-q", "-m", name)
	}

	a.host = backuptest.New(t)
	if _, err := git.PlainInit(filepath.Join(a.host.Dir, "backups.git"), true); err != nil {
		t.Fatal(err)
	}
	a.remote = filepath.Join(a.host.Dir, "backups.git")
	// The production composition: default known hosts, the agent socket.
	t.Setenv("SSH_KNOWN_HOSTS", a.host.KnownHosts)

	sockDir, err := os.MkdirTemp("", "ts")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "template.sock")
	hookSocket := filepath.Join(sockDir, "hook-runner.sock")
	candidates := filepath.Join(base, "candidates")
	hooks := throwaway.NewHookRunner(opts.containers, candidates)
	hookListener, err := throwaway.ListenHookRunner(hookSocket)
	if err != nil {
		t.Fatal(err)
	}
	var hookHandler http.Handler = hooks
	if opts.credentials != nil {
		hookHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/credentials/github" {
				opts.credentials.ServeHTTP(w, r)
				return
			}
			hooks.ServeHTTP(w, r)
		})
	}
	hookServer := &http.Server{Handler: hookHandler}
	go func() { _ = hookServer.Serve(hookListener) }()
	t.Cleanup(func() { _ = hookServer.Close() })
	t2 := filepath.Join(base, "templates")
	config := fmt.Sprintf("workspace-root: %s\nworkspace-repository: %s\nwyrwood-directory: /run/sample\noidc: {issuer: 'https://issuer.example.invalid', client-id: sample}\ntemplate-service: {socket: %s, hook-runner-socket: %s, candidate-folder: %s}\ntemplates:\n"+
		"  - {id: sample-v1, name: Sample, address: 'file://%s@v1'}\n  - {id: sample-v2, name: Sample, address: 'file://%s@v2'}\n"+
		"  - {id: broken, name: Broken, address: 'file://%s/broken'}\n  - {id: flows, name: Flows, address: 'file://%s/flows'}\n  - {id: hangs, name: Hangs, address: 'file://%s/hangs'}\n"+
		"  - {id: leaks, name: Leaks, address: 'file://%s/leaks'}\n",
		a.root, a.host.URL("backups.git"), socket, hookSocket, candidates, a.repo, a.repo, t2, t2, t2, t2)
	approved := map[string]bool{"sample-v1": true, "sample-v2": true, "broken": true, "flows": true, "hangs": true, "leaks": true, "named": true}
	config += fmt.Sprintf("  - {id: named, name: Named, address: 'file://%s/named'}\n", t2)
	for name := range leakTemplates {
		config += fmt.Sprintf("  - {id: %s, name: Leaks, address: 'file://%s/%s'}\n", name, t2, name)
		approved[name] = true
	}
	for id, address := range opts.templates {
		config += fmt.Sprintf("  - {id: %s, name: %s, address: '%s'}\n", id, id, address)
		approved[id] = true
	}
	writeFile(t, base, "config.yml", config)
	startService(t, bin, filepath.Join(base, "config.yml"), filepath.Join(base, "data"), socket, opts.env...)

	root, err := workspace.NewRoot(a.root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.manager = jobs.NewManager(ctx, jobs.Options{Limits: opts.limits})
	flows := New(Options{
		Root:      root,
		Templates: templates.New(socket),
		Approved:  approved,
		Remote:    &backup.Remote{URL: a.host.URL("backups.git"), Auth: backup.AgentAuth(a.host.AgentPath)},
		Runtime:   runtime,
		Hooks:     hooks,
		RunnerImage: func(context.Context, string) (string, error) {
			return opts.image, nil
		},
	})
	a.flows = flows
	a.http = httptest.NewServer(api.New(api.Options{Root: root, Jobs: a.manager, Operations: flows.Operations(), Interviews: flows}))
	t.Cleanup(a.http.Close)
	t.Cleanup(a.manager.Wait)
	t.Cleanup(cancel)
	return a
}

// noCapturedValue checks a failed generation: its error names kind, and
// neither the error nor any event, the job's log included, carries
// capturedSecret.
func noCapturedValue(t *testing.T, job *jobs.Job, kind string) {
	t.Helper()
	all, _, _ := job.EventsAfter(0)
	for _, e := range all {
		if strings.Contains(string(e.Data), capturedSecret) {
			t.Fatalf("event %d %s carries the captured value", e.ID, e.Type)
		}
	}
	snap := job.Snapshot()
	if snap.State != jobs.Failed || snap.Error == nil || snap.Error.Kind != KindGenerationFailed ||
		strings.Contains(snap.Error.Message, capturedSecret) || !strings.Contains(snap.Error.Message, kind) {
		t.Fatalf("%+v %+v, want a generation failure %s without the captured value", snap, snap.Error, kind)
	}
}

// startService runs the template service binary until the test ends.
func startService(t *testing.T, bin, config, data, socket string, env ...string) {
	t.Helper()
	cmd := exec.Command(bin, "-config", config, "-data", data)
	cmd.Env = append(os.Environ(), env...)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		s := bufio.NewScanner(out)
		for s.Scan() {
			t.Logf("template service: %s", s.Text())
		}
		_ = cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		<-exited
	})
	for {
		if c, err := net.Dial("unix", socket); err == nil {
			_ = c.Close()
			return
		}
		select {
		case <-exited:
			t.Fatal("the template service exited before listening")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (a *accept) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(a.http.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// drive runs an operation, answering each batch from answers, and returns
// the ended job and the question ids each batch asked.
func (a *accept) drive(t *testing.T, operation, name, template string, answers map[string]any) (*jobs.Job, [][]string) {
	t.Helper()
	status, body := a.post(t, "/api/jobs", fmt.Sprintf(`{"operation":%q,"workspace":%q,"templateId":%q}`, operation, name, template))
	if status != http.StatusAccepted {
		t.Fatalf("start %s %s: %d %s", operation, name, status, body)
	}
	var snap jobs.Snapshot
	_ = json.Unmarshal([]byte(body), &snap)
	job, _ := a.manager.Get(snap.JobID)
	var asked [][]string
	var cursor int64
	for {
		e, ok := nextInterview(job, cursor)
		if !ok {
			break
		}
		cursor = e.ID
		var r struct {
			State    string
			Session  json.RawMessage
			Document struct {
				Context struct{ Template string }
				Schema  struct{ Properties map[string]json.RawMessage }
			}
		}
		if err := json.Unmarshal(e.Data, &r); err != nil {
			t.Fatal(err)
		}
		if r.State != "questions" {
			break
		}
		var ids []string
		given := map[string]any{}
		for id := range r.Document.Schema.Properties {
			ids = append(ids, id)
			given[id] = answers[id]
		}
		sort.Strings(ids)
		asked = append(asked, ids)
		waitWaiting(t, job, cursor)
		submission, _ := json.Marshal(map[string]any{"template": r.Document.Context.Template, "answers": given})
		if status, body := a.post(t, "/api/jobs/"+job.ID()+"/answers", `{"session":`+string(r.Session)+`,"submission":`+string(submission)+`}`); status != 200 {
			t.Fatalf("answer: %d %s", status, body)
		}
		if len(asked) > 5 {
			t.Fatal("the interview does not complete")
		}
	}
	waitEnded(t, job)
	return job, asked
}

// nextInterview waits for the job's next interview event after cursor; ok
// is false when the job ended without one.
func nextInterview(job *jobs.Job, cursor int64) (jobs.Event, bool) {
	for {
		events, ended, changed := job.EventsAfter(cursor)
		for _, e := range events {
			if e.Type == EventInterview {
				return e, true
			}
			cursor = e.ID
		}
		if ended {
			return jobs.Event{}, false
		}
		<-changed
	}
}

func (a *accept) history(t *testing.T, name string) []*object.Commit {
	t.Helper()
	repo, err := git.PlainOpen(a.remote)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(name), false)
	if err != nil {
		return nil
	}
	var out []*object.Commit
	for hash := ref.Hash(); ; {
		c, err := repo.CommitObject(hash)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
		if c.NumParents() == 0 {
			return out
		}
		hash = c.ParentHashes[0]
	}
}

func evidence(t *testing.T, job *jobs.Job) {
	t.Helper()
	all, _, _ := job.EventsAfter(0)
	for _, e := range all {
		t.Logf("job %s event %d %s %s", job.Snapshot().Operation, e.ID, e.Type, e.Data)
	}
}

// acceptCreateAndUpgrade is the scenario both runtimes run. afterCreate,
// when given, checks the created workspace before the upgrade.
func acceptCreateAndUpgrade(t *testing.T, a *accept, afterCreate func(folder string)) {
	folder := filepath.Join(a.root, "alpha")
	job, asked := a.drive(t, "create", "alpha", "sample-v1", map[string]any{"label": "first"})
	evidence(t, job)
	if snap := job.Snapshot(); snap.State != jobs.Succeeded {
		t.Fatalf("create %+v %+v", snap, snap.Error)
	}
	if !reflect.DeepEqual(asked, [][]string{{"label"}}) {
		t.Fatalf("create asked %v", asked)
	}
	history := a.history(t, "alpha")
	if len(history) != 1 || history[0].NumParents() != 0 {
		t.Fatalf("backup branch %v", history)
	}
	names := treeNames(t, history[0])
	sort.Strings(names)
	want := []string{".devcontainer/devcontainer.json", ".gitignore", ".toha.json", "hook-marker.txt", "local.txt", "result.txt"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("backed-up files %v, want %v", names, want)
	}
	if got := fileAt(t, history[0], "hook-marker.txt"); got != "v1\n" {
		t.Fatalf("generated hook-marker.txt %q", got)
	}
	for _, kept := range []string{"clones/tool/README", "clones/tool/.git", "markers/hook-v1.txt"} {
		if _, err := os.Stat(filepath.Join(folder, kept)); err != nil {
			t.Fatalf("%s missing from the workspace: %v", kept, err)
		}
	}
	if !strings.Contains(fileAt(t, history[0], ".toha.json"), `"label"`) {
		t.Fatal("the applied record lacks the answers")
	}

	if afterCreate != nil {
		afterCreate(folder)
	}

	// An operator edit to a tracked file, kept across the upgrade.
	local, _ := os.ReadFile(filepath.Join(folder, "local.txt"))
	if err := os.WriteFile(filepath.Join(folder, "local.txt"), []byte(strings.Replace(string(local), "third: stable", "third: operator", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	job, asked = a.drive(t, "upgrade", "alpha", "sample-v2", map[string]any{"extra": "added"})
	evidence(t, job)
	if snap := job.Snapshot(); snap.State != jobs.Succeeded {
		t.Fatalf("upgrade %+v %+v", snap, snap.Error)
	}
	if !reflect.DeepEqual(asked, [][]string{{"extra"}}) {
		t.Fatalf("upgrade asked %v, want only the new question", asked)
	}
	history = a.history(t, "alpha")
	if len(history) != 3 || history[2].NumParents() != 0 {
		t.Fatalf("backup history %d commits", len(history))
	}
	if got := fileAt(t, history[1], "local.txt"); !strings.Contains(got, "third: operator") || !strings.Contains(got, "fifth: v1") {
		t.Fatalf("pre-upgrade backup local.txt %q", got)
	}
	upgraded := history[0]
	if got := fileAt(t, upgraded, "local.txt"); !strings.Contains(got, "third: operator") || !strings.Contains(got, "fifth: v2") {
		t.Fatalf("upgraded local.txt %q", got)
	}
	// The upgrade's hook changed its marker in Toha's candidate; only
	// Toha's merge brought the change into the workspace folder, and the
	// hook wrote nothing there itself.
	if got := fileAt(t, upgraded, "hook-marker.txt"); got != "v2\n" {
		t.Fatalf("upgraded hook-marker.txt %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(folder, "hook-marker.txt")); err != nil || string(got) != "v2\n" {
		t.Fatalf("the workspace's hook-marker.txt %q %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(folder, "markers/hook-v2.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the upgrade's hook ran in the workspace folder: %v", err)
	}
	if got := fileAt(t, upgraded, "result.txt"); got != "label: first\nextra: added\n" {
		t.Fatalf("upgraded result.txt %q", got)
	}
	v2 := gitCLI(t, a.repo, "rev-parse", "v2")
	if !strings.Contains(fileAt(t, upgraded, ".toha.json"), v2) {
		t.Fatal("the applied record does not name the new template version")
	}
	if !strings.Contains(result(job), `"apply":{"outcome":"written"`) {
		t.Fatalf("result %s", result(job))
	}
}

func TestCreateAndUpgradeThroughTheTemplateService(t *testing.T) {
	rt := &fakeRuntime{}
	a := newAccept(t, rt)
	acceptCreateAndUpgrade(t, a, nil)
	if got := rt.seen(); !reflect.DeepEqual(got, []string{"up alpha", "rebuild alpha"}) {
		t.Fatalf("runtime %v", got)
	}
}

func TestTemplateServiceFailuresAreVisibleJobs(t *testing.T) {
	a := newAccept(t, &fakeRuntime{})

	t.Run("failed hook", func(t *testing.T) {
		job, _ := a.drive(t, "create", "beta", "broken", map[string]any{"label": "x"})
		evidence(t, job)
		snap := job.Snapshot()
		if snap.State != jobs.Failed || snap.Error.Kind != KindGenerationFailed || !strings.Contains(snap.Error.Message, "(hook-failed)") || !strings.Contains(snap.Error.Message, "recovery is manual") {
			t.Fatalf("%+v %+v", snap, snap.Error)
		}
		entries, _ := os.ReadDir(filepath.Join(a.root, "beta"))
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		if !reflect.DeepEqual(names, []string{".git", "out.txt"}) {
			t.Fatalf("folder %v", names)
		}
		if a.history(t, "beta") != nil {
			t.Fatal("backed up a failed generation")
		}
		if status, body := a.post(t, "/api/jobs", `{"operation":"create","workspace":"beta","templateId":"broken"}`); status != 409 || !strings.Contains(body, KindWorkspaceExists) {
			t.Fatalf("second create %d %s", status, body)
		}
	})

	t.Run("a failed hook's arguments stay out of the job", func(t *testing.T) {
		job, _ := a.drive(t, "create", "leaky", "leaks", map[string]any{"label": "x"})
		evidence(t, job)
		snap := job.Snapshot()
		// The job's error and every event, its log included, leave out the
		// value the failed hook took as an argument.
		all, _, _ := job.EventsAfter(0)
		for _, e := range all {
			if strings.Contains(string(e.Data), capturedSecret) {
				t.Fatalf("event %d %s carries the captured value", e.ID, e.Type)
			}
		}
		if snap.Error == nil || strings.Contains(snap.Error.Message, capturedSecret) {
			t.Fatalf("job error %+v", snap.Error)
		}
		if snap.State != jobs.Failed || snap.Error.Kind != KindGenerationFailed || !strings.Contains(snap.Error.Message, "(hook-failed): hook 1 failed with exit code 4;") {
			t.Fatalf("%+v %+v", snap, snap.Error)
		}
	})

	// Each way a later hook fails with an earlier hook's captured value
	// ends the job with a template service error that names its kind and
	// never the value.
	for name, kind := range leakKinds {
		t.Run("a captured value stays out of a failed job: "+name, func(t *testing.T) {
			job, _ := a.drive(t, "create", name, name, map[string]any{"label": "x"})
			evidence(t, job)
			noCapturedValue(t, job, kind)
		})
	}

	// An answer is in the job's interview events by design; the refused
	// generation's error leaves out the path the plan rendered from it.
	t.Run("a rendered answer stays out of a refused generation", func(t *testing.T) {
		answer := "/" + strings.ToLower(capturedSecret)
		job, _ := a.drive(t, "create", "named", "named", map[string]any{"label": answer})
		evidence(t, job)
		snap := job.Snapshot()
		if snap.State != jobs.Failed || snap.Error == nil || !strings.Contains(snap.Error.Message, "(plan-failed)") || strings.Contains(snap.Error.Message, answer) {
			t.Fatalf("%+v %+v", snap, snap.Error)
		}
	})

	t.Run("flow stop", func(t *testing.T) {
		job, _ := a.drive(t, "create", "gamma", "flows", map[string]any{"label": "stop"})
		if snap := job.Snapshot(); snap.State != jobs.Ended {
			t.Fatalf("%+v %+v", snap, snap.Error)
		}
		if _, err := os.Lstat(filepath.Join(a.root, "gamma")); err == nil {
			t.Fatal("an ended interview wrote the folder")
		}
	})

	t.Run("untracked file refuses the upgrade after the backup", func(t *testing.T) {
		if _, _ = a.drive(t, "create", "delta", "sample-v1", map[string]any{"label": "first"}); len(a.history(t, "delta")) != 1 {
			t.Fatal("create failed")
		}
		writeFile(t, filepath.Join(a.root, "delta"), "notes.txt", "operator notes\n")
		writeFile(t, filepath.Join(a.root, "delta"), "result.txt", "label: edited\n")
		job, _ := a.drive(t, "upgrade", "delta", "sample-v2", map[string]any{"extra": "x"})
		evidence(t, job)
		snap := job.Snapshot()
		if snap.State != jobs.Failed || snap.Error.Kind != KindTemplateRefused || !strings.Contains(snap.Error.Message, "dirty-target") {
			t.Fatalf("%+v %+v", snap, snap.Error)
		}
		history := a.history(t, "delta")
		if len(history) != 2 || fileAt(t, history[0], "result.txt") != "label: edited\n" || fileAt(t, history[0], "notes.txt") != "" {
			t.Fatalf("pre-upgrade backup %d commits", len(history))
		}
	})

	t.Run("conflicted upgrade", func(t *testing.T) {
		rt := a.runtime.(*fakeRuntime)
		if job, _ := a.drive(t, "create", "eta", "sample-v1", map[string]any{"label": "first"}); job.Snapshot().State != jobs.Succeeded {
			t.Fatal("create failed")
		}
		folder := filepath.Join(a.root, "eta")
		local, _ := os.ReadFile(filepath.Join(folder, "local.txt"))
		writeFile(t, folder, "local.txt", strings.Replace(string(local), "fifth: v1", "fifth: operator", 1))
		job, _ := a.drive(t, "upgrade", "eta", "sample-v2", map[string]any{"extra": "x"})
		evidence(t, job)
		snap := job.Snapshot()
		if snap.State != jobs.Failed || snap.Error.Kind != KindMergeConflicted || !strings.Contains(snap.Error.Message, "into local.txt") {
			t.Fatalf("%+v %+v", snap, snap.Error)
		}
		if !strings.Contains(result(job), `"outcome":"conflicted"`) || !strings.Contains(result(job), `"conflicts":["local.txt"]`) {
			t.Fatalf("result %s", result(job))
		}
		history := a.history(t, "eta")
		if len(history) != 3 || !strings.Contains(fileAt(t, history[0], "local.txt"), "<<<<<<<") {
			t.Fatalf("conflicted state not backed up: %d commits", len(history))
		}
		for _, call := range rt.seen() {
			if call == "rebuild eta" {
				t.Fatal("rebuilt a conflicted workspace")
			}
		}
	})

	t.Run("backup host key mismatch", func(t *testing.T) {
		other := backuptest.New(t)
		wrong := filepath.Join(t.TempDir(), "known_hosts")
		a.host.WriteKnownHosts(t, wrong, other.HostKey)
		t.Setenv("SSH_KNOWN_HOSTS", wrong)
		job, _ := a.drive(t, "create", "epsilon", "sample-v1", map[string]any{"label": "first"})
		snap := job.Snapshot()
		if snap.State != jobs.Failed || snap.Error.Kind != KindBackupFailed || !strings.Contains(snap.Error.Message, "key mismatch") {
			t.Fatalf("%+v %+v", snap, snap.Error)
		}
		if a.history(t, "epsilon") != nil {
			t.Fatal("pushed to an unverified host")
		}
	})

	t.Run("same workspace is exclusive", func(t *testing.T) {
		status, body := a.post(t, "/api/jobs", `{"operation":"create","workspace":"zeta","templateId":"sample-v1"}`)
		if status != 202 {
			t.Fatalf("%d %s", status, body)
		}
		var snap jobs.Snapshot
		_ = json.Unmarshal([]byte(body), &snap)
		job, _ := a.manager.Get(snap.JobID)
		waitWaiting(t, job, 0)
		if status, body := a.post(t, "/api/jobs", `{"operation":"create","workspace":"zeta","templateId":"sample-v1"}`); status != 409 || !strings.Contains(body, "workspace-busy") {
			t.Fatalf("%d %s", status, body)
		}
	})
}
