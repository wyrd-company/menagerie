// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

package registrycreds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	openbao "github.com/openbao/openbao/api/v2"
	"github.com/wyrd-company/devcontainer-builder/pkg/registryauth"
)

// maxResponse bounds the body read from the vault socket.
const maxResponse = 1 << 20

// OpenBao reads registry credentials from a KV version 2 mount through the
// Wyrwood vault socket. Wyrwood replaces the request's token with the
// container's own and owns its renewal, so the client sends no token and no
// namespace, and reads no OpenBao settings from the environment.
type OpenBao struct {
	client *openbao.Client
	mount  string
}

// SocketAddress is the OpenBao address of the vault socket in a Wyrwood
// consumer directory.
func SocketAddress(wyrwoodDirectory string) string {
	return "unix://" + strings.TrimSuffix(wyrwoodDirectory, "/") + "/vault.sock"
}

// NewOpenBao returns a Fetcher for the KV version 2 mount at address.
func NewOpenBao(address, mount string) (*OpenBao, error) {
	cfg := openbao.NewConfig()
	cfg.Address = address
	// NewConfig reads no environment, so no ambient token, namespace,
	// address or TLS setting applies.
	client, err := openbao.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("OpenBao client: %w", err)
	}
	return &OpenBao{client: client, mount: mount}, nil
}

// Fetch reads the username and password fields of the secret at path.
func (o *OpenBao) Fetch(ctx context.Context, path string) (registryauth.Credentials, error) {
	// The answer's status decides the reason. The client's error text can
	// quote the response body, so it is not reported.
	resp, _ := o.client.Logical().ReadRawWithContext(ctx, o.mount+"/data/"+path)
	if resp != nil {
		defer resp.Body.Close()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return registryauth.Credentials{}, &Error{Reason: "the operation ended before OpenBao answered", cause: ctxErr}
	}
	if resp == nil {
		return registryauth.Credentials{}, &Error{Reason: "the Wyrwood vault socket could not be reached"}
	}
	body := io.LimitReader(resp.Body, maxResponse)
	switch resp.StatusCode {
	case http.StatusOK:
		return credentials(body)
	case http.StatusNotFound:
		return registryauth.Credentials{}, &Error{Reason: "the secret does not exist"}
	case http.StatusForbidden:
		return registryauth.Credentials{}, &Error{Reason: "OpenBao denied reading the secret"}
	case http.StatusServiceUnavailable:
		if state := wyrwoodState(body); state != "" {
			return registryauth.Credentials{}, &Error{Reason: "Wyrwood holds no OpenBao token for Menagerie (state " + state + ")"}
		}
	}
	return registryauth.Credentials{}, &Error{Reason: fmt.Sprintf("OpenBao answered status %d", resp.StatusCode)}
}

// credentials reads a KV version 2 read response. A deleted or destroyed
// version has no data and counts as missing.
func credentials(body io.Reader) (registryauth.Credentials, error) {
	var doc struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(body).Decode(&doc); err != nil {
		return registryauth.Credentials{}, &Error{Reason: "OpenBao returned a malformed response"}
	}
	if doc.Data.Data == nil {
		return registryauth.Credentials{}, &Error{Reason: "the secret has no current version"}
	}
	username, uok := doc.Data.Data["username"].(string)
	password, pok := doc.Data.Data["password"].(string)
	if !uok || !pok || username == "" || password == "" {
		return registryauth.Credentials{}, &Error{Reason: "the secret needs non-empty string fields username and password"}
	}
	return registryauth.Credentials{Username: username, Password: password}, nil
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
