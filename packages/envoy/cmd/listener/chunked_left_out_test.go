package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
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
//
// The routes run on a fake clock that the case never moves (stillClock), so no bound that the
// registration arms on it can end: its budget (sendRequestBudget, 3.5 s), its consumer calls' and
// its record write's (knownRecordWriteTimeout, 1 s). The case waits for the registration's answer
// through the shared wait (testwait.Receive), which names a registration that never answers. So
// the case asserts what the registration writes, not how soon. On the real clock, a record past
// the max payload, about 1.2 MB written in two chunks, ran past its write's 1 s in envoy-go-race
// leg 2, and the registration was answered 503 (job 141001479204). The rows that assert those
// bounds move their clock past them (registration_known_record_test.go) or keep them
// (write_routes_frozen_bus_test.go).
func TestAChunkedRecordKeepsNoTopicTheFilterLeavesOut(t *testing.T) {
	t.Parallel()
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
			rig.stillClock()
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
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				recorder := httptest.NewRecorder()
				rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", bytes.NewReader(encoded)))
				answered <- recorder
			}()
			recorder := testwait.Receive(t, "the registration of "+tc.sessionID+" to answer with the routes' clock unmoved", answered)
			if recorder.Code != http.StatusOK {
				t.Fatalf("the registration of %s was answered %d, want 200: %s", tc.sessionID, recorder.Code, recorder.Body.String())
			}
			var answer listenerwire.SubscribeResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
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
