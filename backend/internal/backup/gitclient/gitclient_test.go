package gitclient

import (
	"context"
	"encoding/pem"
	"errors"
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

// TestTheInstalledTransportTakesEndpointOptions: sessions with a CA
// bundle, with TLS verification off and through a proxy reach the server,
// as with go-git's own default client. Each server answers 404, so
// transport.ErrRepositoryNotFound means the request arrived.
func TestTheInstalledTransportTakesEndpointOptions(t *testing.T) {
	tlsServer := httptest.NewTLSServer(http.NotFoundHandler())
	defer tlsServer.Close()
	bundle := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw})
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		http.NotFound(w, r)
	}))
	defer proxy.Close()
	cases := map[string]func(ep *transport.Endpoint){
		"CA bundle":    func(ep *transport.Endpoint) { ep.CaBundle = bundle },
		"insecure TLS": func(ep *transport.Endpoint) { ep.InsecureSkipTLS = true },
		"proxy": func(ep *transport.Endpoint) {
			ep.Protocol, ep.Host, ep.Port = "http", "repository.example.invalid", 80
			ep.Proxy = transport.ProxyOptions{URL: proxy.URL}
		},
	}
	for name, set := range cases {
		ep, err := transport.NewEndpoint(tlsServer.URL + "/repo.git")
		if err != nil {
			t.Fatal(err)
		}
		set(ep)
		c, err := client.NewClient(ep)
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.NewUploadPackSession(ep, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := s.AdvertisedReferencesContext(context.Background()); !errors.Is(err, transport.ErrRepositoryNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if proxied.Load() == 0 {
		t.Error("the proxy was not used")
	}
}
