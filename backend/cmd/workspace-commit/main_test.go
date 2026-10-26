package main

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/workspacesocket"
)

// serve answers every request on a new socket with status and body, and
// records the messages it receives.
func serve(t *testing.T, status int, body string, header map[string]string) (string, *[]string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "wc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "workspace.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Message string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if r.Method != http.MethodPost || r.URL.Path != workspacesocket.CommitsPath {
			http.Error(w, "unexpected route", http.StatusTeapot)
			return
		}
		messages = append(messages, req.Message)
		for k, v := range header {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return socket, &messages
}

func TestOutcomesAndExitStatuses(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	const hash = "89abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name   string
		status int
		body   string
		header map[string]string
		code   int
		out    []string
	}{
		{"committed", 200, `{"jobId":"` + id + `","state":"succeeded","commit":"` + hash + `","changed":true,"pushed":true,"error":null}`, nil, 0, []string{"job " + id, "committed " + hash, "backed up: pushed"}},
		{"unchanged", 200, `{"jobId":"` + id + `","state":"succeeded","commit":"` + hash + `","changed":false,"pushed":false,"error":null}`, nil, 0, []string{"nothing to commit; the branch is at " + hash, "already up to date"}},
		{"backup failed", 200, `{"jobId":"` + id + `","state":"failed","commit":"` + hash + `","changed":true,"pushed":false,"error":{"kind":"backup-failed"}}`, nil, 1, []string{"committed " + hash, "failed (backup-failed)"}},
		{"commit failed", 200, `{"jobId":"` + id + `","state":"failed","commit":null,"changed":false,"pushed":false,"error":{"kind":"commit-failed"}}`, nil, 1, []string{"failed (commit-failed)"}},
		{"busy", 409, `{"error":{"kind":"workspace-busy","activeJobId":"` + id + `"}}`, nil, 3, []string{"busy with job " + id}},
		{"rate limited", 429, `{"error":{"kind":"rate-limited","retryAfterSeconds":42}}`, map[string]string{"Retry-After": "42"}, 3, []string{"retry in 42 seconds"}},
		{"shutting down", 503, `{"error":{"kind":"shutting-down","jobId":"` + id + `"}}`, nil, 3, []string{"shutting down before job " + id + " ended; retry later"}},
		{"shutting down before a job", 503, `{"error":{"kind":"shutting-down"}}`, nil, 3, []string{"shutting down; retry later"}},
		{"refused", 400, `{"error":{"kind":"invalid-message"}}`, nil, 2, []string{"refused the request (invalid-message)"}},
		{"gone", 404, `{"error":{"kind":"workspace-not-found"}}`, nil, 1, []string{"answered 404 (workspace-not-found)"}},
		{"not a result", 200, `{}`, nil, 1, []string{"not a commit result"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			socket, messages := serve(t, tc.status, tc.body, tc.header)
			var stdout, stderr bytes.Buffer
			code := run([]string{"-m", "docs: sample"}, socket, &stdout, &stderr)
			all := stdout.String() + stderr.String()
			if code != tc.code {
				t.Fatalf("exit %d, want %d: %s", code, tc.code, all)
			}
			for _, want := range tc.out {
				if !strings.Contains(all, want) {
					t.Errorf("output %q lacks %q", all, want)
				}
			}
			if len(*messages) != 1 || (*messages)[0] != "docs: sample" {
				t.Errorf("messages %q", *messages)
			}
		})
	}
}

func TestUsageAndMessagesAreCheckedBeforeConnecting(t *testing.T) {
	socket, messages := serve(t, 200, `{}`, nil)
	for _, args := range [][]string{
		{}, {"a", "b"}, {"-m", "a", "b"}, {"-x"}, {""}, {"  "}, {"a\x00b"}, {"\xff"}, {strings.Repeat("a", maxMessage+1)},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, socket, &stdout, &stderr); code != 2 {
			t.Errorf("%q: exit %d %s", args, code, stderr.String())
		}
	}
	if len(*messages) != 0 {
		t.Fatalf("sent %q", *messages)
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"plain message"}, socket, &stdout, &stderr); code != 1 || len(*messages) != 1 || (*messages)[0] != "plain message" {
		t.Fatalf("positional message: %d %q", code, *messages)
	}
}

func TestAnUnreachableSocketIsStatusFour(t *testing.T) {
	var stdout, stderr bytes.Buffer
	missing := filepath.Join(t.TempDir(), "missing.sock")
	if code := run([]string{"x"}, missing, &stdout, &stderr); code != 4 || !strings.Contains(stderr.String(), missing) {
		t.Fatalf("exit %d %s", code, stderr.String())
	}
}

// TestTheClientAgreesWithTheSocket checks that the client's rules and
// defaults are the socket's.
func TestTheClientAgreesWithTheSocket(t *testing.T) {
	if defaultSocket != devcontainer.WorkspaceSocketPath || socketEnv != devcontainer.WorkspaceSocketEnv || maxMessage != workspacesocket.MaxMessage {
		t.Fatalf("client %s %s %d", defaultSocket, socketEnv, maxMessage)
	}
	for _, m := range []string{"", " ", "ok", "a\x00", "\xff", "é", strings.Repeat("a", maxMessage), strings.Repeat("a", maxMessage+1), strings.Repeat("é", maxMessage/2) + "a"} {
		if validMessage(m) != workspacesocket.ValidMessage(m) {
			t.Errorf("%s: client %t socket %t", strconv.Quote(m[:min(len(m), 8)]), validMessage(m), workspacesocket.ValidMessage(m))
		}
	}
}

// TestTheClientUsesTheStandardLibraryOnly keeps the client one static
// executable with no module dependency.
func TestTheClientUsesTheStandardLibraryOnly(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
			t.Errorf("imports %s", path)
		}
	}
}
