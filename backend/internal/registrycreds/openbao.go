// ---
// relationships:
//   implements: menagerie
//   references: configuration
// ---

package registrycreds

import (
	"context"
	"errors"
	"strings"

	"github.com/wyrd-company/devcontainer-builder/pkg/registryauth"

	"github.com/wyrd-company/menagerie/backend/internal/openbaokv"
)

// OpenBao reads registry credentials from a KV version 2 mount through the
// Wyrwood vault socket.
type OpenBao struct {
	reader *openbaokv.Reader
}

// SocketAddress is the OpenBao address of the vault socket in a Wyrwood
// consumer directory.
func SocketAddress(wyrwoodDirectory string) string {
	return "unix://" + strings.TrimSuffix(wyrwoodDirectory, "/") + "/vault.sock"
}

// NewOpenBao returns a Fetcher for the KV version 2 mount at address.
func NewOpenBao(address, mount string) (*OpenBao, error) {
	reader, err := openbaokv.New(address, mount)
	if err != nil {
		return nil, err
	}
	return &OpenBao{reader: reader}, nil
}

// Fetch reads the username and password fields of the secret at path.
func (o *OpenBao) Fetch(ctx context.Context, path string) (registryauth.Credentials, error) {
	fields, err := o.reader.Read(ctx, path)
	if err != nil {
		var read *openbaokv.Error
		if errors.As(err, &read) {
			return registryauth.Credentials{}, &Error{Reason: read.Reason, cause: read.Unwrap()}
		}
		return registryauth.Credentials{}, err
	}
	username, uok := fields["username"].(string)
	password, pok := fields["password"].(string)
	if !uok || !pok || username == "" || password == "" {
		return registryauth.Credentials{}, &Error{Reason: "the secret needs non-empty string fields username and password"}
	}
	return registryauth.Credentials{Username: username, Password: password}, nil
}
