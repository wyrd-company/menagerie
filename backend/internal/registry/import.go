// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

package registry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
	"github.com/wyrd-company/menagerie/backend/internal/workspace"
)

// ImportID is the mark of the templates import in Menagerie's data.
const ImportID = 3

// DefaultImportWait bounds how long the import waits for the template
// service, which reads each entry's address.
const DefaultImportWait = time.Minute

// Entry is one entry of the configuration's templates key, which earlier
// releases approved templates with, one entry per template version.
type Entry struct {
	ID, Name, Address, RunnerImage string
}

// ImportOptions compose the templates import.
type ImportOptions struct {
	Data      *store.Store
	Templates TemplateService
	Root      *workspace.Root
	Entries   []Entry
	// Wait bounds the wait for the template service; zero is
	// DefaultImportWait.
	Wait time.Duration
	Logf func(format string, args ...any)
}

// Import takes over the configuration's templates entries once, at the
// first start whose data has no templates import: an entry whose address
// the template service refuses, answering invalid-address, is skipped and
// named by its id and that kind, never by its address. Any other error
// reply, a fault included, fails the start like an unreachable service,
// naming the entry by its id, and records no mark, so the next start
// imports that entry. The entries of one source are one
// template, which the newest entry whose ref is a version gives its current
// version, id, name and runner image, its address being that entry's
// without the ref; its commit stays unknown until a check fetches it. A
// source with no entry whose ref is a version is not imported, and its
// entries' ids are logged. Each workspace whose applied record names an
// imported template's source joins it. The import records its mark with the
// templates in one transaction; a start that does not reach the template
// service within the wait fails, and the next start imports.
func Import(ctx context.Context, opts ImportOptions) error {
	done, err := opts.Data.Imported(ctx, ImportID)
	if err != nil || done {
		return err
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	wait := opts.Wait
	if wait == 0 {
		wait = DefaultImportWait
	}
	deadline := time.Now().Add(wait)
	type read struct {
		entry   Entry
		address *templates.Address
	}
	var order []string
	bySource := map[string][]read{}
	for _, entry := range opts.Entries {
		address, err := readWaiting(ctx, opts.Templates, entry.Address, deadline)
		var answered *templates.Error
		switch {
		case errors.As(err, &answered) && answered.Kind == templates.KindInvalidAddress:
			logf("menagerie: the templates entry %s is not imported: the template service refuses its address (%s)", entry.ID, answered.Kind)
			continue
		case errors.As(err, &answered):
			// Its message is the template service's and could name the
			// address; the kind and status do not.
			return fmt.Errorf("importing the templates key: the template service failed reading the address of entry %s (%d %s); the next start imports", entry.ID, answered.Status, answered.Kind)
		case err != nil:
			return fmt.Errorf("importing the templates key: %w", err)
		}
		if _, seen := bySource[address.Source]; !seen {
			order = append(order, address.Source)
		}
		bySource[address.Source] = append(bySource[address.Source], read{entry, address})
	}
	var imported []store.Template
	for _, source := range order {
		var newest *read
		var newestVersion Version
		for i, r := range bySource[source] {
			if r.address.Reference == nil {
				continue
			}
			v, ok := ParseVersion(*r.address.Reference)
			if !ok {
				continue
			}
			// The tag without `v` is the version's, when both name it.
			plainer := v.Compare(newestVersion) == 0 && !strings.HasPrefix(*r.address.Reference, "v") &&
				strings.HasPrefix(*newest.address.Reference, "v")
			if newest == nil || v.Compare(newestVersion) > 0 || plainer {
				newest, newestVersion = &bySource[source][i], v
			}
		}
		if newest == nil {
			var ids []string
			for _, r := range bySource[source] {
				ids = append(ids, r.entry.ID)
			}
			logf("menagerie: the templates entries %s are not imported: none names a version of its repository, a tag MAJOR.MINOR.PATCH; a manager registers the template in the interface once its repository has a version tag, which joins its workspaces", strings.Join(ids, ", "))
			continue
		}
		imported = append(imported, store.Template{
			ID: newest.entry.ID, Name: newest.entry.Name, Address: newest.address.Base, Source: source,
			RunnerImage: newest.entry.RunnerImage, CurrentVersion: newestVersion.String(), CurrentTag: *newest.address.Reference,
		})
	}
	names, err := opts.Data.Untemplated(ctx)
	if err != nil {
		return err
	}
	sources := map[string]string{}
	for _, name := range names {
		if folder, err := opts.Root.Folder(name); err == nil {
			sources[name] = Source(folder)
		}
	}
	_, err = opts.Data.Import(ctx, ImportID, func(tx *store.Tx) error {
		for _, tpl := range imported {
			var joins []string
			for _, name := range names {
				if sources[name] == tpl.Source {
					joins = append(joins, name)
				}
			}
			recorded, err := tx.RegisterTemplate(tpl, joins)
			if err != nil {
				return fmt.Errorf("importing the templates entry %s: %w", tpl.ID, err)
			}
			logf("menagerie: imported template %s at version %s from the templates key, with %d workspaces", recorded.ID, recorded.CurrentVersion, len(recorded.Workspaces))
		}
		return nil
	})
	return err
}

// readWaiting reads an address, trying again while the template service
// cannot be reached, until deadline.
func readWaiting(ctx context.Context, service TemplateService, address string, deadline time.Time) (*templates.Address, error) {
	for {
		// A service that takes the request and never answers ends the call
		// at the deadline, which is the wait running out.
		call, cancel := context.WithDeadline(ctx, deadline)
		read, err := service.ReadAddress(call, address)
		cancel()
		var unavailable *templates.UnavailableError
		if !errors.As(err, &unavailable) {
			return read, err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("the template service did not answer within the import's wait: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(min(time.Second, time.Until(deadline))):
		}
	}
}
