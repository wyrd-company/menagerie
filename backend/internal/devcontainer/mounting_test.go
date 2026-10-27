package devcontainer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
)

func TestMountingFindsContainersThatMountTheFolderOrAroundIt(t *testing.T) {
	id := func(c string) string { return strings.Repeat(c, 64) }
	bind := func(src string) container.MountPoint {
		return container.MountPoint{Type: mount.TypeBind, Source: src, RW: true}
	}
	list := []container.Summary{
		{ID: id("a"), Mounts: []container.MountPoint{bind("/root/alpha")}},
		{ID: id("b"), Mounts: []container.MountPoint{bind("/root/alpha/.devcontainer")}},
		{ID: id("c"), Mounts: []container.MountPoint{bind("/root")}},
		{ID: id("d"), Mounts: []container.MountPoint{bind("/root/alphabet"), bind("/elsewhere")}},
		{ID: id("e"), Mounts: []container.MountPoint{{Type: mount.TypeVolume, Source: "/root/alpha"}}},
		{ID: id("f"), Mounts: []container.MountPoint{bind("/")}},
		// A read-only bind cannot write the folder, such as the Docker
		// proxy's mount of the workspace root.
		{ID: id("g"), Mounts: []container.MountPoint{{Type: mount.TypeBind, Source: "/root", RW: false}}},
	}
	got := mounting(list, "/root/alpha", []string{id("f")})
	if want := []string{id("a")[:12], id("b")[:12], id("c")[:12]}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mounting %v, want %v", got, want)
	}
}
