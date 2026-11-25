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
// While PRIV is private carol, who sees every project, gets that line too: the rule is chosen by
// whether the reader sees every object (seesEveryObject) alone. With no project private, alice, an
// open viewer, keeps today's rule (R9).
//
// Control: readLineWords reading every project's key for every reader (the rule as the base merge
// brought it); bob's line keeps SECRET-1 until SECRET exists and then drops it, and the test fails.
func TestAResultsLineTellsAReaderNothingOfAProjectItDoesNotSee(t *testing.T) {
	t.Parallel()
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
	want("carol's dry run with no project SECRET", line(callerCarol, seedPrivate, "/dry-run", http.StatusOK), shape)
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/projects", map[string]string{"key": "SECRET", "name": "Project SECRET"}, http.StatusCreated)
	want("bob's dry run once SECRET exists", line(callerBob, seedPrivate, "/dry-run", http.StatusOK), shape)
	want("bob's record once SECRET exists", line(callerBob, seedPrivate, "", http.StatusCreated), shape)
	want("carol's dry run once SECRET exists", line(callerCarol, seedPrivate, "/dry-run", http.StatusOK), shape)

	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	want("alice's dry run with no project private", line(callerAlice, seedPublic, "/dry-run", http.StatusOK), prefix+"and NOPE-1 look the same.")
}

// TestAResultsLineRuleTellsNoReaderWhetherAHiddenProjectExists: which rule a reader's line keeps
// is decided by whether the reader sees every object (seesEveryObject) alone, never by whether
// some project is hidden from it, which the line would otherwise answer. carol, an open viewer who
// sees PRIV, dry-runs a result observing "UTF-8 and NOPE-1 look the same." while PRIV is private,
// and again once alice's project SECRET, which carol does not see, is private too: both lines drop
// every word of an issue key's shape, UTF-8 included, and are the same. With no project private
// she keeps today's rule, which keeps UTF-8 (R9).
//
// Control: the rule chosen by whether some project is hidden from the reader (the round before);
// carol's first line keeps UTF-8 and NOPE-1, her second drops them, and the test fails.
func TestAResultsLineRuleTellsNoReaderWhetherAHiddenProjectExists(t *testing.T) {
	t.Parallel()
	seed := seedPrivateProject(t)
	seed.mustRequest(t, callerCarol, http.MethodPost, "/api/v1/issues/"+seed.issue[seedPrivate]+"/artifacts",
		map[string]any{"name": "verification.md", "content": twoChecks}, http.StatusCreated)
	body := resultBody("k1", "pass", headCommit, map[string]any{"observed": "UTF-8 and NOPE-1 look the same."})
	line := func() string {
		t.Helper()
		return decodeBody[resultDryRun](t, seed.mustRequest(t, callerCarol, http.MethodPost,
			"/api/v1/issues/"+seed.issue[seedPrivate]+"/results/dry-run", body, http.StatusOK)).Line
	}
	prefix := "- Check k1 (before_merge): pass at " + headCommit + ", recorded by implementer. "
	shape := prefix + "and look the same."

	if got := line(); got != shape {
		t.Errorf("carol's line while PRIV, which she sees, is private = %q, want %q", got, shape)
	}
	seed.mustRequest(t, callerAlice, http.MethodPost, "/api/v1/projects", map[string]string{"key": "SECRET", "name": "Project SECRET"}, http.StatusCreated)
	seed.setVisibility(t, "SECRET", model.ProjectPrivate)
	if got := line(); got != shape {
		t.Errorf("carol's line once SECRET, which she does not see, is private too = %q, want %q", got, shape)
	}
	seed.setVisibility(t, "SECRET", model.ProjectPublic)
	seed.setVisibility(t, seedPrivate, model.ProjectPublic)
	if got, want := line(), prefix+"UTF-8 and NOPE-1 look the same."; got != want {
		t.Errorf("carol's line with no project private = %q, want %q", got, want)
	}
}
