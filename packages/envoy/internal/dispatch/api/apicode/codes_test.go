package apicode

import (
	"fmt"
	"net/http"
	"testing"
)

// v2NameClashes names each v2 name two of codes answer at two statuses.
func v2NameClashes(codes []Code) []string {
	var clashes []string
	first := map[string]Code{}
	for _, code := range codes {
		if !code.InV2() {
			continue
		}
		if earlier, seen := first[code.Name()]; seen && earlier.Status() != code.Status() {
			clashes = append(clashes, fmt.Sprintf("%s answers %d as %s and %d as %s", code.Name(), earlier.Status(), earlier, code.Status(), code))
			continue
		}
		first[code.Name()] = code
	}
	return clashes
}

// A v2 name answers with one status wherever it is written, so the v2 document can list each name
// once with its status, and a client can read a code without its status: two constants may share a
// v2 name (ParentCycle and DependencyCycle are ISSUE_CYCLE) only at one status.
func TestAV2NameHasOneStatus(t *testing.T) {
	for _, clash := range v2NameClashes(All()) {
		t.Error(clash)
	}
	t.Run("a second status for a v2 name fails it", func(t *testing.T) {
		scratch := Code{name: ParentCycle.Name(), status: http.StatusBadRequest, v1Name: "SCRATCH", v1Status: http.StatusBadRequest}
		if clashes := v2NameClashes(append(All(), scratch)); len(clashes) != 1 {
			t.Fatalf("a scratch constant answering %s at 400 beside the two at 409: clashes %q, want one", ParentCycle.Name(), clashes)
		}
	})
}
