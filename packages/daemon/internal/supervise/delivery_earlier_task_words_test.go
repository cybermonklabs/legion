package supervise

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// A task an earlier release queued is that release's own text, which no record of this release
// drew: main's tasks are prose, and a production check's look task (workflow/check.go's checkTask
// on main) quotes the checking role's pending summary as it was written, after "woken by the wake
// named at the last look — ". Main's Delivery.message sends any task behind its notes as
// `prefix + text`, word for word. Such a task still reaches a session after the upgrade: a start
// row or a pending delivery main queued is run or sent by this release, and the upgrade's restart
// puts the relaunch's note, or the cut-turn note, in front of it.
//
// This change's own words say such a text goes as written: texts.InFront ("A text that no record
// drew, such as an operator's task, goes in front of no note by this rule but by InFrontOfWords,
// which keeps it as its author wrote it") and texts' drawnBlocks ("A text Render did not draw can
// still hold lines of a block's form, such as two `<details>` sections of a log ... such a text is
// never read here, since its caller joins it as its author wrote it"). The task carries no record
// of this release (no `kind:` line), so the input says it is no drawn text. Delivery.message
// tells an undrawn text by its run alone (operatorsOwn: Generation 0), so main's task of a run is
// read as a record, and the second `<details>` section of the tester's summary is renamed.
func TestATaskAnEarlierReleaseQueuedGoesBehindTheNotesAsThatReleaseWroteIt(t *testing.T) {
	const issue = "LEGION-208"
	task := "Production check of LEGION-208: root.\n" +
		"Repository acme/widgets, pull request #42, merge commit a1b2c3d4. Look 2, woken by the wake named at the last look — " +
		"Production does not carry the merge yet.\n<details>\ndeploy log\n</details>\n<details>\nrollout status\n</details>\n" +
		"Next I read the rollout once it finishes.\n" +
		"Do what the deployment instructions say to reach production and read whether production carries the merge. " +
		"Wait for nothing in this session: end the turn with the report."
	for _, tc := range []struct {
		name  string
		ended EndedWork
		note  string
	}{
		{name: "behind the relaunch's note", ended: EndedWork{Cause: "the daemon restarted", Turn: true, TaskTurn: true},
			note: partText(RelaunchNote(issue, EndedWork{Cause: "the daemon restarted", Turn: true, TaskTurn: true}, true))},
		{name: "behind the cut-turn note", ended: EndedWork{Cause: "the daemon restarted", Told: true},
			note: partText(CutTurnNote(issue, "the daemon restarted"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := Delivery{ID: "outbox:7", Task: task, Generation: 1, Interrupted: true}
			got, _ := d.message(Claim{Issue: issue, Role: claim.RoleTester, EndedWork: tc.ended})
			if want := tc.note + "\n\n" + task; got != want {
				t.Errorf("the earlier release's task is sent as\n%s\nwant the note, a blank line and the task as that release wrote it:\n%s", got, want)
			}
		})
	}
}
