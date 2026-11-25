package api

// 20261125161252 questions-for-controller: INFRA-204 (cybermonklabs/legion) -- api.State gains `questionsForController`, every
// question to a stopped agent that the daemon's answer loop passed to the controller in the last
// seven days (id, issue, role, session, asker, ref, reason, passedAt, and writes, each gh call
// that writes which the loop ran for it), newest first and at most 100. The controller's
// `agent-question` notice is a live wake only, so a controller that starts later reads what it
// missed here. A body this contract writes carries the key on every state, never null. A caller
// below 286 — the contract its `Legion-Plugin-Contract` header states, else the highest its pod's
// claims record below 193, since a plugin from 193 states its contract — is served the state
// without the key (shaping.go's stateNarrowings, questionsForControllerContract,
// withoutQuestionsForController).
// The other bands compose with this one: below 178 the pause's keys are absent, below 193 the
// shaped answers' keys are absent, and below 202 the repositories block has its 152 shape.
// Contract 205 moves no state shape. Contract 213 strips the `caches` capability source below
// its own level. Contract 217 adds only an untyped nested `settings` key, and 219 widens two
// state values and adds a key to a refusal answer. Contracts 220, 228, 246 and 250 still omit,
// below their respective levels, assignment `plan`, `intakeGiveUps`, issue `podHold` and the
// issue and waiting-line `priority` keys. Contract 232 shapes requests only. Contract 256
// still omits claim-view `lastDeathAt` and `lastStartFailureAt` for callers below it, and
// contract 275 omits `dispatchCredential` for callers below it. Callers from 256 through 274
// receive the cause times without `dispatchCredential` or the questions, and callers from 275
// through 285 receive `dispatchCredential` (under its setting) without the questions.
// The 286 reader takes the key as optional for the narrowed body alone, as it takes
// `wakesWaiting`; the boot gate and `legion probe-image` refuse it with a daemon below 286.
// The key's one enumerated field is each question's `role` (answers.go's questionRoles).
// Its values arrived with the key and ride its band. The 286 answer records are 275's under a
// new number, with the questions' key paths and role domain on the state's record. Each state
// record from 46 to 275 adds `"questionsForController.*.role": []`, since every record holds
// the domains of all enumerated fields of its answer (TestTheRecordSetIsOneRecordPerAnswerPerContract)
// and this field has no values below 286. No other record below 286 moves and no request changes.
// (This branch
// took 38, then 52 while main stood at 51, then 57 while main stood at 56, then 58 while main
// stood at 57, then 59 while main stood at 58, then 60 while main stood at 59, then 61 while main
// stood at 59 and #2539 and #2731 also declared 60, then 122 while main stood at 111, then 126
// while main stood at 123, then 149 while main stood at 144 and kept it while main stood at 146,
// then 157 while main stood at 151, then 162 while main stood at 152, then 168 while main stood
// at 160, then 170 while main stood at 160, then 171 while main stood at 168, then 183 while main
// stood at 178, then 191 while main stood at 188, then 190 while main stood at 189, then 198
// while main stood at 189, 190 being declared by #4611 too, and kept 198 at the forward merge that
// brought main in at 193 (#3659), which is below it, then 203 while main stood at 199, 198 being
// declared by #4175 and #4589 too, then 208 while main stood at 202, 203 being declared by #3993,
// #4065, #4175, #4553, #4592, #4596 and #4661 too, then 217 while main stood at 205, 208 being
// declared by #4175 too, then 211 while main stood at 205, 217 being declared by #4273, #4590 and
// #4771 too, then 228 while main stood at 213 (#4553), then 229 while main stood at 213, 228
// being declared by #4590 too, and kept 229 when main came to stand at 217 (#4273), at 219 (#4611)
// and at 220 (#4930), then 259 while main stood at 256, then 263 while main still stood at 256,
// then 286 while main stood at 275.
// It took 198 because a census of every open pull request at 2026-11-20T19:29Z, read with the
// `declared_by_open_pull_requests` of main's .github/scripts/check-daemon-api-contract.sh, found
// 189 declared by #2402, #3512, #3995, #4184, #4206, #4209, #4274, #4533, #4631, #4634 and #4639,
// 190 by this change's own earlier head and #4611, 191 by #4049 and #4573, 192 by #4175, #4180,
// #4182 and #4193, 193 by #3659 and #3712, 194 by #3831 and #4030, 195 by #4590, 196 by #3993,
// #4173, #4273, #4392, #4432, #4493, #4495, #4580, #4592 and #4596, 197 by #4065, #4553 and #4578,
// and nothing at 198, so 198 was then the lowest number above main's that no open pull request
// declared. It took 203 because a census of every open pull request at 2026-11-20T23:55Z, read the
// same way, found 196 declared by #3993, #4173, #4180, #4193, #4273, #4432, #4592 and #4596, 197 by
// #4578, 198 by #4175 and #4589, 199 by #2697, #3831, #4187, #4274, #4392 and #4661, 200 by #4065
// and #4493, 201 by #4553 and #4611, 202 by #4580, and nothing at 203, so 203 was then the lowest
// number above main's 199 that no open pull request declared. It took 208 because a census of every
// open pull request at 2026-11-21T04:50Z, read the same way, found 200 declared by #4049, 201 by
// #4611, 202 by #2827, #3729, #4273, #4274, #4392, #4438, #4457, #4557, #4580, #4590 and #4726, 203
// by #3993, #4065, #4175, #4553, #4592, #4596 and #4661, 204 by #3831, 205 by #3712 and #4573, 206
// by #4540, 207 by #4030 and #4432, and nothing at 208, so 208 was then the lowest number above
// main's 202 that no open pull request declared. It took 229 because a census of every open pull
// request at 2026-11-22T18:02Z, read the same way, found 214 declared by #4592, 215 by #4596, 216
// by #4173, #4180, #4182, #4193, #4259, #4932 and #4984, 217 by #4273, 218 by #3831, 219 by #4611,
// 220 by #4930, 221 by #3993, #4049, #4392, #4432 and #4771, 222 by #4661, 223 by #4805, 224 by
// #4960, 225 by #3712 and #4580, 226 by #4175, 227 by #4065 and 228 by #4590, and nothing at 229,
// so 229 was then the lowest number above main's 213 that neither main nor any other open pull
// request declared. It kept 229 at the forward merge that brought main in at 8f332682, where main
// carried 217 (#4273), because a census of every open pull request at
// 2026-11-23T01:13Z, read the same way, found 218 declared by #3831, 219 by #4611, 220 by #4930,
// 221 by #3993, #4049, #4392, #4432, #4771 and #4940, 222 by #4661, 223 by #4805, 224 by #4960, 225
// by #3712, 226 by #4175, 227 by #4065 and 228 by #4590, and nothing at 229. It kept 229 at the
// forward merges that brought main in at 9f0c16a2 and eb9f37a9, where main still carried 217,
// because a census of every open pull request at 2026-11-23T14:43Z, at the second of them, read
// the same way, found 218 declared by #3831 and #5168, 219 by #4611 and #5161, 220 by #4930, 221
// by #4049, #4392, #4432, #4940 and #5164, 222 by #5156, #5157, #5158 and #5160, 223 by #3993 and
// #4661, 224 by #4960 and #5165, 225 by #3712, 226 by #4175, 227 by #5162 and 228 by #4590, and
// nothing at 229. It kept 229 at the forward merge that brought main in at 7cb1f229, where main
// carried 219 (#4611), because a census of every open pull request at 2026-11-23T20:34Z, read the
// same way, found 220 declared by #4930, 221 by #4049, #4392, #4432, #4940 and #5164, 222 by
// #5156, #5157, #5158 and #5160, 223 by #3993 and #4661, 224 by #4960 and #5165, 225 by #3712, 226
// by #4175, 227 by #5162 and 228 by #4590, and 229 by this change alone, and at the one that
// brought main in at 4a3eb0215e, which moved no contract. It kept 229 at the forward merge that
// brought main in at 7df64124, where main carried 220 (#4930), because a census of every open pull
// request at 2026-11-23T23:24Z, read the same way, found 221 declared by #4049, #4392, #4432, #4940
// and #5164, 222 by #5156, #5157, #5158 and #5160, 223 by #3993 and #4661, 224 by #4960 and #5165,
// 225 by #3712, 226 by #4175, 227 by #5162 and 228 by #4590, and 229 by this change alone, so 229
// was then the lowest number above main's 220 that neither main nor any other open pull request
// declared. It took 259 because a census of every open pull request made between
// 2026-11-24T22:46:34Z and 22:51:07Z, read the same way, found 257 declared by #4580 and 258 by
// #4432 and #5164, with 261 by #5332, 262 by #5204, 265 by #5285, 268 by #5384, 271 by #4173 and
// 276 by #4592 above them, and nothing at 259, so 259 was then the lowest number above main's 256
// that no open pull request declared. It took 263 at the forward merge of 2026-11-25 that brought
// main in at 99327832, where main still carried 256, because a census of every open pull request
// made between 2026-11-25T03:01:00Z and 03:04:31Z, read the same way, found 257 declared by
// #4580, 258 by #5164, 259 by this change's own earlier head (#2230), #3712 and #5156, 260 by
// #3831, 261 by #5332 and 262 by #5204, and nothing at 263, so 263 was then the lowest number
// above main's 256 that no open pull request declared. It took 286 at the forward merge of
// 2026-11-25 that brought main in at 830cfacb, where main carried 275 (#4392), because a census
// of every open pull request made between 2026-11-25T10:43:54Z and 10:46:29Z, read the same way,
// found 276 declared by #4592, 277 by #4771, 278 by #4596, 279 by #4580, 280 by #5307, 281 by
// #4540, 282 by #5488, 283 by #5463, 284 by #5157 and 285 by #5158, and nothing at 286, so 286
// was then the lowest number above main's 275 that no open pull request declared. Read main's own
// number and every open pull request's again at the next merge.
// A renumber moves `questionsForControllerContract` in shaping.go with it, the last number of
// answers.go's SupportedContracts.)
//
// questionsForControllerContract is the daemon contract that added `questionsForController` to
// the state (version.go's 286): every question to a stopped agent that the daemon's answer loop
// passed to the controller in the last QuestionsForControllerWindow. A plugin before it is served
// the state without the key (withoutQuestionsForController, its row of stateNarrowings): one built
// before sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body
// for a key it does not name, empty as surely as naming a question, and one built after strips the
// key instead, so the band keeps the body its own contract's record describes.
// dispatchCredentialContract stands immediately below it, so a caller from 275 to 285 is served
// `dispatchCredential` (under its setting) but not the questions. The one enumerated field it
// added is `questionsForController.*.role` (answers.go's questionRoles), whose values arrived with
// the key and ride its band.
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes moves
// that constant, and the contract the key arrived at stays where it is. It moves only with this
// change's own number, which is why answers.go's SupportedContracts names it rather than its
// number.
const questionsForControllerContract = 20261125161252

func init() {
	registerEntry(contractEntry{
		id: questionsForControllerContract, slug: "questions-for-controller",
		bands: []stateNarrowing{
			{contract: questionsForControllerContract, state: withoutQuestionsForController, dropped: "questionsForController"},
		},
		enumeratedFields: []EnumeratedField{
			// `role` of one question the answer loop passed to the controller
			// (api.QuestionForController.Role). It arrived with `questionsForController`, which is served
			// down below that contract whole (shaping.go's withoutQuestionsForController). The question's
			// `reason` beside it is no row: the schema reads it as a string, so a reason a later loop
			// names is read rather than refused.
			{Answer: AnswerState, KeyPath: "questionsForController.*.role", Values: questionRoles},
		},
	})
}

// questionRoles is the same list of roles at the contract `questionsForController` arrived at,
// which the role of the stopped agent each question was asked of takes
// (api.QuestionForController.Role): no `questionsForController` is served below that contract at
// all (shaping.go's withoutQuestionsForController).
var questionRoles = valuesFrom(questionsForControllerContract, floorRoles...)
