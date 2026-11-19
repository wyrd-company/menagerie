// ---
// relationships:
//   implements: menagerie
//   references: workspace
// ---

package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wyrd-company/menagerie/backend/internal/store"
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

// MaxRecord bounds the applied record this service reads.
const MaxRecord = maxRecord

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
	return ParseApplied(data)
}

// ParseApplied reads the application at the root of a workspace from the
// content of its applied record. Content that is not Toha's JSON, or that
// holds no application at the root, is an error.
func ParseApplied(data []byte) (Applied, error) {
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

// IsCommit says that commit is a full Git object id: 40 or 64 lower-case
// hexadecimal digits.
func IsCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	for _, c := range commit {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// VersionError says why an applied record gives a template no version.
type VersionError struct{ Reason string }

func (e *VersionError) Error() string { return e.Reason }

// RecordVersion is the version an applied record gives tpl, by the one rule
// every path that takes an applied version from a record follows: the
// record's source must be the template's source and its commit a full Git
// object id, and the version is the template's found version whose tag is
// at that commit; when the record's ref is a version it must be that
// version, and when it is not and several tags share the commit, the
// version is the newest of them. A record that gives none is a
// *VersionError naming why, with the ref and commit quoted.
func RecordVersion(rec Applied, tpl store.Template) (store.Version, error) {
	if rec.Source != tpl.Source {
		return store.Version{}, &VersionError{Reason: fmt.Sprintf("the record names another template than template %s", tpl.ID)}
	}
	if !IsCommit(rec.Commit) {
		return store.Version{}, &VersionError{Reason: fmt.Sprintf("the record's commit %q is not a full Git object id", rec.Commit)}
	}
	var at []store.Version
	for _, found := range tpl.Versions {
		if found.Commit == rec.Commit {
			at = append(at, found)
		}
	}
	named, isVersion := ParseVersion(rec.Ref)
	switch {
	case len(at) == 0:
		return store.Version{}, &VersionError{Reason: fmt.Sprintf("no version tag of template %s that its checks found is at the record's commit %q (ref %q)", tpl.ID, rec.Commit, rec.Ref)}
	case !isVersion:
		// Found versions are in ascending order: the last is the newest.
		return at[len(at)-1], nil
	}
	for _, found := range at {
		if found.Version == named.String() {
			return found, nil
		}
	}
	return store.Version{}, &VersionError{Reason: fmt.Sprintf("the record's ref %q names version %s, but the version tag of template %s at its commit %q gives %s", rec.Ref, named, tpl.ID, rec.Commit, at[len(at)-1].Version)}
}

// JoinVersion is the applied version a join, or the first start's import,
// takes from an applied record of tpl: RecordVersion's, at the record's
// commit; else an unknown version with the record's commit, kept only
// when the record names the template's source and a full object id, which
// the template's next version check completes.
func JoinVersion(rec Applied, tpl store.Template) store.AppliedVersion {
	if found, err := RecordVersion(rec, tpl); err == nil {
		return store.AppliedVersion{Version: found.Version, Commit: rec.Commit}
	}
	if rec.Source == tpl.Source && IsCommit(rec.Commit) {
		return store.AppliedVersion{Commit: rec.Commit}
	}
	return store.AppliedVersion{}
}

// Join reads a workspace folder's applied record for a join: the source it
// names, "" for a folder with no readable record, and the applied version
// it gives a template of that source.
func Join(folder string) (string, func(store.Template) store.AppliedVersion) {
	applied, err := ReadApplied(folder)
	if err != nil {
		return "", nil
	}
	return applied.Source, func(tpl store.Template) store.AppliedVersion { return JoinVersion(applied, tpl) }
}
