package docs

import (
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc/schemanodes"
)

// The decision type's server-owned attributes are read off the installed manifest once for each
// manifest (decisionServerAttributes), as storedAskAttributes reads the ask type's: every
// settlement reads the one list, each server attribute of the type with the store name its value
// or its relation is kept under, and builds none of its own. A manifest installed since is read
// again, and answers the same attributes while it declares the same decision type.
func TestTheDecisionTypesServerAttributesAreReadOnceForEachManifest(t *testing.T) {
	want := func(t *testing.T) []decisionServerAttribute {
		t.Helper()
		schema, declared := pmdoc.BlockTypeAttributes(decisionType)
		if !declared {
			t.Fatalf("the installed manifest declares no %s type", decisionType)
		}
		var server []decisionServerAttribute
		for _, attribute := range schema {
			if attribute.Server {
				server = append(server, decisionServerAttribute{name: attribute.Name, kind: schemanodes.StoreName(decisionType, attribute.Name)})
			}
		}
		if len(server) == 0 {
			t.Fatalf("the %s type declares no server attribute", decisionType)
		}
		return server
	}
	read := func(t *testing.T) []decisionServerAttribute {
		t.Helper()
		server, declared := decisionServerAttributes()
		if !declared {
			t.Fatalf("decisionServerAttributes reads no %s type", decisionType)
		}
		if expected := want(t); !reflect.DeepEqual(server, expected) {
			t.Fatalf("decisionServerAttributes = %v, want %v", server, expected)
		}
		return server
	}

	first := read(t)
	if again := read(t); &again[0] != &first[0] {
		t.Errorf("a second read of the decision type's server attributes built a list of its own, want the first read's list")
	}
	restore, err := pmdoc.UseStagedBlockForTest("future", true)
	if err != nil {
		t.Fatalf("install another manifest: %v", err)
	}
	defer restore()
	if later := read(t); &later[0] == &first[0] {
		t.Errorf("a read after another manifest was installed answered the list read off the manifest before it")
	}
}
