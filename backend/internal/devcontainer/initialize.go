// ---
// relationships:
//   implements: menagerie
// ---

package devcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/wyrd-company/devcontainer-builder/pkg/spec"
	"github.com/wyrd-company/devcontainer-builder/pkg/substitute"
)

// InitializeCommand reads initializeCommand from the definition snapshot.
// devcontainer-builder has no typed field for it; the value is decoded from
// dc.Raw, the same snapshot the build and Up use. A missing or null value
// is the zero Command.
func InitializeCommand(dc *spec.DevContainer) (spec.Command, error) {
	var doc struct {
		InitializeCommand json.RawMessage `json:"initializeCommand"`
	}
	if err := json.Unmarshal(dc.Raw, &doc); err != nil {
		return spec.Command{}, fmt.Errorf("read initializeCommand: %w", err)
	}
	var cmd spec.Command
	if len(doc.InitializeCommand) > 0 {
		if err := cmd.UnmarshalJSON(doc.InitializeCommand); err != nil {
			return spec.Command{}, fmt.Errorf("initializeCommand: %w", err)
		}
	}
	return cmd, nil
}

// SubstitutionContext is the pre-container substitution context for a
// workspace, built as devcontainer-builder's Up builds it: the local
// workspace, the configured or default container workspace folder, the
// local environment and the workspace's discovery labels, so that
// ${devcontainerId} agrees with the container Up creates.
func SubstitutionContext(dc *spec.DevContainer, folder string, env map[string]string) substitute.Context {
	labels := map[string]string{labelLocalFolder: folder}
	if dc.Path != "" {
		labels[labelConfigFile] = dc.Path
	}
	return substitute.Context{
		LocalWorkspaceFolder:     folder,
		ContainerWorkspaceFolder: containerWorkspace(dc, folder, env),
		LocalEnv:                 env,
		IDLabels:                 labels,
	}
}

// containerWorkspace is the configured workspaceFolder, else "/" for
// Compose and /workspaces/<basename> for a single container.
func containerWorkspace(dc *spec.DevContainer, folder string, env map[string]string) string {
	value := dc.WorkspaceFolder
	switch {
	case value != "":
	case dc.Mode() == spec.ModeCompose:
		value = "/"
	default:
		value = "/workspaces/" + filepath.Base(folder)
	}
	return substitute.Substitute(substitute.Context{LocalWorkspaceFolder: folder, LocalEnv: env}, value)
}

// SubstituteCommand resolves variables in every command string, keeping
// the command's form and parallel member names.
func SubstituteCommand(ctx substitute.Context, cmd spec.Command) spec.Command {
	out := spec.Command{Shell: substitute.Substitute(ctx, cmd.Shell)}
	if cmd.Args != nil {
		out.Args = substitute.Apply(ctx, cmd.Args)
	}
	if cmd.Parallel != nil {
		out.Parallel = make(map[string]spec.Command, len(cmd.Parallel))
		for name, member := range cmd.Parallel {
			out.Parallel[name] = SubstituteCommand(ctx, member)
		}
	}
	return out
}

// CommandRunner executes a resolved command in Menagerie's own container.
type CommandRunner struct {
	// Dir is the working directory: the workspace folder.
	Dir string
	// Env is the process environment of every command.
	Env []string
	// Output receives each command's output. Parallel members get a prefixed
	// writer of their own.
	Output func(prefix string) io.Writer
}

// Run executes cmd: a shell string through /bin/sh -c, an argv array
// directly, and the members of an object in parallel. It waits for every
// parallel member and fails if any member fails.
func (r CommandRunner) Run(ctx context.Context, cmd spec.Command) error {
	return r.run(ctx, cmd, "")
}

func (r CommandRunner) run(ctx context.Context, cmd spec.Command, prefix string) error {
	switch {
	case cmd.Shell != "":
		return r.exec(ctx, prefix, "/bin/sh", "-c", cmd.Shell)
	case len(cmd.Args) > 0:
		return r.exec(ctx, prefix, cmd.Args[0], cmd.Args[1:]...)
	case len(cmd.Parallel) > 0:
		names := make([]string, 0, len(cmd.Parallel))
		for name := range cmd.Parallel {
			names = append(names, name)
		}
		sort.Strings(names)
		errs := make([]error, len(names))
		var wg sync.WaitGroup
		for i, name := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := r.run(ctx, cmd.Parallel[name], prefix+"["+name+"] "); err != nil {
					errs[i] = fmt.Errorf("%s: %w", name, err)
				}
			}()
		}
		wg.Wait()
		return errors.Join(errs...)
	}
	return nil
}

// commandWaitDelay bounds how long a cancelled command's Wait waits for
// its output pipes after the process is killed, so a child process that
// holds them cannot keep a cancelled job running.
var commandWaitDelay = 5 * time.Second

func (r CommandRunner) exec(ctx context.Context, prefix, name string, args ...string) error {
	c := exec.CommandContext(ctx, name, args...)
	c.WaitDelay = commandWaitDelay
	c.Dir = r.Dir
	c.Env = r.Env
	c.Stdout, c.Stderr = io.Discard, io.Discard
	if r.Output != nil {
		out := r.Output(prefix)
		c.Stdout, c.Stderr = out, out
	}
	return c.Run()
}
