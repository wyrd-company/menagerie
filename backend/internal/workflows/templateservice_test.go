//go:build templateservice

package workflows

// Create and upgrade against the real template service binary, named by
// MENAGERIE_TEMPLATE_SERVICE, with templates fetched from an anonymous
// HTTPS Git host on loopback, which the template service trusts through
// GIT_SSL_CAINFO, and backups pushed through an isolated SSH Git host and
// agent. Git subprocesses here only prepare and serve the test's template
// repositories; the workflows themselves use go-git.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
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
	"github.com/wyrd-company/menagerie/backend/internal/registry"
	"github.com/wyrd-company/menagerie/backend/internal/store"
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

// sampleKit writes version 1 or 2 of a generic template. Version 2
// adds one question. Its hook writes its version into a tracked marker,
// leaves an ignored marker named by its version where it ran, and clones a
// nested repository that the template's .gitignore excludes.
func sampleKit(t *testing.T, dir string, version int) {
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
// workflows composed as the workspace service composes them. origin is the
// HTTPS host that serves the repositories under templates.
type accept struct {
	root, repo, remote string
	templates, origin  string
	// socket is the template service's; dataPath is Menagerie's data and
	// serviceData the template service's data folder.
	socket, dataPath, serviceData string
	// requests records each request the Git host receives, serviceLog the
	// template service's output.
	requests, serviceLog *lockedText
	data                 *store.Store
	host                 *backuptest.Host
	manager              *jobs.Manager
	http                 *httptest.Server
	runtime              Runtime
	flows                *Workflows
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
// under limits. templates registers templates by id at an address with a
// ref, such as a private host's; local serves a clone of each repository
// folder by id and registers it at main; credentials, when set, serves the
// hook runner socket's credential route; authorities are certificate files
// the template service trusts beside the fixture host's; env is added to
// the template service's environment.
type acceptOptions struct {
	containers  throwaway.Containers
	image       string
	limits      jobs.Limits
	templates   map[string]string
	local       map[string]string
	credentials http.Handler
	authorities []string
	env         []string
}

// lockedText collects text from several goroutines.
type lockedText struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *lockedText) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.Write(p)
}

func (l *lockedText) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

// servedTemplates serves the repositories under dir, anonymously, as a Git
// host over HTTPS on loopback, and returns its origin and a certificate
// bundle of its authority and the extra authorities. Each request's
// method, host, target and headers go to seen.
func servedTemplates(t *testing.T, dir string, extra []string, seen io.Writer) (origin, bundle string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal("the template host needs git on PATH")
	}
	backend := &cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Stderr: io.Discard, Env: []string{
		"GIT_PROJECT_ROOT=" + dir, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "HOME=" + t.TempDir(),
	}}
	origin, authority := templateTLSHost(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(seen, "%s %s%s %v\n", r.Method, r.Host, r.RequestURI, r.Header)
		backend.ServeHTTP(w, r)
	}))
	pems, err := os.ReadFile(authority)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range extra {
		more, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		pems = append(pems, more...)
	}
	bundle = filepath.Join(t.TempDir(), "authorities.pem")
	if err := os.WriteFile(bundle, pems, 0o600); err != nil {
		t.Fatal(err)
	}
	return origin, bundle
}

// register records a template in the fixture's data at the version ref
// names: address is its address without a ref, and its own source.
func (a *accept) register(t *testing.T, id, address, version, ref string) {
	t.Helper()
	commit := ""
	if strings.HasPrefix(address, a.origin+"/") {
		commit = gitCLI(t, filepath.Join(a.templates, strings.TrimPrefix(address, a.origin+"/")), "rev-parse", ref+"^{commit}")
	}
	// Its source is the repository's formal name, which names no user.
	source := strings.Replace(address, "https://sample@", "https://", 1)
	_, err := a.data.RegisterTemplate(context.Background(), store.Template{ID: id, Name: id, Address: address, Source: source,
		CurrentVersion: version, CurrentTag: ref, CurrentCommit: commit}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

// sampleFor is the template of workspace name's creates from the sample
// at its first version: a copy of the sample repository registered for
// that workspace alone at 1.0.0, so each workspace's template moves on its
// own in a fixture its subtests share.
func (a *accept) sampleFor(t *testing.T, name string) string {
	t.Helper()
	id := "sample-" + name
	if _, err := a.data.Template(context.Background(), id); err == nil {
		return id
	}
	gitCLI(t, a.templates, "clone", "-q", a.repo, filepath.Join(a.templates, id))
	a.register(t, id, a.origin+"/"+id, "1.0.0", "1.0.0")
	return id
}

// newVersion makes the sample's second version, 2.0.0, the current one of
// workspace name's template, as a manager's upgrade of the template does.
func (a *accept) newVersion(t *testing.T, name string) {
	t.Helper()
	ctx := context.Background()
	id, err := a.data.WorkspaceTemplate(ctx, name)
	if err != nil || id == "" {
		t.Fatalf("workspace %s has no template: %v", name, err)
	}
	commit := gitCLI(t, a.repo, "rev-parse", "2.0.0^{commit}")
	if _, err := a.data.AdvanceTemplate(ctx, id, "2.0.0", "2.0.0", commit, registry.Newer); err != nil && !errors.Is(err, store.ErrNotNewer) {
		t.Fatal(err)
	}
}

// request is a job's body: a create of template, where "sample-v1" names
// the sample at its first version, and an upgrade, which takes no
// template and applies its workspace's template's current version, where
// "sample-v2" first makes that the sample's second.
func (a *accept) request(t *testing.T, operation, name, template string) string {
	t.Helper()
	if operation == "upgrade" {
		if template == "sample-v2" {
			a.newVersion(t, name)
		}
		return fmt.Sprintf(`{"operation":%q,"workspace":%q}`, operation, name)
	}
	if template == "sample-v1" {
		template = a.sampleFor(t, name)
	}
	return fmt.Sprintf(`{"operation":%q,"workspace":%q,"templateId":%q}`, operation, name, template)
}

func newAcceptWith(t *testing.T, runtime Runtime, opts acceptOptions) *accept {
	t.Helper()
	bin := os.Getenv("MENAGERIE_TEMPLATE_SERVICE")
	if bin == "" {
		t.Fatal("MENAGERIE_TEMPLATE_SERVICE must name the template service binary")
	}
	base := t.TempDir()
	a := &accept{root: filepath.Join(base, "workspaces"), templates: filepath.Join(base, "templates"), repo: filepath.Join(base, "templates", "sample"), runtime: runtime,
		requests: &lockedText{}, serviceLog: &lockedText{}}
	for _, d := range []string{a.root, a.repo} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitCLI(t, a.repo, "init", "-q", "-b", "main")
	sampleKit(t, a.repo, 1)
	gitCLI(t, a.repo, "add", "-A")
	gitCLI(t, a.repo, "commit", "-q", "-m", "v1")
	gitCLI(t, a.repo, "tag", "1.0.0")
	sampleKit(t, a.repo, 2)
	gitCLI(t, a.repo, "add", "-A")
	gitCLI(t, a.repo, "commit", "-q", "-m", "v2")
	gitCLI(t, a.repo, "tag", "2.0.0")
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
	for id, source := range opts.local {
		gitCLI(t, base, "clone", "-q", source, filepath.Join(a.templates, id))
		fixtures[id] = ""
	}
	var bundle string
	a.origin, bundle = servedTemplates(t, a.templates, opts.authorities, a.requests)
	// The served addresses name a user, so a fixture may name a branch
	// after the host's port: admission takes only a version's tag after the
	// port of an address with no user name, whose @ could end a password.
	a.origin = strings.Replace(a.origin, "https://", "https://sample@", 1)

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
	a.socket, a.serviceData = socket, filepath.Join(base, "data")
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
	// The template service reads nothing of the templates key, which a
	// file of an earlier release still has.
	config := fmt.Sprintf("workspace-root: %s\nworkspace-repository: %s\nwyrwood-directory: /run/sample\noidc: {issuer: 'https://issuer.example.invalid', client-id: sample}\ntemplate-service: {socket: %s, hook-runner-socket: %s, candidate-folder: %s}\ntemplates:\n  - {id: sample, name: Sample, address: 'file:///absent'}\n",
		a.root, a.host.URL("backups.git"), socket, hookSocket, candidates)
	writeFile(t, base, "config.yml", config)
	startService(t, bin, filepath.Join(base, "config.yml"), a.serviceData, socket, a.serviceLog, append([]string{"GIT_SSL_CAINFO=" + bundle}, opts.env...)...)

	root, err := workspace.NewRoot(a.root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.manager = jobs.NewManager(ctx, jobs.Options{Limits: opts.limits})
	a.dataPath = filepath.Join(base, "state", "menagerie.db")
	data, err := store.Open(a.dataPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = data.Close() })
	a.data = data
	a.register(t, "sample", a.origin+"/sample", "1.0.0", "1.0.0")
	for name := range fixtures {
		a.register(t, name, a.origin+"/"+name, "0.0.0", "main")
	}
	for id, address := range opts.templates {
		at := strings.LastIndex(address, "@")
		a.register(t, id, address[:at], "0.0.0", address[at+1:])
	}
	service := templates.New(socket)
	flows := New(Options{
		Root:      root,
		Data:      data,
		Templates: service,
		Remote:    &backup.Remote{URL: a.host.URL("backups.git"), Auth: backup.AgentAuth(a.host.AgentPath)},
		Runtime:   runtime,
		Hooks:     hooks,
		RunnerImage: func(context.Context, string) (string, error) {
			return opts.image, nil
		},
	})
	a.flows = flows
	a.http = httptest.NewServer(api.New(api.Options{Root: root, Data: data, Status: runtime, Jobs: a.manager, Operations: flows.Operations(), Interviews: flows,
		Registry: &registry.Registry{Data: data, Templates: service, Root: root}, AppliedSource: registry.Source}))
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

// startService runs the template service binary until the test ends, its
// output going to the test's log and to out.
func startService(t *testing.T, bin, config, data, socket string, out io.Writer, env ...string) {
	t.Helper()
	cmd := exec.Command(bin, "-config", config, "-data", data)
	cmd.Env = append(os.Environ(), env...)
	pipe, _ := cmd.StdoutPipe()
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		s := bufio.NewScanner(pipe)
		for s.Scan() {
			t.Logf("template service: %s", s.Text())
			_, _ = fmt.Fprintln(out, s.Text())
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
	status, body := a.post(t, "/api/jobs", a.request(t, operation, name, template))
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
	v2 := gitCLI(t, a.repo, "rev-parse", "2.0.0")
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
		status, body := a.post(t, "/api/jobs", `{"operation":"create","workspace":"zeta","templateId":"sample"}`)
		if status != 202 {
			t.Fatalf("%d %s", status, body)
		}
		var snap jobs.Snapshot
		_ = json.Unmarshal([]byte(body), &snap)
		job, _ := a.manager.Get(snap.JobID)
		waitWaiting(t, job, 0)
		// The waiting create holds the slot, and its data in Menagerie's data
		// refuses a second create of the name before any job.
		if active, ok := a.manager.Active("zeta"); !ok || active.ID() != job.ID() {
			t.Fatalf("the slot is not held by %s", job.ID())
		}
		if status, body := a.post(t, "/api/jobs", `{"operation":"create","workspace":"zeta","templateId":"sample"}`); status != 409 || !strings.Contains(body, "workspace-exists") {
			t.Fatalf("%d %s", status, body)
		}
	})
}

// kitRepository writes a repository under the fixture's templates folder:
// the sample kit at version 1, tagged tag unless tag is "", or with no
// devcontainer definition when bare.
func (a *accept) kitRepository(t *testing.T, name, tag string, bare bool) string {
	t.Helper()
	dir := filepath.Join(a.templates, name)
	sampleKit(t, dir, 1)
	if bare {
		if err := os.RemoveAll(filepath.Join(dir, "template/.devcontainer")); err != nil {
			t.Fatal(err)
		}
	}
	gitCLI(t, dir, "init", "-q", "-b", "main")
	gitCLI(t, dir, "add", "-A")
	gitCLI(t, dir, "commit", "-q", "-m", "v1")
	if tag != "" {
		gitCLI(t, dir, "tag", tag)
	}
	return a.origin + "/" + name
}

func (a *accept) send(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, a.http.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

// A manager registers a template from its address, creates a workspace
// from it, the repository gets a newer tag, the manager upgrades the
// template to it and then the workspace, with no configuration change and
// no restart; every step goes through the API and the real template
// service.
func TestRegisterCreateTagAndUpgradeThroughTheTemplateService(t *testing.T) {
	a := newAccept(t, &fakeRuntime{})
	kit := a.kitRepository(t, "kit", "1.0.0", false)
	// The served host has a port, so its address takes no ref: a
	// registration takes the newest version, and an upgrade names its
	// version beside the address. A check runs the checks and stores
	// nothing.
	status, body := a.post(t, "/api/templates", `{"name":"Kit","address":"`+kit+`","check":true}`)
	if status != http.StatusOK || !strings.Contains(body, `"currentVersion":"1.0.0"`) {
		t.Fatalf("register's check: %d %s", status, body)
	}
	if status, body := a.post(t, "/api/templates", `{"name":"Kit","address":"`+kit+`@1.0.0","check":true}`); status != http.StatusUnprocessableEntity || !strings.Contains(body, `"check":"address"`) {
		t.Fatalf("a port and a ref: %d %s", status, body)
	}
	if status, body := a.send(t, http.MethodGet, "/api/templates", ""); status != http.StatusOK || strings.Contains(body, `"id":"kit"`) {
		t.Fatalf("the check stored the template: %d %s", status, body)
	}
	status, body = a.post(t, "/api/templates", `{"name":"Kit","address":"`+kit+`"}`)
	if status != http.StatusCreated || !strings.Contains(body, `"id":"kit"`) || !strings.Contains(body, `"currentVersion":"1.0.0"`) {
		t.Fatalf("register: %d %s", status, body)
	}
	job, _ := a.drive(t, "create", "alpha", "kit", map[string]any{"label": "first"})
	if snap := job.Snapshot(); snap.State != jobs.Succeeded {
		evidence(t, job)
		t.Fatalf("create %+v %+v", snap, snap.Error)
	}
	folder := filepath.Join(a.root, "alpha")
	// The applied record names the repository by its formal name, without
	// the served addresses' user name.
	source := strings.Replace(kit, "https://sample@", "https://", 1)
	if applied, err := registry.ReadApplied(folder); err != nil || applied.Ref != "1.0.0" || applied.Source != source {
		t.Fatalf("applied %+v %v", applied, err)
	}
	if status, body := a.send(t, http.MethodGet, "/api/workspaces", ""); status != 200 || !strings.Contains(body, `"template":{"id":"kit","name":"Kit"},"name":"alpha"`) {
		t.Fatalf("list: %d %s", status, body)
	}
	if status, body := a.post(t, "/api/jobs", `{"operation":"upgrade","workspace":"alpha"}`); status != http.StatusConflict || !strings.Contains(body, "upgrade-not-needed") {
		t.Fatalf("upgrade at the current version: %d %s", status, body)
	}

	// The repository gets a newer tag.
	repo := filepath.Join(a.templates, "kit")
	sampleKit(t, repo, 2)
	gitCLI(t, repo, "add", "-A")
	gitCLI(t, repo, "commit", "-q", "-m", "v2")
	gitCLI(t, repo, "tag", "v1.1.0")
	if status, body := a.post(t, "/api/templates/kit/upgrades", `{"version":"1.0.0"}`); status != http.StatusConflict || !strings.Contains(body, "version-not-newer") {
		t.Fatalf("upgrade to the current version: %d %s", status, body)
	}
	status, body = a.post(t, "/api/templates/kit/upgrades", `{"version":"1.1.0","check":true}`)
	if status != http.StatusOK || !strings.Contains(body, `"currentVersion":"1.1.0"`) {
		t.Fatalf("template upgrade's check: %d %s", status, body)
	}
	if status, body := a.send(t, http.MethodGet, "/api/templates", ""); status != http.StatusOK || !strings.Contains(body, `"currentVersion":"1.0.0"`) {
		t.Fatalf("the check moved the template: %d %s", status, body)
	}
	status, body = a.post(t, "/api/templates/kit/upgrades", `{"version":"1.1.0"}`)
	if status != http.StatusOK || !strings.Contains(body, `"currentVersion":"1.1.0"`) {
		t.Fatalf("template upgrade: %d %s", status, body)
	}
	job, asked := a.drive(t, "upgrade", "alpha", "", map[string]any{"extra": "added"})
	if snap := job.Snapshot(); snap.State != jobs.Succeeded {
		evidence(t, job)
		t.Fatalf("upgrade %+v %+v", snap, snap.Error)
	}
	if !reflect.DeepEqual(asked, [][]string{{"extra"}}) {
		t.Fatalf("asked %v", asked)
	}
	v110 := gitCLI(t, repo, "rev-parse", "v1.1.0")
	if applied, err := registry.ReadApplied(folder); err != nil || applied.Ref != "v1.1.0" || applied.Commit != v110 {
		t.Fatalf("applied %+v %v", applied, err)
	}
	if status, body := a.post(t, "/api/jobs", `{"operation":"upgrade","workspace":"alpha"}`); status != http.StatusConflict || !strings.Contains(body, "upgrade-not-needed") {
		t.Fatalf("second upgrade: %d %s", status, body)
	}
	if status, body := a.send(t, http.MethodDelete, "/api/templates/kit", ""); status != http.StatusConflict || !strings.Contains(body, "template-in-use") || !strings.Contains(body, "alpha") {
		t.Fatalf("remove: %d %s", status, body)
	}
}

// Each registration check refuses a template the real template service
// reads, naming the check and never the address.
func TestRegistrationChecksAgainstTheTemplateService(t *testing.T) {
	a := newAccept(t, &fakeRuntime{})
	bare := a.kitRepository(t, "bare", "1.0.0", true)
	untagged := a.kitRepository(t, "untagged", "", false)
	tagged := a.kitRepository(t, "tagged", "1.0.0", false)
	gitCLI(t, filepath.Join(a.templates, "tagged"), "tag", "next")
	// A manifest Toha cannot load, and a link that leads out of the
	// checkout, as a hook script could be.
	unreadable := a.kitRepository(t, "unreadable", "", false)
	writeFile(t, filepath.Join(a.templates, "unreadable"), "template.yml", "name: sample\ninterview: {not: a list}\n")
	gitCLI(t, filepath.Join(a.templates, "unreadable"), "commit", "-q", "-am", "unreadable")
	gitCLI(t, filepath.Join(a.templates, "unreadable"), "tag", "1.0.0")
	linked := a.kitRepository(t, "linked", "", false)
	if err := os.Symlink("/etc/hostname", filepath.Join(a.templates, "linked", "hook.sh")); err != nil {
		t.Fatal(err)
	}
	gitCLI(t, filepath.Join(a.templates, "linked"), "add", "-A")
	gitCLI(t, filepath.Join(a.templates, "linked"), "commit", "-q", "-m", "linked")
	gitCLI(t, filepath.Join(a.templates, "linked"), "tag", "1.0.0")
	password := strings.Replace(tagged, "https://", "https://sample-user:sample-password@", 1)
	for _, c := range []struct{ address, check string }{
		{filepath.Join(a.templates, "tagged"), "address"},
		{"file://" + filepath.Join(a.templates, "tagged"), "address"},
		{"sample-kit", "address"},
		{password, "address"},
		{a.origin + "/missing", "fetch"},
		{untagged, "version"},
		// After the port, a ref's @ could end a password: an address whose
		// host has a port takes no ref, with a user name or without, and a
		// version's tag or not.
		{tagged + "@next", "address"},
		{tagged + "@main", "address"},
		{tagged + "@1.0.0", "address"},
		{strings.Replace(tagged, "sample@", "", 1) + "@main", "address"},
		{strings.Replace(tagged, "sample@", "", 1) + "@1.0.0", "address"},
		{unreadable, "read"},
		{linked, "read"},
		{bare, "definition"},
	} {
		status, body := a.post(t, "/api/templates", `{"name":"Kit","address":"`+c.address+`"}`)
		if status != http.StatusUnprocessableEntity || !strings.Contains(body, `"check":"`+c.check+`"`) {
			t.Errorf("%s: %d %s", c.address, status, body)
		}
		for _, quoted := range []string{"sample-password", "127.0.0.1", a.templates} {
			if strings.Contains(body, quoted) {
				t.Errorf("%s: the refusal quotes the address: %s", c.address, body)
			}
		}
	}
	if status, body := a.post(t, "/api/templates", `{"name":"Kit","address":"`+tagged+`"}`); status != http.StatusCreated || !strings.Contains(body, `"currentVersion":"1.0.0"`) {
		t.Fatalf("%d %s", status, body)
	}
	if status, body := a.post(t, "/api/templates", `{"name":"Again","address":"`+tagged+`"}`); status != http.StatusConflict || !strings.Contains(body, "template-exists") {
		t.Fatalf("%d %s", status, body)
	}
}

// A password that Git, gix and Toha read as a host, a port and a path, or
// that a user name's escapes hide, is refused at registration and skipped
// at the first-start import, named by its id, and reaches no channel: no
// reply, which is all the interface shows, nor the data of either service,
// the logs of either, or a request to the Git host. An import entry on a
// host with a port and a ref is skipped the same way, and one at an example
// address with no port is imported.
func TestAHiddenPasswordReachesNoChannel(t *testing.T) {
	a := newAccept(t, &fakeRuntime{})
	kept := a.kitRepository(t, "kept", "1.0.0", false)
	listed := a.kitRepository(t, "listed", "1.0.0", false)
	a.kitRepository(t, "target", "1.0.0", false)
	const marker = "sample-marker-secret"
	origin, err := url.Parse(a.origin)
	if err != nil {
		t.Fatal(err)
	}
	host, port := origin.Host, origin.Port()
	var standard lockedText
	log.SetOutput(&standard)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	// Each form names repository REPO: the registrations a served one, and
	// the import entries another, so that neither, let through, stops the
	// other's channels being scanned.
	hidden := map[string]string{
		// Git, gix and Toha read the Git host on its port, and a path that
		// holds the rest of the password.
		"port-path":       "https://127.0.0.1:" + port + "/" + marker + "@git.example.invalid/REPO@1.0.0",
		"port-path-noref": "https://127.0.0.1:" + port + "/" + marker + "@git.example.invalid/REPO",
		"slash":           "https://sample-user:/" + marker + "@" + host + "/REPO@1.0.0",
		"slash-noref":     "https://sample-user:/" + marker + "@" + host + "/REPO",
		"slash-at":        "https://sample-user:/" + marker + "@x@" + host + "/REPO@1.0.0",
		"escaped":         "https://sample-user%3A" + marker + "@" + host + "/REPO@1.0.0",
		"empty":           "https://" + marker + ":@" + host + "/REPO@1.0.0",
		"ssh":             "ssh://sample-user:12/" + marker + "@" + host + "/REPO@1.0.0",
		"scp":             "git@sample-user:/" + marker + "@" + host + ":REPO@1.0.0",
		"gh":              "gh:sample-user:/" + marker + "@example-org/REPO@1.0.0",
		// Passwords before a host with no path after it, where the host
		// reads as a ref, and passwords a # folder hid.
		"port-nopath":  "https://127.0.0.1:" + port + "/" + marker + "@git.example.invalid",
		"slash-nopath": "https://sample-user:/" + marker + "@" + host,
		"ssh-nopath":   "ssh://git:22/" + marker + "@" + host,
		"gh-nopath":    "gh:sample-user:12/" + marker + "@" + host,
		"hash-x":       "https://sample-user:12#x/" + marker + "@" + host + "/REPO",
		"hash-folder":  "https://sample-user:12/REPO@1.0.0#/" + marker + "@" + host,
		// A user name that holds an `@`, which Git, gix and Toha read as a
		// user name, the Git host on its port, a path that holds the
		// password and a ref; a password before a host that reads as a
		// version's tag, or as a short IPv4 address; and the same shapes
		// with a path that holds an `@`.
		"user-port-ref":        "https://sample-user@127.0.0.1:" + port + "/" + marker + "@gitserver",
		"ssh-user-port-ref":    "ssh://sample-user@127.0.0.1:" + port + "/" + marker + "@gitserver",
		"port-version-host":    "https://sample-user:12/" + marker + "@1.2.3",
		"port-ipv4-host":       "https://sample-user:12/" + marker + "@127.0.1",
		"user-port-path":       "https://sample-user@127.0.0.1:" + port + "/" + marker + "@git.example.invalid/REPO@1.0.0",
		"user-port-path-noref": "https://sample-user@127.0.0.1:" + port + "/" + marker + "@git.example.invalid/REPO",
		"version-host":         "https://sample-user:" + marker + "@1.0.0",
		"ssh-version-host":     "ssh://git:" + marker + "@1.0.0",
	}
	var replies []string
	entries := []registry.Entry{
		{ID: "kept", Name: "Kept", Address: "https://git.example.invalid/example-org/kept@1.0.0"},
		{ID: "kept-port", Name: "Kept, port", Address: kept + "@1.0.0"},
	}
	for id, form := range hidden {
		entries = append(entries, registry.Entry{ID: id, Name: "Hidden", Address: strings.ReplaceAll(form, "REPO", "imported-"+id)})
		if strings.HasPrefix(form, "gh:") {
			// A gh: address expands to github.com: were one let through, its
			// registration would fetch from there. The import fetches nothing.
			continue
		}
		address := strings.ReplaceAll(form, "REPO", "target")
		body, err := json.Marshal(map[string]string{"name": "Hidden", "address": address})
		if err != nil {
			t.Fatal(err)
		}
		status, reply := a.post(t, "/api/templates", string(body))
		if status != http.StatusUnprocessableEntity || !strings.Contains(reply, `"check":"address"`) {
			t.Errorf("%s: %d %s", id, status, reply)
		}
		replies = append(replies, reply)
	}
	// A registration that fetches, so the Git host's record is not empty.
	if status, reply := a.post(t, "/api/templates", `{"name":"Listed","address":"`+listed+`"}`); status != http.StatusCreated {
		t.Errorf("the registration that fetches: %d %s", status, reply)
	}
	root, err := workspace.NewRoot(a.root)
	if err != nil {
		t.Fatal(err)
	}
	// Each step reports and goes on, so the scan below covers every
	// channel even when an earlier step let the password through.
	var logged lockedText
	if err := registry.Import(context.Background(), registry.ImportOptions{
		Data: a.data, Templates: templates.New(a.socket), Root: root, Entries: entries,
		Logf: func(format string, args ...any) { _, _ = fmt.Fprintf(&logged, format+"\n", args...) },
	}); err != nil {
		t.Errorf("the import failed: %v", err)
	}
	for id := range hidden {
		if !strings.Contains(logged.String(), "entry "+id+" is not imported: the template service refuses its address (invalid-address)") {
			t.Errorf("the import does not name %s as refused:\n%s", id, logged.String())
		}
	}
	if !strings.Contains(logged.String(), "entry kept-port is not imported: the template service refuses its address (invalid-address)") || strings.Contains(logged.String(), kept) {
		t.Errorf("the import does not skip the entry with a port and a ref by its id alone:\n%s", logged.String())
	}
	status, list := a.send(t, http.MethodGet, "/api/templates", "")
	if status != http.StatusOK || !strings.Contains(list, `"id":"kept"`) || strings.Contains(list, "kept-port") {
		t.Errorf("the template list: %d %s", status, list)
	}
	channels := map[string]string{
		"registration replies":        strings.Join(replies, "\n"),
		"the template list":           list,
		"the import's log":            logged.String(),
		"the workspace service's log": standard.String(),
		"the template service's log":  a.serviceLog.String(),
		"the Git host's requests":     a.requests.String(),
		"Menagerie's data":            filesText(t, filepath.Dir(a.dataPath)),
		"the template service's data": filesText(t, a.serviceData),
	}
	for channel, text := range channels {
		if strings.Contains(text, "sample-marker") {
			t.Errorf("%s holds the password", channel)
		}
	}
	if !strings.Contains(a.requests.String(), "/listed/") {
		t.Errorf("the Git host saw no request, so its record proves nothing:\n%s", a.requests.String())
	}
}

// filesText is the bytes of every file under dir, as text.
func filesText(t *testing.T, dir string) string {
	t.Helper()
	var text strings.Builder
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text.Write(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return text.String()
}
