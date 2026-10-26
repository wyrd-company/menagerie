// ---
// relationships:
//   implements: menagerie
//   references: [workspace-api, workspace]
// ---

// Command workspace-commit asks Menagerie, from inside a workspace's
// devcontainer, to commit every change of the workspace and back it up:
//
//	workspace-commit [-m] <message>
//
// It talks to the workspace's socket, named by MENAGERIE_WORKSPACE_SOCKET
// and by default /run/menagerie/workspace.sock, and waits for the commit
// job to end. It prints the job id, the commit and whether the backup
// pushed it. Exit status: 0 the job succeeded; 1 it failed (the error kind
// is printed; the detail is in the job's console in Menagerie); 2 usage or
// an invalid message; 3 the workspace is busy, committed too recently
// (when to retry is printed), or Menagerie is shutting down; 4 the socket
// cannot be reached.
//
// It uses the standard library only, so it is one static executable that
// runs in any devcontainer.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"
)

// Exit statuses.
const (
	exitOK          = 0
	exitJobFailed   = 1
	exitUsage       = 2
	exitRetry       = 3
	exitUnreachable = 4
)

const (
	socketEnv     = "MENAGERIE_WORKSPACE_SOCKET"
	defaultSocket = "/run/menagerie/workspace.sock"
	// maxMessage is the socket's limit on a message, in bytes.
	maxMessage = 4096
)

func main() {
	os.Exit(run(os.Args[1:], os.Getenv(socketEnv), os.Stdout, os.Stderr))
}

type result struct {
	JobID   string  `json:"jobId"`
	State   string  `json:"state"`
	Commit  *string `json:"commit"`
	Changed bool    `json:"changed"`
	Pushed  bool    `json:"pushed"`
	Error   *struct {
		Kind string `json:"kind"`
	} `json:"error"`
}

type refusal struct {
	Error struct {
		Kind              string `json:"kind"`
		ActiveJobID       string `json:"activeJobId"`
		RetryAfterSeconds int    `json:"retryAfterSeconds"`
		JobID             string `json:"jobId"`
	} `json:"error"`
}

func run(args []string, socket string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("workspace-commit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	message := flags.String("m", "", "the commit message")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: workspace-commit [-m] <message>")
		fmt.Fprintln(stderr, "Commits every change of this workspace and backs it up through Menagerie.")
	}
	if err := flags.Parse(args); err != nil {
		return exitUsage
	}
	switch {
	case *message == "" && flags.NArg() == 1:
		*message = flags.Arg(0)
	case *message != "" && flags.NArg() == 0:
	default:
		flags.Usage()
		return exitUsage
	}
	if !validMessage(*message) {
		fmt.Fprintf(stderr, "workspace-commit: the message must not be blank, must be UTF-8 of at most %d bytes, and must not hold a NUL\n", maxMessage)
		return exitUsage
	}
	if socket == "" {
		socket = defaultSocket
	}
	body, _ := json.Marshal(map[string]string{"message": *message})
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}}
	res, err := client.Post("http://workspace/v1/commits", "application/json", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(stderr, "workspace-commit: cannot reach Menagerie at %s; is the workspace socket mounted? (%v)\n", socket, unwrap(err))
		return exitUnreachable
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		fmt.Fprintf(stderr, "workspace-commit: the reply was cut off: %v\n", err)
		return exitUnreachable
	}
	if res.StatusCode == http.StatusOK {
		return report(data, stdout, stderr)
	}
	var r refusal
	_ = json.Unmarshal(data, &r)
	switch kind := r.Error.Kind; {
	case res.StatusCode == http.StatusConflict:
		fmt.Fprintf(stderr, "workspace-commit: the workspace is busy with job %s; retry when it has ended\n", r.Error.ActiveJobID)
		return exitRetry
	case res.StatusCode == http.StatusTooManyRequests:
		retry := res.Header.Get("Retry-After")
		if retry == "" {
			retry = fmt.Sprint(r.Error.RetryAfterSeconds)
		}
		fmt.Fprintf(stderr, "workspace-commit: the workspace committed too recently; retry in %s seconds\n", retry)
		return exitRetry
	case res.StatusCode == http.StatusServiceUnavailable:
		if r.Error.JobID != "" {
			fmt.Fprintf(stderr, "workspace-commit: Menagerie is shutting down before job %s ended; retry later\n", r.Error.JobID)
		} else {
			fmt.Fprintln(stderr, "workspace-commit: Menagerie is shutting down; retry later")
		}
		return exitRetry
	case res.StatusCode == http.StatusBadRequest:
		fmt.Fprintf(stderr, "workspace-commit: Menagerie refused the request (%s)\n", orUnknown(kind))
		return exitUsage
	default:
		fmt.Fprintf(stderr, "workspace-commit: Menagerie answered %d (%s)\n", res.StatusCode, orUnknown(kind))
		return exitJobFailed
	}
}

// report prints an ended job's outcome.
func report(data []byte, stdout, stderr io.Writer) int {
	var r result
	if err := json.Unmarshal(data, &r); err != nil || r.JobID == "" {
		fmt.Fprintln(stderr, "workspace-commit: Menagerie's reply is not a commit result")
		return exitJobFailed
	}
	fmt.Fprintf(stdout, "job %s\n", r.JobID)
	switch {
	case r.Commit != nil && r.Changed:
		fmt.Fprintf(stdout, "committed %s\n", *r.Commit)
	case r.Commit != nil:
		fmt.Fprintf(stdout, "nothing to commit; the branch is at %s\n", *r.Commit)
	}
	if r.State == "succeeded" {
		if r.Pushed {
			fmt.Fprintln(stdout, "backed up: pushed")
		} else {
			fmt.Fprintln(stdout, "backed up: already up to date")
		}
		return exitOK
	}
	kind := "unknown"
	if r.Error != nil {
		kind = r.Error.Kind
	}
	fmt.Fprintf(stderr, "workspace-commit: job %s %s (%s); its console in Menagerie has the detail\n", r.JobID, r.State, kind)
	return exitJobFailed
}

// validMessage holds the socket's rules for a message.
func validMessage(m string) bool {
	return strings.TrimSpace(m) != "" && len(m) <= maxMessage && utf8.ValidString(m) && !strings.ContainsRune(m, 0)
}

func orUnknown(kind string) string {
	if kind == "" {
		return "no kind"
	}
	return kind
}

// unwrap drops the URL the HTTP client adds, which names no real host.
func unwrap(err error) error {
	var u interface{ Unwrap() error }
	if errors.As(err, &u) {
		if inner := u.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}
