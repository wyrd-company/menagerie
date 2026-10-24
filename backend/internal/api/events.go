// ---
// relationships:
//   implements: workspace-api
// ---

package api

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// jobEvents streams a job's events as Server-Sent Events. Each event's id is
// its sequence number within the job. A Last-Event-ID header, or a
// lastEventId query parameter, replays only later events. The stream ends
// after the job's result event. It adds no keepalive or deadline: a client
// that loses the connection reconnects with Last-Event-ID.
func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	job, ok := s.opts.Jobs.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, apiError{Kind: "job-not-found", Message: "no job with this id in this process"})
		return
	}
	cursor, err := lastEventID(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, apiError{Kind: "invalid-request", Message: err.Error()})
		return
	}
	// ResponseController reaches the flusher through wrapping middleware.
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}
	for {
		events, ended, changed := job.EventsAfter(cursor)
		for _, e := range events {
			if err := writeEvent(w, e); err != nil {
				return
			}
			cursor = e.ID
		}
		if err := rc.Flush(); err != nil {
			return
		}
		if ended {
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}

// writeEvent frames one event. Every line of the payload gets its own data
// field, so a payload with line breaks still decodes to the same text;
// payloads appended through jobs.Run.Emit are already compacted to one line.
func writeEvent(w io.Writer, e jobs.Event) error {
	var b strings.Builder
	fmt.Fprintf(&b, "id: %d\nevent: %s\n", e.ID, e.Type)
	for _, line := range strings.Split(strings.ReplaceAll(string(e.Data), "\r\n", "\n"), "\n") {
		b.WriteString("data: ")
		b.WriteString(strings.TrimSuffix(line, "\r"))
		b.WriteString("\n")
	}
	b.WriteString("\n")
	_, err := io.WriteString(w, b.String())
	return err
}

func lastEventID(r *http.Request) (int64, error) {
	value := r.Header.Get("Last-Event-ID")
	if value == "" {
		value = r.URL.Query().Get("lastEventId")
	}
	if value == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil || id < 0 {
		return 0, fmt.Errorf("last event id %q is not a non-negative integer", value)
	}
	return id, nil
}
