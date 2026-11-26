package api

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// admissionCapKeys is the three keys the entry adds under the state's `admission`, the keys its
// band (`withoutAdmissionCap`) takes off a caller below the entry's number: every test that
// strips or looks for them reads this list.
var admissionCapKeys = []string{"ceiling", "override", "capChanges"}

// stripAdmissionCapKeys takes the entry's keys off admission, a state's `admission` block as a
// test decodes it, in place, as the band takes them off a caller below the entry;
// withoutAdmissionCapKeys (state_caller_test.go) is its pair over a whole body.
func stripAdmissionCapKeys(admission map[string]any) {
	for _, key := range admissionCapKeys {
		delete(admission, key)
	}
}

// The entry's rows of this package's test tables: the controller's two routes of the cap, whose
// bodies a plugin sends, and the answer they share, a set of the cap by the controller on a
// person's word, which carries every key the answer can: the cap in force, the ceiling, and the
// cap that stands with who set it, when and why. A clear answers no override. The state's three
// keys need no state body of their own: the state golden (populatedState) carries them.
func init() {
	registerEntryTestRows(entryTestRows{
		entry: admissionCapContract,
		requests: map[string]reflect.Type{
			"POST /legion/v1/project/admission-cap":       reflect.TypeFor[ProjectAdmissionCapRequest](),
			"POST /legion/v1/project/admission-cap/clear": reflect.TypeFor[ProjectAdmissionCapClearRequest](),
		},
		answers: func(*testing.T) map[AnswerID]answerFixture {
			return map[AnswerID]answerFixture{
				AnswerAdmissionCap: {roots: typesOf(AdmissionCapResponse{}), bodies: []any{AdmissionCapResponse{Cap: 5, Ceiling: 8,
					Override: &AdmissionCapOverride{Cap: 5, SetBy: PauseActorView{Kind: "controller", Session: "ses_controller"},
						SetAt: time.Date(2026, 11, 24, 6, 0, 0, 0, time.UTC), Reason: "the merge queue waits 52 minutes, above its limit of 45"}}}},
			}
		},
	})
}

// The entry's band is a row of the table like every other, so the caller one contract below the
// entry's number is told what it lost: it is served at the recorded contract at or below it
// (recordedContractAtOrBelow), which stands above every frozen row, so the log holds the entry's
// line among the lines of the entries above it, and the body keeps `admission.cap` and carries
// none of the three keys the entry added. The line is named here as a literal, so a strip applied
// beside the table, which writes no line, fails here.
func TestTheAdmissionCapsBandWritesItsOwnLine(t *testing.T) {
	const dropped = "admission.ceiling, admission.override and admission.capChanges"
	state := carriedState(t)
	below := entryNumber(admissionCapContract) - 1
	body, narrowings := stateNarrowedStating(t, state, statePodAddr+":41000", podClaims(0), below)

	var served struct {
		Admission map[string]json.RawMessage `json:"admission"`
	}
	if err := json.Unmarshal(body, &served); err != nil {
		t.Fatal(err)
	}
	for _, key := range admissionCapKeys {
		if _, carried := served.Admission[key]; carried {
			t.Errorf("the body served to a caller at %d carries `admission.%s`, a key the entry admission-cap added at %d",
				below, key, entryNumber(admissionCapContract))
		}
	}
	if _, carried := served.Admission["cap"]; !carried {
		t.Errorf("the body served to a caller at %d carries no `admission.cap`, which every contract reads", below)
	}
	said := 0
	for _, line := range narrowings {
		if line.Dropped == dropped {
			said++
		}
	}
	if said != 1 {
		t.Errorf("the daemon wrote %+v for a caller at %d, want one line saying it dropped %q", narrowings, below, dropped)
	}
	wantNarrowings(t, narrowings, below, narrowingsExpected(state, below))

	at, atNarrowings := stateNarrowedStating(t, state, statePodAddr+":41000", podClaims(0), entryNumber(admissionCapContract))
	if err := json.Unmarshal(at, &served); err != nil {
		t.Fatal(err)
	}
	for _, key := range admissionCapKeys {
		if _, carried := served.Admission[key]; !carried {
			t.Errorf("the body served to a caller at the entry's own number %d carries no `admission.%s`", entryNumber(admissionCapContract), key)
		}
	}
	for _, line := range atNarrowings {
		if line.Dropped == dropped {
			t.Errorf("a caller at the entry's own number %d was told it dropped %q: %+v", entryNumber(admissionCapContract), dropped, atNarrowings)
		}
	}
}
