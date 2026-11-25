package api

import (
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// checksIssue is the issue whose two tasks merged one after the other, so two of its production
// checks are open at once.
const checksIssue = "LEGION-240"

// twoChecksState is the state golden with checksIssue beside its own issues: the check of #1301,
// opened at the first merge, and the check of #1302, opened at the second, oldest first as the
// projection lists them, with `check` the newest.
func twoChecksState(t *testing.T) State {
	t.Helper()
	state := populatedState(t)
	opened := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	first := CheckView{Repo: "acme/widgets", PullRequest: 1301, MergeCommit: "5a1c0e7d9b3f2a4c6e8d0b1f3a5c7e9d2b4f6a80",
		OpenedAt: opened, Role: string(claim.RoleTester), Looks: 1, LookOpen: true, NextWakeAt: opened.Add(2 * time.Hour)}
	second := CheckView{Repo: "acme/widgets", PullRequest: 1302, MergeCommit: "6b2d1f8e0c4a3b5d7f9e1c2a4b6d8f0e3c5a7b91",
		OpenedAt: opened.Add(3 * time.Hour), Role: string(claim.RoleTester), NextWakeAt: opened.Add(339 * time.Hour)}
	issues := maps.Clone(state.Issues)
	issues[checksIssue] = Issue{Key: checksIssue, Generation: 1, Phase: phase.Implementing, Status: "in_progress", Tree: checksIssue,
		Repo: "acme/widgets", Check: &second, Checks: []CheckView{first, second}}
	state.Issues = issues
	return state
}

// checksServed is the pull request of checksIssue's `check` and of each of its `checks` as body
// carries them, and whether the body carries the key `checks` at all.
func checksServed(t *testing.T, body []byte) (check int, checks []int, carried bool) {
	t.Helper()
	var served struct {
		Issues map[string]map[string]json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatalf("decode state %s: %v", body, err)
	}
	return checksOf(t, served.Issues[checksIssue])
}

// checksOf reads checksServed's three answers off one served issue.
func checksOf(t *testing.T, issue map[string]json.RawMessage) (check int, checks []int, carried bool) {
	t.Helper()
	var one struct {
		PullRequest int `json:"pullRequest"`
	}
	if raw, ok := issue["check"]; ok {
		if err := json.Unmarshal(raw, &one); err != nil {
			t.Fatalf("decode the check %s: %v", raw, err)
		}
	}
	raw, carried := issue["checks"]
	if carried {
		var each []struct {
			PullRequest int `json:"pullRequest"`
		}
		if err := json.Unmarshal(raw, &each); err != nil {
			t.Fatalf("decode the checks %s: %v", raw, err)
		}
		for _, served := range each {
			checks = append(checks, served.PullRequest)
		}
	}
	return one.PullRequest, checks, carried
}

// A caller at checksContract reads each open production check of an issue under `checks`, oldest
// first, and the newest under `check`. A caller one contract below it knows no `checks`, so it is
// served the issue with no `checks` key and `check` the newest open check, as it read an issue of
// one check before (withoutChecks). The record route serves the issue as the state does, and the
// state the source answered keeps both checks for the next caller.
func TestACallerBelowTheChecksContractGetsTheNewestCheckAlone(t *testing.T) {
	const remote = "192.0.2.1:41000"
	state := twoChecksState(t)
	handler := stateServer(t, state, podClaims(DaemonAPIVersion()))
	for _, tc := range []struct {
		name     string
		contract int
		checks   []int
		carried  bool
	}{
		{"a caller stating the contract", checksContract, []int{1301, 1302}, true},
		{"a caller stating the contract before it", checksContract - 1, nil, false},
	} {
		check, checks, carried := checksServed(t, stateStating(t, handler, remote, tc.contract))
		if check != 1302 || carried != tc.carried || !slices.Equal(checks, tc.checks) {
			t.Errorf("%s: the state serves check #%d and checks %v (key carried %t), want check #1302 and checks %v (key carried %t)",
				tc.name, check, checks, carried, tc.checks, tc.carried)
		}
		recorder := recordAsked(t, handler, remote, checksIssue, tc.contract)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: the record of %s answered %d: %s", tc.name, checksIssue, recorder.Code, recorder.Body)
		}
		var issue map[string]json.RawMessage
		if err := json.Unmarshal(recorder.Body.Bytes(), &issue); err != nil {
			t.Fatalf("%s: decode the record %s: %v", tc.name, recorder.Body, err)
		}
		check, checks, carried = checksOf(t, issue)
		if check != 1302 || carried != tc.carried || !slices.Equal(checks, tc.checks) {
			t.Errorf("%s: the record serves check #%d and checks %v (key carried %t), want check #1302 and checks %v (key carried %t)",
				tc.name, check, checks, carried, tc.checks, tc.carried)
		}
	}
	if got := len(state.Issues[checksIssue].Checks); got != 2 {
		t.Errorf("the state the source answered holds %d checks of %s after the narrowing, want both kept", got, checksIssue)
	}
}
