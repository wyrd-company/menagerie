// ---
// relationships:
//   implements: menagerie
// ---

package provisioning

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultRecords is the folder of the retirement records that earlier releases
// wrote in Menagerie's data volume. The first start imports them into
// Menagerie's data and removes them.
const DefaultRecords = "/var/lib/menagerie/provisioning"

// Record is a retirement record of an earlier release: the container names and
// Xolotl keys a workspace's definition stopped, or was about to stop,
// declaring, and the folder a delete began to remove. It holds no secret.
type Record struct {
	Names []string
	Keys  []Key
	// Removing names the folder, by its device and inode, that a delete backed
	// up and went on to remove.
	Removing string
}

// recordFile is a Record as earlier releases wrote it.
type recordFile struct {
	Names []string `json:"names,omitempty"`
	Keys  []struct {
		Policy string `json:"policy"`
		Name   string `json:"name"`
	} `json:"keys,omitempty"`
	Removing string `json:"removing,omitempty"`
}

// Records are the retirement records in a folder, one file per workspace.
type Records struct{ Folder string }

func (s Records) path(workspace string) string {
	return filepath.Join(s.Folder, workspace+".json")
}

// Workspaces lists the workspaces that have a record, in name order. A folder
// that does not exist holds none.
func (s Records) Workspaces() ([]string, error) {
	entries, err := os.ReadDir(s.Folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the retirement records: %w", err)
	}
	var names []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if ok && e.Type().IsRegular() && name != "" && !strings.HasPrefix(name, ".") {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// Read returns the workspace's record. No record is an empty one.
func (s Records) Read(workspace string) (Record, error) {
	data, err := os.ReadFile(s.path(workspace))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, fmt.Errorf("read the retirement record of %s: %w", workspace, err)
	}
	var file recordFile
	if err := json.Unmarshal(data, &file); err != nil {
		return Record{}, fmt.Errorf("read the retirement record of %s: %w", workspace, err)
	}
	record := Record{Names: file.Names, Removing: file.Removing}
	for _, k := range file.Keys {
		record.Keys = append(record.Keys, Key{Policy: k.Policy, Name: k.Name})
	}
	return record, nil
}

// Remove removes the workspace's record. What is gone is done.
func (s Records) Remove(workspace string) error {
	if err := os.Remove(s.path(workspace)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the retirement record of %s: %w", workspace, err)
	}
	return nil
}
