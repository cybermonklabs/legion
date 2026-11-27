package listenerwire_test

import (
	"encoding/json"
	"testing"

	"github.com/sjawhar/envoy/listenerwire"
)

// GET /healthz's answer was a map, whose keys encoding/json writes sorted, and Health declares its
// fields, the embedded groups' among them, in that order: an answer that carries every field writes
// each key of the contract at the value set on it, in the same bytes as the map of those fields,
// and a group set at its zero values still writes each of its keys. The issuer fields are on every
// answer, the starting one too, null when not set.
func TestTheHealthAnswerWritesItsKeysInTheOrderTheMapDid(t *testing.T) {
	failing, completedAt, age, read, unread := int64(0), "2026-11-19T00:00:00Z", 0.5, true, false
	full := listenerwire.Health{
		ActiveInstance:   "pod-a",
		CICache:          "not_applicable",
		Error:            "durable consumer unavailable: refused",
		Google:           &unread,
		Lease:            "unavailable: missing NATS grant for KV bucket envoy_listener",
		DurableLag:       &listenerwire.DurableLag{},
		OIDC:             &read,
		Receive:          "preparing the durable",
		ReceiveFailingMS: &failing,
		Role:             listenerwire.RoleStandby,
		RoleClaimRepair:  &listenerwire.RoleClaimRepair{CompletedAt: &completedAt, AgeSeconds: &age, BoundSeconds: 120},
		SessionCache:     &listenerwire.SessionCache{SessionGoneHorizon: "1h0m0s", SessionMaxAge: "2h0m0s"},
		Status:           listenerwire.HealthDegraded,
	}
	got, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("encode the answer: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatalf("decode the answer %s: %v", got, err)
	}
	// Every key of the contract with the value set on it: DurableLag's zero values, and the two
	// durations of SessionCache set apart from each other and from its zero fields, so each value is
	// read under its own key. role_claim_repair is as its own type writes it.
	repair, err := json.Marshal(full.RoleClaimRepair)
	if err != nil {
		t.Fatalf("encode the role claim repair: %v", err)
	}
	contract := map[string]string{
		"active_instance":      `"pod-a"`,
		"ci_cache":             `"not_applicable"`,
		"error":                `"durable consumer unavailable: refused"`,
		"google":               `false`,
		"lease":                `"unavailable: missing NATS grant for KV bucket envoy_listener"`,
		"num_ack_pending":      `0`,
		"num_pending":          `0`,
		"oidc":                 `true`,
		"receive":              `"preparing the durable"`,
		"receive_failing_ms":   `0`,
		"role":                 `"standby"`,
		"role_claim_repair":    string(repair),
		"session_cache_ready":  `false`,
		"session_cache_size":   `0`,
		"session_gone_horizon": `"1h0m0s"`,
		"session_max_age":      `"2h0m0s"`,
		"session_watch_error":  `""`,
		"status":               `"degraded"`,
	}
	for key, value := range contract {
		if field, ok := fields[key]; !ok {
			t.Errorf("the answer %s carries no %s", got, key)
		} else if string(field) != value {
			t.Errorf("the answer's %s is %s, want %s", key, field, value)
		}
	}
	for key := range fields {
		if _, ok := contract[key]; !ok {
			t.Errorf("the answer %s carries %s, which is no key of the contract", got, key)
		}
	}
	want, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode the map: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("the answer is\n%s\nwhere the map of its fields is\n%s", got, want)
	}
	if starting, _ := json.Marshal(listenerwire.Health{Status: listenerwire.HealthStarting}); string(starting) != `{"google":null,"oidc":null,"status":"starting"}` {
		t.Fatalf("the starting answer is %s, want {\"google\":null,\"oidc\":null,\"status\":\"starting\"}", starting)
	}
}
