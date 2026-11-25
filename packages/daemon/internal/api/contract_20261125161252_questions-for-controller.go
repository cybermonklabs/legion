package api

// 20261125161252 questions-for-controller: INFRA-204 adds `questionsForController` to api.State.
// The daemon lists up to 100 questions to stopped agents that its answer loop passed to the
// controller in the last seven days, newest first. It includes the id, issue, role, session,
// asker, ref, reason, passedAt and each `gh` write the loop ran for the question. The controller's
// `agent-question` notice is a live wake only. A controller that starts later reads missed
// questions in the state.
//
// At this entry's contract, the state always has the key, as `[]` when there are no questions.
// For callers below the entry, its band omits the key. A strict older plugin would refuse the
// whole state even if the key were empty. The current reader accepts a narrowed body without
// the key, as it does for `wakesWaiting`. The boot gate and `legion probe-image` still require
// an exact contract match. The band composes with older bands, including the one that omits
// `dispatchCredential` below the last numbered contract.
//
// The entry fixture adds the question's key paths and the `questionsForController.*.role`
// domain to the state answer. The role values arrive with the key. The entry does not change
// requests or the frozen records. questionsForControllerContract is the entry's ID. Code that
// needs the number for this build uses entryNumber(questionsForControllerContract).
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
