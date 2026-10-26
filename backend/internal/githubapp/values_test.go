package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Distinctive values that no error may carry.
const (
	sentinelBody      = "sentinel-body-4b7a"
	sentinelToken     = "sentinel-token-81c3"
	sentinelSignature = "sentinel-signature-0f6d"
	sentinelOwner     = "sentinel-owner-2a"
	sentinelRepo      = "sentinel-repo-9c"
	sentinelPath      = "sentinel-path-55e1"
	// jwtHeader is the base64url of every App JWT's header.
	jwtHeader = "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9"
)

// TestNoFailureCarriesAValue feeds distinctive values through every
// failure path of a token: the Transit signer, the installation lookup,
// the token request, the transport and the context. No error, and no
// answer of the credential route, carries a token, a JWT, a signature, a
// response body, a URL, a host or a socket path.
func TestNoFailureCarriesAValue(t *testing.T) {
	type world struct {
		minter *Minter
		ctx    context.Context
		values []string
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	// github answers every request with answer.
	github := func(t *testing.T, installations map[string]int64, answer func(w http.ResponseWriter, r *http.Request)) world {
		g := newGitHub(t, newClock())
		g.answer = func(w http.ResponseWriter, r *http.Request) bool { answer(w, r); return true }
		m, _ := g.minter(t, installations)
		return world{minter: m, values: []string{strings.TrimPrefix(g.server.URL, "https://")}}
	}
	status := func(code int) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, fmt.Sprintf(`{"message":%q,"token":%q}`, sentinelBody, sentinelToken), code)
		}
	}
	body := func(code int, text string) func(http.ResponseWriter, *http.Request) {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(text))
		}
	}
	installed := map[string]int64{sentinelOwner: sampleInstallation}
	// transit signs through a fake vault answering answer.
	transit := func(t *testing.T, answer func(http.ResponseWriter, map[string]any)) world {
		v := newFakeVault(t, answer)
		g := newGitHub(t, newClock())
		signer := testTransit(t, v.address)
		m, err := New(Options{AppID: sampleAppID, Signer: signer, Installations: installed, Server: g.server.URL, Client: g.server.Client(), Now: g.clock.Now})
		if err != nil {
			t.Fatal(err)
		}
		return world{minter: m, values: []string{"vault.sock", strings.TrimPrefix(v.address, "unix://")}}
	}
	cases := map[string]func(t *testing.T) world{
		"transit denied": func(t *testing.T) world {
			return transit(t, statusAnswer(http.StatusForbidden, `{"errors":["`+sentinelBody+`"]}`))
		},
		"transit failed": func(t *testing.T) world {
			return transit(t, statusAnswer(http.StatusInternalServerError, `{"errors":["`+sentinelBody+`"]}`))
		},
		"transit unavailable": func(t *testing.T) world {
			return transit(t, statusAnswer(http.StatusServiceUnavailable, `{"errors":["`+sentinelBody+`"]}`))
		},
		"transit malformed": func(t *testing.T) world {
			return transit(t, statusAnswer(http.StatusOK, `{"data":{"signature":"`+sentinelSignature+`"}}`))
		},
		"transit malformed base64": func(t *testing.T) world {
			return transit(t, statusAnswer(http.StatusOK, `{"data":{"signature":"vault:v1:`+sentinelSignature+`"}}`))
		},
		"transit unreachable": func(t *testing.T) world {
			dir, err := os.MkdirTemp("", sentinelPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			signer := testTransit(t, "unix://"+filepath.Join(dir, "vault.sock"))
			m, err := New(Options{AppID: sampleAppID, Signer: signer, Installations: installed})
			if err != nil {
				t.Fatal(err)
			}
			return world{minter: m, values: []string{"vault.sock", dir}}
		},
		"transit context ended": func(t *testing.T) world {
			w := transit(t, transitAnswer("vault:v1:"))
			w.ctx = ended
			return w
		},
		"installation lookup refused":   func(t *testing.T) world { return github(t, nil, status(http.StatusUnauthorized)) },
		"installation not found":        func(t *testing.T) world { return github(t, nil, status(http.StatusNotFound)) },
		"installation lookup failed":    func(t *testing.T) world { return github(t, nil, status(http.StatusInternalServerError)) },
		"installation lookup malformed": func(t *testing.T) world { return github(t, nil, body(http.StatusOK, sentinelBody)) },
		"token 401":                     func(t *testing.T) world { return github(t, installed, status(http.StatusUnauthorized)) },
		"token 403":                     func(t *testing.T) world { return github(t, installed, status(http.StatusForbidden)) },
		"token 404":                     func(t *testing.T) world { return github(t, installed, status(http.StatusNotFound)) },
		"token 422":                     func(t *testing.T) world { return github(t, installed, status(http.StatusUnprocessableEntity)) },
		"token 500":                     func(t *testing.T) world { return github(t, installed, status(http.StatusInternalServerError)) },
		"token 503":                     func(t *testing.T) world { return github(t, installed, status(http.StatusServiceUnavailable)) },
		"token not JSON":                func(t *testing.T) world { return github(t, installed, body(http.StatusCreated, sentinelBody)) },
		"token without expiry": func(t *testing.T) world {
			return github(t, installed, body(http.StatusCreated, `{"token":"`+sentinelToken+`"}`))
		},
		"token with a malformed expiry": func(t *testing.T) world {
			return github(t, installed, body(http.StatusCreated, `{"token":"`+sentinelToken+`","expires_at":"`+sentinelBody+`"}`))
		},
		"redirect": func(t *testing.T) world {
			return github(t, installed, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://"+sentinelPath+".example.invalid/"+sentinelToken, http.StatusFound)
			})
		},
		"GitHub unreachable": func(t *testing.T) world {
			s := httptest.NewTLSServer(http.NotFoundHandler())
			s.Close()
			m, err := New(Options{AppID: sampleAppID, Signer: &localSigner{key: appKey()}, Installations: installed, Server: s.URL, Client: s.Client()})
			if err != nil {
				t.Fatal(err)
			}
			return world{minter: m, values: []string{strings.TrimPrefix(s.URL, "https://")}}
		},
		"untrusted certificate": func(t *testing.T) world {
			s := httptest.NewTLSServer(http.NotFoundHandler())
			t.Cleanup(s.Close)
			m, err := New(Options{AppID: sampleAppID, Signer: &localSigner{key: appKey()}, Installations: installed, Server: s.URL})
			if err != nil {
				t.Fatal(err)
			}
			return world{minter: m, values: []string{strings.TrimPrefix(s.URL, "https://"), "x509"}}
		},
		"signer failed": func(t *testing.T) world {
			g := newGitHub(t, newClock())
			m, err := New(Options{AppID: sampleAppID, Signer: failingSigner{}, Installations: installed, Server: g.server.URL, Client: g.server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			return world{minter: m}
		},
		"GitHub context ended": func(t *testing.T) world {
			w := github(t, installed, status(http.StatusCreated))
			w.ctx = ended
			return w
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := c(t)
			ctx := w.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			repo := Repository{Owner: sentinelOwner, Name: sentinelRepo}
			token, err := w.minter.Token(ctx, repo, Write)
			var e *Error
			if !errors.As(err, &e) || token != "" {
				t.Fatalf("token %q, error %v", token, err)
			}
			values := append([]string{sentinelBody, sentinelToken, sentinelSignature, sentinelOwner, sentinelRepo, sentinelPath, jwtHeader, "Bearer", "https://"}, w.values...)
			for _, v := range values {
				if strings.Contains(err.Error(), v) {
					t.Fatalf("the error carries %q: %v", v, err)
				}
			}
			if w.ctx != nil && !errors.Is(err, context.Canceled) {
				t.Fatalf("an ended context is not reported as such: %v", err)
			}
			// The credential route answers the same failure with the
			// reason alone.
			url := "https://" + w.minter.host + "/" + sentinelOwner + "/" + sentinelRepo
			req := httptest.NewRequest(http.MethodPost, CredentialPath, strings.NewReader(`{"url":"`+url+`"}`)).WithContext(ctx)
			rec := httptest.NewRecorder()
			Handler(w.minter).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("route %d %s", rec.Code, rec.Body)
			}
			for _, v := range values {
				if strings.Contains(rec.Body.String(), v) {
					t.Fatalf("the route's answer carries %q: %s", v, rec.Body)
				}
			}
		})
	}
}

// failingSigner fails with an error whose text carries a sentinel.
type failingSigner struct{}

func (failingSigner) Sign(context.Context, []byte) ([]byte, error) {
	return nil, errors.New("signer said " + sentinelBody)
}
