// ---
// relationships:
//   implements: menagerie
//   references: workspace-api
// ---

package backup

import (
	"bytes"
	"io"
	"sync"
)

// redacted replaces a known value in the remote's text.
const redacted = "[redacted]"

// maxLine bounds the text a redactor holds while it waits for the end of
// a line.
const maxLine = 4096

// redactor writes the remote's progress text to w line by line, a line
// ending at '\n' or '\r', with every known value replaced by redacted. A
// value split across writes is still replaced. Close writes what is left.
type redactor struct {
	mu      sync.Mutex
	w       io.Writer
	values  [][]byte
	longest int
	buf     []byte
}

// newRedactor returns w with values redacted, or w itself when there is
// no value or no w.
func newRedactor(w io.Writer, values ...string) io.WriteCloser {
	r := &redactor{w: w}
	for _, v := range values {
		if v != "" {
			r.values = append(r.values, []byte(v))
			r.longest = max(r.longest, len(v))
		}
	}
	if w == nil {
		return nopCloser{io.Discard}
	}
	if len(r.values) == 0 {
		return nopCloser{w}
	}
	return r
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func (r *redactor) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	for {
		end := bytes.IndexAny(r.buf, "\n\r")
		if end < 0 {
			break
		}
		if err := r.emit(end + 1); err != nil {
			return len(p), err
		}
	}
	// A line longer than maxLine is written in parts. The last
	// longest-1 bytes stay, so a value that starts in them is still
	// whole when it is replaced.
	if len(r.buf) > maxLine {
		r.buf = r.redact(r.buf)
		if keep := r.longest - 1; len(r.buf) > keep {
			if err := r.emit(len(r.buf) - keep); err != nil {
				return len(p), err
			}
		}
	}
	return len(p), nil
}

// emit writes the first n bytes of the buffer, redacted.
func (r *redactor) emit(n int) error {
	line := r.redact(r.buf[:n])
	r.buf = append(r.buf[:0], r.buf[n:]...)
	_, err := r.w.Write(line)
	return err
}

func (r *redactor) redact(b []byte) []byte {
	out := append([]byte(nil), b...)
	for _, v := range r.values {
		out = bytes.ReplaceAll(out, v, []byte(redacted))
	}
	return out
}

// Close writes the text that has no line end yet.
func (r *redactor) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) == 0 {
		return nil
	}
	return r.emit(len(r.buf))
}
