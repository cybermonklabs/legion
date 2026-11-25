package api

// 20261125174131 state-seq: dispatch://INFRA-3931 -- the daemon block of `GET /legion/v1/state` gains `stateSeq`
// (api.DaemonInfo.StateSeq), the sequence of the newest state event the project's daemon
// committed (state_publication.last_seq, record.Store.StatePublication), read in the route's own
// repeatable-read snapshot: every state event at or below it is in the body and every one above
// it is not, so a reader of the daemon's state events (`notifications.legion.<token>.state`)
// starts after a whole read at `stateSeq + 1`, with nothing lost and nothing applied twice. Beside
// it, `stateId` (api.DaemonInfo.StateID) is the id of the store that count is kept in
// (state_publication.store_id), made at the project's first state row and never changed, which
// every state message carries: a reader applies only messages whose `stateId` is its starting
// point's. Beside both, `stateRun` (api.DaemonInfo.StateRun) is the id of the daemon run that
// serves the body, minted at the process's start, which is the `run` every state message of that
// run carries: a reader takes it as its starting point's run, so a boot of that run at or below
// `stateSeq` is one the body already holds and not a daemon whose sequence went back. The route
// writes `stateSeq` on every body, 0 while the project has no state row, `stateId` once the
// project has one, and `stateRun` from a daemon whose state events are on (one with a workflow),
// and a caller whose contract is below 293, whether it states it (`Legion-Plugin-Contract`) or
// the claims of its pod record it, is served the daemon block without any of the three
// (shaping.go's stateSeqContract and withoutStateSeq): one built before sjawhar/legion#2788 reads
// the state with a strict schema and would refuse the whole body for a key it does not name, and
// one built after strips the keys instead.
// `packages/contracts/src/legion-api.ts` reads the three keys as optional, so a reader of this
// contract served such a body, or the body of a daemon before 293, takes them as absent rather
// than as 0.
// The same contract adds the observed view of the route, `GET /legion/v1/state?view=observed`
// (api.ObservedState, StateSource.ObservedState): what Dispatch observes of a daemon and nothing
// more, read in a repeatable-read, read-only snapshot of its own under the same bound as the whole
// state. It carries the whole state's daemon block, its `settings` and its `admission` block,
// unfiltered and in the same order, and `issues`, every issue the whole state lists with only its
// `key`, `phase`, `tree`, `slot.index`, `waitingOn`, `architect.state` and
// `workers.<role>.claim.state`, each absent where the whole state's is absent. The view is
// shaping.go's answer AnswerObservedState, which is no row of Answers, and observedFor shapes it:
// it is served at the caller's contract by the whole state's own bands and value strips, so at
// every contract this daemon serves, each member of the view is the whole state's member at that
// contract: a caller below 293 reads no `stateSeq`, `stateId` or `stateRun` in either body, and a
// caller below 51 no `admission.trees` and no issue's `tree`. A request with no `view` gets the
// whole state, whose body does not change. A request with any other `view` gets the whole state
// too, and the daemon logs a warning that names the values given and the one view the route
// serves, because a later version may refuse such a request.
// `packages/contracts/src/legion-api.ts` reads the view as LegionObservedStateResponse.
// (This change first took 166, the lowest number above main's 152 that no open pull request
// declared, as `.github/scripts/check-daemon-api-contract.sh` read it on 2026-11-18: 153 #4115,
// 154 #3659, 155 #4273, 156 #4065, 157 #3993, 158 #4251, #4259, #4261, #4269 and #4290, 159 #4175,
// 160 #3519, 161 #4173, #4180 and #4193, 162 #2230 and #3712, 163 #4049, 164 #4017 and 165 #4392.
// While it was open, main moved on to 160 and then to 168 (#4488, which passed over 167 as
// declared), past 166, so at the merge of 2026-11-19 that brought main in at a20e0b0593 the number
// moved again: the same check, read on 2026-11-19, named 175 as the lowest number above main's 168
// that no open pull request declares, with 169 declared by #3831 and #4065, 170 by #2230, #4030,
// #4173, #4180 and #4193, 171 by #4115, #4485 and #4495, 172 by #3659, 173 by #4493 and 174 by
// #4273. While it was still open, main took 178 (entry 178 above, #4115), which passed over 176
// and 177 as declared and stands above 175, so at the merge of 2026-11-20 that brought main in at
// bbd99eae86 the number moved a third time: the same check, read on 2026-11-20, named 183 as the
// lowest number above main's 178 that no open pull request declares, with 179 declared by #4392
// and #4540, 180 by #4049 and #4175, 181 by #4273 and #4553, and 182 by #4495. While it was
// still open, main took 188 (entry 188 above, #4543), which stands above 183, so at the merge of
// 2026-11-20 that brought main in at 1c7c825367 the number moved a fourth time: the same check,
// read on 2026-11-20 at 10:40Z, named 193 as the lowest number above main's 188 that no open pull
// request declares, with 189 declared by #3831, #4592 and #4596, 190 by #4493 and #4578, 191 by
// #4495, and 192 by #4611. While it was still open, main took 189 (the entry above, #4581), so at
// the merge of 2026-11-20 that brought main in at a4cd4949fc the number moved a fifth time: the
// same check, read on 2026-11-20 at 16:09Z, named 196 as the lowest number above main's 189 that no
// open pull request declares, with 190 declared by #4578, 191 by #2230, #4049, #4495 and #4573, 192
// by #3993, #4173, #4175, #4182, #4193, #4273, #4392 and #4580, 193 by #3659, #3712, #4065 and
// #4553 as well as this change, 194 by #3831 and #4030, and 195 by #4493 and #4590. At the merge
// of 2026-11-20 that brought main in at 8a9540682d (main's 193, entry 193 above, #3659), the same
// check, read on 2026-11-20 at about 23:20Z, refused 196: the tip of main then carried 199, so the
// number moved a sixth time, to 202, the lowest number above 199 that no open pull request
// declares, with 200 declared by #4065, #4493 and #4580, and 201 by #4553 and #4611. The forward
// merge of 2026-11-20 that brought main in at f1d27d3a82 brought that 199 (#4495's entry above),
// and the same check accepted 202, above it. The forward merge of 2026-11-21 that brought main in
// at 1cdf5b9ceb brought main's own 202 (#4493's entry above, INFRA-4401), so the number moved a
// seventh time: the same check, read on 2026-11-21 at about 03:50Z, named 207 as the lowest
// number above main's 202 that no open pull request declares, with 203 declared by #2230, #3993,
// #4065, #4175, #4553, #4592, #4596 and #4661, 204 by #3831 and #4030, 205 by #4573 and 206 by
// #4540. The forward merge of 2026-11-21 that brought main in at a155b5a33a brought main's own 205
// (#4573's entry above, INFRA-3178), which stands below 207, so this entry kept 207. The forward
// merge of 2026-11-22 that brought main in at 83b91235c5ea brought main's own 213 (#4553's entry
// above, INFRA-4036), which stands above 207, so the number moved an eighth time: a reading of
// `const DaemonAPIVersion` at refs/pull/<n>/head for each of the 444 open pull requests, made
// between 2026-11-22T11:23:44Z and 11:27:41Z, found 214 declared by #4592 and #4918, 215 by #4065
// and #4596, 216 by #4173, #4180, #4182, #4193, #4251, #4259, #4261, #4269, #4290, #4932 and
// #4984, 217 by #4273, #4590 and #4771, 218 by #3831, 219 by #4611, #4902 and #4948, 220 by
// #4930, and no open head declaring 221, so this entry takes 221, the lowest number above main's
// 213 that no other open pull request declares.
// The forward merge of 2026-11-23 that brought main in at e96f1420b413 brought main's own 217
// (#4273's entry above, dispatch://INFRA-3531 and dispatch://INFRA-4567), which stands below 221,
// so this entry kept 221, and so did the forward merge of 2026-11-23 that brought main in at
// 0031d91331. The forward merge of 2026-11-24 that brought main in at 3d57a4c3ee94 brought main's
// own 219 and 220 (the two entries above, the owner's plan and the experiment group), which stand
// below 221, so this entry kept 221: a reading of `const DaemonAPIVersion` at the head commit of
// each of the 514 open pull requests, made between 2026-11-24T06:23:39Z and 06:26:23Z, found 221
// declared also by #4049, #4392, #4682, #4940 and #5164, and every number from 222 to 242
// declared by at least one open pull request. The forward merge of 2026-11-24 that brought main
// in at e14345bfc8bd brought main's own 228 (INFRA-1575's entry above), which stands above 221,
// so the number moved a ninth time: a reading of `const DaemonAPIVersion` at the head commit of
// each of the 496 open pull requests, made between 2026-11-24T09:38:52Z and 09:41:52Z, found 229
// declared by #2230, 230 by #4580 and 231 by #4540, and no open head declaring 232, so this
// entry takes 232, the lowest number above main's 228 that no other open pull request declares.
// The forward merge of 2026-11-24 that brought main in at d27405712c7c brought main's own 232
// (INFRA-4508's entry above), so the number moved a tenth time, to 254, the number INFRA-3118's
// architect allocated to this pull request at 2026-11-24T12:42:03Z, while the numbers are allocated
// rather than computed (dispatch://INFRA-1569).
// The forward merge of 2026-11-24 that brought main in at eead7a5fd713 brought main's own 246 (the
// pod hold's entry above, #4960), which stands below 254, so this entry kept 254.
// The forward merge of 2026-11-24 that brought main in at 1af8002b42b5 brought main's own 250 and
// 256 (the two entries above, the waiting line's priority, #5365, and the claim views' cause
// times, #5349), and 256 stands above 254, so the number moved an eleventh time: a reading of
// `const DaemonAPIVersion` at the head of each of the 141 of the 491 open pull requests that
// change this file or whose file list GitHub cut short, made between 2026-11-24T20:23:31Z and
// 20:26:26Z, found above main's 256 only 257 (#4580), 261 (#5332), 262 (#5204), 265 (#5285), 268
// (#5384) and 271 (#4173). So 258 was the lowest number above main's 256 that no open pull
// request declared, and this entry took 258.
// Then #5164 pushed 258 first, at about 21:06Z on 2026-11-24. INFRA-3118's architect allocates
// the numbers until dispatch://INFRA-1569's #5399 merges, and gave this entry 273
// (dispatch://INFRA-3931/comment/62f173fd-f15d-4edc-94b3-d3aa4b4d8a9d), so the number moved a
// twelfth time.
// A caller from 256 to 274 is served 256's keys, and one from 275 to 292 275's keys, and neither
// is served any of the three.
// stateSeqContract follows dispatchCredentialContract, 275, as the last number of answers.go's
// SupportedContracts.
// That is a reading and never the rule: read main's own number and every open head again at the
// next forward merge rather than from this line.)
// (This change takes 293, the number INFRA-3118's architect allocated to it on 2026-11-25T09:29Z
// under INFRA's controller's ruling that it allocates contract numbers until #5399 merges, at
// the merge that brought main in at ac1c9a332e, which declares 275 (#4392's entry above); it had
// declared 273, which main's 275 passed. The architect's census of 2026-11-25T09:22:54Z read all
// 499 open heads and found no declaration of 293.)
//
// stateSeqContract is the daemon contract that added `daemon.stateSeq`, the sequence of the newest
// state event the project's daemon committed, read in the body's own snapshot, and beside it
// `daemon.stateId`, the id of the store that count is kept in, and `daemon.stateRun`, the id of the
// daemon run that serves the body (version.go's 293, DaemonInfo.StateSeq, StateID and
// StateRun). A plugin before it is served the daemon block without the three keys
// (withoutStateSeq, its row of stateNarrowings): one built before sjawhar/legion#2788 reads the
// state with a strict schema and would refuse the whole body for a key it does not name, and one
// built after strips the keys instead, so the strip is what keeps them from a caller that did not
// ask for them rather than what keeps such a caller reading at all. The same contract added the
// observed view of the state route, shaping.go's answer AnswerObservedState, which is no row of
// Answers: it carries the same daemon block, and observedFor narrows it by the same band.
// dispatchCredentialContract stands below it with no row of stateNarrowings between the two, so a
// caller at or above dispatchCredentialContract and below this contract is served 275's keys and
// none of the three.
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes moves
// that constant, and the contract these keys arrived at stays where it is. It is the last number
// of answers.go's SupportedContracts, which names the constant rather than its number for that
// reason.
const stateSeqContract = 20261125174131

func init() {
	registerEntry(contractEntry{
		id: stateSeqContract, slug: "state-seq",
		bands: []stateNarrowing{
			{contract: stateSeqContract, state: withoutStateSeq,
				dropped: "daemon.stateSeq, daemon.stateId and daemon.stateRun"},
		},
	})
}
