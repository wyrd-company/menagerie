package alerts

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// secret marks the credential part of every webhook URL in these tests.
const secret = "tok-0123456789"

type logLines struct {
	mu    sync.Mutex
	lines []string
}

func (l *logLines) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logLines) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func failedSnapshot() jobs.Snapshot {
	return jobs.Snapshot{
		JobID: "j1", Workspace: "alpha", Operation: "rebuild", Phase: "build", State: jobs.Failed,
		Error: &jobs.Error{Kind: jobs.KindTimedOut, Message: "the build phase passed its 45m limit"},
	}
}

func newWebhook(t *testing.T, target string, logs *logLines, timeout time.Duration) *Webhook {
	t.Helper()
	w, err := New(Options{URL: target, Origin: "https://menagerie.example.test", Logf: logs.logf, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestJobFailedPostsThePayload(t *testing.T) {
	got := make(chan *http.Request, 1)
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- r
		bodies <- body
	}))
	defer srv.Close()
	logs := &logLines{}
	w := newWebhook(t, srv.URL+"/hook/"+secret, logs, 0)
	w.JobFailed(failedSnapshot())
	w.Close()
	r := <-got
	if r.Method != http.MethodPost || r.URL.Path != "/hook/"+secret || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("%s %s %s", r.Method, r.URL, r.Header.Get("Content-Type"))
	}
	want := `{"event":"job-failed","jobId":"j1","workspace":"alpha","operation":"rebuild","phase":"build","state":"failed",` +
		`"error":{"kind":"timed-out","message":"the build phase passed its 45m limit"},"console":"https://menagerie.example.test/?job=j1"}`
	if body := string(<-bodies); body != want {
		t.Fatalf("body %s\nwant %s", body, want)
	}
	if lines := logs.all(); len(lines) != 0 {
		t.Fatalf("log %q", lines)
	}
}

func TestJobLongRunningCarriesANullError(t *testing.T) {
	bodies := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- body
	}))
	defer srv.Close()
	w := newWebhook(t, srv.URL, &logLines{}, 0)
	w.JobLongRunning(jobs.Snapshot{JobID: "j2", Workspace: "beta", Operation: "create", Phase: "generate", State: jobs.Running})
	w.Close()
	var p Payload
	if err := json.Unmarshal(<-bodies, &p); err != nil {
		t.Fatal(err)
	}
	if p.Event != EventJobLongRunning || p.Error != nil || p.State != jobs.Running || p.Console != "https://menagerie.example.test/create?job=j2" {
		t.Fatalf("%+v", p)
	}
}

// TestDeliveryFailuresAreLoggedWithoutTheURL covers every way a delivery
// fails: an error answer, a redirect (never followed), an unreachable
// host and a webhook slower than the time limit.
func TestDeliveryFailuresAreLoggedWithoutTheURL(t *testing.T) {
	followed := make(chan struct{}, 1)
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed <- struct{}{} }))
	defer elsewhere.Close()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/error"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasPrefix(r.URL.Path, "/redirect"):
			http.Redirect(w, r, elsewhere.URL+"/"+secret, http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/slow"):
			<-release
		}
	}))
	defer srv.Close()
	defer close(release)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unreachable := "http://" + closed.Addr().String()
	closed.Close()

	cases := map[string]struct{ target, want string }{
		"error":       {srv.URL + "/error/" + secret + "?auth=" + secret, "the webhook answered 500"},
		"redirect":    {srv.URL + "/redirect/" + secret, "the webhook answered 302"},
		"unreachable": {unreachable + "/" + secret + "?auth=" + secret, "connection refused"},
		"slow":        {srv.URL + "/slow/" + secret, "context deadline exceeded"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			logs := &logLines{}
			w := newWebhook(t, tc.target, logs, 200*time.Millisecond)
			w.JobFailed(failedSnapshot())
			w.Close()
			lines := logs.all()
			if len(lines) != 1 || !strings.Contains(lines[0], tc.want) || !strings.Contains(lines[0], "job-failed alert for job j1") {
				t.Fatalf("log %q", lines)
			}
			if strings.Contains(lines[0], secret) || strings.Contains(lines[0], srv.URL) {
				t.Fatalf("log names the webhook: %q", lines[0])
			}
		})
	}
	select {
	case <-followed:
		t.Fatal("redirect followed")
	default:
	}
}

func TestCloseEndsDelivery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("alert sent after Close")
	}))
	defer srv.Close()
	w := newWebhook(t, srv.URL, &logLines{}, 0)
	w.Close()
	w.JobFailed(failedSnapshot())
	w.Close()
}

func TestNewRefusesAnInvalidURLWithoutEchoingIt(t *testing.T) {
	for _, target := range []string{"", "ftp://hooks.example.test/" + secret, "https:///" + secret, "http://hooks.example.test/%zz" + secret} {
		_, err := New(Options{URL: target})
		if err == nil {
			t.Fatalf("%q accepted", target)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error echoes the URL: %v", err)
		}
	}
}

func TestConsoleURL(t *testing.T) {
	origin := "https://menagerie.example.test"
	for op, want := range map[string]string{
		"start":   origin + "/?job=id%2B1",
		"stop":    origin + "/?job=id%2B1",
		"rebuild": origin + "/?job=id%2B1",
		"create":  origin + "/create?job=id%2B1",
		"upgrade": origin + "/workspaces/a%20b/upgrade?job=id%2B1",
	} {
		if got := ConsoleURL(origin, jobs.Snapshot{JobID: "id+1", Workspace: "a b", Operation: op}); got != want {
			t.Errorf("%s: %s, want %s", op, got, want)
		}
	}
}
