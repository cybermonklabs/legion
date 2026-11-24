// Package backends is the registry of secret stores: the one map cipherd and agent-secrets choose
// a store from, by the name BROKER_SECRETS_STORE and GET /v1/settings carry. Each store is one
// file here, <id>.go, which starts //go:build !cipher_no_<id> and registers the store's Backend,
// so this directory lists the stores of a build and a build tag leaves one out with its SDK. No
// store registers itself from an init function.
package backends

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/envoy/internal/cipher/secretstore"
)

// registry is every store this build holds, by name.
var registry = map[string]secretstore.Backend{}

// register adds b to the registry; each store's file calls it once, as var _ = register(...). A
// second store of one name is a programming error.
func register(b secretstore.Backend) struct{} {
	if _, held := registry[b.Name()]; held {
		panic("secretstore/backends: two stores are named " + b.Name())
	}
	registry[b.Name()] = b
	return struct{}{}
}

// All is every store this build holds, sorted by name.
func All() []secretstore.Backend {
	all := make([]secretstore.Backend, 0, len(registry))
	for _, b := range registry {
		all = append(all, b)
	}
	slices.SortFunc(all, func(a, b secretstore.Backend) int { return cmp.Compare(a.Name(), b.Name()) })
	return all
}

// Lookup answers the store named name, or an error naming every store this build holds.
func Lookup(name string) (secretstore.Backend, error) {
	if b, ok := registry[name]; ok {
		return b, nil
	}
	names := make([]string, 0, len(registry))
	for _, b := range All() {
		names = append(names, b.Name())
	}
	return nil, fmt.Errorf("BROKER_SECRETS_STORE must be one of %s, got %s", strings.Join(names, ", "), strconv.Quote(name))
}
