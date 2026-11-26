package api

// 20261125162112 slots-reading: #4771 -- the slots reading. `GET /legion/v1/state` gains
// `admission.slots`, what the slots in use are spent on (record.SlotsReading): `inUse` and `cap`;
// the holders counted by what each is doing, `working`, `stopped`, `person`, `issue` and `idle`,
// each a `count` with the `oldest` time a holder of it entered that state where the record holds
// one (record.HolderStateOf); the two readings of started work that waits without a slot,
// `waitOnlyForSlot` (the members of the line's returned part that hold no slot and stand in no row
// of `admission.blocked`) and `startedWithoutSlot` (every member of that part that holds no slot),
// each a `count` and an `oldest`; `belowFirstWaiting`, the `count` of holders that rank below the
// first issue that waits for a slot and that issue's key as `first`; and `line`, the reading as
// the one sentence `legion state` prints right after its admission count line (record.SlotsLine).
// Each issue of the project gains `rank`, its own Dispatch rank, and `rootRank`, its tree root's,
// and each holder's `slot` gains `state`, one of the five words, and `since`, when it entered that
// state, absent where the record holds no such time. The controller's tick notice gains `slots`,
// the same reading (record.Notice.Slots), which the payload validator takes on a tick alone.
//
// A caller below this entry's contract is served the state without the five keys
// (withoutSlotsReading, withoutRanksAndSlotStates, the band of slotsReadingContract). The tick
// notice is no answer of this route: an outbox payload is decoded strictly, so no daemon of an
// earlier release decodes a tick that carries `slots`, and the way back of this change's
// migration (`store/migrations/20261121103000_slots_reading_times.up.sql`) deletes those rows
// before a rollback, with the two tables and the column the migration adds for the times the
// reading dates a holder by. No request shape moves.
//
// slotsReadingContract is this entry's id: the daemon contract that added the slots reading to
// the state, `admission.slots`, what the slots in use are spent on with the sentence `legion
// state` prints for it, each issue's `rank` and `rootRank`, and each slot's `state` and `since`.
// A plugin before it is served the state without all five (withoutSlotsReading,
// withoutRanksAndSlotStates): one built before sjawhar/legion#2788 reads the state with a strict
// schema and would refuse the whole body for a key it does not name, and one built after strips
// the keys instead, so each band keeps the body its own contract's record describes. The slot's
// `state` is read as a plain string by every reader that types it, so no value strip is owed.
// The band names the entry by this constant, and code that compares a contract with it reads
// entryNumber(slotsReadingContract), the number this build gives the entry.
const slotsReadingContract = 20261125162112

func init() {
	registerEntry(contractEntry{
		id: slotsReadingContract, slug: "slots-reading",
		bands: []stateNarrowing{
			{contract: slotsReadingContract, state: withoutSlotsReading, issue: withoutRanksAndSlotStates,
				dropped: "admission.slots, the issues' rank and rootRank, and the slots' state and since"},
		},
	})
}
