package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/t3code"
)

// fakeMinter answers one token or error and records the containers asked.
type fakeMinter struct {
	err   error
	asked []string
}

const sampleToken = "SAMPLETOKEN123"

func (f *fakeMinter) Mint(_ context.Context, id string) (t3code.PairingToken, error) {
	f.asked = append(f.asked, id)
	if f.err != nil {
		return t3code.PairingToken{}, f.err
	}
	return t3code.PairingToken{Token: sampleToken, ExpiresAt: "2030-01-02T03:04:05.000Z"}, nil
}

const pairingPath = "/api/workspaces/%s/t3-code-pairing-tokens"

func pairingURL(name string) string { return strings.Replace(pairingPath, "%s", name, 1) }

func TestMintAnswersTheTokenOfTheWorkspacesRunningContainer(t *testing.T) {
	f := newFixture(t, nil)
	resp, body := f.do(t, http.MethodPost, pairingURL("alpha"), "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
	var token t3code.PairingToken
	if err := json.Unmarshal(body, &token); err != nil {
		t.Fatal(err)
	}
	if token.Token != sampleToken || token.ExpiresAt == "" {
		t.Errorf("token = %+v", token)
	}
	if len(f.tokens.asked) != 1 || f.tokens.asked[0] != "c-alpha" {
		t.Errorf("asked %v", f.tokens.asked)
	}
}

func TestMintTakesNoJobSlot(t *testing.T) {
	f := newFixture(t, nil)
	f.rt.hold = make(chan struct{})
	defer close(f.rt.hold)
	f.rt.entered = make(chan string, 1)
	resp, body := f.do(t, http.MethodPost, "/api/jobs", `{"operation":"stop","workspace":"alpha"}`)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("stop: %d %s", resp.StatusCode, body)
	}
	<-f.rt.entered
	if resp, body := f.do(t, http.MethodPost, pairingURL("alpha"), ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint during a job: %d %s", resp.StatusCode, body)
	}
	if len(f.manager.List()) != 1 {
		t.Errorf("jobs = %d, want only the stop job", len(f.manager.List()))
	}
}

func TestMintRefusals(t *testing.T) {
	for _, tc := range []struct {
		name      string
		workspace string
		statusErr error
		mintErr   error
		status    int
		kind      string
		minted    bool
	}{
		{name: "invalid name", workspace: ".hidden", status: http.StatusBadRequest, kind: "invalid-workspace-name"},
		{name: "no folder", workspace: "absent", status: http.StatusNotFound, kind: "workspace-not-found"},
		{name: "stopped", workspace: "beta", status: http.StatusConflict, kind: "workspace-not-running"},
		{name: "no container", workspace: "gamma", status: http.StatusConflict, kind: "workspace-not-running"},
		{name: "docker down", workspace: "alpha", statusErr: errors.New("no daemon"), status: http.StatusServiceUnavailable, kind: "docker-unavailable"},
		{name: "stopped since", workspace: "alpha", mintErr: t3code.ErrNotRunning, status: http.StatusConflict, kind: "workspace-not-running", minted: true},
		{name: "not installed", workspace: "alpha", mintErr: t3code.ErrNotInstalled, status: http.StatusConflict, kind: "t3-code-not-installed", minted: true},
		{name: "no token", workspace: "alpha", mintErr: t3code.ErrFailed, status: http.StatusBadGateway, kind: "pairing-token-failed", minted: true},
		{name: "docker fails", workspace: "alpha", mintErr: errors.New("connection refused"), status: http.StatusServiceUnavailable, kind: "docker-unavailable", minted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.rt.statusErr = tc.statusErr
			f.tokens.err = tc.mintErr
			resp, body := f.do(t, http.MethodPost, pairingURL(tc.workspace), "")
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d, want %d: %s", resp.StatusCode, tc.status, body)
			}
			var got struct{ Error apiError }
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			if got.Error.Kind != tc.kind {
				t.Errorf("kind = %q, want %q", got.Error.Kind, tc.kind)
			}
			if (len(f.tokens.asked) > 0) != tc.minted {
				t.Errorf("asked the minter: %v, want %v", f.tokens.asked, tc.minted)
			}
		})
	}
}

// Everything but the mint itself still works when a service has no minter.
func TestNoMinterMintsNothing(t *testing.T) {
	f := newFixture(t, nil)
	f.server.opts.PairingTokens = nil
	resp, _ := f.do(t, http.MethodPost, pairingURL("alpha"), "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d", resp.StatusCode)
	}
}
