package githubapp

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const sentinelAmbientToken = "sentinel-ambient-token-9e02"

func TestTransitSignsWithPKCS1v15OverSHA256WithoutAnAmbientToken(t *testing.T) {
	for _, name := range []string{"BAO_TOKEN", "VAULT_TOKEN", "BAO_NAMESPACE", "VAULT_NAMESPACE"} {
		t.Setenv(name, sentinelAmbientToken)
	}
	for _, prefix := range []string{"vault:v1:", "vault:v12:"} {
		v := newFakeVault(t, transitAnswer(prefix))
		signer := testTransit(t, v.address)
		input := []byte("header.claims")
		sig, err := signer.Sign(context.Background(), input)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(input)
		if err := rsa.VerifyPKCS1v15(&appKey().PublicKey, crypto.SHA256, sum[:], sig); err != nil {
			t.Fatalf("%s: signature does not verify: %v", prefix, err)
		}
		if v.paths[0] != "/v1/transit/sign/sample-app-key/sha2-256" {
			t.Fatalf("path %s", v.paths[0])
		}
		if want := map[string]any{"input": "aGVhZGVyLmNsYWltcw==", "signature_algorithm": "pkcs1v15"}; len(v.bodies[0]) != 2 || v.bodies[0]["input"] != want["input"] || v.bodies[0]["signature_algorithm"] != want["signature_algorithm"] {
			t.Fatalf("body %v", v.bodies[0])
		}
		for name, values := range v.headers[0] {
			for _, value := range values {
				if strings.Contains(value, sentinelAmbientToken) {
					t.Fatalf("header %s carries the ambient token", name)
				}
			}
		}
		for _, h := range []string{"X-Vault-Token", "Authorization", "X-Vault-Namespace"} {
			if v.headers[0].Get(h) != "" {
				t.Fatalf("header %s sent", h)
			}
		}
	}
}

func TestTransitReasons(t *testing.T) {
	cases := []struct {
		name   string
		answer func(http.ResponseWriter, map[string]any)
		reason string
	}{
		{"denied", statusAnswer(http.StatusForbidden, `{"errors":["permission denied"]}`), "OpenBao denied signing with the Transit key"},
		{"not bound", statusAnswer(http.StatusServiceUnavailable, `{"wyrwood":{"state":"not-bound"}}`), "Wyrwood holds no OpenBao token for Menagerie (state not-bound)"},
		{"no token", statusAnswer(http.StatusServiceUnavailable, `{"wyrwood":{"state":"no-token"}}`), "Wyrwood holds no OpenBao token for Menagerie (state no-token)"},
		{"other state", statusAnswer(http.StatusServiceUnavailable, `{"wyrwood":{"state":"elsewhere"}}`), "OpenBao answered status 503 to the signing request"},
		{"missing key", statusAnswer(http.StatusBadRequest, `{"errors":["signing key not found"]}`), "OpenBao answered status 400 to the signing request"},
		{"no prefix", statusAnswer(http.StatusOK, `{"data":{"signature":"AAAA"}}`), "OpenBao returned a malformed signature"},
		{"bad base64", statusAnswer(http.StatusOK, `{"data":{"signature":"vault:v1:!!"}}`), "OpenBao returned a malformed signature"},
		{"empty", statusAnswer(http.StatusOK, `{"data":{"signature":"vault:v1:"}}`), "OpenBao returned a malformed signature"},
		{"not JSON", statusAnswer(http.StatusOK, `signature`), "OpenBao returned a malformed signature"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := testTransit(t, newFakeVault(t, c.answer).address).Sign(context.Background(), []byte("x"))
			var e *Error
			if !errors.As(err, &e) || e.Reason != c.reason {
				t.Fatalf("%v, want %q", err, c.reason)
			}
		})
	}
}

func TestNewTransitRefusesAKeyNameThatIsNotOneSegment(t *testing.T) {
	for _, key := range []string{"", "a/b", "../x", ".hidden", "-x", "a b", "a?b", strings.Repeat("k", 129)} {
		if _, err := NewTransit("unix:///nonexistent/vault.sock", key); err == nil || (key != "" && strings.Contains(err.Error(), key)) {
			t.Errorf("%q: %v", key, err)
		}
	}
	for _, key := range []string{"menagerie-github-app", "a", "A_b.c-1"} {
		if !ValidTransitKey(key) {
			t.Errorf("%q refused", key)
		}
	}
}
