// ---
// relationships:
//   implements: menagerie
//   references: [workspace, provisioning-declaration]
// ---

package devcontainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	dockeropts "github.com/docker/cli/opts"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
	"github.com/wyrd-company/devcontainer-builder/pkg/host/dockercompose"
	"github.com/wyrd-company/devcontainer-builder/pkg/metadata"
	"github.com/wyrd-company/devcontainer-builder/pkg/spec"
	"github.com/wyrd-company/devcontainer-builder/pkg/substitute"
)

// HostPrerequisitesError names everything a definition needs from the host
// that the host does not have.
type HostPrerequisitesError struct {
	// BindSources are the missing bind sources outside the workspace folder.
	BindSources []string
	// Networks are the missing networks that runArgs names.
	Networks []string
}

func (e *HostPrerequisitesError) Error() string {
	var parts []string
	if len(e.BindSources) > 0 {
		parts = append(parts, "bind sources "+strings.Join(e.BindSources, ", "))
	}
	if len(e.Networks) > 0 {
		parts = append(parts, "networks "+strings.Join(e.Networks, ", "))
	}
	return "the host is missing what this workspace needs: " + strings.Join(parts, "; ")
}

// CheckHostPrerequisites reports in one message, as a
// *HostPrerequisitesError, every bind source outside the workspace folder
// and every network of runArgs that the host lacks. Docker decides: the
// bind sources are host paths, which Menagerie's own filesystem does not
// show. It changes nothing on the host. A definition that cannot be read
// has nothing to check, and the start that follows reports it.
func (r *Runtime) CheckHostPrerequisites(ctx context.Context, folder string, p Progress) error {
	p.Phase("host-prerequisites")
	dc, err := spec.LoadFromWorkspace(folder)
	if err != nil {
		return nil
	}
	env := newLocalEnvironment(r.opts.Environ(), r.opts.LocalEnv).values
	sub := SubstitutionContext(dc, folder, env)
	missing := &HostPrerequisitesError{}
	consumer := consumerFolder(dc, sub)
	for _, source := range bindSources(ctx, dc, folder, env) {
		if _, inside := insideFolder(source, folder); inside || (consumer != "" && filepath.Base(source) == consumer) {
			continue
		}
		found, err := r.bindSourceExists(ctx, source, p)
		if err != nil {
			return err
		}
		if !found {
			missing.BindSources = append(missing.BindSources, source)
		}
	}
	for _, name := range runNetworks(dc, sub) {
		switch _, err := r.client.NetworkInspect(ctx, name, network.InspectOptions{}); {
		case dockerclient.IsErrNotFound(err):
			missing.Networks = append(missing.Networks, name)
		case err != nil:
			return fmt.Errorf("read network %s: %w", name, err)
		}
	}
	if len(missing.BindSources) == 0 && len(missing.Networks) == 0 {
		return nil
	}
	fmt.Fprintln(p.Log(""), missing.Error())
	return missing
}

func (r *Runtime) bindSourceExists(ctx context.Context, source string, p Progress) (bool, error) {
	if r.opts.BindSources == nil || r.opts.RunnerImage == nil {
		return false, errors.New("no container runner is configured to check bind sources")
	}
	image, err := r.opts.RunnerImage(ctx)
	if err != nil {
		return false, err
	}
	return r.opts.BindSources.BindSourceExists(ctx, jobID(p), image, source, p.Log(""))
}

// createBindSources creates each missing bind source folder inside the
// workspace folder, so that a clone of a backup, which has no folder that
// holds no tracked file, starts. The services run as the workspace root's
// owner, so the new folders are theirs. A source that a symbolic link takes
// out of the workspace folder, and one that cannot be read, is not touched:
// Docker resolves it on the host and decides.
func createBindSources(ctx context.Context, dc *spec.DevContainer, folder string, env map[string]string, p Progress) error {
	var rels []string
	for _, source := range bindSources(ctx, dc, folder, env) {
		if rel, inside := insideFolder(source, folder); inside {
			rels = append(rels, rel)
		}
	}
	if len(rels) == 0 {
		return nil
	}
	root, err := os.OpenRoot(folder)
	if err != nil {
		return fmt.Errorf("open workspace folder: %w", err)
	}
	defer root.Close()
	for _, rel := range rels {
		resolved, stays := resolveInside(folder, rel)
		if !stays {
			fmt.Fprintf(p.Log(""), "bind source %s was not checked: it leaves the workspace folder or cannot be read\n", rel)
			continue
		}
		rel = resolved
		_, err := root.Stat(rel)
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist):
			if err := root.MkdirAll(rel, 0o755); err != nil {
				return fmt.Errorf("create bind source folder %s: %w", rel, err)
			}
			fmt.Fprintf(p.Log(""), "created bind source folder %s\n", rel)
		default:
			fmt.Fprintf(p.Log(""), "bind source %s was not checked: %v\n", rel, err)
		}
	}
	return nil
}

// resolveInside returns rel beneath folder with the symbolic links of its
// existing part resolved, or false when they lead out of folder or a part
// cannot be read. os.Root
// refuses every absolute link, also one that stays inside, and it still
// guards the writes this path leads to.
func resolveInside(folder, rel string) (string, bool) {
	real, err := filepath.EvalSymlinks(folder)
	if err != nil {
		return "", false
	}
	existing, rest := filepath.Join(folder, rel), ""
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			if !within(resolved, real) {
				return "", false
			}
			inside, err := filepath.Rel(real, resolved)
			return filepath.Join(inside, rest), err == nil
		}
		if !errors.Is(err, os.ErrNotExist) || existing == folder {
			return "", false
		}
		existing, rest = filepath.Dir(existing), filepath.Join(filepath.Base(existing), rest)
	}
}

// bindSources lists, once each, the absolute bind sources the definition
// file names: its workspaceMount, its mounts after
// devcontainer-builder's rule that the last mount of a target wins and,
// for Compose, the volumes of its services. Mounts that features or image
// metadata add are not known before the build. A Compose project that
// does not load has no volumes here, and the start reports it.
func bindSources(ctx context.Context, dc *spec.DevContainer, folder string, env map[string]string) []string {
	sub := SubstitutionContext(dc, folder, env)
	var sources []string
	add := func(source string) {
		if filepath.IsAbs(source) && !slices.Contains(sources, source) {
			sources = append(sources, source)
		}
	}
	if dc.Mode() == spec.ModeSingle && dc.WorkspaceMount != "" {
		add(mountStringSource(substitute.Substitute(sub, dc.WorkspaceMount)))
	}
	entries, err := metadata.Substitute(metadata.Collect(nil, nil, dc), func(s string) string { return substitute.Substitute(sub, s) })
	if err != nil {
		return sources
	}
	for _, raw := range metadata.Merge(dc, entries).DevContainer.Mounts {
		add(mountSource(raw))
	}
	if dc.Mode() == spec.ModeCompose {
		project, err := dockercompose.LoadProject(ctx, dockercompose.LoadInputs{Spec: dc, Workspace: folder, LocalEnv: env})
		if err != nil {
			return sources
		}
		for _, service := range project.Services {
			for _, volume := range service.Volumes {
				if volume.Type == "bind" {
					add(volume.Source)
				}
			}
		}
	}
	return sources
}

// mountSource is the bind source of one mounts entry, or "". An object is
// read as the string devcontainer-builder serializes it to.
func mountSource(raw any) string {
	switch v := raw.(type) {
	case string:
		return mountStringSource(v)
	case map[string]any:
		kind, _ := v["type"].(string)
		source, _ := v["source"].(string)
		target, _ := v["target"].(string)
		return mountStringSource("type=" + kind + ",src=" + source + ",dst=" + target)
	}
	return ""
}

// mountStringSource reads a mount string with the Docker CLI's parser, as
// devcontainer-builder does: a mount without a type is a volume, and a
// string that parser refuses is no mount here, and the start reports it.
func mountStringSource(s string) string {
	var parsed dockeropts.MountOpt
	if err := parsed.Set(s); err != nil {
		return ""
	}
	m := parsed.Value()[0]
	if m.Type != "bind" || m.Target == "" {
		return ""
	}
	return m.Source
}

// insideFolder returns source relative to folder, when source is folder or
// inside it. A source with a `..` component is not inside: the kernel
// applies `..` after any link before it, so only Docker can tell where it
// leads.
func insideFolder(source, folder string) (string, bool) {
	if slices.Contains(strings.Split(source, string(filepath.Separator)), "..") {
		return "", false
	}
	source = filepath.Clean(source)
	if !within(source, folder) {
		return "", false
	}
	rel, err := filepath.Rel(folder, source)
	return rel, err == nil
}

// consumerFolder is the final folder of a provisioning declaration's
// consumer directory: the container name, which the Wyrwood agent creates
// a directory for. Without a declaration there is none.
func consumerFolder(dc *spec.DevContainer, sub substitute.Context) string {
	if _, declared := dc.Customizations["menagerie"]; !declared {
		return ""
	}
	return runArgValues(dc, sub).name
}

// runNetworks are the user-defined networks that runArgs names, once each.
// Docker creates or owns every other network mode.
func runNetworks(dc *spec.DevContainer, sub substitute.Context) []string {
	var names []string
	for _, name := range runArgValues(dc, sub).networks {
		if container.NetworkMode(name).IsUserDefined() && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	return names
}

type runArgs struct {
	name     string
	networks []string
}

// runArgValues reads the container name and networks of runArgs as
// devcontainer-builder does: the last name counts, each network flag is an
// attachment in the Docker CLI's grammar, and runArgs do not apply to
// Compose. It stops at what devcontainer-builder refuses, which the start
// then reports.
func runArgValues(dc *spec.DevContainer, sub substitute.Context) runArgs {
	var out runArgs
	if dc.Mode() != spec.ModeSingle {
		return out
	}
	args := substitute.Apply(sub, dc.RunArgs)
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "" {
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "--" {
			return out
		}
		// A cluster of short flags gives up its first letter, and a short
		// flag that takes a value takes what is attached to it.
		if len(arg) > 2 && arg[1] != '-' && arg[2] != '=' {
			switch {
			case strings.ContainsRune("ditP", rune(arg[1])):
				args = slices.Insert(args, i+1, "-"+arg[2:])
				arg = arg[:2]
			case strings.ContainsRune("ahuwelpvmc", rune(arg[1])):
				arg = arg[:2] + "=" + arg[2:]
			}
		}
		flag, value, hasValue := strings.Cut(arg, "=")
		if !hasValue {
			if flagTakesNoValue(flag) {
				continue
			}
			if i++; i >= len(args) {
				return out
			}
			value = args[i]
		}
		switch flag {
		case "--name":
			out.name = value
		case "--network", "--net":
			var parsed dockeropts.NetworkOpt
			if parsed.Set(value) != nil {
				return out
			}
			out.networks = append(out.networks, parsed.Value()[0].Target)
		}
	}
	return out
}

// flagTakesNoValue is true for the runArgs flags that devcontainer-builder
// reads without a value.
func flagTakesNoValue(flag string) bool {
	switch flag {
	case "--init", "--privileged", "--read-only", "-P", "--publish-all",
		"-d", "--detach", "-i", "--interactive", "-t", "--tty", "--rm", "--sig-proxy",
		"--oom-kill-disable", "--no-healthcheck":
		return true
	}
	return false
}

// Definition is what provisioning reads from a workspace's definition file.
type Definition struct {
	// Block is the customizations.menagerie block, or nil when the definition
	// has none. It comes only from the definition file, never from the
	// metadata of an image or a feature.
	Block json.RawMessage
	// Compose says that the definition uses Compose.
	Compose bool
	// ContainerName is the --name value of runArgs, read as
	// devcontainer-builder reads it, or "" when there is none.
	ContainerName string
}

// Definition reads the workspace's definition file. An error means that the
// file cannot be read, which the start that follows reports.
func (r *Runtime) Definition(folder string) (Definition, error) {
	dc, err := spec.LoadFromWorkspace(folder)
	if err != nil {
		return Definition{}, err
	}
	out := Definition{Compose: dc.Mode() == spec.ModeCompose}
	if block, declared := dc.Customizations["menagerie"]; declared {
		if out.Block, err = json.Marshal(block); err != nil {
			return Definition{}, fmt.Errorf("read customizations.menagerie: %w", err)
		}
	}
	env := newLocalEnvironment(r.opts.Environ(), r.opts.LocalEnv).values
	out.ContainerName = runArgValues(dc, SubstitutionContext(dc, folder, env)).name
	return out, nil
}

// ContainerHolder returns the workspace folder label of the container called
// name, and whether such a container exists. A container with no label gives
// "" and true.
func (r *Runtime) ContainerHolder(ctx context.Context, name string) (string, bool, error) {
	info, err := r.client.ContainerInspect(ctx, name)
	if dockerclient.IsErrNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect container %s: %w", name, err)
	}
	// An inspect also matches an id, and a container's name can be another's id
	// only by chance; only the name counts here.
	if info.Name != "/"+name {
		return "", false, nil
	}
	if info.Config == nil {
		return "", true, nil
	}
	return info.Config.Labels[labelLocalFolder], true, nil
}
