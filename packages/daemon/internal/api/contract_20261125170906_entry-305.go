package api

// 20261125170906 entry-305: the plugin's state schema and phase list drop what no daemon serves. The state schema of
// `packages/contracts/src/legion-api.ts` no longer declares `slot.lentTo` or `admission.free`,
// which no daemon has written (both came with a widening of contract 12's schemas that changed no
// daemon), and pi-legion's `read_state` summary no longer prints them. The phase `integrating`
// leaves answers.go's `phases` and `workflowPhases`, the enumerations `enumerated-fields.json` and
// `contract-shapes.json` carry, and the plugin's `LEGION_WORKFLOW_PHASES`: `phase.All` has never
// held it, no constraint on an issue's `phase` or `held_from` has ever admitted it, and no
// transition sets it. No answer this daemon serves moves, so nothing is served down: a reader
// below 305 admits a value this daemon never writes, and a reader at 305 is served every value it
// admits. (This merge brought main's entries up to 275, at 830cfacbbc, so this entry moved from 261
// to 305, the number allocated to it.) (This entry took 252 when main declared 232, read at
// d27405712c: a reading of all 481 open heads that ended at 2026-11-24T12:55:40Z found 251 declared
// by #5307 as the highest, and 246 to 250 were held for another tree's work. #4540 then declared 252
// and pushed it first, and 253 to 260 are held or declared by other changes, so this entry moved
// to 261, the lowest number above them, which a reading of every open head made just before this
// commit's push found free. The forward merge of 2026-11-24 that brought in 1af8002b42 brought
// main's 246 (#4960), 250 (#5365) and 256 (#5349), each below 261, so this entry kept 261, and
// answers.go's SupportedContracts ends causeTimesContract, 261. Read main's own number and every
// open head again at the next merge rather than from this line.)
const entry305Contract = 20261125170906

func init() {
	registerEntry(contractEntry{id: entry305Contract, slug: "entry-305"})
}
