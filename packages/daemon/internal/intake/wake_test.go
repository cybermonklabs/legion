package intake

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The Envoy listener's wake report arrives on one subject of its own, which the daemon's fourth
// durable consumer filters on whole. Its payload names the kind, the role token or the session,
// and the listener's own clock in epoch milliseconds — the shape Envelope.KeptAt has, because
// the stream's store time is the NATS server's clock and the daemon measures a wake against its
// own.
//
// A report of a kind this build has no wake for, one that names neither a role nor a session,
// and one with no time are no fact rather than poison: a listener of a later release can report
// what this daemon cannot act on, and acknowledging it loses nothing. An envelope from another
// source on that subject, and a payload that is not an object, are poison.
func TestDecodeTheListenerWakeReport(t *testing.T) {
	const role = "legion-legion-legion-208-implementer"
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	millis := at.UnixMilli()
	for _, tc := range []struct {
		name    string
		source  string
		payload string
		want    Fact
		wantErr string
	}{
		{
			name:    "a role message",
			source:  "envoy",
			payload: `{"kind":"role_message","role":"` + role + `","at":` + strconv.FormatInt(millis, 10) + `}`,
			want:    ListenerWake{Kind: ListenerWakeRole, Role: role, At: at, Report: "wake-1"},
		},
		{
			name:    "an agent message",
			source:  "envoy",
			payload: `{"kind":"agent_message","session":"session-1","at":` + strconv.FormatInt(millis, 10) + `}`,
			want:    ListenerWake{Kind: ListenerWakeAgent, Session: "session-1", At: at, Report: "wake-1"},
		},
		{
			name:    "the listener's liveness report",
			source:  "envoy",
			payload: `{"kind":"listener_ready","at":` + strconv.FormatInt(millis, 10) + `}`,
			want:    ListenerWake{Kind: ListenerWakeReady, At: at, Report: "wake-1"},
		},
		{name: "a role message naming no role", source: "envoy", payload: `{"kind":"role_message","at":` + strconv.FormatInt(millis, 10) + `}`},
		{name: "an agent message naming no session", source: "envoy", payload: `{"kind":"agent_message","at":` + strconv.FormatInt(millis, 10) + `}`},
		{name: "a kind this build has no wake for", source: "envoy", payload: `{"kind":"something_later","at":` + strconv.FormatInt(millis, 10) + `}`},
		{name: "no time", source: "envoy", payload: `{"kind":"listener_ready"}`},
		{
			name:    "another source",
			source:  "dispatch",
			payload: `{"kind":"listener_ready","at":` + strconv.FormatInt(millis, 10) + `}`,
			wantErr: `the listener wake subject has envelope source "dispatch"`,
		},
		{
			name:    "a payload that is not the report",
			source:  "envoy",
			payload: `["listener_ready"]`,
			wantErr: "decode the listener's wake report",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			decoded, err := decodeMessage(legionWakeSubject, "CAPTURE", capturedRepositories, envelopeJSON(t, "wake-1", tc.source, tc.payload))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("decode = %v, want poison naming %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decode the wake report: %v", err)
			}
			if !reflect.DeepEqual(decoded.Fact, tc.want) {
				t.Fatalf("decoded fact = %#v, want %#v", decoded.Fact, tc.want)
			}
			if tc.want != nil && (decoded.Source != "envoy" || decoded.EventID != "wake-1") {
				t.Fatalf("decoded = %#v, want it from envoy under its event id", decoded)
			}
		})
	}
}

// The shared wake subject is a leaf under notifications.legion.>, which the stream already
// carries, and so is each project's own, notifications.legion.<token>.wake, the token spelt as the
// project's claim tokens spell it, so its controller lane and its wake lane share it. The wake and
// ready durables' names are the project's own, beside the other two, and no two are one.
func TestTheWakeConsumerNamesItsProjectOnItsOwnSubject(t *testing.T) {
	if legionWakeSubject != "notifications.legion.wake" {
		t.Fatalf("the wake subject is %q, want notifications.legion.wake", legionWakeSubject)
	}
	for project, want := range map[string]string{"CAPTURE": "notifications.legion.capture.wake", "INFRA": "notifications.legion.infra.wake"} {
		if got, err := wakeSubject(project); err != nil || got != want {
			t.Errorf("wakeSubject(%q) = %q, %v; want %s", project, got, err, want)
		}
	}
	if got := wakeConsumerName("CAPTURE"); got != "legion-go-CAPTURE-wake" {
		t.Fatalf("the wake durable is %q, want legion-go-CAPTURE-wake", got)
	}
	if got := readyConsumerName("CAPTURE"); got != "legion-go-CAPTURE-ready" {
		t.Fatalf("the ready durable is %q, want legion-go-CAPTURE-ready", got)
	}
	names := []string{dispatchConsumerName("CAPTURE"), githubConsumerName("CAPTURE"), wakeConsumerName("CAPTURE"), readyConsumerName("CAPTURE")}
	if sorted := slices.Compact(slices.Sorted(slices.Values(names))); len(sorted) != len(names) {
		t.Fatalf("the durables %q share a name", names)
	}
}

// A wake report on the project's own wake subject is decoded as one on the shared subject is,
// and one on another project's wake subject is no subject of this daemon's.
func TestAWakeReportOnTheProjectsOwnSubjectIsDecoded(t *testing.T) {
	payload := `{"kind":"role_message","role":"legion-capture-capture-7-architect","at":1790000000000}`
	for subject, wantErr := range map[string]bool{
		"notifications.legion.wake":         false,
		"notifications.legion.capture.wake": false,
		"notifications.legion.other.wake":   true,
	} {
		decoded, err := decodeMessage(subject, "CAPTURE", nil, envelopeJSON(t, "wake-"+subject, "envoy", payload))
		if (err != nil) != wantErr {
			t.Errorf("decode on %s: %v, want an error %v", subject, err, wantErr)
			continue
		}
		if !wantErr {
			if wake, ok := decoded.Fact.(ListenerWake); !ok || wake.Role != "legion-capture-capture-7-architect" {
				t.Errorf("decode on %s = %#v, want the role wake", subject, decoded.Fact)
			}
		}
	}
}

// Security[untrusted-input]: the report's clock is the payload's, and the payload is whatever
// reached the subject. The listener refuses a client publish on its own wake subject
// (packages/envoy/cmd/listener/api.go), but a publisher that reaches the stream another way
// chooses every field of the envelope and the payload, the envelope's source among them, so the
// daemon bounds the time it reads to its own: a `listener_ready` dated in the year 9999 would
// otherwise hold R7's release gate open for ever, since the stamp keeps the greatest time it is
// given (store.PodRecords.RecordListenerReady), and released pods would then miss every role
// and agent message while the listener itself said nothing.
func TestAListenerReportDatedInTheFutureIsReadAsNow(t *testing.T) {
	const year9999 = 253370764800000
	before := time.Now().UTC()
	decoded, err := decodeMessage(legionWakeSubject, "CAPTURE", capturedRepositories,
		envelopeJSON(t, "wake-forged", "envoy", `{"kind":"listener_ready","at":`+strconv.FormatInt(year9999, 10)+`}`))
	if err != nil {
		t.Fatalf("decode the forged report: %v", err)
	}
	wake, ok := decoded.Fact.(ListenerWake)
	if !ok {
		t.Fatalf("decoded fact = %#v, want a listener wake", decoded.Fact)
	}
	after := time.Now().UTC()
	if wake.At.Before(before) || wake.At.After(after) {
		t.Fatalf("the report's time = %s, want it bounded to the daemon's own clock between %s and %s",
			wake.At, before, after)
	}
}
