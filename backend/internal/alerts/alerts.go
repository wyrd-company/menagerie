// ---
// relationships:
//   implements: menagerie
//   references: [configuration, job-without-timeout-blocks-workspace]
// ---

// Package alerts tells a webhook about failed and long-running jobs. Each
// alert is one JSON POST with one attempt and a time limit. Alerts wait in
// one queue and are posted one at a time, in the order they were sent, by
// a background sender, so an alert never delays or fails a job and a
// job's warning reaches the webhook before its failure. The webhook URL
// can carry a credential: it is never written to a log or an error.
package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

// Timeout bounds each delivery, and how long Close waits.
const Timeout = 10 * time.Second

// QueueSize is how many alerts can wait for delivery. An alert sent while
// the queue is full is dropped, with a log line.
const QueueSize = 64

// Alert events.
const (
	EventJobFailed      = "job-failed"
	EventJobLongRunning = "job-long-running"
)

// Payload is the JSON body of an alert.
type Payload struct {
	Event     string      `json:"event"`
	JobID     string      `json:"jobId"`
	Workspace string      `json:"workspace"`
	Operation string      `json:"operation"`
	Phase     string      `json:"phase"`
	State     jobs.State  `json:"state"`
	Error     *jobs.Error `json:"error"`
	Console   string      `json:"console"`
}

// Options configure a Webhook.
type Options struct {
	// URL is the http or https webhook URL.
	URL string
	// Origin is the browser origin of Menagerie, such as
	// https://menagerie.example.test, that console links start with.
	Origin string
	// Logf receives one line per failed delivery; nil logs through the
	// standard logger.
	Logf func(format string, args ...any)
	// Timeout replaces the delivery time limit in tests.
	Timeout time.Duration
}

// Webhook delivers alerts. It satisfies jobs.Alerts: its methods only
// queue the alert, so they never block.
type Webhook struct {
	url     string
	origin  string
	logf    func(string, ...any)
	timeout time.Duration
	client  *http.Client

	queue chan queued
	// stop ends the delivery in progress and drops the rest when Close
	// has waited its time limit.
	stop    context.Context
	cancel  context.CancelFunc
	drained chan struct{}

	mu     sync.Mutex
	closed bool
}

// queued is one alert waiting for delivery.
type queued struct {
	event string
	jobID string
	body  []byte
}

// ValidURL accepts an http or https URL with a host. Its error never
// contains the URL.
func ValidURL(webhook string) error {
	u, err := url.Parse(webhook)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an http or https URL with a host")
	}
	return nil
}

// New returns a Webhook for opts. Its error never contains the URL.
func New(opts Options) (*Webhook, error) {
	if err := ValidURL(opts.URL); err != nil {
		return nil, fmt.Errorf("alert webhook %w", err)
	}
	w := &Webhook{url: opts.URL, origin: opts.Origin, logf: opts.Logf, timeout: opts.Timeout}
	if w.logf == nil {
		w.logf = log.Printf
	}
	if w.timeout <= 0 {
		w.timeout = Timeout
	}
	// A redirect ends the delivery before net/http reads its Location, so
	// the alert and any credential in the URL go only where they were
	// configured to.
	w.client = &http.Client{
		Transport:     refuseRedirects{next: http.DefaultTransport},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errRedirect },
	}
	w.queue = make(chan queued, QueueSize)
	w.stop, w.cancel = context.WithCancel(context.Background())
	w.drained = make(chan struct{})
	go w.run()
	return w, nil
}

// JobFailed sends a job-failed alert.
func (w *Webhook) JobFailed(s jobs.Snapshot) { w.send(EventJobFailed, s) }

// JobLongRunning sends a job-long-running alert.
func (w *Webhook) JobLongRunning(s jobs.Snapshot) { w.send(EventJobLongRunning, s) }

// Close stops new alerts and waits, up to the delivery time limit, for the
// queued ones to be delivered. Those still queued then are dropped.
func (w *Webhook) Close() {
	w.mu.Lock()
	if !w.closed {
		w.closed = true
		close(w.queue)
	}
	w.mu.Unlock()
	limit := time.NewTimer(w.timeout)
	defer limit.Stop()
	select {
	case <-w.drained:
	case <-limit.C:
		w.cancel()
		<-w.drained
	}
}

// run delivers the queued alerts one at a time, in order.
func (w *Webhook) run() {
	defer close(w.drained)
	defer w.cancel()
	for a := range w.queue {
		err := errors.New("shutting down")
		if w.stop.Err() == nil {
			err = w.deliver(a.body)
		}
		if err != nil {
			w.logf("menagerie: %s alert for job %s not delivered: %v", a.event, a.jobID, err)
		}
	}
}

func (w *Webhook) send(event string, s jobs.Snapshot) {
	body, err := json.Marshal(Payload{
		Event:     event,
		JobID:     s.JobID,
		Workspace: s.Workspace,
		Operation: s.Operation,
		Phase:     s.Phase,
		State:     s.State,
		Error:     s.Error,
		Console:   ConsoleURL(w.origin, s),
	})
	if err != nil {
		w.logf("menagerie: %s alert for job %s: %v", event, s.JobID, err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	select {
	case w.queue <- queued{event: event, jobID: s.JobID, body: body}:
	default:
		w.logf("menagerie: %s alert for job %s dropped: %d alerts already wait", event, s.JobID, QueueSize)
	}
}

// errRedirect ends a delivery that the webhook answered with a redirect.
var errRedirect = errors.New("redirect refused")

// refuseRedirects turns every 3xx answer into errRedirect, so the client
// never parses a Location header, whose text could hold the webhook URL.
type refuseRedirects struct{ next http.RoundTripper }

func (t refuseRedirects) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err == nil && resp.StatusCode >= 300 && resp.StatusCode <= 399 {
		resp.Body.Close()
		return nil, errRedirect
	}
	return resp, err
}

// deliver posts body once. Its error is one of a fixed set of messages:
// net/http error text can carry the webhook URL, or a Location built from
// it, so it is never passed on.
func (w *Webhook) deliver(body []byte) error {
	ctx, cancel := context.WithTimeout(w.stop, w.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("request error")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	switch {
	case errors.Is(err, errRedirect):
		return errRedirect
	case err != nil && ctx.Err() != nil:
		return errors.New("timeout")
	case err != nil:
		return errors.New("transport error")
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// ConsoleURL is the browser address of the job's console: the screen that
// runs the job's operation, with the job selected.
func ConsoleURL(origin string, s jobs.Snapshot) string {
	job := "?job=" + url.QueryEscape(s.JobID)
	switch s.Operation {
	case "create":
		return origin + "/create" + job
	case "upgrade":
		return origin + "/workspaces/" + url.PathEscape(s.Workspace) + "/upgrade" + job
	default:
		return origin + "/" + job
	}
}
