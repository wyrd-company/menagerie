// ---
// relationships:
//   implements: menagerie
//   references: workspace-api
// ---

package backup

import (
	"bytes"
	"io"
	"slices"
	"sync"
)

// redacted replaces a known value in the remote's text.
const redacted = "[redacted]"

// maxLine bounds the text a redactor holds while it waits for the end of
// a line.
const maxLine = 4096

// redactor writes the remote's progress text to w line by line, a line
// ending at '\n' or '\r', with every known value replaced by redacted. A
// value split across writes is still replaced. The text is read once,
// from the start: at each position the longest value that matches is
// replaced, so a value that contains another goes whole, and replacement
// text is never read again. Close writes what is left.
type redactor struct {
	mu sync.Mutex
	w  io.Writer
	// values are the known values, longest first.
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
	slices.SortStableFunc(r.values, func(a, b []byte) int { return len(b) - len(a) })
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
	// A line longer than maxLine is written in parts. Only positions
	// before the last longest-1 bytes are read, so every value that
	// starts at one of them lies whole in the buffer, and a value that
	// starts in the rest waits for the bytes that complete it.
	if len(r.buf) > maxLine {
		if err := r.emit(len(r.buf) - (r.longest - 1)); err != nil {
			return len(p), err
		}
	}
	return len(p), nil
}

// emit reads the buffer from its start up to position limit, writes it
// redacted, and keeps what it did not read. A value that starts before
// limit is read whole.
func (r *redactor) emit(limit int) error {
	var out []byte
	i := 0
	for i < limit {
		if v := r.match(r.buf[i:]); v > 0 {
			out = append(out, redacted...)
			i += v
			continue
		}
		out = append(out, r.buf[i])
		i++
	}
	r.buf = append(r.buf[:0], r.buf[i:]...)
	_, err := r.w.Write(out)
	return err
}

// match is the length of the longest value b starts with, or 0.
func (r *redactor) match(b []byte) int {
	for _, v := range r.values {
		if bytes.HasPrefix(b, v) {
			return len(v)
		}
	}
	return 0
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
