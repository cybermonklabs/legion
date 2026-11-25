package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// An operator's task (`legion claims deliver --task`) is the operator's own words, which no record
// drew. Taken back from a turn its process could not finish, it is sent again behind the cut-turn
// note, and main's Delivery.message (supervise/delivery.go at 8966cf45^2) joins the two as
// `prefix + text`: the note, then the task word for word. This change joins them by texts.InFront,
// whose rule, as its handoff states it, is that "an undrawn text (an operator's task) is joined as
// it is", and the note stays word for word too. An operator's words can hold lines of the form a
// block's tags take, such as two `<details>` sections of a pasted log, or a `<summary>` section
// that the next line names; behind a note that holds none of those tags, the task still goes as
// the operator wrote it.
func TestAnOperatorsTaskBehindACutTurnNoteGoesAsTheOperatorWroteIt(t *testing.T) {
	const issue, cause = "LEGION-208", "the pod was replaced"
	note := partText(CutTurnNote(issue, cause))
	for _, task := range []string{
		"Compare the two runs:\n<details>\nfirst run\n</details>\n<details>\nsecond run\n</details>",
		"Read this first.\n<summary>\nthe tests pass\n</summary>\nThe <summary> above is my own.",
	} {
		d := Delivery{ID: "OPERATORID", Task: task, Interrupted: true}
		got, _ := d.message(Claim{Issue: issue, Role: claim.RoleImplementer, EndedWork: EndedWork{Cause: cause, Told: true}})
		if want := note + "\n\n" + task; got != want {
			t.Errorf("the operator's task is sent as\n%s\nwant the cut-turn note, a blank line and the task as the operator wrote it:\n%s", got, want)
		}
	}
}
