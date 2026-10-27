// ---
// relationships:
//   implements: menagerie
//   references: [workspace, configuration]
// ---

package commits

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/backup"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// Schedule sweeps the workspace root on each tick until ctx ends. The
// caller owns ticks, such as a time.Ticker's channel: the first sweep is
// at the first tick, not at the call.
func (c *Commits) Schedule(ctx context.Context, ticks <-chan time.Time, logf func(string, ...any)) {
	if logf == nil {
		logf = log.Printf
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			c.Sweep(logf)
		}
	}
}

// Sweep starts a commit job with AutomaticMessage on every workspace that
// has no unfinished job and has a change the job would commit. A busy
// workspace is skipped, never queued. Checking for changes changes
// nothing. It returns the workspaces it started a job on.
//
// A failure is logged with the workspace's name and a fixed text: the
// underlying error can quote the workspace's own files.
func (c *Commits) Sweep(logf func(string, ...any)) []string {
	names, err := c.opts.Root.Names()
	if err != nil {
		logf("menagerie: automatic commit: the workspace root could not be read")
		return nil
	}
	var started []string
	for _, name := range names {
		if _, busy := c.opts.Jobs.Active(name); busy {
			continue
		}
		folder, err := c.opts.Root.Folder(name)
		if err != nil {
			continue
		}
		dirty, err := backup.Pending(folder, name)
		if err != nil {
			logf("menagerie: automatic commit: workspace %s: its changes could not be read", name)
			continue
		}
		if !dirty {
			continue
		}
		_, _, err = c.Start(name, AutomaticMessage)
		var busy *jobs.BusyError
		switch {
		case errors.As(err, &busy):
		case err != nil:
			logf("menagerie: automatic commit: workspace %s: the commit job could not start", name)
		default:
			started = append(started, name)
		}
	}
	return started
}
