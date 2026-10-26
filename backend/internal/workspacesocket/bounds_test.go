package workspacesocket

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// short gives the listeners deadlines a test can wait out.
func short(o *Options) {
	o.bounds = &bounds{maxWorkspaceConnections, maxConnections, 200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond}
}

func (f *fixture) dial(name string) net.Conn {
	f.t.Helper()
	conn, err := net.Dial("unix", f.socket(name))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { conn.Close() })
	return conn
}

// closedSoon reports whether the server closes conn within the wait,
// reading and dropping whatever it sends first.
func closedSoon(conn net.Conn, wait time.Duration) bool {
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	_, err := io.Copy(io.Discard, conn)
	var ne net.Error
	return err == nil || !(errors.As(err, &ne) && ne.Timeout())
}

func TestTheBoundsAreTheDecidedOnes(t *testing.T) {
	if maxWorkspaceConnections != 32 || maxConnections != 256 || headerTimeout != 10*time.Second || bodyTimeout != 10*time.Second || idleTimeout != time.Minute {
		t.Fatal("a connection bound changed")
	}
	f := newFixture(t)
	if f.sockets.bounds != (bounds{32, 256, 10 * time.Second, 10 * time.Second, time.Minute}) {
		t.Fatalf("bounds %+v", f.sockets.bounds)
	}
}

func TestAWorkspaceHasAtMost32Connections(t *testing.T) {
	f := newFixture(t)
	f.workspace("alpha")
	f.workspace("beta")
	held := make([]net.Conn, maxWorkspaceConnections)
	for i := range held {
		held[i] = f.dial("alpha")
	}
	if !closedSoon(f.dial("alpha"), 2*time.Second) {
		t.Fatal("connection 33 of a workspace was kept")
	}
	if r := f.post("beta", commitBody("other workspace")); r.status != http.StatusOK {
		t.Fatalf("another workspace was refused: %d %s", r.status, r.body)
	}
	held[0].Close()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		r, err := f.call(context.Background(), "alpha", http.MethodGet, "/", nil)
		if err == nil && r.status == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a closed connection's count was not given back: %v", err)
		}
	}
}

func TestTheProcessHasAtMost256Connections(t *testing.T) {
	f := newFixture(t)
	names := make([]string, maxConnections/maxWorkspaceConnections+1)
	for i := range names {
		names[i] = fmt.Sprintf("w%d", i)
		f.workspace(names[i])
	}
	for _, name := range names[:len(names)-1] {
		for range maxWorkspaceConnections {
			f.dial(name)
		}
	}
	// Each held connection is accepted before a later one to its socket;
	// wait until all are counted.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		f.sockets.conns.mu.Lock()
		total := f.sockets.conns.total
		f.sockets.conns.mu.Unlock()
		if total == maxConnections {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections counted", total)
		}
	}
	if !closedSoon(f.dial(names[len(names)-1]), 2*time.Second) {
		t.Fatal("connection 257 of the process was kept")
	}
}

func TestAHeaderMustArriveInTime(t *testing.T) {
	f := newFixture(t, short)
	f.workspace("alpha")
	conn := f.dial("alpha")
	if _, err := io.WriteString(conn, "POST "+CommitsPath+" HTTP/1.1\r\nHost: workspace\r\n"); err != nil {
		t.Fatal(err)
	}
	if !closedSoon(conn, 3*time.Second) {
		t.Fatal("a connection that never ends its header was kept")
	}
}

func TestABodyMustArriveInTime(t *testing.T) {
	f := newFixture(t, short)
	f.workspace("alpha")
	conn := f.dial("alpha")
	if _, err := io.WriteString(conn, "POST "+CommitsPath+" HTTP/1.1\r\nHost: workspace\r\nContent-Type: application/json\r\nContent-Length: 64\r\n\r\n{\"message\":"); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer to a body that stopped: %v", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if r := (answer{status: res.StatusCode, body: data}); r.status != http.StatusBadRequest || r.kind(t) != KindInvalidRequest {
		t.Fatalf("answer %d %s", r.status, r.body)
	}
	if len(f.jobs.List()) != 0 {
		t.Fatal("a job started")
	}
}

func TestAnIdleConnectionIsClosed(t *testing.T) {
	f := newFixture(t, short)
	f.workspace("alpha")
	conn := f.dial("alpha")
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: workspace\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	res, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.Close {
		t.Fatal("the server closed after one request; the idle bound is not tested")
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, err = reader.ReadByte()
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatal("an idle connection was kept")
	}
}

func TestTheWaitForTheJobHasNoDeadline(t *testing.T) {
	f := newFixture(t, short)
	folder := f.workspace("alpha")
	gate := make(chan struct{})
	f.remote.gate = gate
	go func() {
		// The backup takes longer than every deadline together.
		time.Sleep(time.Second)
		close(gate)
	}()
	r := f.post("alpha", commitBody("slow backup"))
	var res Result
	r.decode(t, &res)
	if r.status != http.StatusOK || res.State != jobs.Succeeded {
		t.Fatalf("answer %d %s", r.status, r.body)
	}
	if _, message := head(t, folder); message != "slow backup" {
		t.Fatalf("head %q", message)
	}
}

// waitingRequest posts a commit whose backup waits on gate, and returns
// once its job is in its backup phase.
func (f *fixture) waitingRequest(gate chan struct{}) (string, <-chan answer) {
	f.t.Helper()
	f.remote.gate = gate
	answers := make(chan answer, 1)
	go func() {
		r, err := f.call(context.Background(), "alpha", http.MethodPost, CommitsPath, []byte(commitBody("during shutdown")))
		if err != nil {
			r = answer{body: []byte(err.Error())}
		}
		answers <- r
	}()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if list := f.jobs.List(); len(list) == 1 && list[0].Phase == "backup" {
			return list[0].JobID, answers
		}
		if time.Now().After(deadline) {
			f.t.Fatal("the job did not reach its backup")
		}
	}
}

func TestShuttingDownAnswersAWaitingRequest(t *testing.T) {
	for name, stop := range map[string]func(f *fixture, cancel context.CancelFunc){
		"close":          func(f *fixture, _ context.CancelFunc) { f.sockets.Close() },
		"context ending": func(_ *fixture, cancel context.CancelFunc) { cancel() },
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := newFixture(t, func(o *Options) { o.Context = ctx })
			f.workspace("alpha")
			gate := make(chan struct{})
			defer close(gate)
			id, answers := f.waitingRequest(gate)
			stop(f, cancel)
			var r answer
			select {
			case r = <-answers:
			case <-time.After(5 * time.Second):
				t.Fatal("no answer")
			}
			var e struct{ Error Error }
			r.decode(t, &e)
			if r.status != http.StatusServiceUnavailable || e.Error.Kind != KindShuttingDown || e.Error.JobID != id {
				t.Fatalf("answer %d %s", r.status, r.body)
			}
			var generic any
			r.decode(t, &generic)
			valueFree(t, "shutting down", generic)
		})
	}
}

func TestARequestAfterShutdownStartsNoJob(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newFixture(t, func(o *Options) { o.Context = ctx })
	f.workspace("alpha")
	cancel()
	r := f.post("alpha", commitBody("too late"))
	if r.status != http.StatusServiceUnavailable || r.kind(t) != KindShuttingDown || strings.Contains(string(r.body), "jobId") {
		t.Fatalf("answer %d %s", r.status, r.body)
	}
	if len(f.jobs.List()) != 0 {
		t.Fatal("a job started")
	}
}

// sentinel stands for a path given in the configuration; no start-up
// diagnostic may hold it.
const sentinel = "s3ntinel-private-path"

func TestStartUpDiagnosticsNameNoPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), sentinel)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	client := filepath.Join(dir, ClientName)
	if err := os.WriteFile(client, []byte("sample client\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	clean := func(label string, err error, want string) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), "no such file") {
			t.Errorf("%s: %v, want %q without the path", label, err, want)
		}
	}
	for label, folder := range map[string]string{"missing": filepath.Join(dir, "missing"), "link": link, "file": file} {
		_, err := Open(Options{Folder: folder, Client: client})
		want := "workspace-socket.folder must be a folder of the services' user"
		if label == "missing" {
			want = "workspace-socket.folder must exist"
		}
		clean(label, err, want)
	}
	_, err := Open(Options{Folder: dir, Client: filepath.Join(dir, "missing-client")})
	clean("client", err, "the workspace socket client")

	f := newFixture(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(f.folder, "linked")); err != nil {
		t.Fatal(err)
	}
	_, err = f.sockets.Ensure("linked")
	clean("workspace folder", err, "workspace socket of linked: its folder must be a folder")
	if err := os.MkdirAll(filepath.Join(f.folder, "gamma", devcontainer.WorkspaceSocketName), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = f.sockets.Ensure("gamma")
	clean("socket file", err, "workspace socket of gamma: its socket file is a folder")
}
