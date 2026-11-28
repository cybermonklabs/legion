package api

import "github.com/sjawhar/legion/daemon/internal/record"

// 20261125161823 tree-in-backlog-or-icebox: the held word of a member of a tree whose root
// stands in backlog or icebox is `tree_in_backlog_or_icebox` (record.HeldTreeInBacklogOrIcebox),
// and the route table names no child route of a removed operation by its path.
//
// The word is a value of `admission.held[].reason` on `GET /legion/v1/state` and of
// `held[].reason` on the controller's tick notice. It replaces the word the field carried for
// that hold since 98, which this daemon no longer writes. A caller below this entry's number
// (which the caller states, or the daemon reads from its pod) is served each such row as
// `unclassified` with no `heldBy` (shaping.go's withoutUnadmittedHeldWords), a word every reader
// of the field admits. The word rides a value strip and no band. The numbered records keep
// the former word in older readers' domains. This entry's fixture removes it from the
// current domain and adds the new word, and the current parser rejects the former word.
//
// `packages/contracts/src/legion-api.ts`'s `LEGION_HELD_REASONS` and the controller's
// `references/admission-held.md` name the new word, `legion state`'s `admission.held:` line counts
// under it, and the issue's `why` says "Its tree's root <root> stands in backlog or icebox, …".
// The migration 20261124002047 moves the word in every unsent tick notice, so a notice queued
// before the upgrade reaches the controller in the new word.
//
// `POST /legion/v1/children/{op}` is registered to removedChildRoute in place of the one route
// it answered. Every child route this daemon does not serve answers 410 ROUTE_REMOVED with the
// status to write in Dispatch, and `POST /legion/v1/children/rerun` keeps its own, more specific
// row. An older plugin calling the removed operation gets the answer it got at 205, and a call
// of a child route no plugin ever sent gets that answer too rather than 404 `no route`.
// RemovedRoutes names the pattern at the historical removal boundary, 205. The request gate
// resolves each recorded route through the server's mux, so the request records of 193, 199 and
// 202 still pass. This entry changes no plugin request: its request shapes are inherited.
const treeInBacklogOrIceboxContract = 20261125161823

func init() {
	registerEntry(contractEntry{
		id:   treeInBacklogOrIceboxContract,
		slug: "tree-in-backlog-or-icebox",
		enumeratedValues: []enumeratedValues{{
			answer:  AnswerState,
			keyPath: "admission.held.*.reason",
			values: []EnumeratedValue{
				{Value: string(record.HeldTreeInBacklogOrIcebox), Since: treeInBacklogOrIceboxContract},
			},
		}},
	})
}
