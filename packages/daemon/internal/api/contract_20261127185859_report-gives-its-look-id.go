package api

// 20261127185859 report-gives-its-look-id: a production check's look report gives its look's
// secret id: HandoffCompleteRequest gains optional lookId, the id that only the look's own task
// gives the checking role (look_id on the tool), and the check route takes the report's look from
// it and never from the turn the claim is in. A turn names no prompt, so the turn match could take
// a testing turn's verdict for a pass of production, or refuse a report of the current look. After
// CHECK_NOT_OPEN and CHECK_ROLE_MISMATCH, the route refuses a report with no lookId with the 400
// CHECK_LOOK_ID_REQUIRED, and a lookId that is not the current look's id, or any id while the
// current look has none yet, with the 409 CHECK_LOOK_NOT_OPEN, which until now answered only a
// report that a new look overtook. Each answer names the current look and says that a report
// gives check_opened_at and look_id from the task of its look, and no answer holds a look id. The
// check path no longer answers CHECK_LOOK_NOT_SERVED, which stays the code of a done issue's
// completion that names no check. A phase completion that gives lookId is refused with the 400
// CHECK_LOOK_ID_UNEXPECTED, as one that gives wakeAt is. No refusal of a report or of a done
// issue's completion tells the agent to read check.openedAt from read_record any more.
// An earlier daemon refuses the new member and an earlier plugin sends none, so the boot and
// image gates require a paired daemon, plugin and worker image. This request-only change adds
// no answer key or enumeration value: no shaping band changes, and every previous contract's
// answer and request record stays intact.
const reportGivesItsLookIdContract = 20261127185859

func init() {
	registerEntry(contractEntry{id: reportGivesItsLookIdContract, slug: "report-gives-its-look-id"})
}
