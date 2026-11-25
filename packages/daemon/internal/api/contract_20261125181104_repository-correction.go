package api

// 20261125181104 repository-correction: INFRA-3136 -- an issue's repository follows the one Dispatch resolves it to, and
// `GET /legion/v1/state` says so: an issue takes four keys, `repoBefore` and `repoCorrectedAt`,
// the repository the issue left and the moment the daemon applied the correction, and
// `repoChangePending` and `repoChangeReason`, a repository Dispatch has named that the daemon has
// not applied and why it has not. The four are the whole of the wire change: no key is removed,
// no key changes its meaning, and an issue no correction has reached and nothing waits on carries
// none of them, so most issues are served exactly as before. A caller below this contract is
// served every issue with the four stripped (`withoutRepositoryCorrection`, the band of
// `repositoryCorrectionContract`, the last row of shaping.go's stateNarrowings), composed with
// every band of a lower contract, because one built before sjawhar/legion#2788 reads the state
// with a strict schema and would refuse the whole body for a key it does not name. The record
// route serves its one issue through the same band. A caller is placed as 193 places every
// caller: by the contract its `Legion-Plugin-Contract` header states, else by its pod's claims,
// which count no claim at 193 or above, so a plugin of 193 meets the band by its header alone.
// `packages/contracts/src/legion-api.ts` mirrors the four on `legionIssue`, each optional. A
// caller at this entry's number is served what one at the contract before it is with the four
// keys beside it, so this entry's fixture adds the four key paths to the state's records and the
// record's, and the record of the owed answer below, which no contract before this entry has.
//
// The same contract adds the claim route `POST /legion/v1/claims/owed` (`api.OwedRequest`, the
// claim token, session and secret `claims/activity` carries), which answers `{"owes": bool,
// "work": string, "architect"?: string}` (`api.OwedResponse`): whether the claim owes work now,
// read from the issue's record as `owedBy` (`api/owed.go`) reads it, whose doc holds the rule,
// and what that work is called, "the testing phase of LEGION-208" or "the production check of
// LEGION-208", empty when nothing is owed.
// `architect` is the architect claim that owns the claim now, the end of the owner walk the
// daemon's own notices of the issue take (`record.OwnerWalk.Architect`): named for a phase worker
// whose issue the workflow records, owed or not, and absent for an architect, the controller, a
// failed claim and an unrecorded issue. The end-of-turn reminder renders that answer in both
// harnesses and keeps no model of the phase of its own (dispatch://INFRA-2746), and its architect
// notice and the stall watch's notice go to the architect it names. A plugin below this entry
// calls no such route, and nothing else of the claim wire changes;
// `packages/contracts/src/legion-api.ts` mirrors the two bodies as `LegionOwedRequest` and
// `LegionOwedResponse`, and `packages/contracts/fixtures/daemon-api/owed.json` is the answer's
// golden. The answer is the row `AnswerOwed` this entry adds to answers.go's Answers, which
// arrives with its route at this entry and is the identity at every contract that records it,
// and the request is a row this entry's test file adds to the request gate's table
// (pluginRequests), so this entry's fixture holds its three fields and a later type that stops
// reading one of them is red.
//
// The daemon ships four store migrations with it.
// `20261112012123_issue_repository_correction.up.sql` adds the four `issues` columns the keys are
// read from and the four `claims` columns of the repository an update round's pod was built from
// and of the repository a claim's process works, which no route serves. It alters `issues` and
// `claims` alone, so it applies whether it runs before or after a migration of a higher version,
// as on a database a daemon of 256 migrated, which runs it late. An older daemon binary put back
// on this schema writes none of them and reads none of them, and it carries the write-once guard
// this change removes, so it refuses a corrected repository as it always did while a correction
// this release applied stands.
// `20261112215432_delivery_resume.up.sql` adds `pending_task_deliveries.resume`, the wait a held
// task that resumes an answered root carries, so the task's text is written at each send. An
// older daemon neither reads nor writes that column: it sends the task text a row holds, which is
// empty for a resume task this release queued and never sent. It also adds `parked_decisions.seq`,
// Dispatch's sequence of a kept spec decision's review, `parked_decisions.artifact`, the document
// that review is of (the empty string on every row already there), and
// `parked_decisions.version`, the version reviewed (null on every row already there), which an
// older daemon neither reads nor writes: a row it inserts carries no sequence, no document, no
// version and its own clock, as before this release, while a decision it keeps over a row this
// release kept is written beside that row's sequence, document and version, the earlier
// review's, which is why the rollback docs/kubernetes.md gives under this contract clears the
// three columns first.
// `20261118073048_phase_entry_awaiting.up.sql` replaces `phases.entry_verbatim` with
// `phases.entry_awaiting`, the wait a resume answered, so the sweep's send again of a lost resume
// start is rendered at that send rather than sent as the text written at the raise. A daemon of
// the release before this one names `entry_verbatim` in its phases reads and writes, so it cannot
// run on this schema until the block docs/kubernetes.md gives under this contract puts it back.
// `20261122212816_readmitted_approval_artifact.up.sql` adds `readmitted_approvals.artifact`, the
// document whose approval a root's re-admission kept, so its architect's start asks the kept
// approval of that document; every row already there takes the empty string. An earlier daemon
// names the column in no statement, and its keep of an approval over a row this release kept
// leaves the column as it stood, the document of the earlier approval, which the rollback
// docs/kubernetes.md gives under this contract clears.
//
// This release's image pins Oh My Pi v18.18.6-sami.20261125-001804 (`.omp-pin`), which main's
// #5473 moved the pin to while this change was open, after #5183's v18.18.6-sami.20261123-024918.
// It carries all of v18.18.6-sami.20261118-035823, the first fork release whose `prompt` refusal
// and `prompt_result` carry `code: "session_busy"` when the agent is busy (dispatch://INFRA-3538;
// shimwire.CodeSessionBusy names the refusals that code covers). The daemon reads that code and
// no words. A pod of the release before this one, whichever release of main precedes this entry,
// runs the same Oh My Pi and sends the code already. A pod still on an image before
// v18.18.6-sami.20261118-035823, until its update round, sends its busy refusal with no code, and
// the daemon reads it as busy from its own record of the claim working or from the agent's
// get_state (supervise's agentRefusalOutcome). While the record does not hold the claim working,
// an agent on such an image is still charged in two cases: it does not answer get_state within
// the RPC bound, or its turn ended between its refusal and the ask, as a notice's turn can when the
// refusal came during its pre-turn work. Its refusal is then charged as unrecognised, and its
// prompt's result as a prompt that ran no turn. Nothing of the daemon API changes with the pin.
//
// This entry was numbered 294 before the cutover (d41b74eb), and
// scripts/daemon-contract-convert.sh converted it at the merge of main at 7102278681e2.
//
// repositoryCorrectionContract is the id of this change's entry, the daemon contract that added
// what a correction of an issue's repository left on its record (dispatch://INFRA-3136):
// `repoBefore` and `repoCorrectedAt`, the repository the issue left and the moment the daemon
// applied the correction, and `repoChangePending` and `repoChangeReason`, a repository Dispatch
// has named that the daemon has not applied and why. A plugin before it is served every issue
// without the four (withoutRepositoryCorrection): one built before sjawhar/legion#2788 reads the
// state with a strict schema and would refuse the whole body for a key it does not name, and one
// built after strips the key instead, so the band keeps the body its own contract's record
// describes. A pod's reading counts no claim at shapedAnswersContract or above (podContracts), so
// a caller from shapedAnswersContract to below this contract meets the band by the contract its
// header states. dispatchCredentialContract stands right below it with no row of stateNarrowings
// between the two, so a caller at that contract is served its keys without the four, and one
// below it is served the band of each row of a contract above its own as well. The same contract
// added the claim route `POST /legion/v1/claims/owed` with its answer (answers.go's AnswerOwed),
// which no contract below it records.
//
// It is the entry's id and never a contract number: each build numbers the entry by its place in
// main's merge order (entryNumber), and every row below names the entry by this constant, so no
// line moves when another entry merges before it.
const repositoryCorrectionContract = 20261125181104

func init() {
	registerEntry(contractEntry{
		id: repositoryCorrectionContract, slug: "repository-correction",
		bands: []stateNarrowing{
			{contract: repositoryCorrectionContract, issue: withoutRepositoryCorrection,
				dropped: "the issues' repoBefore, repoCorrectedAt, repoChangePending and repoChangeReason"},
		},
		answers: []Answer{
			// This change's entry: what a live role's claim owes now (`api.OwedResponse`), which the
			// end-of-turn reminder renders. The route arrives with the answer at this entry, so no
			// contract below it records either.
			{ID: AnswerOwed, Routes: []string{"POST /legion/v1/claims/owed"}, Since: repositoryCorrectionContract},
		},
		identityAnswers: []AnswerID{AnswerOwed},
	})
}

const AnswerOwed AnswerID = "owed"
