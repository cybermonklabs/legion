package envoy

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/redact"
	"github.com/sjawhar/envoy/listenerwire"
)

// The listener writes its error and role refusal bodies as listenerwire.APIError and
// listenerwire.RoleRefusal, each word a string, and its role holder and session answers as
// listenerwire.RoleHolder and listenerwire.SessionInfo, the session id and the title strings; this
// client reads them as the same declarations with each of those words held as redact.RemoteText. A
// body the listener writes is the bytes it always wrote, and it reads the same through both: the
// listener's own instantiation gets back what was written, and each box of the client's holds,
// compared as it came, the string written there.
func TestAListenerBodyReadsTheSameThroughTheClientsInstantiation(t *testing.T) {
	t.Run("APIError", func(t *testing.T) {
		for _, tc := range []struct {
			written listenerwire.APIError
			bytes   string
		}{
			{
				written: listenerwire.APIError{Code: listenerRefillingCode, Error: "the agent list is refilling", Expected: []string{"session_id", "topics"}},
				bytes:   `{"code":"refilling","error":"the agent list is refilling","expected":["session_id","topics"]}`,
			},
			{written: listenerwire.APIError{Error: "unknown session"}, bytes: `{"error":"unknown session"}`},
		} {
			data := writtenBytes(t, tc.written, tc.bytes)
			var plain listenerwire.APIError
			var boxed listenerwire.APIErrorAs[redact.RemoteText]
			decode(t, data, &plain)
			decode(t, data, &boxed)
			if !reflect.DeepEqual(plain, tc.written) {
				t.Errorf("%s reads back as %+v through APIError, want %+v", data, plain, tc.written)
			}
			sameWord(t, data, "code", boxed.Code, tc.written.Code)
			sameWord(t, data, "error", boxed.Error, tc.written.Error)
			if len(boxed.Expected) != len(tc.written.Expected) {
				t.Fatalf("%s reads %d expected fields through the client's instantiation, want %d", data, len(boxed.Expected), len(tc.written.Expected))
			}
			for i, field := range tc.written.Expected {
				sameWord(t, data, "expected", boxed.Expected[i], field)
			}
		}
	})
	t.Run("RoleRefusal", func(t *testing.T) {
		lastSeen, released := int64(1700000000000), false
		for _, tc := range []struct {
			written listenerwire.RoleRefusal
			state   holderState
			bytes   string
		}{
			{
				written: listenerwire.RoleRefusal{
					Error: "holder lapsed", Reason: "holder_lapsed", Holder: "ses_holder", HolderState: StateUnresponsive,
					ClientInstanceID: "instance_1", HandedBack: true, ClaimRevision: 7, LastSeen: &lastSeen, ClaimReleased: &released,
				},
				state: holderStateUnresponsive,
				bytes: `{"error":"holder lapsed","reason":"holder_lapsed","holder":"ses_holder","holder_state":"unresponsive","client_instance_id":"instance_1","handed_back":true,"claim_revision":7,"last_seen":1700000000000,"claim_released":false}`,
			},
			{
				written: listenerwire.RoleRefusal{Error: "no holder", Reason: "unclaimed"},
				state:   holderStateOther,
				bytes:   `{"error":"no holder","reason":"unclaimed","claim_revision":0}`,
			},
		} {
			data := writtenBytes(t, tc.written, tc.bytes)
			var plain listenerwire.RoleRefusal
			var boxed listenerwire.RoleRefusalAs[redact.RemoteText, redact.RemoteText, holderState]
			decode(t, data, &plain)
			decode(t, data, &boxed)
			if !reflect.DeepEqual(plain, tc.written) {
				t.Errorf("%s reads back as %+v through RoleRefusal, want %+v", data, plain, tc.written)
			}
			sameWord(t, data, "error", boxed.Error, tc.written.Error)
			sameWord(t, data, "reason", boxed.Reason, string(tc.written.Reason))
			sameWord(t, data, "holder", boxed.Holder, tc.written.Holder)
			sameWord(t, data, "client_instance_id", boxed.ClientInstanceID, tc.written.ClientInstanceID)
			if boxed.HolderState != tc.state {
				t.Errorf("%s reads holder_state as %d through the client's instantiation, want %d", data, boxed.HolderState, tc.state)
			}
			if boxed.HandedBack != tc.written.HandedBack || boxed.ClaimRevision != tc.written.ClaimRevision ||
				!reflect.DeepEqual(boxed.LastSeen, tc.written.LastSeen) || !reflect.DeepEqual(boxed.ClaimReleased, tc.written.ClaimReleased) {
				t.Errorf("%s reads its other fields as %+v through the client's instantiation, want those of %+v", data, boxed, tc.written)
			}
		}
	})
	t.Run("RoleHolder", func(t *testing.T) {
		lastSeen := int64(1700000000000)
		written := listenerwire.RoleHolder{
			Capabilities: []string{"steer"}, ClaimRevision: 7, ClientInstanceID: "instance_1", Dir: "/work",
			Holder: "ses_holder", HolderState: StateLive, Host: "box", LastSeen: &lastSeen, MachineID: "box",
			Role: "legion-planner", Title: "planner",
		}
		data := writtenBytes(t, written, `{"capabilities":["steer"],"claim_revision":7,"client_instance_id":"instance_1","dir":"/work","holder":"ses_holder","holder_state":"live","host":"box","last_seen":1700000000000,"machine_id":"box","role":"legion-planner","title":"planner"}`)
		var plain listenerwire.RoleHolder
		var boxed listenerwire.RoleHolderAs[redact.RemoteText]
		decode(t, data, &plain)
		decode(t, data, &boxed)
		if !reflect.DeepEqual(plain, written) {
			t.Errorf("%s reads back as %+v through RoleHolder, want %+v", data, plain, written)
		}
		sameWord(t, data, "holder", boxed.Holder, written.Holder)
		sameWord(t, data, "title", boxed.Title, written.Title)
		// Its words print as they came, so the client's instantiation writes the body back as the
		// listener wrote it, every other field with them.
		writtenBytes(t, boxed, string(data))
	})
	t.Run("SessionInfo", func(t *testing.T) {
		written := listenerwire.SessionInfo{
			SessionID: "ses_one", State: StateLive, MachineID: "box", Host: "box", Dir: "/work", Title: "planner",
			Capabilities: []string{"steer"}, Topics: []string{"agent.ses_one"}, Roles: []string{"legion-planner"},
			UpdatedAt: 1, LastSeen: 2, Delivery: "consumer",
		}
		data := writtenBytes(t, written, `{"session_id":"ses_one","state":"live","machine_id":"box","host":"box","dir":"/work","port":0,"title":"planner","capabilities":["steer"],"self_subscribed":false,"topics":["agent.ses_one"],"roles":["legion-planner"],"updated_at":1,"last_seen":2,"delivery":"consumer"}`)
		var plain listenerwire.SessionInfo
		var boxed listenerwire.SessionInfoAs[redact.RemoteText]
		decode(t, data, &plain)
		decode(t, data, &boxed)
		if !reflect.DeepEqual(plain, written) {
			t.Errorf("%s reads back as %+v through SessionInfo, want %+v", data, plain, written)
		}
		sameWord(t, data, "session_id", boxed.SessionID, written.SessionID)
		sameWord(t, data, "title", boxed.Title, written.Title)
		writtenBytes(t, boxed, string(data))
	})
}

// writtenBytes is written as the listener encodes it, which must be want.
func writtenBytes(t *testing.T, written any, want string) []byte {
	t.Helper()
	data, err := json.Marshal(written)
	if err != nil {
		t.Fatalf("encode %+v: %v", written, err)
	}
	if string(data) != want {
		t.Fatalf("the listener writes %+v as %s, want %s", written, data, want)
	}
	return data
}

func decode(t *testing.T, data []byte, into any) {
	t.Helper()
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("decode %s into %T: %v", data, into, err)
	}
}

// sameWord fails unless the box holds want, compared as it came.
func sameWord(t *testing.T, data []byte, field string, got redact.RemoteText, want string) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s reads %s through the client's instantiation as a box that is not %q", data, field, want)
	}
}
