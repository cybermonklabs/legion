package intake

import (
	"encoding/json"
	"reflect"
	"strconv"
	"testing"
	"time"
)

// decodeMessage reads the envelope of every durable frame the daemon consumes with encoding/json
// into envoyEnvelope, which names no `role` member, so the `role` field an envelope may carry (the
// role a message Dispatch's API sent for a route to a role was sent for) is ignored: each
// subject's envelope decodes to the same message, fact and id with the field as without it.
func TestDecodeMessageReadsAnEnvelopeThatCarriesTheRoleFieldAsWithoutIt(t *testing.T) {
	t.Parallel()
	at := strconv.FormatInt(time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC).UnixMilli(), 10)
	for _, tc := range []struct {
		name, subject string
		data          []byte
	}{
		{"a Dispatch issue event", "notifications.dispatch.issue.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t)},
		{"a GitHub pull request event", "notifications.github.sjawhar.legion.pr.42", capturedGitHubEnvelope(t, "pr-opened.json")},
		{"the listener's wake report", legionWakeSubject, envelopeJSON(t, "wake-1", "envoy", `{"kind":"role_message","role":"legion-legion-legion-208-implementer","at":`+at+`}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want, err := decodeMessage(tc.subject, "CAPTURE", capturedRepositories, nil, tc.data)
			if err != nil {
				t.Fatalf("decode the envelope without the field: %v", err)
			}
			if want.Fact == nil {
				t.Fatal("the envelope without the field decodes to no fact, so the row would compare nothing")
			}
			got, err := decodeMessage(tc.subject, "CAPTURE", capturedRepositories, nil, withRoleField(t, tc.data))
			if err != nil {
				t.Fatalf("decode the envelope with the field: %v, want it decoded as without it", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("the envelope with the field decodes to %#v, want %#v, as without it", got, want)
			}
		})
	}
}

// withRoleField is data, an envelope as the stream stores it, with a top-level `role` member.
func withRoleField(t *testing.T, data []byte) []byte {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("read the envelope: %v", err)
	}
	if _, ok := envelope["role"]; ok {
		t.Fatal("the envelope already carries a role field")
	}
	envelope["role"] = json.RawMessage(`"legion-legion-legion-1-architect"`)
	out, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("encode the envelope: %v", err)
	}
	return out
}
