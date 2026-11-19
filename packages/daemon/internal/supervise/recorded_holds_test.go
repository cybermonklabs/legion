package supervise

import (
	"slices"
	"testing"
)

// A session file whose last part is `.` or `..` names no file name (RecordedHolds, review round 12
// of #4220): its base name is not the file's, and two records ending so would hold `.` for no
// claim, a conflict whose settle statement names `.`. The session id is still held, and a clean
// file's names are as before.
//
// The control (the pull request records the run): RecordedHolds without the `.` and `..` check
// holds the base names `..` and `.`.
func TestASessionFileEndingInDotNamesNoFileName(t *testing.T) {
	for _, tc := range []struct {
		file string
		want []Hold
	}{
		{"/state/sessions/2026-11-17T00-00-00-000Z_ses_x.jsonl/..", []Hold{{HoldSessionID, "ses_x"}}},
		{"/state/sessions/2026-11-17T00-00-00-000Z_ses_x.jsonl/.", []Hold{{HoldSessionID, "ses_x"}}},
		{"/state/sessions/2026-11-17T00-00-00-000Z_ses_x.jsonl", []Hold{
			{HoldSessionID, "ses_x"}, {HoldFileName, "2026-11-17T00-00-00-000Z_ses_x.jsonl"}, {HoldFileID, "ses_x"},
		}},
	} {
		if got := RecordedHolds("ses_x", tc.file); !slices.Equal(got, tc.want) {
			t.Errorf("RecordedHolds(ses_x, %q) = %v, want %v", tc.file, got, tc.want)
		}
	}
}
