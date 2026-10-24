package auth

import (
	"strings"
	"testing"
)

func TestPendingLoginsAreBounded(t *testing.T) {
	p := newPendingLogins(2)
	p.add("a", pendingLogin{nonce: "1"})
	p.add("b", pendingLogin{nonce: "2"})
	p.add("c", pendingLogin{nonce: "3"})
	if _, ok := p.take("a"); ok {
		t.Fatal("oldest pending sign-in kept")
	}
	if l, ok := p.take("c"); !ok || l.nonce != "3" {
		t.Fatal("newest pending sign-in lost")
	}
	if _, ok := p.take("c"); ok {
		t.Fatal("pending sign-in taken twice")
	}
	if l, ok := p.take("b"); !ok || l.nonce != "2" {
		t.Fatal("pending sign-in lost")
	}
}

func TestConfigValidation(t *testing.T) {
	bad := map[string]string{
		"ftp://menagerie.example.invalid/auth/callback":          "must use http or https",
		"https:///auth/callback":                                 "must name a host",
		"https://user@menagerie.example.invalid/auth/callback":   "must not hold user information",
		"https://menagerie.example.invalid/auth/callback?x=1":    "must not have a query or fragment",
		"https://menagerie.example.invalid/auth/callback?":       "must not have a query or fragment",
		"https://menagerie.example.invalid/auth/callback#x":      "must not have a query or fragment",
		"https://menagerie.example.invalid/auth/callback/":       "must have the path /auth/callback",
		"https://menagerie.example.invalid/prefix/auth/callback": "must have the path /auth/callback",
		"menagerie.example.invalid/auth/callback":                "must use http or https",
	}
	for raw, want := range bad {
		if err := ValidateRedirectURL(raw); err == nil || err.Error() != want {
			t.Errorf("ValidateRedirectURL(%q) = %v, want %q", raw, err, want)
		}
	}
	for _, ok := range []string{"https://menagerie.example.invalid/auth/callback", "http://127.0.0.1:8080/auth/callback"} {
		if err := ValidateRedirectURL(ok); err != nil {
			t.Errorf("ValidateRedirectURL(%q) = %v", ok, err)
		}
	}
	if err := ValidateScopes([]string{"profile"}); err == nil {
		t.Error("scopes without openid accepted")
	}
	if err := ValidateScopes([]string{"openid", "two words"}); err == nil {
		t.Error("scope with a space accepted")
	}
	if err := ValidateScopes(nil); err != nil {
		t.Error(err)
	}
	if _, err := New(Config{Issuer: "issuer.example.invalid", ClientID: "", RedirectURL: "x"}); err == nil ||
		!strings.Contains(err.Error(), "issuer must be") || !strings.Contains(err.Error(), "client id") || !strings.Contains(err.Error(), "redirect URL") {
		t.Errorf("New accepted an invalid configuration: %v", err)
	}
}
