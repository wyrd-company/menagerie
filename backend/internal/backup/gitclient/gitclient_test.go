package gitclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
)

// TestTheInstalledTransportRefusesRedirectsOfMarkedRequestsOnly uses the
// transport importing the package installed, as production does.
func TestTheInstalledTransportRefusesRedirectsOfMarkedRequestsOnly(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		http.NotFound(w, r)
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/other.git/info/refs?"+r.URL.RawQuery, http.StatusFound)
	}))
	defer origin.Close()
	refs := func(ctx context.Context) {
		t.Helper()
		ep, err := transport.NewEndpoint(origin.URL + "/repo.git")
		if err != nil {
			t.Fatal(err)
		}
		c, err := client.NewClient(ep)
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.NewUploadPackSession(ep, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.AdvertisedReferencesContext(ctx); err == nil {
			t.Fatal("the redirect's answer was taken as references")
		}
	}
	refs(WithoutRedirects(context.Background()))
	if n := reached.Load(); n != 0 {
		t.Fatalf("a marked request followed the redirect (%d requests)", n)
	}
	refs(context.Background())
	if reached.Load() == 0 {
		t.Fatal("an unmarked request did not keep go-git's redirect policy")
	}
}

func TestInstallCoversHTTPAndHTTPS(t *testing.T) {
	if client.Protocols["http"] != client.Protocols["https"] {
		t.Fatal("http and https use different transports")
	}
	defer Install(nil)
	Install(&http.Client{})
	if client.Protocols["http"] != client.Protocols["https"] {
		t.Fatal("Install left one scheme on another transport")
	}
}
