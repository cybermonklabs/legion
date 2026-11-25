package record

import (
	"reflect"
	"testing"
	"time"
)

// A merge snapshot may name its pull request alone: an older issue's merge, whose head and base
// the evidence did not hold (Merge, PutMergeSnapshot). A next change of such an issue knows
// neither where its branch starts nor the head that merged, and its branch row keeps both unknown
// rather than invent them: a start that names nothing (IssueBranch.From), which the executor reads
// as the recorded default branch with no merged head. That row is taken as it is built, and it
// reads back as it was written, still told apart from a row of no next change, which carries no
// start at all.
func TestABranchRowOfASnapshotThatNamesOnlyItsPullRequestKeepsItsStartUnknown(t *testing.T) {
	issue := Issue{Key: "ACME-1", Generation: 4, NextChange: 4, Merge: Merge{Number: 17}}
	built := IssueBranchOf(issue)
	if want := (IssueBranch{Generation: 4, From: &BranchFrom{}}); !reflect.DeepEqual(built, want) {
		t.Fatalf("IssueBranchOf = %+v, want %+v: a next change whose snapshot names only pull request 17", built, want)
	}
	for _, tc := range []struct {
		name string
		row  IssueBranch
	}{
		{"a next change that knows neither its base nor its merged head", built},
		{"a generation that is no next change", IssueBranch{Generation: 4}},
	} {
		encoded, err := NewOutboxRow("ACME-1", tc.row, time.Now())
		if err != nil {
			t.Fatalf("%s: the branch row %+v is refused: %v", tc.name, tc.row, err)
		}
		decoded, err := DecodeOutboxPayload(encoded)
		if err != nil {
			t.Fatalf("%s: the stored branch row %s is refused: %v", tc.name, encoded.Payload, err)
		}
		if !reflect.DeepEqual(decoded, tc.row) {
			t.Fatalf("%s: the branch row reads back as %+v, want %+v", tc.name, decoded, tc.row)
		}
	}
}
