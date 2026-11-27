// results_each_head_test.go holds GET /api/v1/issues/{key}/results to the rule of each open head
// for a `before_merge` check: each open head has one state, that state comes only from the
// results at its own commit, and a pass of the check stands only when each open head reads yes.
// Each test reads the route's answer as JSON into the types of this file, not into the server's
// own, so it builds at a commit before the rule and fails there on its own assertion.
package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// The heads of these tests: each a commit that one pull request of the rig's issue is open at.
const (
	eachHeadH1 = "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"
	eachHeadH2 = "b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2b2"
	eachHeadH3 = "c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3c3"
	eachHeadH4 = "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"
	// eachHeadMerged is the head of a merged pull request of the issue.
	eachHeadMerged = "e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5e5"
)

// eachHeadAnswer is the results read, with only the members these tests read.
type eachHeadAnswer struct {
	Checks []eachHeadCheck `json:"checks"`
}

// eachHeadCheck is one check of the read and its pass_stands.
type eachHeadCheck struct {
	BlockID    string `json:"block_id"`
	PassStands struct {
		Answer   string            `json:"answer"`
		Reason   string            `json:"reason"`
		Roles    []string          `json:"roles"`
		Results  []string          `json:"results"`
		Subjects []eachHeadSubject `json:"subjects"`
	} `json:"pass_stands"`
}

// eachHeadSubject is one subject of a check's pass_stands: one head and its state.
type eachHeadSubject struct {
	Basis struct {
		Kind        string `json:"kind"`
		Commit      string `json:"commit"`
		PullRequest *int   `json:"pull_request"`
		Merged      bool   `json:"merged"`
		Repo        string `json:"repo"`
	} `json:"basis"`
	Answer  string          `json:"answer"`
	Reason  string          `json:"reason"`
	Roles   []string        `json:"roles"`
	Results []string        `json:"results"`
	CI      json.RawMessage `json:"ci"`
}

// eachHeadRead is check blockID as GET /api/v1/issues/{issue}/results answers it, read at commit
// at where at is not empty.
func eachHeadRead(t *testing.T, handler http.Handler, issue, blockID, at string) eachHeadCheck {
	t.Helper()
	target := "/api/v1/issues/" + issue + "/results"
	if at != "" {
		target += "?at=" + at
	}
	response := dispatchRequest(t, handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", issue, response.Code, response.Body.String())
	}
	var read eachHeadAnswer
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s: %v (body %s)", issue, err, response.Body.String())
	}
	for _, check := range read.Checks {
		if check.BlockID == blockID {
			return check
		}
	}
	t.Fatalf("the read of %s holds no check %s: %s", issue, blockID, response.Body.String())
	return eachHeadCheck{}
}

// eachHeadAt is the subject of check at commit, failing the test where the answer names none.
func eachHeadAt(t *testing.T, check eachHeadCheck, commit string) eachHeadSubject {
	t.Helper()
	for _, subject := range check.PassStands.Subjects {
		if subject.Basis.Commit == commit {
			return subject
		}
	}
	t.Fatalf("%s's pass_stands %+v names no subject at %s", check.BlockID, check.PassStands, commit)
	return eachHeadSubject{}
}

// eachHeadResult records role's verdict of check at commit on issue, failing the test unless the
// route takes it, and answers the stored result's id.
func eachHeadResult(t *testing.T, handler http.Handler, issue, check, role, verdict, commit string) string {
	t.Helper()
	fields := map[string]any{"role": role}
	if verdict == "fail" {
		fields["failed"] = 1
	}
	response := recordResult(t, handler, issue, resultBody(check, verdict, commit, fields))
	if response.Code != http.StatusCreated {
		t.Fatalf("record %s's %s of %s at %s: status=%d body=%s", role, verdict, check, commit, response.Code, response.Body.String())
	}
	var recorded struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &recorded); err != nil || recorded.Result.ID == "" {
		t.Fatalf("decode the recorded %s of %s: %v (body %s)", verdict, check, err, response.Body.String())
	}
	return recorded.Result.ID
}

// eachHeadWants fails the test unless subject reads answer for reason.
func eachHeadWants(t *testing.T, what string, subject eachHeadSubject, answer, reason string) {
	t.Helper()
	if subject.Answer != answer || subject.Reason != reason {
		t.Fatalf("%s: the head at %s reads %q (reason %q), want %q (reason %q)",
			what, subject.Basis.Commit, subject.Answer, subject.Reason, answer, reason)
	}
}

// eachHeadSame reports whether got and want hold the same ids or roles, in any order.
func eachHeadSame(got, want []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want)))
}

// TestEachOpenHeadHoldsTheStateOfItsOwnResults: R2. #70 is open at H1 and #71 at H2. The tester's
// pass of k1 at H1 makes H1 yes, and H2, which holds no result, no for no_result. The tester's
// fail at H2 then makes H2 no for its fail, and H1 stays yes.
func TestEachOpenHeadHoldsTheStateOfItsOwnResults(t *testing.T) {
	t.Parallel()
	rig := newHeadRuleRig(t)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 71, eachHeadH2, "open", rig.issue)

	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	check := eachHeadRead(t, rig.handler, rig.issue, "k1", "")
	eachHeadWants(t, "a pass at H1 alone", eachHeadAt(t, check, eachHeadH1), "yes", "")
	eachHeadWants(t, "no result at H2", eachHeadAt(t, check, eachHeadH2), "no", "no_result")

	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH2)
	check = eachHeadRead(t, rig.handler, rig.issue, "k1", "")
	eachHeadWants(t, "the pass at H1 beside a fail at H2", eachHeadAt(t, check, eachHeadH1), "yes", "")
	eachHeadWants(t, "the fail at H2", eachHeadAt(t, check, eachHeadH2), "no", "fail")
	if len(check.PassStands.Subjects) != 2 {
		t.Fatalf("k1 has %d subjects, want one for each of the two open heads: %+v", len(check.PassStands.Subjects), check.PassStands)
	}
}

// TestTheStateAtAHeadComesOnlyFromItsCommit: R3. The state belongs to the commit: two pull
// requests at one head share it, a pull request that moves to a new head takes that head's state,
// and the old commit keeps its own, which a read at that commit still gives.
func TestTheStateAtAHeadComesOnlyFromItsCommit(t *testing.T) {
	t.Parallel()
	rig := newHeadRuleRig(t)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 71, eachHeadH2, "open", rig.issue)

	// 1. A pass at H1 and nothing at H2.
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	check := eachHeadRead(t, rig.handler, rig.issue, "k1", "")
	eachHeadWants(t, "nothing at H2", eachHeadAt(t, check, eachHeadH2), "no", "no_result")
	if check.PassStands.Answer != "no" {
		t.Fatalf("with no result at H2, k1 answers %+v, want no", check.PassStands)
	}

	// 2. #71 moves to H1, so the two pull requests share one head and its state.
	writeHead(t, rig.database, ciJobRepository, 71, eachHeadH1, "open", rig.issue)
	check = eachHeadRead(t, rig.handler, rig.issue, "k1", "")
	for _, subject := range check.PassStands.Subjects {
		eachHeadWants(t, "two pull requests at H1", subject, "yes", "")
	}
	if len(check.PassStands.Subjects) == 0 || check.PassStands.Answer != "yes" {
		t.Fatalf("with both pull requests at H1, which holds a pass, k1 answers %+v, want yes at each head", check.PassStands)
	}

	// 3. A fail at a new commit H3, and #70 moves there.
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "fail", eachHeadH3)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH3, "open", rig.issue)
	check = eachHeadRead(t, rig.handler, rig.issue, "k1", "")
	eachHeadWants(t, "#70 at H3, which holds a fail", eachHeadAt(t, check, eachHeadH3), "no", "fail")

	// 4. #70 moves to H4, where the tester then passes: H4 reads yes, and H3 keeps its no.
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH4, "open", rig.issue)
	eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH4)
	check = eachHeadRead(t, rig.handler, rig.issue, "k1", "")
	eachHeadWants(t, "#70 at H4, which holds a pass", eachHeadAt(t, check, eachHeadH4), "yes", "")
	if at := eachHeadRead(t, rig.handler, rig.issue, "k1", eachHeadH3); at.PassStands.Answer != "no" {
		t.Fatalf("a read at H3, which holds the fail alone, answers %+v for k1, want no", at.PassStands)
	}
}

// TestAPassStandsForTheCheckOnlyWhenEachOpenHeadReadsYes: R7. Three open heads and one merged
// head. Passes at two open heads leave the third at no, a pass at the third makes the check's
// answer yes, and a pass at the merged head alone stands for nothing. Each case has a rig of its
// own, so a fault in one case reads as that case's failure, and the passes at the open heads are
// read with no pass at the merged head beside them.
func TestAPassStandsForTheCheckOnlyWhenEachOpenHeadReadsYes(t *testing.T) {
	t.Parallel()
	open := []string{eachHeadH1, eachHeadH2, eachHeadH3}
	newRig := func(t *testing.T) headRuleRig {
		t.Helper()
		rig := newHeadRuleRig(t)
		for index, head := range open {
			writeHead(t, rig.database, ciJobRepository, 70+index, head, "open", rig.issue)
		}
		writeHead(t, rig.database, ciJobRepository, 69, eachHeadMerged, "merged", rig.issue)
		return rig
	}

	t.Run("a pass at each open head in turn", func(t *testing.T) {
		t.Parallel()
		rig := newRig(t)
		for index, head := range open {
			eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", head)
			check := eachHeadRead(t, rig.handler, rig.issue, "k1", "")
			want := "no"
			if index == len(open)-1 {
				want = "yes"
			}
			if check.PassStands.Answer != want {
				t.Fatalf("with passes at %d of %d open heads, k1 answers %+v, want %s", index+1, len(open), check.PassStands, want)
			}
			if got := len(check.PassStands.Subjects); got != len(open) {
				t.Fatalf("k1 has %d subjects, want one for each of the %d open heads and none for the merged one: %+v",
					got, len(open), check.PassStands)
			}
		}
	})
	t.Run("the head with no pass is last", func(t *testing.T) {
		t.Parallel()
		rig := newHeadRuleRig(t)
		writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
		writeHead(t, rig.database, ciJobRepository, 71, eachHeadH2, "open", rig.issue)
		writeHead(t, rig.database, ciJobRepository, 72, eachHeadH3, "open", rig.issue)
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH3)
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH2)
		check := eachHeadRead(t, rig.handler, rig.issue, "k1", "")
		if check.PassStands.Answer != "no" {
			t.Fatalf("passes at the first two heads, with the no head last, answer %+v, want no", check.PassStands)
		}
		subjects := check.PassStands.Subjects
		if len(subjects) != 3 || subjects[2].Basis.Commit != eachHeadH1 {
			t.Fatalf("the no head is not last: %+v", subjects)
		}
	})

	t.Run("a pass at the merged head alone", func(t *testing.T) {
		t.Parallel()
		rig := newRig(t)
		eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadMerged)
		if check := eachHeadRead(t, rig.handler, rig.issue, "k1", ""); check.PassStands.Answer != "no" {
			t.Fatalf("a pass at the merged head alone answers %+v, want no", check.PassStands)
		}
	})
}

// TestTheRolesOfAHeadAreThePassesAtThatHead: R6. The implementer's pass is at H1 and the tester's
// at H2. Each head names the one role whose pass counts there and that pass's id, and the check
// answers yes with no role, since no role passed at each head. The tester's pass at H1 too makes
// the check's answer name the tester, and H1's results then hold both passes.
func TestTheRolesOfAHeadAreThePassesAtThatHead(t *testing.T) {
	t.Parallel()
	rig := newHeadRuleRig(t)
	writeHead(t, rig.database, ciJobRepository, 70, eachHeadH1, "open", rig.issue)
	writeHead(t, rig.database, ciJobRepository, 71, eachHeadH2, "open", rig.issue)

	implementerAtH1 := eachHeadResult(t, rig.handler, rig.issue, "k1", "implementer", "pass", eachHeadH1)
	testerAtH2 := eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH2)
	check := eachHeadRead(t, rig.handler, rig.issue, "k1", "")
	if check.PassStands.Answer != "yes" || len(check.PassStands.Roles) != 0 {
		t.Fatalf("with the implementer's pass at H1 alone and the tester's at H2 alone, k1 answers %+v, want yes with no role", check.PassStands)
	}
	for _, want := range []struct {
		head, role, result string
	}{{eachHeadH1, "implementer", implementerAtH1}, {eachHeadH2, "tester", testerAtH2}} {
		subject := eachHeadAt(t, check, want.head)
		if subject.Answer != "yes" || !slices.Equal(subject.Roles, []string{want.role}) || !slices.Equal(subject.Results, []string{want.result}) {
			t.Fatalf("the head at %s reads %+v, want yes from %s alone, decided by %s", want.head, subject, want.role, want.result)
		}
	}

	testerAtH1 := eachHeadResult(t, rig.handler, rig.issue, "k1", "tester", "pass", eachHeadH1)
	check = eachHeadRead(t, rig.handler, rig.issue, "k1", "")
	if check.PassStands.Answer != "yes" || !slices.Equal(check.PassStands.Roles, []string{"tester"}) {
		t.Fatalf("with the tester's pass at each head, k1 answers %+v, want yes from the tester alone", check.PassStands)
	}
	if subject := eachHeadAt(t, check, eachHeadH1); !eachHeadSame(subject.Roles, []string{"implementer", "tester"}) ||
		!eachHeadSame(subject.Results, []string{implementerAtH1, testerAtH1}) {
		t.Fatalf("the head at H1 reads %+v, want the roles implementer and tester, decided by %s and %s", subject, implementerAtH1, testerAtH1)
	}
}
