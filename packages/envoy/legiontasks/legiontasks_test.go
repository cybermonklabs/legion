package legiontasks

import (
	"bytes"
	"testing"
	"time"
)

// The names are written out literally rather than built from one another, so a change to any of
// them, which every program sharing the bucket and every plugin reading the relay frame must take
// at once, fails here until reviewed.
func TestTheNamesAreTheOnesEveryProgramSpells(t *testing.T) {
	for got, want := range map[string]string{
		Bucket:         "legion_tasks",
		Stream:         "KV_legion_tasks",
		HeaderClaim:    "Envoy-Task-Claim",
		HeaderEpoch:    "Envoy-Task-Epoch",
		HeaderRevision: "Envoy-Task-Revision",
		HeaderOp:       "Envoy-Task-Op",
		OpPut:          "put",
		OpDelete:       "delete",
	} {
		if got != want {
			t.Errorf("a name is %q, want %q", got, want)
		}
	}
}

// A project owns the keys of its own claim tokens and no other project's: a token holds no hyphen,
// so neither a longer token that starts with it nor a shorter one it starts with shares its
// prefix, and a project token that is not one owns nothing.
func TestOwnedByIsTheProjectsOwnPrefixAlone(t *testing.T) {
	for _, c := range []struct {
		key, project string
		want         bool
	}{
		{"legion-ab-infra-12-implementer", "ab", true},
		{"legion-ab-controller", "ab", true},
		{"legion-ab0-infra-12-implementer", "ab0", true},
		{"legion-abc-infra-12-implementer", "ab", false},
		{"legion-ab-infra-12-implementer", "abc", false},
		{"legion-ab-infra-12-implementer", "a", false},
		{"legion-ab-infra-12-implementer", "", false},
		// Project a's claim on issue B-1, which a token holding a hyphen would take.
		{"legion-a-b-1-implementer", "a-b", false},
		{"legion-a-b-1-implementer", "a", true},
		{"legion-AB-infra-12-implementer", "AB", false},
		{"legion-ab", "ab", false},
		{"legion--infra-12-implementer", "", false},
		{"other-legion-ab-infra-12-implementer", "ab", false},
		{"", "ab", false},
	} {
		if got := OwnedBy(c.key, c.project); got != c.want {
			t.Errorf("OwnedBy(%q, %q) = %t, want %t", c.key, c.project, got, c.want)
		}
	}
}

// A value is written as the one member "task", and reads back as itself.
func TestAValueIsEncodedAsTheOneTaskMemberAndDecodedBack(t *testing.T) {
	for _, c := range []struct {
		value Value
		want  string
	}{
		{Value{Task: 1}, `{"task":1}`},
		{Value{Task: 7}, `{"task":7}`},
		{Value{Task: 18446744073709551615}, `{"task":18446744073709551615}`},
	} {
		data, err := c.value.Encode()
		if err != nil {
			t.Fatalf("Encode(%+v): %v", c.value, err)
		}
		if !bytes.Equal(data, []byte(c.want)) {
			t.Errorf("Encode(%+v) = %s, want %s", c.value, data, c.want)
		}
		back, err := Decode(data)
		if err != nil {
			t.Fatalf("Decode(%s): %v", data, err)
		}
		if back != c.value {
			t.Errorf("Decode(%s) = %+v, want %+v", data, back, c.value)
		}
	}
}

// No task is numbered 0, so no value of it is written.
func TestAValueOfTaskZeroIsNotEncoded(t *testing.T) {
	if data, err := (Value{}).Encode(); err == nil {
		t.Fatalf("Encode of task 0 = %s, want an error", data)
	}
}

// The one member "task" with a positive integer is a value however JSON spaces or escapes it.
func TestDecodeAcceptsTheOneTaskMember(t *testing.T) {
	for data, want := range map[string]uint64{
		`{"task":1}`:                    1,
		" { \"task\" : 42 } \n":         42,
		`{"t\u0061sk":3}`:               3,
		`{"task":18446744073709551615}`: 18446744073709551615,
	} {
		got, err := Decode([]byte(data))
		if err != nil {
			t.Errorf("Decode(%q): %v", data, err)
			continue
		}
		if got != (Value{Task: want}) {
			t.Errorf("Decode(%q) = %+v, want task %d", data, got, want)
		}
	}
}

// Anything but exactly the one member "task" holding a positive integer is refused.
func TestDecodeRefusesAnythingElse(t *testing.T) {
	for _, data := range []string{
		``,
		`not json`,
		`null`,
		`1`,
		`"task"`,
		`[]`,
		`[{"task":1}]`,
		`{}`,
		`{"task":0}`,
		`{"task":-1}`,
		`{"task":1.5}`,
		`{"task":1.0}`,
		`{"task":1e3}`,
		`{"task":01}`,
		`{"task":18446744073709551616}`,
		`{"task":"1"}`,
		`{"task":null}`,
		`{"task":true}`,
		`{"task":{}}`,
		`{"task":[1]}`,
		`{"Task":1}`,
		`{"TASK":1}`,
		`{"number":1}`,
		`{"task":1,"extra":2}`,
		`{"extra":2,"task":1}`,
		`{"task":1,"task":2}`,
		`{"task":1`,
		`{"task":1}x`,
		`{"task":1}{}`,
		`{"task":1} {"task":2}`,
	} {
		if got, err := Decode([]byte(data)); err == nil {
			t.Errorf("Decode(%q) = %+v, want an error", data, got)
		}
	}
}

// The epoch is the stream's creation time in UTC, to the nanosecond, whatever zone it was read in.
func TestEpochIsTheCreationTimeInUTCToTheNanosecond(t *testing.T) {
	created := time.Date(2026, 11, 13, 17, 21, 44, 123456789, time.FixedZone("plus-one", 3600))
	if got, want := Epoch(created), "2026-11-13T16:21:44.123456789Z"; got != want {
		t.Errorf("Epoch(%v) = %q, want %q", created, got, want)
	}
	if Epoch(created) != Epoch(created.UTC()) {
		t.Errorf("the epoch of one instant differs by the zone it was read in")
	}
}
