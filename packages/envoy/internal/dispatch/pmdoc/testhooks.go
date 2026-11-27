package pmdoc

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
)

// createStaged is whether this process creates the elements its manifest stages for the next
// version (CreateStagedForTest). It is apart from the manifest, so the store's installer, which
// installs the manifest again at each wake, leaves it as it is.
var createStaged atomic.Bool

// CreateStagedForTest makes this process create each element its manifest reads but stages for
// the next version, until restore is called: refuseUncreatable refuses none. It changes what a
// write may create and nothing a reader is told (SchemaCreatable, the editor's offer). Dispatch's
// test setting DISPATCH_TEST_CREATE_STAGED calls it, and only beside DISPATCH_TEST_HOOKS=1, so a
// test process can write what the next release creates. restore may be called more than once.
func CreateStagedForTest() (restore func()) {
	previous := createStaged.Swap(true)
	return func() { createStaged.Store(previous) }
}

// UseManifestForTest makes manifest, a schema manifest in blocks.json's format, the one this
// package reads and writes by, in place of the installed one, until restore is called. A test
// uses it to act as a build whose manifest declares an element the embedded one does not, or
// numbers it differently. GET /api/v1/schema/blocks keeps serving what it served before
// (SchemaJSON), the embedded file unless something installed another. The manifest is one
// atomic value, so a swap races no reader, but an operation that reads it twice may read both
// manifests: a test installs it before it starts what reads it.
func UseManifestForTest(manifest []byte) (restore func(), err error) {
	compiled, err := compileManifest(manifest)
	if err != nil {
		return nil, err
	}
	previous := active.Load()
	compiled.json = previous.json
	compiled.install()
	return previous.install, nil
}

// SetBlockSchemaForTest makes value the block schema, as if the server had been built with it:
// every reader of this package, SchemaVersion, LinkKinds and the bytes SchemaJSON serves. It
// refuses value when it is not a valid manifest, and changes nothing then. restore puts back the
// manifest in use before. Tests reach it through pmdoctest.UseBlockSchema, which says when a test
// may call it. It differs from UseManifestForTest in taking the manifest as a value rather than
// as bytes, and in swapping the bytes SchemaJSON serves with it.
func SetBlockSchemaForTest(value BlockSchema) (restore func(), err error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode test block schema: %w", err)
	}
	compiled, err := compileManifest(encoded)
	if err != nil {
		return nil, fmt.Errorf("test block schema: %w", err)
	}
	previous := active.Load()
	compiled.install()
	return previous.install, nil
}

// UseStagedBlockForTest makes the embedded manifest, its version raised by one and one more typed
// block declared, the manifest this package reads and writes by, until restore is called
// (UseManifestForTest): a block named name, of block content, rendered by the host, read from
// the raised version and created from it when creatable, or from the version after it when not,
// as the build that introduces an element declares it. A test uses it to act as that build,
// before the element became creatable or as the build it became creatable in. The version is
// raised because an element is read from the version that introduces it, and this build's own
// version introduces the elements it ships.
func UseStagedBlockForTest(name string, creatable bool) (restore func(), err error) {
	var manifest map[string]any
	if err := json.Unmarshal(SchemaJSON(), &manifest); err != nil {
		return nil, fmt.Errorf("decode the embedded manifest: %w", err)
	}
	version, ok := manifest["version"].(float64)
	if !ok {
		return nil, errors.New("the embedded manifest declares no version")
	}
	types, ok := manifest["types"].([]any)
	if !ok {
		return nil, errors.New("the embedded manifest declares no types array")
	}
	staged := int(version) + 1
	creatableSince := staged + 1
	if creatable {
		creatableSince = staged
	}
	manifest["version"] = staged
	manifest["types"] = append(types, map[string]any{
		"name": name, "content": "block+", "render": "host", "attributes": map[string]any{},
		"since": staged, "creatable_since": creatableSince,
	})
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode the manifest staging the typed block %q: %w", name, err)
	}
	return UseManifestForTest(encoded)
}
