package api

// 20261125181047 p-0-bound: dispatch://INFRA-5636 -- a P0 issue starts at once, outside the admission cap, under a P0
// bound that can be set live (R10 of dispatch://INFRA-3199). An issue whose own Dispatch priority
// is P0 takes a slot at the pass that would start it but for the cap: the cap counts only the
// holders that are not P0, and the P0 bound, which has no value unless legion.yaml's
// `admission_p0_bound` gives one, limits how many P0 issues may hold a slot. Four routes: the
// controller's `POST /legion/v1/project/p0-bound` (`{grantId, bound?, unbounded?, reason}`, exactly
// one of `bound`, a whole number of at least 1, and `unbounded: true`, no bound whatever legion.yaml
// says) and `POST /legion/v1/project/p0-bound/clear` (`{grantId, reason}`, which returns the
// project to legion.yaml's bound), which take a controller grant, the controller session's or one
// the operator's bearer minted, and refuse any other with 403 `CONTROLLER_REQUIRED` (the pause's
// actor rule: a grant the operator's bearer minted changes the bound as the operator), and the
// operator's `POST /legion/v1/operator/project/p0-bound` (`{bound?, unbounded?, reason}`) and `POST
// /legion/v1/operator/project/p0-bound/clear` (`{reason}`), which take the operator's bearer as
// every operator route does (`legion project set-p0-bound` and `clear-p0-bound`). None names a
// project: the daemon changes its own. A set that names neither a bound nor none, both, or a bound
// below 1 is refused `400 INVALID_P0_BOUND`; every change refuses a blank reason with `400
// P0_BOUND_REASON_REQUIRED`; a daemon with no workflow refuses all four with `409
// P0_BOUND_UNAVAILABLE`. Each refusal is a Failure and changes nothing. A change is one fact
// (`intake.P0BoundSet`) whose admission appends it to the new table `project_p0_bound_changes`
// (migration `20261125181828`) and makes its pass in the same transaction, so the bound is in force
// from that pass on; every pass reads the project's newest change again. The controller's two
// routes answer `{bound, configured, live?}` (`api.P0BoundView`, answers.go's AnswerP0Bound, whose
// routes arrive with it, so no contract below 282 records either): `bound`, the bound in force or
// null for none; `configured`, legion.yaml's, null for none; and `live`, `{bound, setBy, setAt,
// reason}` with `bound` null for a set of none and `setBy` a pause's actor view, the value set
// while the daemon runs that stands, absent where none stands. The operator's routes answer the
// same body to the `legion` command, which no plugin parses. `GET /legion/v1/state` gains two keys
// under `admission`: `admission.p0Holders`, how many slot holders are P0, and `admission.p0Bound`,
// the P0BoundView. `admission.cap` keeps its number and now counts no P0 holder. `settings` gains
// `admission_p0_bound`, absent where the file gives none, which no supported reader types. The
// records of 282 hold the two keys under `admission`, and no record below 282 does: a caller whose
// contract (the one it states, else its pod's) is below 282 is served the admission without them
// (the band of shaping.go's stateNarrowings, `withoutP0Start`). No value of an enumerated field
// moves. (INFRA-3118's architect gave this entry 258 on 2026-11-24 at 15:50:02Z; #5164
// (dispatch://INFRA-3570) pushed 258 first and kept it, and the architect gave this entry 282 at
// 23:04:57Z, above main's 256. Read the base's own number and every open pull request again at the
// next merge rather than from this line.)
//
// p0BoundContract is version.go's entry for dispatch://INFRA-5636: the four P0 bound routes, the
// controller's answer (AnswerP0Bound), and the state's `admission.p0Holders` and
// `admission.p0Bound`, which a caller below it is served without (shaping.go's withoutP0Start). Its
// constant rather than its number stands in SupportedContracts, as priorityContract's does, so a
// renumber of the entry moves the two together.
const p0BoundContract = 20261125181047

func init() {
	registerEntry(contractEntry{
		id: p0BoundContract, slug: "p-0-bound",
		bands: []stateNarrowing{
			{contract: p0BoundContract, state: withoutP0Start, dropped: "admission.p0Holders and admission.p0Bound"},
		},
		answers: []Answer{
			// version.go's 282: the P0 bound the controller's set or clear left in force (`api.P0BoundView`).
			// The two routes arrive with the answer, so no contract below 282 records either.
			{ID: AnswerP0Bound, Routes: []string{
				"POST /legion/v1/project/p0-bound",
				"POST /legion/v1/project/p0-bound/clear",
			}, Since: p0BoundContract},
		},
		identityAnswers: []AnswerID{AnswerP0Bound},
	})
}

const AnswerP0Bound AnswerID = "p0-bound"
