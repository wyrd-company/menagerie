// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

package githubapp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"

	openbao "github.com/openbao/openbao/api/v2"
)

// transitKeyPattern is the shape of a Transit key name Menagerie accepts:
// one path segment.
var transitKeyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidTransitKey reports whether key is a Transit key name Menagerie
// accepts: letters, digits, '_', '.' and '-', starting with a letter or
// digit.
func ValidTransitKey(key string) bool { return len(key) <= 128 && transitKeyPattern.MatchString(key) }

// Transit signs App JWTs with an RSA key in OpenBao's Transit engine at
// the mount transit, through the Wyrwood vault socket. Wyrwood replaces
// the request's token with the container's own and owns its renewal, so
// the client sends no token and no namespace, and reads no OpenBao
// settings from the environment.
type Transit struct {
	client *openbao.Client
	key    string
}

// NewTransit returns a Signer for the Transit key named key, reached at
// the OpenBao address, such as registrycreds.SocketAddress's.
func NewTransit(address, key string) (*Transit, error) {
	if !ValidTransitKey(key) {
		return nil, errors.New("github app: the Transit key name is not one path segment")
	}
	cfg := openbao.NewConfig()
	cfg.Address = address
	// NewConfig reads no environment, so no ambient token, namespace,
	// address or TLS setting applies.
	client, err := openbao.NewClient(cfg)
	if err != nil {
		return nil, errors.New("github app: the OpenBao client could not be created")
	}
	return &Transit{client: client, key: key}, nil
}

// signaturePrefix is the "vault:v{n}:" prefix of a Transit signature.
var signaturePrefix = regexp.MustCompile(`^vault:v[0-9]+:`)

// Sign asks Transit to sign input with PKCS#1 v1.5 over SHA-256 and
// returns the raw signature.
func (t *Transit) Sign(ctx context.Context, input []byte) ([]byte, error) {
	req := t.client.NewRequest(http.MethodPost, "/v1/transit/sign/"+t.key+"/sha2-256")
	if err := req.SetJSONBody(map[string]string{
		"input":               base64.StdEncoding.EncodeToString(input),
		"signature_algorithm": "pkcs1v15",
	}); err != nil {
		return nil, &Error{Reason: "the signing request could not be built"}
	}
	// The answer's status decides the reason. The client's error text can
	// quote the response body, so it is not reported.
	resp, _ := t.client.RawRequestWithContext(ctx, req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err := ctx.Err(); err != nil {
		return nil, &Error{Reason: "the operation ended before OpenBao answered", cause: err}
	}
	if resp == nil {
		return nil, &Error{Reason: "the Wyrwood vault socket could not be reached"}
	}
	body := io.LimitReader(resp.Body, maxResponse)
	switch resp.StatusCode {
	case http.StatusOK:
		return signature(body)
	case http.StatusForbidden:
		return nil, &Error{Reason: "OpenBao denied signing with the Transit key"}
	case http.StatusServiceUnavailable:
		if state := wyrwoodState(body); state != "" {
			return nil, &Error{Reason: "Wyrwood holds no OpenBao token for Menagerie (state " + state + ")"}
		}
	}
	return nil, &Error{Reason: fmt.Sprintf("OpenBao answered status %d to the signing request", resp.StatusCode)}
}

// signature reads a Transit sign response: "vault:v{n}:" and the
// signature in standard base64.
func signature(body io.Reader) ([]byte, error) {
	var doc struct {
		Data struct {
			Signature string `json:"signature"`
		} `json:"data"`
	}
	malformed := &Error{Reason: "OpenBao returned a malformed signature"}
	if json.NewDecoder(body).Decode(&doc) != nil {
		return nil, malformed
	}
	prefix := signaturePrefix.FindString(doc.Data.Signature)
	if prefix == "" {
		return nil, malformed
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(doc.Data.Signature, prefix))
	if err != nil || len(sig) == 0 {
		return nil, malformed
	}
	return sig, nil
}

// wyrwoodState is the state of a Wyrwood unavailable answer, or "".
func wyrwoodState(body io.Reader) string {
	var doc struct {
		Wyrwood struct {
			State string `json:"state"`
		} `json:"wyrwood"`
	}
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return ""
	}
	switch doc.Wyrwood.State {
	case "not-bound", "no-token":
		return doc.Wyrwood.State
	}
	return ""
}
