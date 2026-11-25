package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestThePrivateProjectsLegionReadsAnswerAsWhatNobodyHolds: carol names PRIV's Legion daemon, so
// its daemon setting, its board and its issue's role holders each answer her. alice, who does not
// see PRIV, reads each as she reads the same route of a project or an issue nobody holds, byte for
// byte once the key asked for is substituted: a project's Legion routes and an issue's role
// holders say nothing of a project its reader may not see.
//
// Controls: legionProject reads its project as the server, and alice reads PRIV's daemon setting
// and board; readIssueRoleHolders binds the server's reader, and alice reads PRIV's issue's holders.
func TestThePrivateProjectsLegionReadsAnswerAsWhatNobodyHolds(t *testing.T) {
	seed := seedPrivateProject(t)
	seed.mustRequest(t, callerCarol, http.MethodPut, "/api/v1/projects/"+seedPrivate+"/legion/daemon",
		map[string]any{"state_url": "http://legion-priv.internal:8080"}, http.StatusOK)
	issue := seed.issue[seedPrivate]
	const nobodyProject, nobodyIssue = "NOPE", "NOPE-1"
	for _, read := range []struct{ name, hidden, missing, hiddenKey, missingKey string }{
		{"the daemon setting", "/api/v1/projects/" + seedPrivate + "/legion/daemon", "/api/v1/projects/" + nobodyProject + "/legion/daemon", seedPrivate, nobodyProject},
		{"the board", "/api/v1/projects/" + seedPrivate + "/legion/board", "/api/v1/projects/" + nobodyProject + "/legion/board", seedPrivate, nobodyProject},
		{"the role holders", "/api/v1/issues/" + issue + "/role-holders", "/api/v1/issues/" + nobodyIssue + "/role-holders", issue, nobodyIssue},
	} {
		if seen := seed.request(t, callerCarol, http.MethodGet, read.hidden, nil); seen.Code != http.StatusOK {
			t.Fatalf("carol reads PRIV's %s as %d %s, want 200, so the comparison proves nothing", read.name, seen.Code, seen.Body.String())
		}
		hidden := seed.request(t, callerAlice, http.MethodGet, read.hidden, nil)
		missing := seed.request(t, callerAlice, http.MethodGet, read.missing, nil)
		want := strings.ReplaceAll(missing.Body.String(), read.missingKey, read.hiddenKey)
		if missing.Code < 400 || hidden.Code != missing.Code || hidden.Body.String() != want {
			t.Errorf("alice reads PRIV's %s as %d %s, want what one nobody holds answers, %d %s",
				read.name, hidden.Code, hidden.Body.String(), missing.Code, want)
		}
	}
}
