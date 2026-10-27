// ---
// relationships:
//   implements: menagerie
//   references: [workspace, menagerie-has-host-root, proxy-ignores-unknown-config-keys]
// ---

// Package dockerproxy makes the Docker clients Menagerie talks to the
// Docker proxy with. A workspace job's client names the job's workspace in
// a request header, which the proxy checks every create, start and exec
// against; the header comes from the workspace root and the workspace's
// name, never from the workspace's definition. Every client keeps the
// reason the proxy gives for a denial, which the Engine API error would
// otherwise drop.
package dockerproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	dockerclient "github.com/docker/docker/client"
)

// The headers that name a request's workspace: HeaderWorkspace on a
// devcontainer operation's requests, HeaderThrowawayWorkspace on a
// throwaway command container's.
const (
	HeaderWorkspace          = "X-Sockguard-Workspace"
	HeaderThrowawayWorkspace = "X-Sockguard-Throwaway-Workspace"
)

// proxyMessage is the message of the proxy's denial body.
const proxyMessage = "request denied by sockguard policy"

// marker starts the message a denial carries through the Engine API
// error; the quoted reason follows it, so wrapping text cannot join it.
const marker = "the Docker proxy denied the request: "

// noReason stands for the reason of a denial whose body has none.
const noReason = "the proxy gave no reason; its response.deny_verbosity is minimal"

// maxDenial bounds the denial body read; a denial is a short JSON object.
const maxDenial = 64 << 10

// NewClient returns a client for the Engine named by the environment, as
// dockerclient.FromEnv reads it, that keeps the proxy's denial reasons.
// headers, when given, go with every request, upgraded ones included.
func NewClient(headers map[string]string) (*dockerclient.Client, error) {
	opts := []dockerclient.Opt{dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation(), withDenialReasons()}
	if len(headers) > 0 {
		opts = append(opts, dockerclient.WithHTTPHeaders(headers))
	}
	return dockerclient.NewClientWithOpts(opts...)
}

// Workspace is the identity of the workspace folder: the workspace root
// with its symbolic links resolved, joined with the folder's name. The
// proxy compares it with bind sources and labels, which it resolves the
// same way.
func Workspace(folder string) (string, error) {
	root, err := filepath.EvalSymlinks(filepath.Dir(folder))
	if err != nil {
		return "", errors.New("the workspace root cannot be resolved")
	}
	return filepath.Join(root, filepath.Base(folder)), nil
}

// withDenialReasons wraps the client's transport, as the options before it
// configured it, with denials.
func withDenialReasons() dockerclient.Opt {
	return func(c *dockerclient.Client) error {
		hc := c.HTTPClient()
		hc.Transport = &denials{base: hc.Transport}
		return dockerclient.WithHTTPClient(hc)(c)
	}
}

// denials rewrites the proxy's denial body so the Engine API error the
// client makes of it carries the reason. Every other response passes
// unchanged.
type denials struct{ base http.RoundTripper }

func (d *denials) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusForbidden || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return resp, err
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxDenial+1))
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	body := raw
	var denial struct {
		Message string `json:"message"`
		Reason  string `json:"reason"`
	}
	if len(raw) <= maxDenial && json.Unmarshal(raw, &denial) == nil && denial.Message == proxyMessage {
		reason := oneLine(denial.Reason)
		if reason == "" {
			reason = noReason
		}
		body, _ = json.Marshal(map[string]string{"message": marker + strconv.Quote(reason)})
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Length")
	return resp, nil
}

// oneLine keeps a reason on one line, so it cannot add lines to a job
// console or error.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Denied is a request the proxy denied.
type Denied struct {
	// Reason is the proxy's reason.
	Reason string
}

func (d *Denied) Error() string { return marker + strconv.Quote(d.Reason) }

// Denial returns the proxy's denial in err's chain, or nil when the proxy
// denied nothing. It reads only the denial's reason, never the rest of
// err's text.
func Denial(err error) *Denied {
	var d *Denied
	if errors.As(err, &d) {
		return d
	}
	if err == nil || !cerrdefs.IsPermissionDenied(err) {
		return nil
	}
	msg := err.Error()
	i := strings.Index(msg, marker)
	if i < 0 {
		return nil
	}
	quoted, err := strconv.QuotedPrefix(msg[i+len(marker):])
	if err != nil {
		return nil
	}
	reason, err := strconv.Unquote(quoted)
	if err != nil {
		return nil
	}
	return &Denied{Reason: reason}
}

// Describe is a job error sentence for a denial of an operation.
func Describe(d *Denied) error {
	return fmt.Errorf("the Docker proxy denied a request of this operation: %s", d.Reason)
}
