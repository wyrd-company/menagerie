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
)

// DefaultRecords is the folder of the retirement records, in Menagerie's data
// volume, which no devcontainer can reach.
const DefaultRecords = "/var/lib/menagerie/provisioning"

// Record is what a workspace's definition stopped, or may stop, declaring. It
// holds no secret. It only grows until a job that ends with the devcontainer
// started consumes it.
type Record struct {
	Names []string `json:"names,omitempty"`
	Keys  []Key    `json:"keys,omitempty"`
	// Removing says that a delete finished its backup and began to remove the
	// folder. It is kept here because a devcontainer can write anything in the
	// folder, and a removal can take any of it first.
	Removing bool `json:"removing,omitempty"`
}

func (r Record) empty() bool { return len(r.Names) == 0 && len(r.Keys) == 0 && !r.Removing }

// Records keeps one Record per workspace in a folder.
type Records struct{ Folder string }

func (s Records) path(workspace string) string {
	return filepath.Join(s.Folder, workspace+".json")
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
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, fmt.Errorf("read the retirement record of %s: %w", workspace, err)
	}
	return record, nil
}

// Write replaces the workspace's record in one step. An empty record removes
// the file.
func (s Records) Write(workspace string, record Record) error {
	if record.empty() {
		if err := os.Remove(s.path(workspace)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove the retirement record of %s: %w", workspace, err)
		}
		return nil
	}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("write the retirement record of %s: %w", workspace, err)
	}
	if err := os.MkdirAll(s.Folder, 0o700); err != nil {
		return fmt.Errorf("write the retirement record of %s: %w", workspace, err)
	}
	temp, err := os.CreateTemp(s.Folder, ".record-*")
	if err != nil {
		return fmt.Errorf("write the retirement record of %s: %w", workspace, err)
	}
	defer os.Remove(temp.Name())
	_, err = temp.Write(append(data, '\n'))
	if err == nil {
		err = temp.Sync()
	}
	if closed := temp.Close(); err == nil {
		err = closed
	}
	if err == nil {
		err = os.Rename(temp.Name(), s.path(workspace))
	}
	if err != nil {
		return fmt.Errorf("write the retirement record of %s: %w", workspace, err)
	}
	return nil
}

// add returns the record with the declaration's name and key, once each.
func (r Record) add(d *Declaration) Record {
	if !slices.Contains(r.Names, d.ContainerName) {
		r.Names = append(r.Names, d.ContainerName)
	}
	if d.Xolotl != nil && !slices.Contains(r.Keys, *d.Xolotl) {
		r.Keys = append(r.Keys, *d.Xolotl)
	}
	return r
}
