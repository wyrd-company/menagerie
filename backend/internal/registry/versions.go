// ---
// relationships:
//   implements: [menagerie, list-template-versions-in-the-template-service]
//   references: [workspace-api, template-service-api]
// ---

package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/store"
	"github.com/wyrd-company/menagerie/backend/internal/templates"
)

// DefaultCheckWait bounds one version check of a template when the
// Registry names no CheckWait.
const DefaultCheckWait = 2 * time.Minute

// checkRun is one version check of a template, which every check of that
// template asked for while it runs shares.
type checkRun struct {
	done chan struct{}
	tpl  store.Template
	err  error
}

// CheckVersions checks a registered template for new versions: it lists
// the versions of its repository through the template service, and they
// replace its found versions. A failed check keeps the found versions and
// records its time and reason: the template service's refusal kind, or
// template-service-unavailable when the service cannot be reached, fails
// or does not answer within the check's wait. Either way the answer is the
// template with that record; only a template that is not registered, or
// Menagerie's data failing, is an error. Two checks of one template at once
// share one run, which a caller that goes away does not end.
func (r *Registry) CheckVersions(ctx context.Context, id string) (store.Template, error) {
	r.mu.Lock()
	if r.running == nil {
		r.running = map[string]*checkRun{}
	}
	run, ok := r.running[id]
	if ok && r.shared != nil {
		r.shared(id)
	}
	if !ok {
		run = &checkRun{done: make(chan struct{})}
		r.running[id] = run
		wait := r.CheckWait
		if wait <= 0 {
			wait = DefaultCheckWait
		}
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), wait)
		go func() {
			defer cancel()
			run.tpl, run.err = r.checkVersions(bounded, id)
			r.mu.Lock()
			delete(r.running, id)
			r.mu.Unlock()
			close(run.done)
		}()
	}
	r.mu.Unlock()
	select {
	case <-run.done:
		return run.tpl, run.err
	case <-ctx.Done():
		return store.Template{}, ctx.Err()
	}
}

func (r *Registry) checkVersions(ctx context.Context, id string) (store.Template, error) {
	tpl, err := r.Data.Template(ctx, id)
	if err != nil {
		return store.Template{}, err
	}
	versions, err := r.Templates.ListVersions(ctx, tpl.Address)
	// The outcome is recorded even when the check's wait has ended.
	record := context.WithoutCancel(ctx)
	if err != nil {
		return r.Data.RecordCheckFailure(record, id, CheckFailure(err), r.now())
	}
	return r.Data.RecordCheck(record, id, found(versions.Versions), r.now())
}

// CheckFailure is the reason a version check records for a template
// service error: the refusal kinds of a listing that are about the
// template's repository or address, and template-service-unavailable for
// every other failure, an unreachable service, a fault and no answer within
// the wait included. It never quotes the template service's message.
func CheckFailure(err error) string {
	var refused *templates.Error
	if errors.As(err, &refused) && refused.Status < http.StatusInternalServerError {
		switch refused.Kind {
		case store.CheckTemplateNotFound, store.CheckTemplateResolve, store.CheckInvalidAddress:
			return refused.Kind
		}
	}
	return store.CheckServiceUnavailable
}

// Schedule checks every registered template for new versions now, then at
// each tick, until ctx ends. It logs a failed check, and a moved tag, by
// the template's id and the failure's kind alone.
func (r *Registry) Schedule(ctx context.Context, ticks <-chan time.Time, logf func(format string, args ...any)) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	for {
		r.checkAll(ctx, logf)
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

func (r *Registry) checkAll(ctx context.Context, logf func(format string, args ...any)) {
	list, err := r.Data.Templates(ctx)
	if err != nil {
		if ctx.Err() == nil {
			logf("menagerie: the template version checks could not read the templates: %v", err)
		}
		return
	}
	for _, tpl := range list {
		if ctx.Err() != nil {
			return
		}
		checked, err := r.CheckVersions(ctx, tpl.ID)
		switch {
		case err != nil:
			if ctx.Err() == nil {
				logf("menagerie: the version check of template %s failed: %v", tpl.ID, err)
			}
		case checked.CheckFailure != "":
			logf("menagerie: the version check of template %s failed (%s); its found versions are kept", tpl.ID, checked.CheckFailure)
		case checked.CurrentMoved():
			logf("menagerie: %s", MovedMessage(checked))
		}
	}
}

// MovedMessage says that a template's current version's tag moved, and
// what a manager does about it.
func MovedMessage(tpl store.Template) string {
	return fmt.Sprintf("the tag of version %s, template %s's current version, has moved to another commit than the one Menagerie recorded; creates and upgrades from it are refused until a manager upgrades the template to version %s again, which checks it and records the new commit", tpl.CurrentVersion, tpl.ID, tpl.CurrentVersion)
}
