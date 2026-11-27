package api

import (
	"testing"
)

// repositoryCorrectionKeys is the four keys repositoryCorrectionContract added to each issue, as
// the wire names them.
var repositoryCorrectionKeys = []string{"repoBefore", "repoCorrectedAt", "repoChangePending", "repoChangeReason"}

// What the record holds about a correction of an issue's repository reaches an architect through
// the state route, which is what every worker's `read_record` reads: the repository the issue
// left and the moment the daemon applied the correction, and, for a change Dispatch has named
// that the daemon has not applied, the repository it named and why the daemon has not applied it.
// Those last two are what let a reader of a record that still names the old repository read why
// it does, rather than read nothing. An issue no correction and no waiting change has reached
// carries none of the four keys, which `omitempty` leaves off the wire rather than writing as
// empty strings a strict reader would refuse.
//
// The issue's pull request is read beside them: its repository is the one it was opened on, which
// a correction leaves behind, so a reader of one record reads that the issue has open work on a
// repository it has left.
func TestTheStateShowsARepositoryCorrectionAndAChangeStillToBeApplied(t *testing.T) {
	body := stateFrom(t, populatedState(t), "192.0.2.1:41000", podClaims(DaemonAPIVersion()))
	issues := issueViews(t, body)

	// LEGION-210 works zeta/tools because Dispatch corrected it off acme/widgets.
	corrected, shown := issues["LEGION-210"]
	if !shown {
		t.Fatalf("the state %s carries no issue LEGION-210", body)
	}
	for field, want := range map[string]any{
		"repo":            "zeta/tools",
		"repoBefore":      "acme/widgets",
		"repoCorrectedAt": "2126-09-22T07:40:00Z",
	} {
		if corrected[field] != want {
			t.Errorf("LEGION-210's %s = %v, want %v", field, corrected[field], want)
		}
	}
	for _, field := range []string{"repoChangePending", "repoChangeReason"} {
		if value, carried := corrected[field]; carried {
			t.Errorf("LEGION-210, whose correction was applied, carries %s = %v", field, value)
		}
	}

	// The pull request LEGION-210 opened before the correction is open on acme/widgets still,
	// which this record says beside the repository the issue works: no key of its own, and no
	// notice, say it. The daemon applies no GitHub event of that pull request to the issue any
	// more (workflow's ownPullRequest), so a reader that compares the two names is reading a
	// state rather than guessing at a loss.
	pull, shown := corrected["pullRequest"].(map[string]any)
	if !shown {
		t.Fatalf("LEGION-210 carries no pullRequest in %s", body)
	}
	for field, want := range map[string]any{"repo": "acme/widgets", "number": float64(1243), "state": "open"} {
		if pull[field] != want {
			t.Errorf("LEGION-210's pull request %s = %v, want %v: the repository the issue left, and the pull request still open on it", field, pull[field], want)
		}
	}
	if pull["repo"] == corrected["repo"] {
		t.Errorf("LEGION-210's pull request is on %v, the repository the issue works: the fixture no longer shows a pull request left behind", pull["repo"])
	}

	// LEGION-212 still works the repository its record names: Dispatch named another one and the
	// daemon has not applied it.
	waiting, shown := issues["LEGION-212"]
	if !shown {
		t.Fatalf("the state %s carries no issue LEGION-212", body)
	}
	for field, want := range map[string]any{
		"repo":              "acme/widgets",
		"repoChangePending": "acme/gadgets",
		"repoChangeReason":  "the review App is not installed on acme/gadgets",
	} {
		if waiting[field] != want {
			t.Errorf("LEGION-212's %s = %v, want %v", field, waiting[field], want)
		}
	}
	for _, field := range []string{"repoBefore", "repoCorrectedAt"} {
		if value, carried := waiting[field]; carried {
			t.Errorf("LEGION-212, which no correction reached, carries %s = %v", field, value)
		}
	}

	// LEGION-211 is an issue no correction and no waiting change has reached.
	for _, field := range repositoryCorrectionKeys {
		if value, carried := issues["LEGION-211"][field]; carried {
			t.Errorf("LEGION-211, whose repository nothing changed, carries %s = %v", field, value)
		}
	}
}

// A caller whose plugin is older than the four keys is served the issues without them: a plugin
// before sjawhar/legion#2788 reads the state with a strict schema and refuses the whole body for
// a key it does not name, which is every `read_record` of every worker in that pod. Such a caller
// is placed as every caller is: by the contract its header states, which every build from
// shapedAnswersContract on states, else by the claims of its pod, which count no claim at
// shapedAnswersContract or above (podContracts). So the band reaches a caller that states a
// contract below entryNumber(repositoryCorrectionContract), the number this build gives the entry
// that added the four keys, and a pod whose claims record one below shapedAnswersContract. This
// daemon's own contract is never below that number: a caller at this contract is served them.
func TestACallerBelowTheRepositoryCorrectionEntryIsServedNoneOfItsKeys(t *testing.T) {
	added := entryNumber(repositoryCorrectionContract)
	if added > DaemonAPIVersion() {
		t.Fatalf("the repository correction's entry is numbered %d, above DaemonAPIVersion %d: no caller could be served the four keys",
			added, DaemonAPIVersion())
	}
	handler := stateServer(t, populatedState(t), podClaims(pauseContract))
	behind := map[string][]byte{
		"a caller stating the contract below": stateStating(t, handler, "192.0.2.1:41000", added-1),
		"a pod below shapedAnswersContract":   stateAsked(t, handler, statePodAddr+":41000"),
	}
	for caller, body := range behind {
		for key, view := range issueViews(t, body) {
			for _, field := range repositoryCorrectionKeys {
				if value, carried := view[field]; carried {
					t.Errorf("the issue %s served to %s carries %s = %v", key, caller, field, value)
				}
			}
			if view["repo"] == nil {
				t.Errorf("the issue %s served to %s carries no repo; the strip takes the four keys and leaves the repository the issue works", key, caller)
			}
		}
	}
}
