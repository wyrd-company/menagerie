package devcontainer

import (
	"context"
	"io"
	"strings"
	"testing"
)

type quietProgress struct{}

func (quietProgress) Phase(string)         {}
func (quietProgress) Log(string) io.Writer { return io.Discard }

// A step after the build refuses to run before it: it has no image to use.
func TestCreateAndUpNeedTheBuild(t *testing.T) {
	folder, _ := definitionFixture(t, `{"image": "sample"}`)
	b, err := New(nil, Options{}).BeginStart(folder, quietProgress{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Create(context.Background()); err == nil || !strings.Contains(err.Error(), "not built") {
		t.Fatalf("create before the build: %v", err)
	}
	if err := b.Up(context.Background()); err == nil || !strings.Contains(err.Error(), "not built") {
		t.Fatalf("up before the build: %v", err)
	}
}
