package provisioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// previous is a host of the previous release: a workspace root with real
// folders, the retirement records it wrote, and the start log.
type previous struct {
	*host
	root    *workspace.Root
	records Records
	mu      sync.Mutex
	log     []string
}

func newPrevious(t *testing.T, workspaces ...string) *previous {
	t.Helper()
	dir := t.TempDir()
	root, err := workspace.NewRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := &previous{host: newHost(t), root: root, records: Records{Folder: filepath.Join(t.TempDir(), "provisioning")}}
	p.definitions = map[string]devcontainer.Definition{}
	p.containerNames = map[string][]string{}
	for _, name := range workspaces {
		if err := os.MkdirAll(filepath.Join(dir, name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		p.declare(name, "container-"+name, name)
	}
	return p
}

func (p *previous) folder(name string) string { return filepath.Join(p.root.Path(), name) }

// declare gives a workspace a valid declaration of a container name and, when
// key is not "", of the Xolotl key host-sample/<key>.
func (p *previous) declare(name, container, key string) {
	block := `{"wyrwood":{"accessGroup":1000,"policies":["workspace"]}}`
	if key != "" {
		block = fmt.Sprintf(`{"wyrwood":{"accessGroup":1000,"policies":["workspace"]},"xolotl":{"keyPolicy":"host-sample","keyName":%q}}`, key)
	}
	p.definitions[p.folder(name)] = devcontainer.Definition{Block: json.RawMessage(block), ContainerName: container}
}

// record writes a retirement record as the previous release did.
func (p *previous) record(name, content string) {
	p.t.Helper()
	if err := os.MkdirAll(p.records.Folder, 0o700); err != nil {
		p.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.records.Folder, name+".json"), []byte(content), 0o600); err != nil {
		p.t.Fatal(err)
	}
}

func (p *previous) start(agent string) error {
	return Import(context.Background(), ImportOptions{
		Data: p.data, Runtime: p.host, Workspaces: p.root, Records: p.records, Agent: agent,
		Logf: func(format string, args ...any) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.log = append(p.log, fmt.Sprintf(format, args...))
		},
	})
}

func (p *previous) logged(t *testing.T, parts ...string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range p.log {
		if containsAll(line, parts...) {
			return
		}
	}
	t.Errorf("no start log line has %q:\n%s", parts, strings.Join(p.log, "\n"))
}

func containsAll(line string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(line, part) {
			return false
		}
	}
	return true
}

func bound(n string, current bool) store.Name {
	return store.Name{Name: n, Agent: sampleAgent, Current: current}
}

// A host of the previous release with provisioned workspaces starts with each
// workspace's declared name imported as its current name, recorded on
// provisioning.wyrwood.agent, and a delete of each removes its binding and key.
func TestAHostOfThePreviousReleaseIsTakenOverAndItsWorkspacesDeleted(t *testing.T) {
	p := newPrevious(t, "alpha", "beta")
	// alpha's upgrade changed its name and key, and its job did not retire the
	// old ones yet.
	p.record("alpha", `{"names":["container-old"],"keys":[{"policy":"host-sample","name":"old"}]}`)
	if err := p.start(sampleAgent); err != nil {
		t.Fatal(err)
	}
	alpha, beta := p.held("alpha"), p.held("beta")
	if !slices.Equal(alpha.Names, []store.Name{bound("container-alpha", true), bound("container-old", false)}) ||
		!slices.Equal(alpha.Keys, []Key{{Policy: "host-sample", Name: "alpha"}, {Policy: "host-sample", Name: "old"}}) {
		t.Fatalf("alpha holds %+v", alpha)
	}
	if !slices.Equal(beta.Names, []store.Name{bound("container-beta", true)}) || !slices.Equal(beta.Keys, []Key{{Policy: "host-sample", Name: "beta"}}) {
		t.Fatalf("beta holds %+v", beta)
	}
	if left, err := p.records.Workspaces(); err != nil || len(left) != 0 {
		t.Fatalf("records left %v %v", left, err)
	}
	p.logged(t, "imported 3 container names and 3 Xolotl keys")

	s := p.steps()
	for _, ws := range []string{"alpha", "beta"} {
		if err := p.deleteAt(s, p.folder(ws)); err != nil {
			t.Fatal(err)
		}
		p.wantGone(t, ws)
	}
	unbound := slices.Sorted(slices.Values(p.unbound))
	if want := []string{sampleAgent + "/container-alpha", sampleAgent + "/container-beta", sampleAgent + "/container-old"}; !slices.Equal(unbound, want) {
		t.Fatalf("unbound %v", unbound)
	}
	for _, key := range []string{"alpha", "beta", "old"} {
		if !slices.Contains(p.seen(), "revoke host-sample/"+key) {
			t.Errorf("no revoke of %s in %v", key, p.seen())
		}
	}
}

// Each import runs once: a later start takes nothing over again, so a
// definition edited after the upgrade gives the workspace nothing.
func TestTheImportRunsAtTheFirstStartOnly(t *testing.T) {
	p := newPrevious(t, "alpha")
	if err := p.start(sampleAgent); err != nil {
		t.Fatal(err)
	}
	p.declare("alpha", "container-later", "later")
	p.record("alpha", `{"names":["container-recorded-later"]}`)
	if err := os.MkdirAll(p.folder("gamma"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := p.start(sampleAgent); err != nil {
		t.Fatal(err)
	}
	if held := p.held("alpha"); !slices.Equal(held.Names, []store.Name{bound("container-alpha", true)}) || len(held.Keys) != 1 {
		t.Fatalf("alpha holds %+v", held)
	}
	if exists, _ := p.data.Exists(context.Background(), "gamma"); exists {
		t.Fatal("import 1 ran again")
	}
}

// A name that two workspaces would hold goes to the one whose declaration names
// it; when both do, to the one whose container holds it, else to the first in
// name order. The start log names both.
func TestANameBothDeclarationsNameGoesToOne(t *testing.T) {
	for name, tc := range map[string]struct {
		holder string
		won    string
		why    string
	}{
		"the container's":   {"beta", "beta", "its container holds the name"},
		"the first by name": {"", "alpha", "first in name order"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPrevious(t, "alpha", "beta")
			p.declare("alpha", "container-shared", "")
			p.declare("beta", "container-shared", "")
			if tc.holder != "" {
				p.holders["container-shared"] = p.folder(tc.holder)
			}
			if err := p.start(sampleAgent); err != nil {
				t.Fatal(err)
			}
			lost := map[string]string{"alpha": "beta", "beta": "alpha"}[tc.won]
			if held := p.held(tc.won); held.Current() != "container-shared" {
				t.Fatalf("%s holds %+v", tc.won, held)
			}
			if held := p.held(lost); !held.Empty() {
				t.Fatalf("%s holds %+v", lost, held)
			}
			p.logged(t, "container name container-shared", "alpha and beta", "goes to "+tc.won, tc.why)

			// The other workspace's next provisioning job fails in read-definition.
			err := p.steps().Start(context.Background(), p.folder(lost), p.host)
			wantKind(t, err, KindInvalid)
			if !strings.Contains(err.Error(), "workspace "+tc.won) {
				t.Fatalf("message %v", err)
			}
		})
	}
}

// Neither declaration names it: a retirement record against a name only a
// container holds, of a workspace whose definition cannot be read.
func TestANameNeitherDeclarationNamesGoesToOne(t *testing.T) {
	for name, tc := range map[string]struct {
		container bool
		won       string
		why       string
	}{
		"the container's":   {true, "beta", "its container holds the name"},
		"the first by name": {false, "alpha", "first in name order"},
	} {
		t.Run(name, func(t *testing.T) {
			p := newPrevious(t, "alpha", "beta")
			p.record("alpha", `{"names":["container-shared"]}`)
			p.failDefinition(p.folder("beta"))
			if tc.container {
				p.containerNames[p.folder("beta")] = []string{"container-shared"}
				p.holders["container-shared"] = p.folder("beta")
			} else {
				p.record("beta", `{"names":["container-shared"]}`)
			}
			if err := p.start(sampleAgent); err != nil {
				t.Fatal(err)
			}
			held := p.held(tc.won)
			if !slices.Contains(held.Names, bound("container-shared", false)) {
				t.Fatalf("%s holds %+v", tc.won, held)
			}
			p.logged(t, "container name container-shared", "alpha and beta", "goes to "+tc.won, tc.why)
		})
	}
}

// A key goes to the workspace whose declaration names it, else to the first in
// name order, and the start log names both.
func TestAKeyTwoWorkspacesWouldHoldGoesToOne(t *testing.T) {
	p := newPrevious(t, "alpha", "beta", "gamma")
	p.record("alpha", `{"keys":[{"policy":"host-sample","name":"beta"}]}`)
	p.declare("gamma", "container-gamma", "shared")
	p.record("beta", `{"keys":[{"policy":"host-sample","name":"shared"}]}`)
	if err := p.start(sampleAgent); err != nil {
		t.Fatal(err)
	}
	if held := p.held("beta"); !slices.Contains(held.Keys, Key{Policy: "host-sample", Name: "beta"}) || slices.Contains(held.Keys, Key{Policy: "host-sample", Name: "shared"}) {
		t.Fatalf("beta holds %+v", held)
	}
	if held := p.held("gamma"); !slices.Equal(held.Keys, []Key{{Policy: "host-sample", Name: "shared"}}) {
		t.Fatalf("gamma holds %+v", held)
	}
	p.logged(t, "Xolotl key host-sample/beta", "alpha and beta", "goes to beta", "its declaration names it")
	p.logged(t, "Xolotl key host-sample/shared", "beta and gamma", "goes to gamma")
}

func TestADeclaredNameAContainerOutsideHoldsIsNotTaken(t *testing.T) {
	p := newPrevious(t, "alpha")
	p.holders["container-alpha"] = "/elsewhere"
	if err := p.start(sampleAgent); err != nil {
		t.Fatal(err)
	}
	if held := p.held("alpha"); len(held.Names) != 0 || len(held.Keys) != 1 {
		t.Fatalf("alpha holds %+v", held)
	}
	p.logged(t, "alpha declares container name container-alpha", "a container outside it holds")
}

// A container of another workspace that holds the declared name keeps it from
// the declaration, and the name goes to the container's workspace when it
// claims it otherwise, from its record.
func TestADeclaredNameAnotherWorkspacesContainerHoldsGoesToThatWorkspace(t *testing.T) {
	p := newPrevious(t, "alpha", "beta")
	p.declare("alpha", "container-beta", "")
	p.holders["container-beta"] = p.folder("beta")
	p.declare("beta", "container-new", "")
	p.record("beta", `{"names":["container-beta"]}`)
	p.record("alpha", `{"names":["container-alpha"]}`)
	if err := p.start(sampleAgent); err != nil {
		t.Fatal(err)
	}
	if held := p.held("beta"); !slices.Contains(held.Names, bound("container-beta", false)) {
		t.Fatalf("beta holds %+v", held)
	}
	// alpha keeps what its record names, and declares no current name it can hold.
	if held := p.held("alpha"); !slices.Equal(held.Names, []store.Name{bound("container-alpha", false)}) {
		t.Fatalf("alpha holds %+v", held)
	}
}

// A block that is not valid still names what an earlier definition may have
// provisioned, and its name is current, so a delete removes them.
func TestABlockThatIsNotValidIsImported(t *testing.T) {
	p := newPrevious(t, "alpha")
	p.definitions[p.folder("alpha")] = devcontainer.Definition{
		Block:         json.RawMessage(`{"wyrwood":{"accessGroup":"wrong"},"xolotl":{"keyPolicy":"host-sample","keyName":"alpha"}}`),
		ContainerName: "container-alpha",
	}
	if err := p.start(sampleAgent); err != nil {
		t.Fatal(err)
	}
	if held := p.held("alpha"); !slices.Equal(held.Names, []store.Name{bound("container-alpha", true)}) || len(held.Keys) != 1 {
		t.Fatalf("alpha holds %+v", held)
	}
}

// Without provisioning.wyrwood, the names are kept with no agent; a job that
// would unbind one fails as provisioning-unavailable and keeps the name; the
// first start that has the section records the agent.
func TestNamesImportedWithoutTheSectionWaitForItsAgent(t *testing.T) {
	p := newPrevious(t, "alpha", "beta")
	p.failDefinition(p.folder("beta"))
	p.containerNames[p.folder("beta")] = []string{"container-beta"}
	if err := p.start(""); err != nil {
		t.Fatal(err)
	}
	if held := p.held("alpha"); !slices.Equal(held.Names, []store.Name{{Name: "container-alpha", Current: true}}) {
		t.Fatalf("alpha holds %+v", held)
	}
	// Without the section, the names of beta's containers are not imported.
	if held := p.held("beta"); !held.Empty() {
		t.Fatalf("beta holds %+v", held)
	}
	p.logged(t, "have no Wyrwood agent")

	// A Menagerie started again with the section, as an operator would.
	s := p.with(Clients{Bindings: p.host, Keys: p.host})
	err := p.deleteAt(s, p.folder("alpha"))
	wantKind(t, err, KindUnavailable)
	if !strings.Contains(err.Error(), "imported with no provisioning.wyrwood") {
		t.Fatalf("message %v", err)
	}
	if held := p.held("alpha"); len(held.Names) != 1 {
		t.Fatalf("alpha holds %+v", held)
	}

	if err := p.start(sampleAgent); err != nil {
		t.Fatal(err)
	}
	if held := p.held("alpha"); !slices.Equal(held.Names, []store.Name{bound("container-alpha", true)}) {
		t.Fatalf("alpha holds %+v", held)
	}
	p.logged(t, "recorded Wyrwood agent "+sampleAgent+" on 1 imported container names")
	if err := p.deleteAt(p.steps(), p.folder("alpha")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.unbound, []string{sampleAgent + "/container-alpha"}) {
		t.Fatalf("unbound %v", p.unbound)
	}
}

// What a record names for a workspace whose folder is gone is still the
// workspace's, so the list shows it and a delete removes it; the folder a
// delete began to remove carries over.
func TestARecordWithNoFolderAndARemovalCarryOver(t *testing.T) {
	p := newPrevious(t, "alpha")
	p.record("gone", `{"names":["container-gone"],"keys":[{"policy":"host-sample","name":"gone"}]}`)
	p.record("alpha", `{"removing":"1:2"}`)
	if err := p.start(sampleAgent); err != nil {
		t.Fatal(err)
	}
	if removing, err := p.data.Removing(context.Background(), "alpha"); err != nil || removing != "1:2" {
		t.Fatalf("removing %q %v", removing, err)
	}
	noFolder, err := p.data.Discover(context.Background(), []string{"alpha"})
	if err != nil || !slices.Equal(noFolder, []string{"gone"}) {
		t.Fatalf("no-folder %v %v", noFolder, err)
	}
	if err := p.deleteAt(p.steps(), p.folder("gone")); err != nil {
		t.Fatal(err)
	}
	p.wantCalls(t, "unbind container-gone", "remove-containers", "revoke host-sample/gone", "remove-folder")
	p.wantGone(t, "gone")
}

// A record the import cannot read fails the start, and nothing is imported or
// removed, so the next start tries again.
func TestAnUnreadableRecordFailsTheImportWhole(t *testing.T) {
	p := newPrevious(t, "alpha")
	p.record("alpha", `{`)
	if err := p.start(sampleAgent); err == nil || !strings.Contains(err.Error(), "retirement record of alpha") {
		t.Fatalf("%v", err)
	}
	if held := p.held("alpha"); !held.Empty() {
		t.Fatalf("alpha holds %+v", held)
	}
	if left, _ := p.records.Workspaces(); !slices.Equal(left, []string{"alpha"}) {
		t.Fatalf("records left %v", left)
	}
	p.record("alpha", `{}`)
	if err := p.start(sampleAgent); err != nil {
		t.Fatal(err)
	}
	if held := p.held("alpha"); held.Current() != "container-alpha" {
		t.Fatalf("alpha holds %+v", held)
	}
}

// failDefinition makes a workspace's definition unreadable, as a file that does
// not parse is.
func (p *previous) failDefinition(folder string) {
	p.Lock()
	defer p.Unlock()
	if p.unreadable == nil {
		p.unreadable = map[string]bool{}
	}
	p.unreadable[folder] = true
}

var errUnreadable = errors.New("devcontainer.json: invalid character")

func TestSelectionImportWithoutCurrentDeclaredNameUsesLegacyMembers(t *testing.T) {
	for _, current := range []string{"", "different-name"} {
		t.Run("current="+current, func(t *testing.T) {
			p := newPrevious(t, "sample")
			ctx := context.Background()
			if current != "" {
				p.hold("sample", current, true)
			}
			p.definitions[p.folder("sample")] = definition(`{"wyrwood":{"accessGroup":1000,"policies":["a"],"values":{"account":"legacy-account"}},"xolotl":{"keyPolicy":"legacy-policy","keyName":"legacy-name"}}`)
			p.readErr = errors.New("no binding read is allowed")
			steps := p.steps()
			if err := steps.ImportSelections(ctx, p.root, func(format string, args ...any) { p.log = append(p.log, fmt.Sprintf(format, args...)) }); err != nil {
				t.Fatal(err)
			}
			got, err := p.data.Selection(ctx, "sample")
			if err != nil || !got.Imported || !slices.Equal(got.Policies, []string{"a", "client-key", "workspace"}) || got.Values["account"] != "legacy-account" || got.Key == nil || got.Key.String() != "legacy-policy/legacy-name" {
				t.Fatalf("legacy selection lost: %+v %v", got, err)
			}
			p.logged(t, "account=legacy-account", "sample")
			// Import 5's mark and the selection commit together; another startup
			// cannot re-import a later template's legacy identity or policies.
			p.definitions[p.folder("sample")] = definition(sampleBlock)
			if err := steps.ImportSelections(ctx, p.root, t.Logf); err != nil {
				t.Fatal(err)
			}
			again, err := p.data.Selection(ctx, "sample")
			if err != nil || again.Revision != got.Revision || !slices.Equal(again.Policies, got.Policies) || again.Key.String() != got.Key.String() {
				t.Fatalf("reimported: %+v %v", again, err)
			}
		})
	}
}

func TestDelayedSelectionImportKeepsOriginallyAbsentKey(t *testing.T) {
	p := newPrevious(t, "sample")
	ctx := context.Background()
	p.hold("sample", "container-sample", true)
	p.definitions[p.folder("sample")] = definition(`{"wyrwood":{"accessGroup":1000,"policies":["workspace"]}}`)
	p.readErr = errors.New("controller unavailable")
	steps := p.steps()
	if err := steps.ImportSelections(ctx, p.root, t.Logf); err != nil {
		t.Fatal(err)
	}
	pending, err := p.data.Selection(ctx, "sample")
	if err != nil || pending.Imported || pending.Key != nil {
		t.Fatalf("%+v %v", pending, err)
	}
	// An upgrade changed the legacy member while the read was delayed.
	p.definitions[p.folder("sample")] = definition(sampleBlock)
	p.readErr = nil
	if err = steps.EnsureImported(ctx, p.folder("sample")); err != nil {
		t.Fatal(err)
	}
	imported, err := p.data.Selection(ctx, "sample")
	if err != nil || !imported.Imported || imported.Key != nil {
		t.Fatalf("template chose a key: %+v %v", imported, err)
	}
}
