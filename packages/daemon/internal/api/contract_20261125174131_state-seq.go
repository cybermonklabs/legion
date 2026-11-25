package api

// 20261125174131 state-seq: dispatch://INFRA-3931 -- the daemon block of `GET /legion/v1/state`
// gains `stateSeq` (api.DaemonInfo.StateSeq), the sequence of the newest state event the
// project's daemon committed (state_publication.last_seq, record.Store.StatePublication), read in
// the route's own repeatable-read snapshot: every state event at or below it is in the body and
// every one above it is not, so a reader of the daemon's state events
// (`notifications.legion.<token>.state`) starts after a whole read at `stateSeq + 1`, with nothing
// lost and nothing applied twice. Beside it, `stateId` (api.DaemonInfo.StateID) is the id of the
// store that count is kept in (state_publication.store_id), made at the project's first state row
// and never changed, which every state message carries: a reader applies only messages whose
// `stateId` is its starting point's. Beside both, `stateRun` (api.DaemonInfo.StateRun) is the id
// of the daemon run that serves the body, minted at the process's start, which is the `run` every
// state message of that run carries: a reader takes it as its starting point's run, so a boot of
// that run at or below `stateSeq` is one the body already holds and not a daemon whose sequence
// went back. The route writes `stateSeq` on every body, 0 while the project has no state row,
// `stateId` once the project has one, and `stateRun` from a daemon whose state events are on (one
// with a workflow), and a caller whose contract is below this entry's number, whether it states
// it (`Legion-Plugin-Contract`) or the claims of its pod record it, is served the daemon block
// without any of the three (shaping.go's withoutStateSeq, this entry's band of stateNarrowings):
// one built before sjawhar/legion#2788 reads the state with a strict schema and would refuse the
// whole body for a key it does not name, and one built after strips the keys instead.
// `packages/contracts/src/legion-api.ts` reads the three keys as optional, so a reader of this
// contract served such a body, or the body of a daemon before this entry, takes them as absent
// rather than as 0.
// The same entry adds the observed view of the route, `GET /legion/v1/state?view=observed`
// (api.ObservedState, StateSource.ObservedState): what Dispatch observes of a daemon and nothing
// more, read in a repeatable-read, read-only snapshot of its own under the same bound as the whole
// state. It carries the whole state's daemon block, its `settings` and its `admission` block,
// unfiltered and in the same order, and `issues`, every issue the whole state lists with only its
// `key`, `phase`, `tree`, `slot.index`, `waitingOn`, `architect.state` and
// `workers.<role>.claim.state`, each absent where the whole state's is absent. The view is
// shaping.go's answer AnswerObservedState, which is no row of Answers, and observedFor shapes it:
// it is served at the caller's contract by the whole state's own bands and value strips, so at
// every contract this daemon serves, each member of the view is the whole state's member at that
// contract: a caller below this entry's number reads no `stateSeq`, `stateId` or `stateRun` in
// either body, and a caller below 51 no `admission.trees` and no issue's `tree`. A request with no
// `view` gets the whole state, whose body does not change. A request with any other `view` gets
// the whole state too, and the daemon logs a warning that names the values given and the one view
// the route serves, because a later version may refuse such a request.
// `packages/contracts/src/legion-api.ts` reads the view as LegionObservedStateResponse.
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
