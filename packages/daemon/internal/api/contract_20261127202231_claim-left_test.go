package api

import (
	"maps"
	"strconv"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The test rows of the claim-left entry (contract_20261127202231_claim-left.go,
// dispatch://INFRA-3144). The entry adds no route and no answer, only each issue's `claimLeft`,
// which the populated state golden does not carry: state.json is internal/daemon's composition of
// its seed, whose issues no claim release judged. So the entry adds one state body that carries
// the key with every word of its `reason` (states), which the records of the state and of the
// issue record are taken from, and writes the key on one issue of narrowingState (carry), so its
// band finds something to take there and writes its own line.
func init() {
	registerEntryTestRows(entryTestRows{
		entry:  claimLeftContract,
		states: claimLeftStates,
		carry:  withClaimLeft,
	})
}

// claimLeftAt is the instant every entry of these rows was written at.
var claimLeftAt = time.Date(2026, 11, 27, 18, 4, 5, 0, time.UTC)

// claimLeftStates is one state body whose issues each hold the entry of one word of
// claimLeftReasons, so every value the entry's enumerated row declares is carried at
// `issues.*.claimLeft.reason`, and at the record's `claimLeft.reason` through the issues the
// record's fixture takes from it. Each issue is a root a person moved out of the workflow, the
// issue a claim release runs for. The entry of `unread` names no holder, as Dispatch named none,
// so the body carries `claimLeft` both with `holder` and without it.
func claimLeftStates(t *testing.T) []State {
	t.Helper()
	issues := make(map[string]Issue, len(claimLeftReasons))
	for i, reason := range claimLeftReasons {
		key := "LEFT-" + strconv.Itoa(i+1)
		holder := "session:ses_left_" + strconv.Itoa(i+1)
		if reason.Value == string(record.ClaimLeftUnread) {
			holder = ""
		}
		issues[key] = Issue{
			Key: key, Generation: 1, Phase: phase.Done, Status: "backlog", Tree: key,
			Repo: "acme/widgets", DefaultBranch: "main", Workers: map[claim.Role]PhaseView{},
			ClaimLeft: &ClaimLeftView{Holder: holder, Reason: record.ClaimLeftReason(reason.Value), At: claimLeftAt},
		}
	}
	return []State{{
		Daemon: DaemonInfo{
			Project:       "left",
			SchemaVersion: 1,
			Boots:         1,
			FirstBootAt:   time.Date(2026, 11, 20, 14, 3, 27, 0, time.UTC),
			StartedAt:     time.Date(2026, 11, 27, 9, 15, 2, 0, time.UTC),
		},
		Admission: Admission{Cap: 1},
		Issues:    issues,
		Settings:  SettingsOf(minimalSettingsConfig(t)),
	}}
}

// withClaimLeft is state with LEGION-207 holding the entry of a person's claim a release left,
// written on a copy of its issues, so the state it was given is left as it was.
func withClaimLeft(state State) State {
	issues := maps.Clone(state.Issues)
	issue := issues["LEGION-207"]
	issue.ClaimLeft = &ClaimLeftView{Holder: "user:alice", Reason: record.ClaimLeftPerson, At: claimLeftAt}
	issues["LEGION-207"] = issue
	state.Issues = issues
	return state
}
