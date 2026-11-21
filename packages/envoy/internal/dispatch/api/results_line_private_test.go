package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestAResultsLineTellsAReaderNothingOfAProjectItDoesNotSee: the pull-request body line a result's
// record and its dry run answer drops every word holding an issue key, and a reader that sees less
// than every project would learn from the words it kept which guessed keys name a project. So for
// such a reader the line drops every word of an issue key's shape, whichever projects exist
// (readLineWords, lineWordsForAnyKey): bob, a restricted member PRIV's list names, dry-runs a
// result of PRIV's check observing "SECRET-1 and NOPE-1 look the same.", and his line is the same
// before and after carol makes a project SECRET, which he does not see; his record answers it too.
// carol, who sees every project, keeps today's rule: her line keeps NOPE-1 and, once SECRET exists,
// drops SECRET-1. With no project private, alice, an open viewer, keeps today's rule as well (R9).
//
// Control: readLineWords reading every project's key for every reader (the rule as the base merge
// brought it); bob's line keeps SECRET-1 until SECRET exists and then drops it, and the test fails.
func TestAResultsLineTellsAReaderNothingOfAProjectItDoesNotSee(t *testing.T) {
	seed := seedPrivateProject(t)
	for _, project := range []string{seedPublic, seedPrivate} {
		seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[project]+"/artifacts",
			map[string]any{"name": "verification.md", "content": twoChecks}, http.StatusCreated)
	}
	const observed = "SECRET-1 and NOPE-1 look the same."
	prefix := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. "
	body := resultBody("k1", "pass", headCommit, map[string]any{"observed": observed})
	line := func(caller privateCaller, project, route string, status int) string {
		t.Helper()
		return decodeBody[resultDryRun](t, seed.mustRequest(t, caller, http.MethodPost,
			"/api/v1/issues/"+seed.issue[project]+"/results"+route, body, status)).Line
	}
	shape := prefix + "and look the same."
	want := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: the line is %q, want %q", what, got, want)
		}
	}

	want("bob's dry run with no project SECRET", line(callerBob, seedPrivate, "/dry-run", http.StatusOK), shape)
	want("carol's dry run with no project SECRET", line(callerCarol, seedPrivate, "/dry-run", http.StatusOK), prefix+observed)
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{"key": "SECRET", "name": "Project SECRET"}, http.StatusCreated)
	want("bob's dry run once SECRET exists", line(callerBob, seedPrivate, "/dry-run", http.StatusOK), shape)
	want("bob's record once SECRET exists", line(callerBob, seedPrivate, "", http.StatusCreated), shape)
	want("carol's dry run once SECRET exists", line(callerCarol, seedPrivate, "/dry-run", http.StatusOK), prefix+"and NOPE-1 look the same.")

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	want("alice's dry run with no project private", line(callerAlice, seedPublic, "/dry-run", http.StatusOK), prefix+"and NOPE-1 look the same.")
}
