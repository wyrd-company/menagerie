package throwaway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"

	"github.com/wyrd-company/menagerie/backend/internal/dockerproxy"
)

// engine is a fake Docker Engine API. It knows no image until one is
// pulled, keeps the containers it creates, and hands each create to
// onCreate, which decides when the container exists and what the reply is.
type engine struct {
	t        *testing.T
	mu       sync.Mutex
	pulled   bool
	next     int
	live     map[string]map[string]string
	creates  []container.CreateRequest
	removed  []string
	handlers sync.WaitGroup
	// onCreate answers a create; register adds the container and returns
	// its id. Nil registers it and replies with its id.
	onCreate func(w http.ResponseWriter, r *http.Request, register func() string)
	// fail maps a stage to the message the Engine fails it with: inspect,
	// pull, pull-stream, create, copy, attach, start, wait, wait-result,
	// copy-back, list or remove.
	fail map[string]string
	// copied are the archives copied into containers; archive is the one
	// a copy out of a container returns.
	copied  [][]byte
	archive []byte
	// onWait runs before a wait is answered.
	onWait func()
	// unfiltered answers a list with every container, as a proxy that
	// drops label filters would.
	unfiltered bool
	// denied maps a stage to the reason a Docker proxy denies it with.
	denied map[string]string
	// headers holds each request's throwaway workspace header.
	headers []string
}

// failing answers the request with the stage's failure, if it has one.
func (e *engine) failing(w http.ResponseWriter, stage string, status int) bool {
	if reason, ok := e.denied[stage]; ok {
		body, _ := json.Marshal(map[string]string{"message": "request denied by sockguard policy", "reason": reason})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write(body)
		return true
	}
	message, ok := e.fail[stage]
	if !ok {
		return false
	}
	body, _ := json.Marshal(map[string]string{"message": message})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
	return true
}

var apiVersion = regexp.MustCompile(`^/v[0-9.]+`)

func newEngine(t *testing.T) (*engine, *dockerclient.Client) {
	t.Helper()
	e := &engine{t: t, live: map[string]map[string]string{}}
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	client, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("tcp://" + srv.Listener.Addr().String()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return e, client
}

func (e *engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.handlers.Add(1)
	defer e.handlers.Done()
	path := apiVersion.ReplaceAllString(r.URL.Path, "")
	e.mu.Lock()
	e.headers = append(e.headers, r.Header.Get(dockerproxy.HeaderThrowawayWorkspace))
	e.mu.Unlock()
	switch {
	case path == "/_ping":
		w.Header().Set("Api-Version", "1.47")
		_, _ = io.WriteString(w, "OK")
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/"):
		if e.failing(w, "inspect", http.StatusInternalServerError) {
			return
		}
		e.mu.Lock()
		pulled := e.pulled
		e.mu.Unlock()
		if !pulled {
			http.Error(w, `{"message":"no such image"}`, http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"Id":"sha256:sample"}`))
	case r.Method == http.MethodPost && path == "/images/create":
		if e.failing(w, "pull", http.StatusInternalServerError) {
			return
		}
		if message, ok := e.fail["pull-stream"]; ok {
			body, _ := json.Marshal(map[string]any{"error": message, "errorDetail": map[string]string{"message": message}})
			_, _ = w.Write(append(body, '\n'))
			return
		}
		e.mu.Lock()
		e.pulled = true
		e.mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"pulled"}` + "\n"))
	case r.Method == http.MethodPost && path == "/containers/create":
		if e.failing(w, "create", http.StatusBadRequest) {
			return
		}
		var req container.CreateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"message":"bad create"}`, http.StatusBadRequest)
			return
		}
		register := func() string {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.next++
			id := fmt.Sprintf("c%d", e.next)
			e.live[id] = req.Labels
			e.creates = append(e.creates, req)
			return id
		}
		if e.onCreate != nil {
			e.onCreate(w, r, register)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"Id":%q}`, register())
	case r.Method == http.MethodGet && path == "/containers/json":
		if e.failing(w, "list", http.StatusInternalServerError) {
			return
		}
		args, err := filters.FromJSON(r.URL.Query().Get("filters"))
		if err != nil {
			http.Error(w, `{"message":"bad filters"}`, http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		var list []container.Summary
		for id, labels := range e.live {
			if e.unfiltered || args.MatchKVList("label", labels) {
				list = append(list, container.Summary{ID: id, Labels: labels})
			}
		}
		e.mu.Unlock()
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/archive"):
		if e.failing(w, "copy", http.StatusInternalServerError) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.copied = append(e.copied, body)
		e.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/archive"):
		if e.failing(w, "copy-back", http.StatusInternalServerError) {
			return
		}
		stat, _ := json.Marshal(container.PathStat{Name: "target", Mode: 0o755 | 1<<31})
		w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
		w.Header().Set("Content-Type", "application/x-tar")
		_, _ = w.Write(e.archive)
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/attach"):
		if e.failing(w, "attach", http.StatusInternalServerError) {
			return
		}
		// The command writes nothing: the raw stream ends at once.
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = buf.WriteString("HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		_ = buf.Flush()
		_ = conn.Close()
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/wait"):
		if e.failing(w, "wait", http.StatusInternalServerError) {
			return
		}
		if e.onWait != nil {
			e.onWait()
		}
		if message, ok := e.fail["wait-result"]; ok {
			_ = json.NewEncoder(w).Encode(map[string]any{"StatusCode": 1, "Error": map[string]string{"Message": message}})
			return
		}
		_, _ = w.Write([]byte(`{"StatusCode":0}`))
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/start"):
		if e.failing(w, "start", http.StatusBadRequest) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/containers/"):
		if e.failing(w, "remove", http.StatusInternalServerError) {
			return
		}
		id := strings.TrimPrefix(path, "/containers/")
		e.mu.Lock()
		_, ok := e.live[id]
		delete(e.live, id)
		e.removed = append(e.removed, id)
		e.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"no such container"}`, http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, `{"message":"not in the fake"}`, http.StatusNotImplemented)
	}
}

// left lists the containers that still exist once every request ended.
func (e *engine) left() map[string]map[string]string {
	e.handlers.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.live
}

func sample(t *testing.T) Command {
	folder := t.TempDir()
	return Command{Kind: KindHook, Job: "job-1", Workspace: folder, Image: "runner:sample", Folder: folder, Dir: "/", Argv: []string{"true"}}
}

// TestRunnerNotesStayOutOfTheCommandsStreams pulls a missing image: the
// note goes to the job console, never into a stream the hook may capture.
func TestRunnerNotesStayOutOfTheCommandsStreams(t *testing.T) {
	e, client := newEngine(t)
	e.onCreate = func(w http.ResponseWriter, _ *http.Request, _ func() string) {
		http.Error(w, `{"message":"refused"}`, http.StatusConflict)
	}
	var stdout, stderr, log bytes.Buffer
	c := sample(t)
	c.Stdout, c.Stderr, c.Log = &stdout, &stderr, &log
	if _, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(context.Background(), c); err == nil {
		t.Fatal("a refused create succeeded")
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Fatalf("the command's streams got %q %q", stdout.String(), stderr.String())
	}
	if !strings.Contains(log.String(), "pulling runner image") {
		t.Fatalf("console %q", log.String())
	}
}

// TestAContainerWhoseCreationReplyIsLostIsRemoved creates a container
// whose reply never arrives: Run finds it by its own label and removes it.
// Every container carries that label and refuses new privileges.
func TestAContainerWhoseCreationReplyIsLostIsRemoved(t *testing.T) {
	e, client := newEngine(t)
	e.onCreate = func(w http.ResponseWriter, _ *http.Request, register func() string) {
		register()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		_ = conn.Close()
	}
	other := map[string]string{LabelKind: KindHook, LabelRoot: "/srv", LabelID: "another"}
	e.live["other"] = other
	if _, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(context.Background(), sample(t)); err == nil {
		t.Fatal("a lost create succeeded")
	}
	if left := e.left(); len(left) != 1 || left["other"] == nil {
		t.Fatalf("left %v", left)
	}
	if len(e.creates) != 1 || len(e.creates[0].Labels[LabelID]) != 32 {
		t.Fatalf("creates %+v", e.creates)
	}
	if opts := e.creates[0].HostConfig.SecurityOpt; len(opts) != 1 || opts[0] != "no-new-privileges" {
		t.Fatalf("security options %v", opts)
	}
}

// TestACancellationWhileTheContainerIsCreatedRemovesIt cancels Run while
// a slow Engine creates its container: Run waits for the reply and removes
// the container it names.
func TestACancellationWhileTheContainerIsCreatedRemovesIt(t *testing.T) {
	e, client := newEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.onCreate = func(w http.ResponseWriter, r *http.Request, register func() string) {
		cancel()
		// The Engine finishes the container whether or not the caller
		// still waits for it.
		time.Sleep(300 * time.Millisecond)
		id := register()
		if r.Context().Err() != nil {
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"Id":%q}`, id)
	}
	if _, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(ctx, sample(t)); err == nil {
		t.Fatal("a cancelled run succeeded")
	}
	if left := e.left(); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}

// TestACandidateIsCopiedInAndBackAndNeverMounted runs a command on a
// copied target: the container mounts nothing, the target is copied in,
// and the copy the Engine returns replaces it.
func TestACandidateIsCopiedInAndBackAndNeverMounted(t *testing.T) {
	e, client := newEngine(t)
	target := filepath.Join(t.TempDir(), "target")
	writeTree(t, target, map[string]string{"marker.txt": "v1\n", "gone.txt": "x"})
	e.archive = archive(t, []entry{{name: "target/", mode: 0o755, dir: true}, {name: "target/marker.txt", mode: 0o644, body: "v2\n"}})
	c := sample(t)
	c.Folder, c.Target, c.Dir = "", target, target
	if code, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(context.Background(), c); err != nil || code != 0 {
		t.Fatalf("run %d %v", code, err)
	}
	if mounts := e.creates[0].HostConfig.Mounts; len(mounts) != 0 {
		t.Fatalf("mounts %+v", mounts)
	}
	if len(e.copied) != 1 || !bytes.Contains(e.copied[0], []byte(strings.TrimPrefix(target, "/")+"/marker.txt")) {
		t.Fatal("the target was not copied in")
	}
	if got := readTree(t, target); !reflect.DeepEqual(got, map[string]string{"marker.txt": "v2\n"}) {
		t.Fatalf("target %v", got)
	}
	e.archive = archive(t, []entry{{name: "target/", mode: 0o755, dir: true}, {name: "target/../escaped.txt", mode: 0o644, body: "x"}})
	_, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(context.Background(), c)
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Stage != StageCopyBack || runErr.Class != "unsafe-entry" {
		t.Fatalf("error %v, want an unsafe-entry copy-back", err)
	}
	if got := readTree(t, target); !reflect.DeepEqual(got, map[string]string{"marker.txt": "v2\n"}) {
		t.Fatalf("a refused copy changed the target: %v", got)
	}
	c.Folder = target
	if _, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(context.Background(), c); err == nil {
		t.Fatal("a command with both a mounted folder and a copied target ran")
	}
}

// TestACancelledCommandsTargetIsNotCopiedBack cancels the command while it
// runs: its target stays as it was before the command, and the container
// is removed.
func TestACancelledCommandsTargetIsNotCopiedBack(t *testing.T) {
	e, client := newEngine(t)
	target := filepath.Join(t.TempDir(), "target")
	writeTree(t, target, map[string]string{"marker.txt": "v1\n"})
	e.archive = archive(t, []entry{{name: "target/", mode: 0o755, dir: true}})
	ctx, cancel := context.WithCancel(context.Background())
	e.onWait = func() {
		cancel()
		time.Sleep(100 * time.Millisecond)
	}
	c := sample(t)
	c.Folder, c.Target, c.Dir = "", target, target
	if _, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(ctx, c); err == nil {
		t.Fatal("a cancelled run succeeded")
	}
	if got := readTree(t, target); !reflect.DeepEqual(got, map[string]string{"marker.txt": "v1\n"}) {
		t.Fatalf("target %v", got)
	}
	if left := e.left(); len(left) != 0 {
		t.Fatalf("left %v", left)
	}
}

// TestAFoldersCommandMountsOnlyItsFolder pins the one mount of a throwaway
// container: its job's folder at its own path. Nothing else of the host,
// such as the workspace socket folder, is ever mounted into one.
func TestAFoldersCommandMountsOnlyItsFolder(t *testing.T) {
	e, client := newEngine(t)
	c := sample(t)
	if code, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(context.Background(), c); err != nil || code != 0 {
		t.Fatalf("run %d %v", code, err)
	}
	mounts := e.creates[0].HostConfig.Mounts
	if len(mounts) != 1 || mounts[0].Type != "bind" || mounts[0].Source != c.Folder || mounts[0].Target != c.Folder || len(e.creates[0].HostConfig.Binds) != 0 {
		t.Fatalf("mounts %+v binds %v", mounts, e.creates[0].HostConfig.Binds)
	}
}

// TestACommandNamesItsWorkspace runs a command through a client made for
// its workspace: every request names the workspace's identity to the
// Docker proxy, the container carries it as LabelWorkspace, and never the
// devcontainer label, even when the configured labels hold it.
func TestACommandNamesItsWorkspace(t *testing.T) {
	e, base := newEngine(t)
	var made []map[string]string
	opts := DockerOptions{
		Root:   "/srv",
		Labels: map[string]string{labelLocalFolder: "/srv/other", "test": "x"},
		Client: func(headers map[string]string) (*dockerclient.Client, error) {
			made = append(made, headers)
			return dockerclient.NewClientWithOpts(dockerclient.WithHost(base.DaemonHost()), dockerclient.WithHTTPHeaders(headers))
		},
	}
	c := sample(t)
	want, err := dockerproxy.Workspace(c.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := NewDocker(base, opts).Run(context.Background(), c); err != nil || code != 0 {
		t.Fatalf("run %d %v", code, err)
	}
	if len(made) != 1 || made[0][dockerproxy.HeaderThrowawayWorkspace] != want || len(made[0]) != 1 {
		t.Fatalf("clients made with %v, want one naming %s", made, want)
	}
	e.handlers.Wait()
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, h := range e.headers {
		if h != want {
			t.Errorf("request %d names workspace %q, want %q", i, h, want)
		}
	}
	labels := e.creates[0].Labels
	if labels[LabelWorkspace] != want {
		t.Errorf("label %s = %q, want %q", LabelWorkspace, labels[LabelWorkspace], want)
	}
	if _, ok := labels[labelLocalFolder]; ok {
		t.Errorf("the container carries %s", labelLocalFolder)
	}
}

// TestACommandWithoutAWorkspaceIsRefused refuses a command that names no
// workspace before any request.
func TestACommandWithoutAWorkspaceIsRefused(t *testing.T) {
	e, client := newEngine(t)
	c := sample(t)
	c.Workspace = ""
	if _, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(context.Background(), c); err == nil || err.Error() != "the command names no workspace" {
		t.Fatalf("run error %v", err)
	}
	e.handlers.Wait()
	if len(e.headers) != 0 {
		t.Fatalf("%d requests were sent", len(e.headers))
	}
}

// TestRemoveLeftoversRemovesOnlyItsOwn answers the leftover list with
// every container, as a proxy that drops label filters would: only this
// root's throwaway containers are removed, never a devcontainer or another
// root's command.
func TestRemoveLeftoversRemovesOnlyItsOwn(t *testing.T) {
	e, client := newEngine(t)
	e.unfiltered = true
	e.live = map[string]map[string]string{
		"own-hook":   {LabelKind: KindHook, LabelRoot: "/srv"},
		"own-init":   {LabelKind: KindInitializeCommand, LabelRoot: "/srv"},
		"devcontain": {labelLocalFolder: "/srv/alpha"},
		"other-root": {LabelKind: KindHook, LabelRoot: "/elsewhere"},
		"odd-kind":   {LabelKind: "other", LabelRoot: "/srv"},
	}
	n, err := NewDocker(client, DockerOptions{Root: "/srv"}).RemoveLeftovers(context.Background())
	if err != nil || n != 2 {
		t.Fatalf("removed %d, %v", n, err)
	}
	left := e.left()
	for _, id := range []string{"devcontain", "other-root", "odd-kind"} {
		if _, ok := left[id]; !ok {
			t.Errorf("%s was removed", id)
		}
	}
}

// TestALostCreationRemovesOnlyItsContainer loses a create's reply while
// the list answers every container: only the container with the run's id
// is removed.
func TestALostCreationRemovesOnlyItsContainer(t *testing.T) {
	e, client := newEngine(t)
	e.unfiltered = true
	e.live["devcontain"] = map[string]string{labelLocalFolder: "/srv/alpha"}
	e.onCreate = func(w http.ResponseWriter, _ *http.Request, register func() string) {
		register()
		http.Error(w, `{"message":"lost"}`, http.StatusInternalServerError)
	}
	if _, err := NewDocker(client, DockerOptions{Root: "/srv"}).Run(context.Background(), sample(t)); err == nil {
		t.Fatal("run succeeded")
	}
	left := e.left()
	if _, ok := left["devcontain"]; !ok || len(left) != 1 {
		t.Fatalf("left %v, want only the devcontainer", left)
	}
}

// TestAProxyDenialNamesItsReason denies the create as the Docker proxy
// does: the error names the stage and the proxy's reason.
func TestAProxyDenialNamesItsReason(t *testing.T) {
	e, _ := newEngine(t)
	e.denied = map[string]string{"create": "bind source /srv/other is outside the workspace"}
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+srv.Listener.Addr().String())
	t.Setenv("DOCKER_TLS_VERIFY", "")
	t.Setenv("DOCKER_CERT_PATH", "")
	client, err := dockerproxy.NewClient(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	_, err = NewDocker(client, DockerOptions{Root: "/srv"}).Run(context.Background(), sample(t))
	var re *RunError
	if !errors.As(err, &re) || re.Stage != StageCreate || re.Denial != "bind source /srv/other is outside the workspace" {
		t.Fatalf("error %v, want a create RunError with the proxy's reason", err)
	}
	if !strings.HasSuffix(err.Error(), ": the Docker proxy denied it: bind source /srv/other is outside the workspace") {
		t.Errorf("error text %q", err)
	}
}
