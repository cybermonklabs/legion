package api

// 20261127181502 owner-names-its-task: the owner's three routes record the task a call is of.
// POST /legion/v1/signoff, POST /legion/v1/phase/backward and POST /legion/v1/phase/retry each
// take an optional `task`, the id of the task of the issue's plan the acceptance, the move back
// or the retry-or-escalate decision is of (SignOffRequest.Task, PhaseBackwardRequest.Task and
// PhaseRetryRequest.Task; intake.SignOff.Task, intake.BackwardMove.Task and
// intake.RetryOrEscalate.Task). An issue's plan can hold more than one task, so a call that
// names none is of the one task at accepting, or on the retry route of the one task held, which
// refuses a request that cannot be read that way (TASK_REQUIRED) and one whose named task is not
// held (TASK_NOT_AT_PHASE). A phase worker's move back is of the task of the step its claim's
// agent runs, so its `task` may be left out and, when given, must be that one.
//
// The entry records a field the daemon already reads, and the field is no key this branch added:
// the three request types have read `task` since 46ad46cd822f, which sits in the base of the
// INFRA-3570 stack, and the plugin's schemas have taken it as optional for as long
// (LegionSignOffRequest, LegionPhaseBackwardRequest and LegionPhaseRetryRequest). No record held
// it — not request-shapes.json at any contract, not the routes of any entry's fixture on any
// head of that stack, and requests.go's AcceptedByName carries the handoff's `ready` row alone —
// so the request gate read it as a field of this daemon's contract that its record does not hold
// (TestEveryRecordedRequestFieldIsReadByName). A daemon built before 46ad46cd822f reads no
// `task` on the three and refuses the field where its decoder is strict; every build of this
// stack reads it, so this entry moves the record and not what the daemon does.
//
// It is request-only: it adds no answer key and no enumeration value, so no shaping band moves
// and every earlier contract's answer record stays as it was.
//
// Its fixture (contracts/20261127181502_owner-names-its-task.json) was written by hand, and holds
// the three routes and nothing else. Do not write it again — `go test ./internal/api/
// -update-entry=20261127181502` — while this tree still carries the numbered entries 221, 222,
// 224 and 267 that other branches of this stack converted to entry files. The writer takes the
// last entry's routes from what the request types read and its records from what the daemon
// serves at the entry's number less the records of the entries before it, so over such a tree it
// writes into this entry's fixture every field and key those four numbers add and no record of
// this tree holds: `returnTo` on POST /legion/v1/phase/backward, and the 2,210 answer keys the
// shape gate reports at every contract from 228 up. Once their entry files are in the tree, the
// delta left for this entry is the three `task` rows the fixture already holds, and writing it
// again leaves it as it is. A run of `-update` with no `-update-entry` writes the fixture of an
// entry that has none and leaves this one alone (TestEntryFixtures).
const ownerNamesItsTaskContract = 20261127181502

func init() {
	registerEntry(contractEntry{id: ownerNamesItsTaskContract, slug: "owner-names-its-task"})
}
