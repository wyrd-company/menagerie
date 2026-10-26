package backup

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/backup/backuptest"
)

// lockedBuffer is a progress writer safe for go-git's sideband goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestTheRedactorReplacesValuesSplitAcrossWrites(t *testing.T) {
	const url = "https://git.example.invalid/" + sentinelRemote + ".git"
	text := "remote: rejected " + sampleToken + " for " + url + "\rremote: 50%\r\nremote: kept text\nend " + sampleToken
	want := "remote: rejected [redacted] for [redacted]\rremote: 50%\r\nremote: kept text\nend [redacted]"
	for split := 0; split <= len(text); split++ {
		var out bytes.Buffer
		r := newRedactor(&out, sampleToken, url)
		_, _ = r.Write([]byte(text[:split]))
		_, _ = r.Write([]byte(text[split:]))
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if out.String() != want {
			t.Fatalf("split at %d: %q", split, out.String())
		}
	}
	// One byte at a time; each line is written once it ends.
	var out bytes.Buffer
	r := newRedactor(&out, sampleToken)
	for i := range len(text) {
		_, _ = r.Write([]byte{text[i]})
		if i == strings.Index(text, "\r") && !strings.HasSuffix(out.String(), "\r") {
			t.Fatalf("a line ending at \\r was held back: %q", out.String())
		}
	}
	_ = r.Close()
	if strings.Contains(out.String(), sampleToken) || !strings.Contains(out.String(), "remote: kept text\n") {
		t.Fatalf("byte by byte: %q", out.String())
	}
}

func TestTheRedactorBoundsALineWithoutAnEnd(t *testing.T) {
	var out bytes.Buffer
	r := newRedactor(&out, sampleToken)
	long := strings.Repeat("x", maxLine-5)
	// The token straddles the point where the long line is written in
	// parts.
	_, _ = r.Write([]byte(long + sampleToken[:8]))
	_, _ = r.Write([]byte(sampleToken[8:] + strings.Repeat("y", maxLine)))
	if out.Len() == 0 {
		t.Fatal("a line longer than the bound was held whole")
	}
	_ = r.Close()
	if got := out.String(); strings.Contains(got, sampleToken) || got != long+redacted+strings.Repeat("y", maxLine) {
		t.Fatalf("long line: %d bytes, token kept %t", len(got), strings.Contains(got, sampleToken))
	}
}

func TestTheRedactorPassesTextThroughWithoutValues(t *testing.T) {
	var out bytes.Buffer
	r := newRedactor(&out, "")
	_, _ = r.Write([]byte("partial"))
	if out.String() != "partial" {
		t.Fatalf("no values: %q", out.String())
	}
	if err := newRedactor(nil, sampleToken).Close(); err != nil {
		t.Fatal(err)
	}
}

// TestTheRemotesTextNamesNeitherTheTokenNorTheURL: real server-side Git
// writes the token, its basic-authentication form and the URL into the
// sideband, split across writes, on a push and on a fetch. The job's
// progress keeps the remote's other text.
func TestTheRemotesTextNamesNeitherTheTokenNorTheURL(t *testing.T) {
	host := backuptest.NewHTTPS(t, t.TempDir(), "x-access-token", sampleToken)
	bare(t, host.Dir, sentinelRemote+".git")
	url := host.URL(sentinelRemote + ".git")
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + sampleToken))
	remote := &Remote{URL: url, Auth: TokenAuth(&fakeTokens{prefix: host.Server.URL, token: sampleToken}, nil)}
	// The script writes each value in two parts, apart in time, so they
	// reach the client in separate sideband packets.
	script := "#!/bin/sh\n" +
		"printf 'diagnostic: %s' '" + sampleToken[:7] + "' >&2\nsleep 0.2\n" +
		"printf '%s at %s' '" + sampleToken[7:] + "' '" + url[:12] + "' >&2\nsleep 0.2\n" +
		"printf '%s\\n' '" + url[12:] + "' >&2\n" +
		"printf 'auth %s\\n' '" + basic + "' >&2\n" +
		"printf 'tail %s' '" + sampleToken + "' >&2\n"
	source := committedWorkspace(t)
	if _, err := remote.Push(context.Background(), source, "alpha", nil); err != nil {
		t.Fatal(err)
	}

	check := func(op, text string) {
		t.Helper()
		for _, v := range []string{sampleToken, url, basic, sentinelRemote} {
			if strings.Contains(text, v) {
				t.Fatalf("%s progress carries %q:\n%s", op, v, text)
			}
		}
		if !strings.Contains(text, "diagnostic: [redacted] at [redacted]") || !strings.Contains(text, "auth [redacted]") || !strings.Contains(text, "tail [redacted]") {
			t.Fatalf("%s progress lost the remote's text:\n%s", op, text)
		}
	}

	// Push: a pre-receive hook that rejects.
	hook := filepath.Join(host.Dir, sentinelRemote+".git", "hooks", "pre-receive")
	if err := os.MkdirAll(filepath.Dir(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte(script+"exit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, source, "next.txt", "next\n")
	if _, err := CommitAll(source, "alpha", "next", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var pushed lockedBuffer
	if _, err := remote.Push(context.Background(), source, "alpha", &pushed); err == nil {
		t.Fatal("the hook did not reject the push")
	}
	check("push", pushed.String())

	// Fetch: pack-objects run through a hook that writes first.
	wrapper := filepath.Join(t.TempDir(), "pack-objects")
	if err := os.WriteFile(wrapper, []byte(script+"exec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(host.Home, ".gitconfig"), []byte("[uploadpack]\n\tpackObjectsHook = "+wrapper+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The fetching workspace's own commit differs, so the fetch needs a
	// pack.
	other := newWorkspace(t, "alpha")
	write(t, other, "other.txt", "other\n")
	if _, err := CommitRoot(other, "alpha", Output{Record: "other.txt"}, "other", &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var fetched lockedBuffer
	if _, err := remote.Fetch(context.Background(), other, "alpha", "alpha", &fetched); err != nil {
		t.Fatal(err)
	}
	check("fetch", fetched.String())
}
