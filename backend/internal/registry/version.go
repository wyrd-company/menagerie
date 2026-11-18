// ---
// relationships:
//   implements: menagerie
// ---

package registry

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a template version's numbers. A version is a tag
// MAJOR.MINOR.PATCH, with an optional leading v, whose numbers have no
// leading zero; a tag with a pre-release or build part is not one. These are
// the template service's rules, which lists a repository's versions.
type Version struct{ Major, Minor, Patch uint64 }

// ParseVersion reads a tag or a version string as a version.
func ParseVersion(tag string) (Version, bool) {
	parts := strings.Split(strings.TrimPrefix(tag, "v"), ".")
	if len(parts) != 3 {
		return Version{}, false
	}
	var numbers [3]uint64
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') || strings.TrimLeft(part, "0123456789") != "" {
			return Version{}, false
		}
		n, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return Version{}, false
		}
		numbers[i] = n
	}
	return Version{numbers[0], numbers[1], numbers[2]}, true
}

// Compare orders versions by their numbers: negative when v is older than
// o, zero when they are one version, positive when v is newer.
func (v Version) Compare(o Version) int {
	for _, d := range [3][2]uint64{{v.Major, o.Major}, {v.Minor, o.Minor}, {v.Patch, o.Patch}} {
		switch {
		case d[0] < d[1]:
			return -1
		case d[0] > d[1]:
			return 1
		}
	}
	return 0
}

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Newer says whether version a is newer than version b; a string that is
// not a version is newer than nothing and older than nothing.
func Newer(a, b string) bool {
	va, ok := ParseVersion(a)
	vb, okb := ParseVersion(b)
	return ok && okb && va.Compare(vb) > 0
}
