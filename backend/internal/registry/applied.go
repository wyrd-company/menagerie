// ---
// relationships:
//   implements: menagerie
//   references: workspace
// ---

package registry

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RecordName is Toha's applied record in a workspace folder.
const RecordName = ".toha.json"

// maxRecord bounds the applied record this service reads.
const maxRecord = 16 << 20

// Applied is what a workspace's applied record says of the template applied
// to the workspace's root: the source Toha names the template's repository
// and folder by, the ref it was applied from, and the commit.
type Applied struct {
	Source string
	// Ref is the ref of the template the record names, or "".
	Ref    string
	Commit string
}

// ReadApplied reads the application at the root of the workspace folder from
// its applied record. A record that is not a regular file of at most 16 MiB,
// that is not Toha's JSON, or that holds no application at the root, is an
// error; a folder with no record is os.ErrNotExist.
func ReadApplied(folder string) (Applied, error) {
	path := filepath.Join(folder, RecordName)
	info, err := os.Lstat(path)
	if err != nil {
		return Applied{}, err
	}
	if !info.Mode().IsRegular() {
		return Applied{}, errors.New("the applied record is not a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return Applied{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRecord+1))
	if err != nil {
		return Applied{}, err
	}
	if len(data) > maxRecord {
		return Applied{}, errors.New("the applied record is larger than 16 MiB")
	}
	var record struct {
		Applications []struct {
			Target   string  `json:"target"`
			Template string  `json:"template"`
			Source   string  `json:"source"`
			Commit   *string `json:"commit"`
		} `json:"applications"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return Applied{}, errors.New("the applied record is not Toha's JSON")
	}
	for _, app := range record.Applications {
		if app.Target != "." || app.Source == "" {
			continue
		}
		applied := Applied{Source: app.Source, Ref: refOf(app.Template, app.Source)}
		if app.Commit != nil {
			applied.Commit = *app.Commit
		}
		return applied, nil
	}
	return Applied{}, errors.New("the applied record holds no application at the workspace's root")
}

// refOf is the ref in a template's formal name: Toha writes it as the
// source's repository, `@` and the ref, then the source's `#` folder.
func refOf(template, source string) string {
	repository, folder, hasFolder := strings.Cut(source, "#")
	if hasFolder {
		suffix := "#" + folder
		if !strings.HasSuffix(template, suffix) {
			return ""
		}
		template = strings.TrimSuffix(template, suffix)
	}
	ref, ok := strings.CutPrefix(template, repository+"@")
	if !ok {
		return ""
	}
	return ref
}

// Source is the source a workspace folder's applied record names, or "" when
// it has no readable record.
func Source(folder string) string {
	applied, err := ReadApplied(folder)
	if err != nil {
		return ""
	}
	return applied.Source
}
