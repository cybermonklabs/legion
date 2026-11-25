package api

// 20261125161823 tree-in-backlog-or-icebox: the held word of a member of a tree whose root stands in backlog or icebox is
// `tree_in_backlog_or_icebox` (record.HeldTreeInBacklogOrIcebox), and the route table names no
// child route of a removed operation by its path. The word is a value of
// `admission.held[].reason` on `GET /legion/v1/state` and of `held[].reason` on the controller's
// tick notice. It takes the place of the word the field carried for that hold since 98, which
// this daemon no longer writes, and it arrives at this contract (answers.go's heldReasons, at
// treeInBacklogOrIceboxContract). A caller whose contract (the one it states, else its pod's
// lowest) is below 280 is served each such row as `unclassified` with no `heldBy` (shaping.go's
// withoutUnadmittedHeldWords), a word every reader of the field admits. So the records of
// `contract-shapes.json` below 280 hold the domain of `admission.held.*.reason` without the word
// that was renamed, as the daemon serves them, and the records of 280 hold the new word.
// `packages/contracts/src/legion-api.ts`'s `LEGION_HELD_REASONS` and the controller's
// `references/admission.md` name the new word, `legion state`'s `admission.held:` line counts
// under it, and the issue's `why` says "Its tree's root <root> stands in backlog or icebox, …".
// The migration 20261124002047 moves the word in every unsent tick notice, so a notice queued
// before the upgrade reaches the controller in the new word.
// The child route: `POST /legion/v1/children/{op}` is registered to removedChildRoute in place of
// the row of the one route it answered, so every child route this daemon does not serve answers
// 410 ROUTE_REMOVED with the status to write in Dispatch, and `POST /legion/v1/children/rerun`
// keeps its own, more specific row. An older plugin's call of the removed operation gets the
// answer it got at 205, and a call of a child route no plugin ever sent gets that answer too
// rather than 404 `no route`. requests.go's RemovedRoutes names the pattern, removed at 205, and
// the request gate resolves each recorded route through the server's mux to the pattern that
// serves it, so the request records of 193, 199 and 202 still pass. No request of a plugin moves,
// and the request records of 280 are 232's under its own number.
// (This entry took 241 when main declared 220: a reading of every open pull request of
// cybermonklabs/legion on 2026-11-24 found each number from 221 to 240 declared by at least one.
// The forward merge of 2026-11-24 that brought in e14345bfc8 brought main's 228 (INFRA-1575's
// entry above). A reading of `const DaemonAPIVersion` at refs/pull/<n>/head for each of the 493
// open pull requests that changes this file or whose file list GitHub cut short, 137 heads, made
// between 2026-11-24T09:50:35Z and 09:53:15Z, found 229 (#2230), 230 (#4580) and 231 (#4540)
// declared above 228, 232 declared by none, and 241 declared by #4771 as well as by this change's
// own pull request, so this entry moved to 232, the lowest number above main's 228 that no open
// pull request declared then. The forward merge of 2026-11-24 that brought in 1b636f3193 found
// main still at 228 and #5271, which declares 232, in the merge queue ahead of this change: a
// reading of `const DaemonAPIVersion` at refs/pull/<n>/head for all 483 open pull requests,
// 2026-11-24T11:34Z-11:36Z, found the highest at 245 (#4960, #5158), and INFRA-3118's tree holds
// 246 to 250, so this entry moved to 251. The forward merge of 2026-11-24 that brought in
// 3dbaa2b23a brought main's 232 (#5271, INFRA-4508's entry above); 251 stays above it. The forward
// merge of 2026-11-24 that brought in 25cbe2b315 brought main's 246 (#4960's entry above); 251
// stays above it. The forward merge of 2026-11-24 that brought in 5df00e2f13 brought main's 250
// (#5365's entry above); 251 stays above it. The forward merge of 2026-11-25 that brought in
// c227d04661 found main at 256 (#5349's entry above); INFRA-3118's architect, who allocates
// contract numbers until INFRA-1569's #5399 merges, allocated 280 to this change. The forward
// merge of 2026-11-25 that brought in main 830cfacbbce4 found main at 275 (#4392's entry above).
// From this entry the last number of answers.go's SupportedContracts is
// `api.treeInBacklogOrIceboxContract`, after main's 232, 246, 250, 256 and 275. Read the base's
// own number and every open pull request again at the next merge rather than from this line.)
//
// treeInBacklogOrIceboxContract is the daemon contract at which the held word of a member of a
// tree whose root stands in backlog or icebox became `tree_in_backlog_or_icebox`
// (record.HeldTreeInBacklogOrIcebox, version.go's 280). The word rides a value strip and no band:
// a caller whose restore is below this contract is served each such row as `unclassified` with no
// `heldBy` (withoutUnadmittedHeldWords), which every reader of the field admits. The number is
// written once as the word's arrival, in the declared table (answers.go's heldReasons).
//
// It is a literal of its own and never DaemonAPIVersion: the number a later contract takes moves
// that constant, and the contract the word arrived at stays where it is. It moves only with this
// change's own number, and it is the last number of answers.go's SupportedContracts, which names
// the constant rather than its number for that reason.
const treeInBacklogOrIceboxContract = 20261125161823

func init() {
	registerEntry(contractEntry{id: treeInBacklogOrIceboxContract, slug: "tree-in-backlog-or-icebox"})
}
