package throwaway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"
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
	// list or remove.
	fail map[string]string
}

// failing answers the request with the stage's failure, if it has one.
func (e *engine) failing(w http.ResponseWriter, stage string, status int) bool {
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
	switch {
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
			if args.MatchKVList("label", labels) {
				list = append(list, container.Summary{ID: id, Labels: labels})
			}
		}
		e.mu.Unlock()
		_ = json.NewEncoder(w).Encode(list)
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/archive"):
		if e.failing(w, "copy", http.StatusInternalServerError) {
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
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
	return Command{Kind: KindHook, Job: "job-1", Image: "runner:sample", Folder: t.TempDir(), Dir: "/", Argv: []string{"true"}}
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
