// results_pass_and_fail_test.go holds GET /api/v1/issues/{key}/results to R4 for a
// `before_merge` check: a pass and a fail that count at one commit make the state at that commit
// not known, in either order and from any role. A later pass there does not end that state, and a
// retraction of the wrong result does. The test reads the route's answer as JSON into the types of
// this file, so it builds at a commit before the rule and fails there on its own assertion.
package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// passAndFailHead is the head of the one open pull request of the tests' issue.
const passAndFailHead = "f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6f6"

// passAndFailCheck is one check of the results read and its pass_stands, with only the members
// this test reads.
type passAndFailCheck struct {
	BlockID    string `json:"block_id"`
	PassStands struct {
		Answer   string `json:"answer"`
		Reason   string `json:"reason"`
		Subjects []struct {
			Basis struct {
				Commit string `json:"commit"`
			} `json:"basis"`
			Answer  string   `json:"answer"`
			Reason  string   `json:"reason"`
			Results []string `json:"results"`
		} `json:"subjects"`
	} `json:"pass_stands"`
}

// passAndFailRig is a Dispatch whose issue has pull request #70 open at passAndFailHead, and the
// check k1, a `before_merge` check that no CI job proves.
type passAndFailRig struct {
	*ciJobRig
}

func newPassAndFailRig(t *testing.T) passAndFailRig {
	t.Helper()
	rig := newCIJobRig(t)
	writeHead(t, rig.database, ciJobRepository, 70, passAndFailHead, "open", rig.issue)
	return passAndFailRig{rig}
}

// record records role's verdict of k1 at the head, and answers the stored result's id.
func (r passAndFailRig) record(t *testing.T, role, verdict string) string {
	t.Helper()
	fields := map[string]any{"role": role}
	if verdict == "fail" {
		fields["failed"] = 1
	}
	response := recordResult(t, r.handler, r.issue, resultBody("k1", verdict, passAndFailHead, fields))
	if response.Code != http.StatusCreated {
		t.Fatalf("record %s's %s of k1: status=%d body=%s", role, verdict, response.Code, response.Body.String())
	}
	var recorded struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &recorded); err != nil || recorded.Result.ID == "" {
		t.Fatalf("decode the recorded %s: %v (body %s)", verdict, err, response.Body.String())
	}
	return recorded.Result.ID
}

// retract retracts result as its recorder, failing the test unless the route takes it.
func (r passAndFailRig) retract(t *testing.T, result string) {
	t.Helper()
	response := dispatchRequest(t, r.handler, http.MethodPost, "/api/v1/results/"+result+"/retract",
		map[string]any{"reason": "Recorded against the wrong build."}, "alice")
	if response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("retract %s: status=%d body=%s", result, response.Code, response.Body.String())
	}
}

// read is k1 as the results read answers it, at commit at where at is not empty.
func (r passAndFailRig) read(t *testing.T, at string) passAndFailCheck {
	t.Helper()
	target := "/api/v1/issues/" + r.issue + "/results"
	if at != "" {
		target += "?at=" + at
	}
	response := dispatchRequest(t, r.handler, http.MethodGet, target, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the results of %s: status=%d body=%s", r.issue, response.Code, response.Body.String())
	}
	var read struct {
		Checks []passAndFailCheck `json:"checks"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &read); err != nil {
		t.Fatalf("decode the results of %s: %v", r.issue, err)
	}
	for _, check := range read.Checks {
		if check.BlockID == "k1" {
			return check
		}
	}
	t.Fatalf("the read of %s holds no check k1: %s", r.issue, response.Body.String())
	return passAndFailCheck{}
}

// head fails the test unless the head's subject reads answer for reason, with results where
// results is not nil, and unless the check's answer is answer too.
func (r passAndFailRig) head(t *testing.T, what, answer, reason string, results []string) {
	t.Helper()
	check := r.read(t, "")
	if len(check.PassStands.Subjects) != 1 || check.PassStands.Subjects[0].Basis.Commit != passAndFailHead {
		t.Fatalf("%s: k1 has the subjects %+v, want the head of #70 alone", what, check.PassStands.Subjects)
	}
	subject := check.PassStands.Subjects[0]
	if subject.Answer != answer || subject.Reason != reason || check.PassStands.Answer != answer {
		t.Fatalf("%s: the head reads %q (reason %q) and the check %q, want %q (reason %q) for both",
			what, subject.Answer, subject.Reason, check.PassStands.Answer, answer, reason)
	}
	if results != nil && !slices.Equal(subject.Results, results) {
		t.Fatalf("%s: the head's results are %v, want %v", what, subject.Results, results)
	}
}

// TestAPassAndAFailAtOneHeadReadNotKnown: R4. A pass and a fail of k1 at one head make that head
// not known, in either order and from any role; a later pass does not end it, and a read at the
// commit gives the same. A retraction of the fail leaves the pass, and of the pass the fail.
func TestAPassAndAFailAtOneHeadReadNotKnown(t *testing.T) {
	t.Parallel()
	// The case production printed as a pass from tester and implementer: the tester's pass at one
	// commit, then its fail at the same commit under the same words.
	t.Run("a pass then a fail", func(t *testing.T) {
		rig := newPassAndFailRig(t)
		pass := rig.record(t, "tester", "pass")
		fail := rig.record(t, "tester", "fail")
		rig.head(t, "the tester's pass, then its fail", "unknown", "pass_and_fail", []string{pass, fail})
	})
	t.Run("a fail then a pass", func(t *testing.T) {
		rig := newPassAndFailRig(t)
		rig.record(t, "tester", "fail")
		rig.record(t, "tester", "pass")
		rig.head(t, "the tester's fail, then its pass", "unknown", "pass_and_fail", nil)
	})
	t.Run("a pass of the implementer and a fail of the tester", func(t *testing.T) {
		rig := newPassAndFailRig(t)
		rig.record(t, "implementer", "pass")
		rig.record(t, "tester", "fail")
		rig.head(t, "the implementer's pass and the tester's fail", "unknown", "pass_and_fail", nil)
	})
	t.Run("a later pass", func(t *testing.T) {
		rig := newPassAndFailRig(t)
		rig.record(t, "tester", "pass")
		fail := rig.record(t, "tester", "fail")
		later := rig.record(t, "tester", "pass")
		rig.head(t, "a pass after the pass and the fail", "unknown", "pass_and_fail", []string{later, fail})
	})
	t.Run("a read at the commit", func(t *testing.T) {
		rig := newPassAndFailRig(t)
		rig.record(t, "tester", "pass")
		rig.record(t, "tester", "fail")
		if at := rig.read(t, passAndFailHead); at.PassStands.Answer != "unknown" || at.PassStands.Reason != "pass_and_fail" {
			t.Fatalf("a read at the head answers %+v, want unknown for pass_and_fail", at.PassStands)
		}
	})
	t.Run("the fail retracted", func(t *testing.T) {
		rig := newPassAndFailRig(t)
		pass := rig.record(t, "tester", "pass")
		rig.retract(t, rig.record(t, "tester", "fail"))
		rig.head(t, "the pass, with the fail retracted", "yes", "", []string{pass})
	})
	t.Run("the pass retracted", func(t *testing.T) {
		rig := newPassAndFailRig(t)
		pass := rig.record(t, "tester", "pass")
		fail := rig.record(t, "tester", "fail")
		rig.retract(t, pass)
		rig.head(t, "the fail, with the pass retracted", "no", "fail", []string{fail})
	})
}

// TestAHeadNamesTheNewestResultsThatDecideIt: R10 and R9. A head that a pass and a fail make not
// known names the newest pass and the newest fail there, and a head that reads no for a fail names
// the newest fail there. Each case holds two results of each kind the head picks from, so a head
// that named an older one would name another result than the one each case wants.
func TestAHeadNamesTheNewestResultsThatDecideIt(t *testing.T) {
	t.Parallel()
	t.Run("the newest pass and the newest fail", func(t *testing.T) {
		rig := newPassAndFailRig(t)
		rig.record(t, "tester", "pass")
		rig.record(t, "tester", "fail")
		pass := rig.record(t, "implementer", "pass")
		fail := rig.record(t, "implementer", "fail")
		rig.head(t, "two passes and two fails", "unknown", "pass_and_fail", []string{pass, fail})
	})
	t.Run("the newest pass of each role", func(t *testing.T) {
		rig := newPassAndFailRig(t)
		rig.record(t, "tester", "pass")
		tester := rig.record(t, "tester", "pass")
		implementer := rig.record(t, "implementer", "pass")
		rig.head(t, "two tester passes and an implementer pass", "yes", "", []string{implementer, tester})
	})
	t.Run("the newest of two fails", func(t *testing.T) {
		rig := newPassAndFailRig(t)
		rig.record(t, "tester", "fail")
		fail := rig.record(t, "implementer", "fail")
		rig.head(t, "two fails", "no", "fail", []string{fail})
	})
}
