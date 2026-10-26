// ---
// relationships:
//   implements: [menagerie, template-service-api]
//   references: job-without-timeout-blocks-workspace
// ---

package throwaway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// HookPath is the hook runner's one route.
const HookPath = "/v1/hooks/run"

// Hook runner error kinds.
const (
	KindHookTokenRefused  = "hook-token-refused"
	KindInvalidHook       = "invalid-hook"
	KindHookCancelled     = "hook-cancelled"
	KindRunnerUnavailable = "runner-image-unavailable"
	KindHookContainer     = "hook-container-failed"
	KindMalformedHook     = "malformed-request"
	KindHookNotFound      = "not-found"
)

// maxHookRequest bounds a hook request body: a program, its arguments and
// a few paths.
const maxHookRequest = 1 << 20

// HookJob is the job a hook grant belongs to.
type HookJob struct {
	// ID is the job id, a container label.
	ID string
	// Folder is the workspace folder the job's template writes.
	Folder string
	// Image returns the runner image; it is called for the job's first
	// hook only.
	Image func(ctx context.Context) (string, error)
	// Log returns a writer to the job console; prefix starts each line.
	Log func(prefix string) io.Writer
}

// HookRunner runs the template service's hooks in throwaway containers.
// It serves the hook runner socket: each request names a grant's token,
// and runs in the folder, image and console of that grant's job.
type HookRunner struct {
	containers Containers

	mu     sync.Mutex
	grants map[string]*HookGrant
}

// NewHookRunner returns a HookRunner that runs hooks with containers.
func NewHookRunner(containers Containers) *HookRunner {
	return &HookRunner{containers: containers, grants: map[string]*HookGrant{}}
}

// HookGrant lets the template service run hooks for one generate or apply
// request, until it is revoked.
type HookGrant struct {
	runner *HookRunner
	token  string
	job    HookJob
	ctx    context.Context
	cancel context.CancelFunc
	// running counts the hooks in flight. Add happens only while the
	// grant is registered, under runner.mu.
	running sync.WaitGroup

	mu       sync.Mutex
	image    string
	imageErr error
}

// Grant registers job and returns its grant. Hooks run with a context
// derived from ctx, the job's: cancelling it kills and removes them.
func (h *HookRunner) Grant(ctx context.Context, job HookJob) (*HookGrant, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("hook token: %w", err)
	}
	g := &HookGrant{runner: h, token: hex.EncodeToString(raw), job: job}
	g.ctx, g.cancel = context.WithCancel(ctx)
	h.mu.Lock()
	h.grants[g.token] = g
	h.mu.Unlock()
	return g, nil
}

// Token is the value the template service sends with each hook.
func (g *HookGrant) Token() string { return g.token }

// revokeWait bounds how long Revoke waits for running hooks to stop. A
// hook's container removal itself is bounded by cleanupTimeout.
var revokeWait = cleanupTimeout + 5*time.Second

// Revoke refuses the token from now on, kills and removes every hook
// container still running for it, and waits for them, at most revokeWait.
// It reports whether every hook stopped in time.
func (g *HookGrant) Revoke() bool {
	g.runner.mu.Lock()
	delete(g.runner.grants, g.token)
	g.runner.mu.Unlock()
	g.cancel()
	done := make(chan struct{})
	go func() {
		g.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(revokeWait):
		fmt.Fprintf(g.job.Log(""), "a hook container did not stop within %s; Menagerie removes it when it next starts\n", revokeWait)
		return false
	}
}

// ImageFailure is the error of a hook that found no runner image, or nil.
func (g *HookGrant) ImageFailure() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	var imageErr *ImageError
	if errors.As(g.imageErr, &imageErr) {
		return g.imageErr
	}
	return nil
}

func (g *HookGrant) recordImageFailure(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.imageErr == nil {
		g.imageErr = err
	}
}

// runnerImage resolves the job's runner image once.
func (g *HookGrant) runnerImage(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.image != "" {
		return g.image, nil
	}
	if g.job.Image == nil {
		return "", &ImageError{Err: errors.New("no runner image is configured")}
	}
	image, err := g.job.Image(ctx)
	if err != nil {
		var imageErr *ImageError
		if !errors.As(err, &imageErr) {
			err = &ImageError{Err: err}
		}
		return "", err
	}
	g.image = image
	return image, nil
}

// acquire returns the grant of token and counts one running hook on it.
func (h *HookRunner) acquire(token string) *HookGrant {
	h.mu.Lock()
	defer h.mu.Unlock()
	g, ok := h.grants[token]
	if !ok || token == "" {
		return nil
	}
	g.running.Add(1)
	return g
}

// hookRequest is one hook, as the template service sends it.
type hookRequest struct {
	Token        string      `json:"token"`
	Target       string      `json:"target"`
	Program      hookProgram `json:"program"`
	Cwd          *string     `json:"cwd"`
	TemplateRoot string      `json:"templateRoot"`
	Capture      struct {
		Stdout bool `json:"stdout"`
		Stderr bool `json:"stderr"`
	} `json:"capture"`
}

// hookProgram is exactly one of run, an argv, and script, a file of the
// template with its arguments.
type hookProgram struct {
	Run    []string `json:"run,omitempty"`
	Script *struct {
		Path string   `json:"path"`
		Args []string `json:"args"`
	} `json:"script,omitempty"`
}

// hookReply is a hook's outcome. A captured stream's raw bytes are
// base64 in JSON; an uncaptured stream is null.
type hookReply struct {
	ExitCode int     `json:"exitCode"`
	Success  bool    `json:"success"`
	Stdout   *[]byte `json:"stdout"`
	Stderr   *[]byte `json:"stderr"`
}

// hookError is a refused hook. Its message is a fixed sentence: never a
// path, an argument, a captured stream or an error's own text.
type hookError struct {
	status  int
	kind    string
	message string
}

func refuse(status int, kind, message string) *hookError {
	return &hookError{status: status, kind: kind, message: message}
}

// describe is the text of a failed run that may leave this package: a
// *RunError's, which is fixed per stage, else a fixed sentence per kind. A
// Containers other than *Docker can fail with any text.
func describe(err error) string {
	var runErr *RunError
	var imageErr *ImageError
	switch {
	case errors.As(err, &imageErr):
		return "no runner image is available for the hook"
	case errors.As(err, &runErr):
		return runErr.Error()
	}
	return "the hook's throwaway container failed"
}

// errHookDir refuses a working directory that is not a folder inside the
// workspace folder.
var errHookDir = refuse(http.StatusBadRequest, KindInvalidHook, "the hook's working directory is not a folder in the workspace folder")

// workingDir is the hook's working directory: cwd resolved inside folder
// with symbolic links followed, named under folder as the container mounts
// it. A cwd that is missing, not a folder, or resolves outside folder is
// refused before any container exists.
func workingDir(folder string, cwd *string) (string, *hookError) {
	rel := ""
	if cwd != nil {
		rel = *cwd
	}
	if rel != "" && !filepath.IsLocal(rel) {
		return "", refuse(http.StatusBadRequest, KindInvalidHook, "the hook's working directory must be a relative path inside the workspace folder")
	}
	root, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return "", errHookDir
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, rel))
	if err != nil {
		return "", errHookDir
	}
	inside, err := filepath.Rel(root, resolved)
	if err != nil || (inside != "." && !filepath.IsLocal(inside)) {
		return "", errHookDir
	}
	// The Engine would create a missing working directory as root; a
	// hook's working directory must exist, as it must for a process.
	if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
		return "", errHookDir
	}
	return filepath.Join(folder, inside), nil
}

// ServeHTTP runs one hook. No reply, error or log line carries a captured
// stream, the hook's environment or its arguments.
func (h *HookRunner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != HookPath {
		writeHookError(w, refuse(http.StatusNotFound, KindHookNotFound, "no such route"))
		return
	}
	if r.Method != http.MethodPost {
		writeHookError(w, refuse(http.StatusMethodNotAllowed, "method-not-allowed", "the route accepts POST only"))
		return
	}
	var req hookRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxHookRequest))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeHookError(w, refuse(http.StatusBadRequest, KindMalformedHook, "the hook request is not a hook document"))
		return
	}
	reply, herr := h.run(r.Context(), &req)
	if herr != nil {
		writeHookError(w, herr)
		return
	}
	body, _ := json.Marshal(reply)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (h *HookRunner) run(reqCtx context.Context, req *hookRequest) (*hookReply, *hookError) {
	g := h.acquire(req.Token)
	if g == nil {
		return nil, refuse(http.StatusForbidden, KindHookTokenRefused, "the hook token is unknown or revoked")
	}
	defer g.running.Done()
	if filepath.Clean(req.Target) != filepath.Clean(g.job.Folder) || !filepath.IsAbs(req.Target) {
		return nil, refuse(http.StatusForbidden, KindHookTokenRefused, "the hook's target is not the folder of the token's job")
	}
	argv, copyRoot, herr := program(req)
	if herr != nil {
		return nil, herr
	}
	dir, herr := workingDir(g.job.Folder, req.Cwd)
	if herr != nil {
		return nil, herr
	}
	// The hook stops when the template service hangs up or the grant is
	// revoked, which the job's cancellation does.
	ctx, cancel := context.WithCancel(reqCtx)
	defer cancel()
	stop := context.AfterFunc(g.ctx, cancel)
	defer stop()
	if g.ctx.Err() != nil {
		return nil, refuse(http.StatusConflict, KindHookCancelled, "the job was cancelled; the hook did not run")
	}
	image, err := g.runnerImage(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, refuse(http.StatusConflict, KindHookCancelled, "the job was cancelled; the hook did not run")
		}
		g.recordImageFailure(err)
		return nil, refuse(http.StatusServiceUnavailable, KindRunnerUnavailable, describe(err))
	}
	console := g.job.Log("hook: ")
	var stdout, stderr bytes.Buffer
	cmd := Command{Kind: KindHook, Job: g.job.ID, Image: image, Folder: g.job.Folder, Dir: dir, Argv: argv, Env: []string{}, Copy: copyRoot, Stdout: console, Stderr: console, Log: g.job.Log("")}
	if req.Capture.Stdout {
		cmd.Stdout = &stdout
	}
	if req.Capture.Stderr {
		cmd.Stderr = &stderr
	}
	fmt.Fprintf(g.job.Log(""), "running a template hook in a throwaway container of %s\n", image)
	code, err := h.containers.Run(ctx, cmd)
	if err != nil {
		var imageErr *ImageError
		switch {
		case ctx.Err() != nil:
			return nil, refuse(http.StatusConflict, KindHookCancelled, "the job was cancelled; the hook's container was removed")
		case errors.As(err, &imageErr):
			g.recordImageFailure(err)
			return nil, refuse(http.StatusServiceUnavailable, KindRunnerUnavailable, describe(err))
		}
		return nil, refuse(http.StatusInternalServerError, KindHookContainer, describe(err))
	}
	reply := &hookReply{ExitCode: code, Success: code == 0}
	// A captured stream is never nil, so an empty one is "" and not null.
	if req.Capture.Stdout {
		b := append([]byte{}, stdout.Bytes()...)
		reply.Stdout = &b
	}
	if req.Capture.Stderr {
		b := append([]byte{}, stderr.Bytes()...)
		reply.Stderr = &b
	}
	return reply, nil
}

// program checks the hook's program and returns its argv and, for a
// script, the template root to copy into the container.
func program(req *hookRequest) ([]string, string, *hookError) {
	p := req.Program
	switch {
	case p.Run != nil && p.Script == nil:
		if len(p.Run) == 0 || p.Run[0] == "" {
			return nil, "", refuse(http.StatusBadRequest, KindInvalidHook, "the hook's program is empty")
		}
		return p.Run, "", nil
	case p.Script != nil && p.Run == nil:
		root := req.TemplateRoot
		if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" {
			return nil, "", refuse(http.StatusBadRequest, KindInvalidHook, "the hook's template root must be an absolute clean folder")
		}
		rel, err := filepath.Rel(root, p.Script.Path)
		if err != nil || !filepath.IsAbs(p.Script.Path) || !filepath.IsLocal(rel) {
			return nil, "", refuse(http.StatusBadRequest, KindInvalidHook, "the hook's script must be inside its template root")
		}
		if info, err := os.Lstat(root); err != nil || !info.IsDir() {
			return nil, "", refuse(http.StatusBadRequest, KindInvalidHook, "the hook's template root is not a folder")
		}
		return append([]string{p.Script.Path}, p.Script.Args...), root, nil
	}
	return nil, "", refuse(http.StatusBadRequest, KindInvalidHook, "the hook's program must be exactly one of run and script")
}

func writeHookError(w http.ResponseWriter, e *hookError) {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{"kind": e.kind, "message": e.message}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_, _ = w.Write(body)
}

// ListenHookRunner listens on the hook runner socket at path, open to this
// user only. A socket left by an ended process is replaced; a live socket
// or any other file is refused.
func ListenHookRunner(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.Dial("unix", path); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: another process is listening on this socket", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}
