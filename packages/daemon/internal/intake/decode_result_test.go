package intake

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A result recorded against a check of an issue's Verification document, a retraction of one, a
// move of one of the issue's pull requests, and a new deploy of a program a live check of the
// issue reads are each a fact of the issue Dispatch appends them on, which for a result is the
// issue whose document holds the check, whichever issue the result was recorded on.
// result.recorded carries its result id, its check, its verdict and its sequence;
// result.retracted its result id, its check and its sequence, with no verdict; and
// issue.pull_request_attribution_changed and deploy.recorded their issue and their sequence alone.
// Each decodes the same from the live stream and from the issue's event log, which the boot
// catch-up reads. A result event that names no result or no check still wakes its issue, with
// what it could not read named.
func TestAResultEventIsAFactOfItsIssue(t *testing.T) {
	tester := map[string]any{"kind": "session", "id": "session-tester"}
	person := map[string]any{"kind": "user", "id": "alice"}
	attribution := map[string]any{"kind": "system", "id": "delivery-attribution"}
	commit := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, eventType string
		actor, payload  map[string]any
		want            DispatchResultChanged
	}{{
		name: "a recorded fail", eventType: "result.recorded", actor: tester,
		payload: map[string]any{"result_id": "result-1", "issue": "LEGION-8", "check_issue": "LEGION-7",
			"check_artifact_id": "verification-7", "check_block_id": "c-close", "check_version": 3, "when": "after_merge",
			"verdict": "fail", "role": "tester", "commit_sha": commit},
		want: DispatchResultChanged{Key: "LEGION-7", Seq: 12, ResultID: "result-1", Check: "c-close", Verdict: "fail"},
	}, {
		name: "a recorded pass", eventType: "result.recorded", actor: tester,
		payload: map[string]any{"result_id": "result-2", "issue": "LEGION-7", "check_issue": "LEGION-7",
			"check_artifact_id": "verification-7", "check_block_id": "c-close", "check_version": 3, "when": "after_merge",
			"verdict": "pass", "role": "tester", "commit_sha": commit},
		want: DispatchResultChanged{Key: "LEGION-7", Seq: 12, ResultID: "result-2", Check: "c-close", Verdict: "pass"},
	}, {
		name: "a retraction", eventType: "result.retracted", actor: person,
		payload: map[string]any{"result_id": "result-2", "issue": "LEGION-7", "check_issue": "LEGION-7",
			"check_block_id": "c-close", "commit_sha": commit, "reason": "the run read the wrong commit"},
		want: DispatchResultChanged{Key: "LEGION-7", Seq: 12, ResultID: "result-2", Check: "c-close"},
	}, {
		name: "a pull request that reached the issue", eventType: "issue.pull_request_attribution_changed", actor: attribution,
		payload: map[string]any{"repo": "acme/widgets", "number": 5, "url": "https://github.com/acme/widgets/pull/5",
			"from": nil, "to": "LEGION-7", "source": "branch"},
		want: DispatchResultChanged{Key: "LEGION-7", Seq: 12},
	}, {
		name: "a pull request that left the issue", eventType: "issue.pull_request_attribution_changed", actor: attribution,
		payload: map[string]any{"repo": "acme/widgets", "number": 5, "url": "https://github.com/acme/widgets/pull/5",
			"from": "LEGION-7", "to": "LEGION-9", "source": "link"},
		want: DispatchResultChanged{Key: "LEGION-7", Seq: 12},
	}, {
		name: "a deploy of a program a live check reads", eventType: "deploy.recorded", actor: tester,
		payload: map[string]any{"repo": "acme/widgets", "program": "widgets-api", "commit": commit,
			"deployed_at": "2026-11-25T08:00:00Z", "source": "route"},
		want: DispatchResultChanged{Key: "LEGION-7", Seq: 12},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			live := decodeEvent(t, tc.eventType, tc.actor, tc.payload)
			if !reflect.DeepEqual(live.Fact, tc.want) || len(live.Unread) != 0 {
				t.Fatalf("%s decoded to %#v, unread %q; want %#v", tc.eventType, live.Fact, live.Unread, tc.want)
			}
			logged := decodeLoggedEvent(t, tc.eventType, tc.actor, tc.payload)
			if !reflect.DeepEqual(logged.Fact, tc.want) || len(logged.Unread) != 0 {
				t.Fatalf("%s read from the event log decoded to %#v, unread %q; want %#v", tc.eventType, logged.Fact, logged.Unread, tc.want)
			}
		})
	}
	for _, tc := range []struct {
		eventType string
		payload   map[string]any
		want      DispatchResultChanged
		unread    []string
	}{
		{"result.recorded", map[string]any{"check_block_id": "c-close", "verdict": "fail"},
			DispatchResultChanged{Key: "LEGION-7", Seq: 12, Check: "c-close", Verdict: "fail"}, []string{"result.recorded has no result_id"}},
		{"result.recorded", map[string]any{"result_id": "result-3"},
			DispatchResultChanged{Key: "LEGION-7", Seq: 12, ResultID: "result-3"},
			[]string{"result.recorded has no check_block_id", "result.recorded has no verdict"}},
		{"result.retracted", map[string]any{"result_id": "result-3"},
			DispatchResultChanged{Key: "LEGION-7", Seq: 12, ResultID: "result-3"}, []string{"result.retracted has no check_block_id"}},
	} {
		decoded := decodeEvent(t, tc.eventType, tester, tc.payload)
		if !reflect.DeepEqual(decoded.Fact, tc.want) || !slices.Equal(decoded.Unread, tc.unread) {
			t.Errorf("%s of %v decoded to %#v, unread %q; want %#v, unread %q", tc.eventType, tc.payload, decoded.Fact, decoded.Unread, tc.want, tc.unread)
		}
	}
}
