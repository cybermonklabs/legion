package api

// 20261125211005 wake-due-earliest-moment: INFRA-5682 (cybermonklabs/legion) -- a paced wake waits
// in one of two queues in front of the one token bucket: the first for a role with an Envoy message
// waiting and for the relaunch of a role whose own process died, the second for every other paced
// wake, which gets one turn in four while both queues hold wakes (wakepace.Pacer). A waiting wake
// holds a place in its queue and no moment of its own, so the `dueAt` of each wake of `GET
// /legion/v1/state`'s `wakesWaiting`, and of a claim view's `wake`, is no longer the moment the
// pacing gave the wake, which was when it ran (contracts 53 and 193). It is the pacing's estimate,
// at the read, of the moment it lets that wake's place out, as the queues stand
// (wakepace.Place.Estimate). A change to the queues or to the pace can move that moment earlier or
// later, so two reads can name two moments: for example, a wake ahead of it in its own queue that
// leaves moves it earlier, and a wake that joins and is let out ahead of it moves it later.
// The outbox row of a `catch-up` wake stands held, with no due moment,
// until the pacing lets its place out. No shape of the state or of a request moves, and the
// plugin's one read of `dueAt`, the earliest of a state's wakes (pi-legion's wakesWaitingSummary),
// reads it as the moment the next wake is due, which it still is, so the entry adds no row to any
// table: it is an entry because it changes what a field both sides read means.
const wakeDueEarliestMomentContract = 20261125211005

func init() {
	registerEntry(contractEntry{id: wakeDueEarliestMomentContract, slug: "wake-due-earliest-moment"})
}
