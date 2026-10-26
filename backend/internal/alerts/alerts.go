// ---
// relationships:
//   implements: menagerie
//   references: [configuration, job-without-timeout-blocks-workspace]
// ---

// Package alerts tells a webhook about failed and long-running jobs. Each
// alert is one JSON POST, made in the background with one attempt and a
// time limit, so an alert never delays or fails a job. The webhook URL can
// carry a credential: it is never written to a log or an error.
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

// Timeout bounds each delivery, and so how long Close waits.
const Timeout = 10 * time.Second

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

// Webhook delivers alerts. It satisfies jobs.Alerts.
type Webhook struct {
	url     string
	origin  string
	logf    func(string, ...any)
	timeout time.Duration
	client  *http.Client

	mu      sync.Mutex
	closed  bool
	pending sync.WaitGroup
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
	// A redirect is reported as a non-2xx answer rather than followed, so
	// the alert goes only where it was configured to.
	w.client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return w, nil
}

// JobFailed sends a job-failed alert.
func (w *Webhook) JobFailed(s jobs.Snapshot) { w.send(EventJobFailed, s) }

// JobLongRunning sends a job-long-running alert.
func (w *Webhook) JobLongRunning(s jobs.Snapshot) { w.send(EventJobLongRunning, s) }

// Close stops new deliveries and waits for those in flight, each of which
// ends within the delivery time limit.
func (w *Webhook) Close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
	w.pending.Wait()
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
	w.pending.Add(1)
	go func() {
		defer w.pending.Done()
		if err := w.deliver(body); err != nil {
			w.logf("menagerie: %s alert for job %s not delivered: %v", event, s.JobID, err)
		}
	}()
}

func (w *Webhook) deliver(body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), w.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("the webhook request could not be built")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return withoutURL(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the webhook answered %d", resp.StatusCode)
	}
	return nil
}

// withoutURL drops the request URL that net/http adds to a client error.
func withoutURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
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
