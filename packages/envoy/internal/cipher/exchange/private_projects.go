package exchange

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sjawhar/envoy/internal/contracts"
)

// projectKeyPattern is a whole project key, the one rule Dispatch creates keys by.
var projectKeyPattern = regexp.MustCompile(contracts.ProjectKeyPattern)

// PrivateProjects is the set of Dispatch projects the operator declares private, as
// BROKER_PRIVATE_PROJECTS lists them. Its zero value is no private project, which is what a
// configuration that leaves the setting unset reads as. It is immutable once parsed: no method
// hands out what it holds, so a PrivateProjects copied into several readers answers each of them
// the same way.
type PrivateProjects struct {
	// keys is every key the setting names, sorted, each once; nil for none.
	keys []string
}

// ParsePrivateProjects reads setting as BROKER_PRIVATE_PROJECTS spells it: project keys separated
// by white space. An empty or blank setting is no private project. A key that is no project key
// (contracts.ProjectKeyPattern) and a key named twice are refused, naming the key, so a list that
// is not one is never read as some part of it. Unlike a token's list (oauthwire.ParseProjects),
// the set has no bound on its size: it is the operator's declaration, not what one token names.
func ParsePrivateProjects(setting string) (PrivateProjects, error) {
	keys := strings.Fields(setting)
	if len(keys) == 0 {
		return PrivateProjects{}, nil
	}
	for _, key := range keys {
		if !projectKeyPattern.MatchString(key) {
			return PrivateProjects{}, fmt.Errorf("%q is not a project key: %s", key, contracts.ProjectKeyRule)
		}
	}
	slices.Sort(keys)
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			return PrivateProjects{}, fmt.Errorf("%s is named twice, and the list names each private project once", keys[i])
		}
	}
	return PrivateProjects{keys: keys}, nil
}

// Keys answers every private project's key, sorted, and nil for none. The slice is the caller's
// own, so the set stays as it was parsed.
func (p PrivateProjects) Keys() []string {
	return slices.Clone(p.keys)
}

// Has reports whether the operator declared the project of key private. Only a whole key matches,
// in the case Dispatch writes it.
func (p PrivateProjects) Has(key string) bool {
	_, found := slices.BinarySearch(p.keys, key)
	return found
}
