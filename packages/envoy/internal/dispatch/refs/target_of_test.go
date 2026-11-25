package refs

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

const targetOfServer = "https://dispatch.test"

// What a text cites is not all the graph stores: an external URL names no node, and neither does
// a party - an agent's session, a role or a person - which is a reader rather than a record.
// TargetOf is the one place that is decided, so a write stores no edge to a party (refTargets)
// and a read addresses none (api.parseReferencesQuery).
func TestTargetOfStoresRecordsAndNeitherURLsNorParties(t *testing.T) {
	for _, test := range []struct {
		reference string
		kind      string
		id        string
	}{
		{reference: "https://example.com/runbook"},
		{reference: "dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551"},
		{reference: "dispatch://role/reviewer"},
		{reference: "dispatch://person/alice@example.com"},
		{reference: "dispatch://CORE-1", kind: "issue", id: "CORE-1"},
		// An address with a trailing slash, and one to a tab of the issue's page, name the issue
		// itself, so the edge a text writes to either is the issue's.
		{reference: "dispatch://CORE-1/", kind: "issue", id: "CORE-1"},
		{reference: "dispatch://CORE-1/plan", kind: "issue", id: "CORE-1"},
		{reference: "dispatch://CORE-1/log", kind: "issue", id: "CORE-1"},
		{reference: "https://dispatch.test/issues/CORE-1/children", kind: "issue", id: "CORE-1"},
		{reference: "https://dispatch.test/issues/CORE-1/artifact/notes-md", kind: "artifact", id: "CORE-1/notes-md"},
		{reference: "dispatch://CORE-1/spec", kind: "artifact", id: "CORE-1/spec"},
		{
			reference: "dispatch://agent/01a1058e-f14f-7684-87eb-3dc885955551/artifact/shot-png",
			kind:      "artifact",
			id:        "agent/01a1058e-f14f-7684-87eb-3dc885955551/shot-png",
		},
		{reference: "dispatch://CORE/component/dispatch-server", kind: "component", id: "CORE/dispatch-server"},
	} {
		t.Run(test.reference, func(t *testing.T) {
			found := text.Extract(test.reference, targetOfServer)
			if len(found) != 1 {
				t.Fatalf("%s is %d references, want one", test.reference, len(found))
			}
			target, stored := TargetOf(found[0])
			if stored != (test.kind != "") {
				t.Fatalf("TargetOf(%#v) stored = %t, want %t", found[0], stored, test.kind != "")
			}
			want := Target{Kind: test.kind, ID: test.id}
			if !stored {
				want = Target{}
			}
			if target != want {
				t.Errorf("TargetOf(%#v) = %#v, want %#v", found[0], target, want)
			}
		})
	}
}
