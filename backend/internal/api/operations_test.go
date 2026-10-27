package api

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
	"github.com/wyrd-company/menagerie/backend/internal/dockerproxy"
	"github.com/wyrd-company/menagerie/backend/internal/jobs"
)

func TestContainerFailureKinds(t *testing.T) {
	for want, err := range map[string]error{
		KindIdentityLabelRefused:    fmt.Errorf("start: %w", &devcontainer.IdentityLabelError{Key: "devcontainer.local_folder", Where: "runArgs"}),
		KindProxyDenied:             fmt.Errorf("up: %w", &dockerproxy.Denied{Reason: "privileged mode is not allowed"}),
		KindInitializeCommandFailed: &devcontainer.InitializeError{Err: &dockerproxy.Denied{Reason: "r"}},
	} {
		var f *jobs.Failure
		if got := ContainerFailure(err); !errors.As(got, &f) || f.Kind != want || f.Err != err {
			t.Errorf("ContainerFailure(%v) = %v, want kind %s", err, got, want)
		}
	}
	plain := errors.New("plain")
	if got := ContainerFailure(plain); got != plain {
		t.Errorf("ContainerFailure(plain) = %v", got)
	}
}
