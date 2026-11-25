package api

// 20261125170933 admission-cap: dispatch://INFRA-5616 -- a person changes the project's admission cap while the daemon
// runs, with no pull request and no restart, from 1 up to the deployment's ceiling: the operator,
// or the controller on a person's cited words. Four routes: the controller's `POST
// /legion/v1/project/admission-cap` (`{grantId, cap, reason}`) and `POST
// /legion/v1/project/admission-cap/clear` (`{grantId, reason}`), which take a controller grant,
// the controller session's or one the operator's bearer minted, and refuse any other with 403
// `CONTROLLER_REQUIRED`, whose sentence says the change requires a controller grant (the pause's
// actor rule: a grant the operator's bearer minted changes the cap as the operator), and the
// operator's `POST /legion/v1/operator/project/admission-cap` (`{cap, reason}`) and `POST
// /legion/v1/operator/project/admission-cap/clear` (`{reason}`), which take the operator's bearer
// as every operator route does (`legion project set-admission-cap` and `clear-admission-cap`).
// None names a project: the daemon changes its own. The deployment's `admission_cap` (else
// `LEGION_ADMISSION_CAP`) is the cap the daemon starts under and what a clear returns to; its
// `admission_ceiling` (else `LEGION_ADMISSION_CEILING`, else `admission_cap`), which the loader
// holds at least 1 and at least `admission_cap`, is the ceiling. A set refuses a cap below 1 with
// `400 INVALID_ADMISSION_CAP` and one above the ceiling with `409 ADMISSION_CAP_ABOVE_CEILING`,
// whose sentence names the ceiling and `admission_ceiling`; every change refuses a blank reason
// with `400 ADMISSION_CAP_REASON_REQUIRED` and a reason longer than 2,048 characters with `400
// ADMISSION_CAP_REASON_TOO_LONG`, whose sentence names the bound; a daemon with no workflow
// refuses all four with `409 ADMISSION_CAP_UNAVAILABLE`. Each refusal is a Failure, its code and
// its sentence, and changes nothing. A set or a clear is one fact (`intake.AdmissionCapSet`) whose
// admission stamps the change when it applies, under the one lock every fact takes, appends it to
// the new table `project_admission_cap_changes` (migration `20261124040825`), which keeps every
// change, and makes its pass in the same transaction, so the cap is in force from that pass on;
// every pass reads the project's newest change again (admit's capAt) and admits under the smaller
// of the cap it set and the ceiling, or under the starting cap after a clear or where none was
// made, so a cap set before a restart holds after it, under the ceiling of that boot. A lower cap
// stops nothing that runs: it starts nothing new until fewer issues than it hold a slot. A set of
// the cap that stands, and a clear where none stands, change nothing: they append no row, and the
// daemon logs one Debug line for each. The controller's two routes answer `{cap, ceiling,
// override?}` (`api.AdmissionCapResponse`, answers.go's AnswerAdmissionCap, whose routes arrive
// with it, so no contract below 307 records either); the operator's answer the same body to the
// `legion` command, which no plugin parses. `cap` is the cap the passes admit under, `ceiling` the
// deployment's `admission_ceiling`, and `override`, `{cap, setBy, setAt, reason}` with `setBy` a
// pause's actor view (`{kind: controller, session}` or `{kind: operator}`), is the cap set while
// the daemon runs, absent after a clear. `GET /legion/v1/state` gains three keys under
// `admission`: `admission.ceiling`, `admission.override` and `admission.capChanges`, the newest 50
// changes, newest first, `[]` before the first change, each `{id, cap?, capBefore, capAfter,
// ceiling, setBy, setAt, reason}` (`cap` absent for a clear), the record the project's Evals tab
// reads (dispatch://INFRA-5616/spec#b-r7-tick-log, dispatch://INFRA-979/spec#b-r39), so its names
// are held as they stand. `admission.cap`, which every reader has typed since before the floor,
// now says the cap the passes admit under where it said the configured cap, the same number until
// a cap is set. The records of 307 hold the three keys, and no record below 307 does: a caller
// whose contract (the one it states, else its pod's) is below 307 is served the admission without
// them (the band of shaping.go's stateNarrowings, `withoutAdmissionCap`). `settings.admission_cap`
// stays the configured starting cap. No value of an enumerated field moves: the actor's `kind` is
// a discriminator read with a `z.literal`, as the pause's is.
// (This change takes 307, the number INFRA-3118's architect allocated to it
// (dispatch://INFRA-3118/artifact/open-items-md@v467), at the merge that brought main in at
// 830cfacbbc, which declares 275: a reading of `const DaemonAPIVersion` at the head of each of
// the 502 open pull requests, from 2026-11-25T11:02:09Z to 11:08:05Z, found none above 295, and
// 296 to 306 are reserved for changes not yet pushed. It had declared 243, then 268.)
//
// admissionCapContract is version.go's entry for dispatch://INFRA-5616: the four cap routes, the
// controller's answer (AnswerAdmissionCap), and the state's `admission.ceiling`,
// `admission.override` and `admission.capChanges`, which a caller below it is served without
// (shaping.go's withoutAdmissionCap). Its constant rather than its number stands in
// SupportedContracts, as OwnerPlanContract's does, so a renumber of the entry moves the two
// together.
const admissionCapContract = 20261125170933

func init() {
	registerEntry(contractEntry{
		id: admissionCapContract, slug: "admission-cap",
		bands: []stateNarrowing{
			{contract: admissionCapContract, state: withoutAdmissionCap,
				dropped: "admission.ceiling, admission.override and admission.capChanges"},
		},
		answers: []Answer{
			// version.go's admissionCapContract: the cap the controller's set or clear of the admission cap
			// left (`api.AdmissionCapResponse`). The two routes arrive with the answer, so no contract
			// below it records any of them.
			{ID: AnswerAdmissionCap, Routes: []string{
				"POST /legion/v1/project/admission-cap",
				"POST /legion/v1/project/admission-cap/clear",
			}, Since: admissionCapContract},
		},
		identityAnswers: []AnswerID{AnswerAdmissionCap},
	})
}

const AnswerAdmissionCap AnswerID = "admission-cap"
