package model

//ci:reads packages/contracts/fixtures/dispatch-events/check-state-changed.json TestCheckStateChangedEventPayloadGolden compares the bytes the check.state_changed payload writes with the committed fixture, which it writes again under UPDATE_GOLDEN=1

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// checkStateChangedFixturePath is the file the Legion daemon's intake tests and the contracts'
// Bun test read: the bytes Go writes for a check.state_changed event, so each reader is held to
// what Dispatch sends rather than to its own copy of the shape.
var checkStateChangedFixturePath = filepath.Join("..", "..", "..", "..", "contracts", "fixtures", "dispatch-events", "check-state-changed.json")

// checkStateChangedFixture is the fixture's whole shape: the event's type and actor, and its
// payload in each of its two forms.
type checkStateChangedFixture struct {
	Type      string                        `json:"type"`
	Actor     Actor                         `json:"actor"`
	CheckForm CheckStateChangedEventPayload `json:"check_form"`
	SetForm   CheckStateChangedEventPayload `json:"set_form"`
}

// The check form is the state of one check at one head: here k1 at the head of #70 reads no,
// because a fail stands there. The set form names a head's set of checks with no check of its
// own, for an issue that has no Verification document and so no check at all.
func checkStateChangedForms() checkStateChangedFixture {
	return checkStateChangedFixture{
		Type:  CheckStateChangedEventType,
		Actor: CheckStatesActor,
		CheckForm: CheckStateChangedEventPayload{
			Issue:           "ACME-1",
			CheckArtifactID: new("0a3f6c1e-5b2d-4e8a-9c7f-1d2e3f4a5b6c"),
			Repo:            "acme/widgets",
			Head:            "1111111111111111111111111111111111111111",
			PullRequests:    []int{70},
			Checks:          []string{"k1", "k2"},
			Check:           new("k1"),
			CheckVersion:    new(1),
			State:           new("no"),
			Reason:          "fail",
			Roles:           []string{},
			Results:         []string{"7d1c2b3a-4e5f-4a6b-8c7d-9e0f1a2b3c4d"},
			CICase:          "",
			Serial:          1,
		},
		SetForm: CheckStateChangedEventPayload{
			Issue:           "ACME-2",
			CheckArtifactID: nil,
			Repo:            "acme/widgets",
			Head:            "2222222222222222222222222222222222222222",
			PullRequests:    []int{71},
			Checks:          []string{},
			Check:           nil,
			CheckVersion:    nil,
			State:           nil,
			Reason:          "",
			Roles:           []string{},
			Results:         []string{},
			CICase:          "",
			Serial:          1,
		},
	}
}

// TestCheckStateChangedEventPayloadGolden holds
// packages/contracts/fixtures/dispatch-events/check-state-changed.json to the bytes this struct
// writes. `UPDATE_GOLDEN=1 go test ./internal/dispatch/model/ -run
// TestCheckStateChangedEventPayloadGolden` writes the file again.
func TestCheckStateChangedEventPayloadGolden(t *testing.T) {
	encoded, err := json.MarshalIndent(checkStateChangedForms(), "", "  ")
	if err != nil {
		t.Fatalf("marshal the two forms: %v", err)
	}
	encoded = append(encoded, '\n')

	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(checkStateChangedFixturePath), 0o755); err != nil {
			t.Fatalf("create the fixture directory: %v", err)
		}
		if err := os.WriteFile(checkStateChangedFixturePath, encoded, 0o644); err != nil {
			t.Fatalf("write %s: %v", checkStateChangedFixturePath, err)
		}
		return
	}
	want, err := os.ReadFile(checkStateChangedFixturePath)
	if err != nil {
		t.Fatalf("read %s (run with UPDATE_GOLDEN=1 to write it): %v", checkStateChangedFixturePath, err)
	}
	if !bytes.Equal(encoded, want) {
		t.Fatalf("%s is not what the payload writes (run with UPDATE_GOLDEN=1 to write it)\n got: %s\nwant: %s",
			checkStateChangedFixturePath, encoded, want)
	}
}

// A writer that leaves a list unset writes it as [], never null: the contracts' schema reads each
// list as an array and refuses null, and a reader that joins the checks would fail on it.
func TestCheckStateChangedEventPayloadWritesEachListAsAList(t *testing.T) {
	unset := checkStateChangedForms().SetForm
	unset.PullRequests, unset.Checks, unset.Roles, unset.Results = nil, nil, nil, nil
	got, err := json.Marshal(unset)
	if err != nil {
		t.Fatalf("marshal the set form with its lists unset: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got, &fields); err != nil {
		t.Fatalf("decode %s: %v", got, err)
	}
	for _, list := range []string{"pull_requests", "checks", "roles", "results"} {
		value, written := fields[list]
		if !written {
			t.Errorf("%s is not written, want []", list)
			continue
		}
		if string(value) != "[]" {
			t.Errorf("%s is written %s, want []", list, string(value))
		}
	}
}

// The CI case is a closed list: "" for a check no CI job runs, and each case of
// contracts.CheckCICasesEnded and contracts.CheckCICasesNotEnded, which @legion/contracts' schema
// closes `ci_case` to and the Legion daemon reads. The payload's enum tag names exactly that list,
// in that order, so the struct never names a case the schema or the daemon refuses.
func TestTheCICaseTagIsContractsList(t *testing.T) {
	field, _ := reflect.TypeFor[CheckStateChangedEventPayload]().FieldByName("CICase")
	want := "," + strings.Join(append(append([]string{}, contracts.CheckCICasesEnded...), contracts.CheckCICasesNotEnded...), ",")
	if got := field.Tag.Get("enum"); got != want {
		t.Fatalf("CheckStateChangedEventPayload.CICase names the enum %q, want contracts' %q", got, want)
	}
}
