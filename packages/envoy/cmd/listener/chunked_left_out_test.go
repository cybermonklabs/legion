package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/listenerwire"
)

// A registration whose record is past the NATS max payload, so the interest row and the
// known-session record are stored in chunks, keeps a topic the session's consumer filter leaves
// out out of both records, as a registration of a record that fits in one message does. The topic
// is of the fourth kind, one that does not begin with notifications. (*.team.chunked, which as a
// wildcard matches notifications.team.chunked). The answer's warnings name it, neither record
// holds it, and a publish on notifications.team.chunked counts no recipient, while a publish on a
// topic a wildcard of the session's covers counts it once. That wildcard is a team topic's
// (notifications.team.counted.>) beside the wide topics: the publish route refuses a
// notifications.dispatch. topic, which Dispatch alone publishes, so no publish can be on a topic
// the wide wildcard over the issues covers. The control is the same registration with topics short
// enough to fit in one message. (The fifth kind does not arise at a registration on a stream as
// this build deploys it: the route takes an uncaptured wildcard pair off first.)
func TestAChunkedRecordKeepsNoTopicTheFilterLeavesOut(t *testing.T) {
	// Not parallel: a registration of about 1.18 MB of topics must finish inside the route's own
	// 3.5 s budget (sendRequestBudget), and load beside it stretched such a registration past it.
	const (
		fourth   = "*.team.chunked"
		matched  = "notifications.team.chunked"
		covering = "notifications.team.counted.>"
		counted  = "notifications.team.counted.k-0"
	)
	for _, tc := range []struct {
		name       string
		sessionID  string
		tokenBytes int
		pastMax    bool
	}{
		{"a record past the max payload, stored in chunks", "ses_chunked_left_out", 3900, true},
		{"control: a record under the max payload", "ses_whole_left_out", 10, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wide := wideTopics(300, tc.tokenBytes, "legion-controller")
			wide = wide[:len(wide)-1] // no role lane: the session holds no role
			topics := append(slices.Clone(wide), covering, fourth)
			conn := testnats.Connect(t, testnats.URL(t))
			t.Cleanup(conn.Close)
			if size, maxPayload := len(mustJSON(t, topics)), int(conn.MaxPayload()); (size > maxPayload) != tc.pastMax {
				t.Fatalf("UNKNOWN: the topics take %d bytes as JSON against the server's max payload of %d bytes: the case wants past it %v", size, maxPayload, tc.pastMax)
			}

			rig := newRecipientRig(t)
			rig.register(t, recipientSender, []string{counted})
			encoded, err := json.Marshal(map[string]any{
				"session_id":      tc.sessionID,
				"self_subscribed": true,
				"title":           "a wide session",
				"topics":          topics,
				"delivery":        map[string]any{"consumer": true, "epoch": "epoch-" + tc.sessionID},
			})
			if err != nil {
				t.Fatal(err)
			}
			var answer listenerwire.SubscribeResponse
			if err := json.Unmarshal([]byte(rig.post(t, "/v1/interests/subscribe", string(encoded))), &answer); err != nil {
				t.Fatalf("decode the registration's answer: %v", err)
			}
			if !slices.ContainsFunc(answer.Warnings, func(warning string) bool { return strings.HasPrefix(warning, fourth+" was not registered") }) {
				t.Errorf("the answer's warnings %q name no %s as not registered", answer.Warnings, fourth)
			}

			row, _, err := rig.harness.registry.RefreshAt(t.Context(), tc.sessionID)
			if err != nil {
				t.Fatalf("read %s's interest row from the bucket: %v", tc.sessionID, err)
			}
			if rowBytes, maxPayload := len(mustJSON(t, row)), int(conn.MaxPayload()); (rowBytes > maxPayload) != tc.pastMax {
				t.Fatalf("UNKNOWN: the stored row takes %d bytes against the max payload of %d: the case wants past it %v", rowBytes, maxPayload, tc.pastMax)
			}
			var record []string
			// The known-session store follows its bucket through a watch, so the record arrives
			// when the watch delivers it, in milliseconds on an idle machine: the package's bound
			// leaves that far more than ten times its need.
			waitFor(t, "the known-session store to hold "+tc.sessionID+"'s record", func() bool {
				known, ok := rig.known.Get(tc.sessionID)
				record = known.Topics
				return ok && slices.Contains(known.Topics, wide[1])
			})
			if slices.Contains(row.Topics, fourth) {
				t.Errorf("the interest row holds %s", fourth)
			}
			if slices.Contains(record, fourth) {
				t.Errorf("the known-session record holds %s", fourth)
			}
			for _, kept := range wide {
				if !slices.Contains(row.Topics, kept) {
					t.Fatalf("the interest row lacks %s, one of the %d wide topics registered", kept, len(wide))
				}
			}

			assertCounts(t, rig.publish(t, matched, "fourth-"+tc.sessionID), 0, 0)
			assertCounts(t, rig.publish(t, counted, "counted-"+tc.sessionID), 1, 1)
		})
	}
}
