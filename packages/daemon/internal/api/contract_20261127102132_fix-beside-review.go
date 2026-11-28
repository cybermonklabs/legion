package api

import "github.com/sjawhar/legion/daemon/internal/record"

// 20261127102132 fix-beside-review: a check that a CI job runs and that fails at the head of a
// task starts the task's first implementation step again as a fix, in a working copy of its own,
// while the other steps go on. The shim's `adopt-working-copy` takes two optional members, each a
// full commit id (40 lowercase hex digits, else the frame is malformed): `fixFrom`, the commit the
// fix's working copy starts from, which the shim makes or moves in the directory beside its own
// workspace (workspace.FixDir) and adopts in place of that workspace, and `moveTo`, the commit the
// task's working copy moves to before anything else runs. Its answer echoes each member it acted
// on: `fixDir`, the fix's directory, and `movedTo`. A shim built before this entry ignores both
// members and answers with neither echo, which the daemon reads as a pod that its update round
// must replace, and the fix's task waits, with no charge to its prompt budget, until the round
// does.
// `GET /legion/v1/state` and `GET /legion/v1/issues/{key}/record` serve an issue's `fix` while a
// fix of its task runs, and an issue's `holdReason` takes two more values: `fix`, an issue held
// before its retro while a fix of its task runs, and `ci`, an issue held before its retro until
// each check a CI job runs passes at its head. `admission.held`'s `reason` takes their words,
// `fix_running` and `ci_at_head`, each with `heldBy`. A caller whose contract is below this
// entry's number is served no `fix`, an issue held for either reason with no `holdReason`, and a
// row of either word as `unclassified` with no `heldBy`, the row it read for that issue before:
// the band is this entry's row of `bands` (shaping.go's withoutFix), and the values are this
// entry's `enumeratedValues`, which 219's frozen value strips hold back (shaping.go's
// withoutUnadmittedHoldReason and withoutUnadmittedHeldWords).
// `retry_or_escalate` and `legion claims retry` answer `409 FIX_RUNNING` on a hold of the reason
// `fix` and `409 CI_PENDING` on a hold of the reason `ci`, each naming what ends the hold, and
// change nothing; only this release writes either reason, so no issue a release before ran gets
// the new answers. `FIX_RUNNING` and `CI_PENDING` are values of the refusal's `code`, which no
// reader enumerates. A plugin of a release before this entry is relaunched at the daemon's boot on
// this contract (refreshStalePlugins), so no plugin before it completes a fix.
//
// FixBesideReviewContract is the id of this entry, at whose number
// (entryNumber(FixBesideReviewContract)) a failed CI check starts a fix beside the other steps:
// the adoption's `fixFrom` and `moveTo` with their echoes, an issue's `fix` in the served state,
// the hold reasons `fix` and `ci` with their held words `fix_running` and `ci_at_head`, and the
// refusals `FIX_RUNNING` and `CI_PENDING`. Every served value that arrives with the fix arrives at
// that number, and a caller below it is served none.
const FixBesideReviewContract = 20261127102132

func init() {
	registerEntry(contractEntry{
		id: FixBesideReviewContract, slug: "fix-beside-review",
		bands: []stateNarrowing{
			{contract: FixBesideReviewContract, issue: withoutFix, dropped: "the issues' fix"},
		},
		enumeratedValues: []enumeratedValues{
			{answer: AnswerState, keyPath: issueKeyPath + "holdReason", values: []EnumeratedValue{
				{Value: string(record.HoldForCI), Since: FixBesideReviewContract},
				{Value: string(record.HoldForFix), Since: FixBesideReviewContract},
			}},
			{answer: AnswerState, keyPath: "admission.held.*.reason", values: []EnumeratedValue{
				{Value: string(record.HeldCIAtHead), Since: FixBesideReviewContract},
				{Value: string(record.HeldFixRunning), Since: FixBesideReviewContract},
			}},
		},
	})
}
