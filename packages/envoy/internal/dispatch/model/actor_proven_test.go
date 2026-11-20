package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A stored actor says whether the credential proved it, and the mark survives the trip through
// the jsonb column every actor lives in. Only a proven actor writes the key: a claimed actor and
// a row written before the mark read the same, as not proven, so a reader never has to tell an
// old row from a claimed one. Recorded drops the request's credential and keeps the mark, which
// is a fact of the record.
func TestAnActorsProvenMarkRoundTripsAndIsWrittenOnlyWhenTrue(t *testing.T) {
	owner := "alice@example.com"
	for _, tc := range []struct {
		name  string
		actor Actor
		json  string
	}{
		{
			"a session its credential proved",
			Actor{Kind: "session", ID: "ses_1", Owner: &owner, Proven: true},
			`{"kind":"session","id":"ses_1","owner":"alice@example.com","proven":true}`,
		},
		{
			"a session a request only named",
			Actor{Kind: "session", ID: "ses_1", Owner: &owner},
			`{"kind":"session","id":"ses_1","owner":"alice@example.com"}`,
		},
		{
			"a person whose agent's session its credential proved",
			Actor{Kind: "user", ID: owner, Proven: true, Act: &ActorAct{Session: "ses_1", SessionProven: true, Scopes: []string{ScopePerson}}},
			`{"kind":"user","id":"alice@example.com","act":{"session":"ses_1","session_proven":true,"scopes":["person"]},"proven":true}`,
		},
		{
			"a person whose agent named its own session",
			Actor{Kind: "user", ID: owner, Proven: true, Act: &ActorAct{Session: "ses_1", TokenID: "tok_1"}},
			`{"kind":"user","id":"alice@example.com","act":{"session":"ses_1","token_id":"tok_1"},"proven":true}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.actor)
			if err != nil {
				t.Fatalf("encode %+v: %v", tc.actor, err)
			}
			if string(encoded) != tc.json {
				t.Fatalf("encoded = %s, want %s", encoded, tc.json)
			}
			var decoded Actor
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("decode %s: %v", encoded, err)
			}
			if !reflect.DeepEqual(decoded, tc.actor) {
				t.Fatalf("decoded = %+v, want %+v", decoded, tc.actor)
			}
		})
	}

	var before Actor
	if err := json.Unmarshal([]byte(`{"kind":"session","id":"ses_1"}`), &before); err != nil {
		t.Fatalf("decode a row written before the mark: %v", err)
	}
	if before.Proven {
		t.Fatalf("a row written before the mark decodes as proven: %+v", before)
	}

	proven := Actor{Kind: "session", ID: "ses_1", Proven: true, Credential: &Credential{Session: "ses_1"}}
	if recorded := proven.Recorded(); recorded.Credential != nil || !recorded.Proven {
		t.Fatalf("Recorded() = %+v, want the credential dropped and the mark kept", recorded)
	}
}
