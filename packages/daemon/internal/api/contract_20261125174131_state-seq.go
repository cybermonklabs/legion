package api

// 20261125174131 state-seq: dispatch://INFRA-3931 -- the daemon block of `GET /legion/v1/state`
// gains `stateSeq`, `stateId` and `stateRun` (api.DaemonInfo's StateSeq, StateID and StateRun,
// which say what each holds, when the route writes it and how a reader of the daemon's state
// events, `notifications.legion.<token>.state`, starts from a whole read with them; the contract's
// `daemonInfo`). A caller whose contract is below this entry's number, whether it states it
// (`Legion-Plugin-Contract`) or the claims of its pod record it, is served the daemon block without
// any of the three (shaping.go's withoutStateSeq, this entry's band of stateNarrowings): one built
// before sjawhar/legion#2788 reads the state with a strict schema and would refuse the whole body
// for a key it does not name, and one built after strips the keys instead.
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
// shaping.go's answer AnswerObservedState, which is no row of Answers, served at the caller's
// contract as observedFor says: a caller below this entry's number reads no `stateSeq`, `stateId`
// or `stateRun` in either body, and a caller below 51 no `admission.trees` and no issue's `tree`.
// A request with no `view` gets the whole state, whose body does not change. A request whose
// `view` is other than exactly one `observed` gets the whole state too (stateRoute says when the
// daemon warns of one).
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
