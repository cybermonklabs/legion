package docs

// This file holds the two prices the room budget charges a loaded room at - the heap one copy of
// a document is measured to hold for each byte of its encoded state, and the heap it is measured
// to hold for each struct - and the arithmetic that turns a room's tracked state and its struct
// count into the weight the budget carries (roomWeight) and into what one copy of it takes of
// the pod's load concurrency (copyWeight).

// bytesOfHeapAStateByte is the heap one copy of a document holds for each byte of its encoded
// state. Measured again 2026-11-09 on ygo v1.55.1-sami.8, whose crdt.Item carries a parked
// struct's allowed roots and so costs 32 bytes more a struct than the sami.7 the first
// measurement was taken on, over the eight shapes a run without DISPATCH_MEASURE_STRUCT_HEAP
// measures (TestAResidentRoomsHeapPerByteOfItsState): the most 16.36 bytes a state byte (a
// paragraph typed into 100,000 times before its last character and then deleted, 13.66 at
// sami.7), the least 1.01 (one paragraph of a mebibyte typed in one transaction), a spread of
// 16.2 between them. It is set past the most.
//
// These two prices replaced the six the budget charged before - an element, a struct held, a
// rendered byte, a byte at the densest, a struct added, a split - because the encoded state and
// the struct count are the two measures of a document every shape's heap follows within those
// spreads, and because they are the measures the room can keep without reading the document: a
// whole-state encode costs
// 18.7 ms and 5,495,143 bytes allocated on a mebibyte of prose, 64 times what a keystroke's apply
// costs, so nothing on the keystroke path may take one.
const bytesOfHeapAStateByte = 17

// bytesOfHeapAStruct is the heap one copy of a document holds for each struct it holds.
// Measured 2026-11-09 on ygo v1.55.1-sami.8 over the four shapes where a room's structs and its
// state bytes come apart (TestAResidentRoomsHeapPerStructItHolds, run with
// `DISPATCH_TEST_DATABASE_URL="$LEGION_TEST_PG_DSN" go test ./internal/dispatch/docs/ -count=1
// -timeout 40m -run TestAResidentRoomsHeapPerStructItHolds -v`): the most 625.48 bytes a struct
// (a room of 200,000 empty paragraphs, 250,190,840 bytes of heap over its two copies), then
// 617.48 (200,000 elements outside the Proof schema), 409.76 (a mebibyte of prose) and the
// least 185.31 (a run of half a million characters with every other one deleted, 499,722
// structs in 185,208,392 bytes), a spread of 3.4 between the most and the least. It is set past
// the most.
//
// An empty element is the heaviest struct there is because it is a whole shared type - the item,
// its content, the type and the type's own maps - where a run of text is one item holding a
// string, which is why the split-dense room measures under a third of it a struct and is
// answered by its own count instead.
//
// Of the four, only a mebibyte of prose is a room the state term already covers (10.48 bytes a
// state byte of the 17 charged); the other three hold 17.73, 34.91 and 41.39, so the state term
// alone under-charges each of them and the struct term is what answers them.
const bytesOfHeapAStruct = 630

// roomCopies is the copies of its document a room is charged at, from its load (roomBudget.load)
// to its retirement (retire): its live document and the rendered replica its update observer
// keeps (renderedReplica), which the room holds from its load on, the copy its load decoded of
// the stored state (keepReplica). Every byte of state the room takes, the replica takes too: the
// replica holds the live document's whole state and then takes every update the room commits
// (renderedReplica.observe).
const roomCopies = 2

// roomWeight is what the budget charges a room whose tracked state is stateBytes long and whose
// document holds structs structs: the larger of its state at bytesOfHeapAStateByte and its
// structs at bytesOfHeapAStruct, in each of its two copies. A mebibyte of prose, whose state is
// 3,035,240 bytes and which holds 77,600 structs, is charged 103,198,160 by its state, where
// its structs alone would charge it 97,776,000; it holds 63,594,680
// (TestAResidentRoomsHeapPerStructItHolds).
//
// Two terms and not one because the two measures come apart in both directions, and a budget
// that bounds what the open rooms hold must follow whichever is the larger. The state bytes are
// tracked, never re-read (roomApplies.state): the bytes of the stored state the room's load
// decoded, plus what each commit since added to the encoded state as the library counts it -
// each remote apply's own report (crdt.ApplyReport.StateBytes), and for a commit no report
// covers, the replica's report of the same update (renderedReplica.countCommit) - re-based at
// each load and at each compaction that folds the room's whole stored history into one row
// (Service.rebaseRoomState). That figure reads at or above the state itself, which is the safe
// side for a budget, the splits a delete makes included, since the report counts each split
// where it is made. But one update can take a room far past what its state bytes charge it:
// a delete of every other character of a run splits
// one item into a struct for each character, and the splits a delete makes are not the update's
// own, so one 2.2 MB frame took a room from 4 structs to 1,100,002, its charge by 61.4 MB and its
// two copies by 336.6 MB (dispatch://INFRA-179, review 5891480448). The decode cap refuses that
// exact room now, the apply's allowance counting the splits it would make
// (crdt.ApplyBudget.Split, peerApplyAllowance), but every such delete short of the cap stands,
// and the struct term is what answers it: structs is what the room holds, as the library
// reports it at the end of each apply that commits - the structs its store holds with those
// parked in its pending queue beside them (crdt.ApplyReport.Structs, roomApplies.structsHeld),
// read off the room's own record with neither an encode nor a walk, so the second term costs
// nothing to read. The
// parked structs are counted because the room stores them and its next load decodes them, so a
// room holding a backlog costs the pod what that backlog costs it.
//
// A caller that can reach no document passes no structs, and zero means the state term alone
// decides, which is what every site that weighs bytes of a stored history before anything has
// decoded them passes (loadDecodedHistory, fold, buildForkHeld).
func roomWeight(stateBytes, structs int64) int64 {
	return max(stateBytes*bytesOfHeapAStateByte, structs*bytesOfHeapAStruct) * roomCopies
}

// copyWeight is what one whole-document copy built from stateBytes bytes of state holding
// structs structs takes of the pod's load concurrency (loadSlots): the larger of the two terms
// roomWeight charges in the one copy, against the two roomWeight charges a room for. A mebibyte
// of prose, whose state is 3,035,240 bytes over 77,600 structs, takes 51,599,080 bytes, where
// its room is charged 103,198,160.
//
// A split-dense copy is as heavy in the pool as it is in the budget, so it is weighed the same
// way: the structs decide where the state does not.
//
// It is clamped to at least one byte, so every taker holds something of the pool and the pool is
// what bounds them rather than their number, and to at most loadSlotBytes, one slot: that is the
// figure the slot was sized from, the heaviest whole-document decode measured and more
// (loadSlotBytes), and a copy capped at it can always be admitted on its own, however heavy the
// document, since the pool is never smaller than one slot (newLoadSlots).
func copyWeight(stateBytes, structs int64) int64 {
	return min(max(max(stateBytes*bytesOfHeapAStateByte, structs*bytesOfHeapAStruct), 1), loadSlotBytes)
}
