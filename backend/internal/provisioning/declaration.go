// ---
// relationships:
//   implements: [menagerie, provisioning-declaration]
//   references: workspace
// ---

package provisioning

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/wyrd-company/menagerie/backend/internal/devcontainer"
)

// maxAccessGroup is the largest numeric group the block accepts. The next
// value is the number the kernel reads as "no group".
const maxAccessGroup = 4294967294

// The block's value rules, as its schema states them.
var (
	policyPattern     = regexp.MustCompile(`^[a-z][a-z0-9]*(-[a-z0-9]+)*$`)
	valueNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	valuePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
	xolotlNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// Declaration is a workspace's provisioning declaration: what its rendered
// definition asks of Wyrwood and Xolotl.
type Declaration struct {
	// ContainerName is the --name value of runArgs.
	ContainerName string
	Wyrwood       WyrwoodBlock
	// Xolotl is the client key to issue, or nil for none.
	Xolotl *Key
}

// WyrwoodBlock is what the workspace's binding holds.
type WyrwoodBlock struct {
	AccessGroup uint32
	Policies    []string
	Values      map[string]string
}

// Key addresses a Xolotl client key.
type Key struct {
	Policy string `json:"policy"`
	Name   string `json:"name"`
}

func (k Key) String() string { return k.Policy + "/" + k.Name }

// Parse reads the declaration of def. A definition with no block gives nil.
// An error names the field and the rule, so a block that this Menagerie
// cannot read fails the job before any step.
func Parse(def devcontainer.Definition) (*Declaration, error) {
	if def.Block == nil {
		return nil, nil
	}
	if def.Compose {
		return nil, errors.New("a Compose definition cannot have customizations.menagerie, because runArgs do not apply to it")
	}
	if def.ContainerName == "" {
		return nil, errors.New("customizations.menagerie needs a container name: add --name to runArgs")
	}
	root, err := object(def.Block, "customizations.menagerie", "wyrwood", "xolotl")
	if err != nil {
		return nil, err
	}
	raw, ok := root["wyrwood"]
	if !ok {
		return nil, errors.New("customizations.menagerie.wyrwood is required")
	}
	block, err := parseWyrwood(raw)
	if err != nil {
		return nil, err
	}
	d := &Declaration{ContainerName: def.ContainerName, Wyrwood: block}
	if raw, ok := root["xolotl"]; ok {
		if d.Xolotl, err = parseXolotl(raw); err != nil {
			return nil, err
		}
	}
	return d, nil
}

const wyrwoodField = "customizations.menagerie.wyrwood"

func parseWyrwood(raw json.RawMessage) (WyrwoodBlock, error) {
	fields, err := object(raw, wyrwoodField, "accessGroup", "policies", "values")
	if err != nil {
		return WyrwoodBlock{}, err
	}
	var out WyrwoodBlock
	if out.AccessGroup, err = parseAccessGroup(fields["accessGroup"]); err != nil {
		return WyrwoodBlock{}, err
	}
	if out.Policies, err = parsePolicies(fields["policies"]); err != nil {
		return WyrwoodBlock{}, err
	}
	if raw, ok := fields["values"]; ok {
		if out.Values, err = parseValues(raw); err != nil {
			return WyrwoodBlock{}, err
		}
	}
	return out, nil
}

func parseAccessGroup(raw json.RawMessage) (uint32, error) {
	const field = wyrwoodField + ".accessGroup"
	var group uint64
	if raw == nil {
		return 0, errors.New(field + " is required")
	}
	if bytes.Equal(raw, []byte("null")) || json.Unmarshal(raw, &group) != nil || group > maxAccessGroup {
		return 0, fmt.Errorf("%s must be a whole number from 0 to %d", field, uint64(maxAccessGroup))
	}
	return uint32(group), nil
}

func parsePolicies(raw json.RawMessage) ([]string, error) {
	const field = wyrwoodField + ".policies"
	var policies []string
	if raw == nil {
		return nil, errors.New(field + " is required")
	}
	if json.Unmarshal(raw, &policies) != nil || len(policies) == 0 {
		return nil, errors.New(field + " must be a list with at least one policy name")
	}
	seen := map[string]bool{}
	for _, policy := range policies {
		if len(policy) > 64 || !policyPattern.MatchString(policy) {
			return nil, fmt.Errorf("%s has %q, which is not a policy name", field, policy)
		}
		if seen[policy] {
			return nil, fmt.Errorf("%s has %q twice", field, policy)
		}
		seen[policy] = true
	}
	return policies, nil
}

func parseValues(raw json.RawMessage) (map[string]string, error) {
	const field = wyrwoodField + ".values"
	if !isObject(raw) {
		return nil, errors.New(field + " must be an object")
	}
	var values map[string]string
	if json.Unmarshal(raw, &values) != nil {
		return nil, errors.New(field + " must hold only text values")
	}
	for name, value := range values {
		if !valueNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%s has %q, which is not a value name", field, name)
		}
		if len(value) > 63 || !valuePattern.MatchString(value) {
			return nil, fmt.Errorf("%s.%s must hold only letters, digits, _ and -, and start with a letter or digit", field, name)
		}
	}
	return values, nil
}

func parseXolotl(raw json.RawMessage) (*Key, error) {
	const field = "customizations.menagerie.xolotl"
	fields, err := object(raw, field, "keyPolicy", "keyName")
	if err != nil {
		return nil, err
	}
	policy, err := xolotlName(fields["keyPolicy"], field+".keyPolicy")
	if err != nil {
		return nil, err
	}
	name, err := xolotlName(fields["keyName"], field+".keyName")
	if err != nil {
		return nil, err
	}
	return &Key{Policy: policy, Name: name}, nil
}

func xolotlName(raw json.RawMessage, field string) (string, error) {
	var name string
	if raw == nil {
		return "", errors.New(field + " is required")
	}
	if json.Unmarshal(raw, &name) != nil || len(name) > 63 || !xolotlNamePattern.MatchString(name) {
		return "", fmt.Errorf("%s must be 1 to 63 of a-z, 0-9, _ and -, and start with a letter or digit", field)
	}
	return name, nil
}

// object decodes a JSON object and refuses any key outside allowed. Go's
// struct decoding matches keys in any case, and the block's keys are exact.
func object(raw json.RawMessage, field string, allowed ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if !isObject(raw) || json.Unmarshal(raw, &fields) != nil {
		return nil, errors.New(field + " must be an object")
	}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(allowed, key) {
			return nil, fmt.Errorf("%s has the unknown key %q; it can have %s", field, key, strings.Join(allowed, ", "))
		}
	}
	return fields, nil
}

func isObject(raw json.RawMessage) bool {
	return bytes.HasPrefix(bytes.TrimSpace(raw), []byte("{"))
}

// declaredKey reads only the Xolotl key of a block, so that another
// workspace's invalid block still holds the key it names.
func declaredKey(block json.RawMessage) (Key, bool) {
	var doc struct {
		Xolotl struct {
			KeyPolicy string `json:"keyPolicy"`
			KeyName   string `json:"keyName"`
		} `json:"xolotl"`
	}
	if json.Unmarshal(block, &doc) != nil || doc.Xolotl.KeyPolicy == "" || doc.Xolotl.KeyName == "" {
		return Key{}, false
	}
	return Key{Policy: doc.Xolotl.KeyPolicy, Name: doc.Xolotl.KeyName}, true
}
