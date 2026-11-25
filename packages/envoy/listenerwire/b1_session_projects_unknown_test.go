package listenerwire_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/listenerwire"
)

// TestSessionInfoKeepsAListThatIsNotSentApartFromAnEmptyOne (K6): a session row's projects are
// the projects of the session's credential, [] for a credential that lists none. A listener that
// records no projects for a session sends no key, and Dispatch reads that as a list it does not
// know, which is not a credential that lists none (access.SessionClause). So a row read and written
// again through SessionInfo keeps the three apart: no key stays no key, [] stays [], and a list
// stays that list. A null reads as no key.
//
// Control: Projects as a []string under omitempty; [] is written as no key, and the test fails
// naming the row that sent [].
func TestSessionInfoKeepsAListThatIsNotSentApartFromAnEmptyOne(t *testing.T) {
	for _, row := range []struct {
		name, sent, want string
	}{
		{"a row with no projects key", `{"session_id":"s1"}`, ""},
		{"a row whose projects are null", `{"session_id":"s1","projects":null}`, ""},
		{"a row whose projects are []", `{"session_id":"s1","projects":[]}`, `"projects":[]`},
		{"a row that lists projects", `{"session_id":"s1","projects":["PRIV","PUB"]}`, `"projects":["PRIV","PUB"]`},
	} {
		var info listenerwire.SessionInfo
		if err := json.Unmarshal([]byte(row.sent), &info); err != nil {
			t.Fatalf("%s: read %s: %v", row.name, row.sent, err)
		}
		written, err := json.Marshal(info)
		if err != nil {
			t.Fatalf("%s: write it again: %v", row.name, err)
		}
		switch {
		case row.want == "" && strings.Contains(string(written), `"projects"`):
			t.Errorf("%s is written again as %s, want no projects key", row.name, written)
		case row.want != "" && !strings.Contains(string(written), row.want):
			t.Errorf("%s is written again as %s, want it to carry %s", row.name, written, row.want)
		}
	}
}
