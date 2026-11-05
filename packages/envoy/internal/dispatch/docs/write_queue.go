package docs

// This file holds the two sizes of what one browser's socket may carry: the message floor every
// frame it sends is read under (maxMessageBytes) and the budget of frames the server queues for
// it before ygo closes it (peerWriteQueueBytes).

// maxMessageBytes is the most one websocket message a browser sends may carry, ygo's
// Server.MaxMessageBytes, whose own default is 64 MiB (sjawhar/ygo v1.55.1-sami.1,
// provider/websocket/server.go:150-157). It is a floor against a client that is not the editor at
// all, never the bound on a document: the bound is weighPeerUpdate's, which hands the apply an
// allowance ygo counts the frame against as it decodes it and rolls the whole apply back where
// it passes, so a frame at this floor that the hook refuses costs the pod the frame itself and
// nothing lasting.
//
// The floor has to clear every whole-state frame an editor hands over, since a frame over it is
// closed by the websocket library with a code the editor reconnects on, a loop the editor's
// handoff must never enter. It is the larger of four times the heaviest frame the server's own
// writes store, 4 x 12,598,044 = 50,392,176, and four times the most state one room may hold,
// 4 x maxRoomStateBytes = 54,000,000, rounded up to the next mebibyte: 54 MiB, under the 64 MiB
// ygo allowed before, past which this design would have had to come back to the architect. A
// room at the state cap hands over a frame about the size of its state, so four such handoffs
// fit the floor. Measured frames sit well inside it: heaviestDocument at 5,360,879 bytes, and
// that document grown through the creep allowance at 9,577,606
// (TestTheHeaviestWholeStateFrameFitsTheMessageFloor,
// TestTheHeaviestWholeStateFrameABrowserCanGrowFitsTheMessageFloor), and
// TestTheHeaviestWholeStateFrameTheServerStoresFitsTheMessageFloor holds the floor over both of
// its terms.
const maxMessageBytes = 54 << 20

// peerWriteQueueBytes is ygo's Server.PeerWriteQueueBytes: the bytes of frames queued for one
// browser's socket and not yet taken off that queue by its writer. ygo refuses a frame to a socket
// whose queued bytes already reach this and closes it, so the frame that crosses is queued and a
// socket holds the budget and one frame at most (sjawhar/ygo v1.55.1-sami.1,
// provider/websocket/write_budget.go:6-36). The three frames of a room's handshake are neither
// refused nor charged, since a browser cannot ask for them again (writeHandshake, :48-58); the
// writer credits a frame as it takes it off the queue, before it writes it (creditLocked, peer.go:973-977),
// so a socket also holds the frame its writer is in the middle of; and the close makes the read
// loop drop the input the peer had already sent (closeConn, peer.go:943-948, reearth/ygo#343), so
// one socket is closed and logged once however many asks it had in flight.
//
// It is the bound on what one browser can make the server hold. ygo's other bound,
// PeerWriteQueueSize, counts frames alone - 512 of them, whatever their size
// (defaultPeerWriteQueueSize, provider/websocket/server.go:224-228) - so what a socket can hold is
// that count times the largest frame its room produces. A socket that completes the handshake and
// then sends SyncStep1 frames whose answers it never reads is sent the room's whole state for each
// one (peer.go:96-108): measured on 2026-10-31 over a real socket on a document of a mebibyte of
// prose, 3,005,633 bytes a SyncStep1
// (TestASocketThatNeverReadsIsClosedWithinItsWriteQueueBudget), so the frame cap alone let one
// such socket hold 1,538,884,096 bytes, more than a third of the pod, for the 20,992 bytes it
// uploaded.
//
// 60 MiB, 62,914,560 bytes, is that bound, from three figures:
//
//   - It clears every frame a room can send one socket, because a frame arriving while the budget
//     or more waits closes a socket whatever it is doing: a budget at or under the largest frame
//     would close an honest reader holding one such frame for the next frame, which it could not
//     have avoided. The largest are a whole-state SyncStep2, which the hook's state cap holds to
//     about maxRoomStateBytes, 13,500,000 bytes, and the SyncStep2 or Update ygo relays from
//     another browser of the room, which it relays as it arrived and the read limit holds to
//     maxMessageBytes, 56,623,104, each with Hocuspocus's framing of the room name and the two
//     tags, under 64 bytes. The budget clears the larger of the two by 6,291,456 bytes.
//
//   - An honest reader on a slow link is never closed for lagging: those 6,291,456 bytes are what
//     it may fall behind beyond the largest single frame, and a reader of an ordinary document the
//     whole budget less one frame. Measured on 2026-10-31 over a real socket reading one frame
//     every 250 ms while another browser typed 200 keystrokes into a mebibyte document and handed
//     its whole state back at the end, as a reconnecting browser does, the most that reader was
//     behind by was 3,008,506 to 3,009,102 bytes over four runs, the last under sjawhar/ygo
//     v1.54.1-sami.3
//     (TestAThrottledReaderIsNotClosedByTheWriteQueueBudget): the relayed handoff of about
//     3,008,500 bytes and the keystrokes behind it, 4.78 per cent of the budget and under half of
//     what it clears the largest frame by.
//
//   - Twenty sockets that never read, each holding the budget and the frame that crossed it, fit
//     the Go memory limit beside a room budget charged to its limit. The calibration,
//     3,865,470,566, leaves 1,717,986,918 bytes above roomBudgetBytes, 2,147,483,648. The frames
//     those twenty hold are bounded by the same room budget: a room is charged its tracked state
//     at bytesOfHeapAStateByte in each of roomCopies copies (roomWeight), and a room's
//     whole-state frame is about its state, so the rooms under a full budget can send
//     2,147,483,648 / 28 = 76,695,844 bytes of frames between them. So the twenty hold
//     20 x 62,914,560 + 76,695,844 = 1,334,987,044 bytes, which beside the full room budget is
//     3,482,470,692, inside the limit with 382,999,874 bytes left. Measured on 2026-10-31 with
//     twenty such sockets on twenty mebibyte documents
//     (TestTwentySocketsThatNeverReadStayUnderThePodsMemory), they held 1,258,425,600 bytes
//     between them over four runs, the last under sjawhar/ygo v1.54.1-sami.3, 62,921,280 each,
//     and the server peaked at 1,552 to 2,093 MiB of the pod's 4,096 as its kernel would charge
//     it, holding 1,570,798,216 to 2,141,035,272 bytes of what its limit bounds.
//
// It stays at 60 MiB under the one price the budget now charges, which leaves those twenty
// sockets more room than the six prices before it did: a socket's own queue is what dominates
// the sum, and the budget is what bounds the frames beside it. 64 MiB, which the draft of this
// against ygo's draft did not fit, would now fit the limit too -
// 20 x 67,108,864 + 76,695,844 + 2,147,483,648 = 3,566,356,692 - but nothing asks for it: the
// measurements above were taken at 60 MiB, and a larger budget buys an honest reader nothing it
// does not already have, 6,291,456 bytes of lag beyond the largest frame it can be sent.
//
// It is the one bound here that does not scale with the Go memory limit (MemoryBounds). What
// sizes it is the frames a room sends, and no frame bound scales: a budget at or under the
// largest frame would close an honest reader at any limit, so it can never shrink below
// maxMessageBytes, and a larger limit buys a silent socket nothing - it is closed at the budget
// either way, and the bytes it was holding are the pod's loss, not its allowance. The arithmetic
// above stays true at every limit at or above the calibration: what the limit leaves above the
// room budget grows with the limit, since the budget is a fixed share of it, and the frames the
// rooms under a full budget can send grow with the budget, while these twenty queues stay 60 MiB
// each. Below the calibration it is the limit, not the budget, that gives way: fewer than twenty
// silent sockets fit beside a full room budget, and no budget that clears the largest frame
// could make twenty fit, since twenty frames at the message floor are 1,132,462,080 bytes on
// their own (TestTheWriteQueueBudgetsArithmetic weighs both limits).
//
// The close is logged at Warn, "peer queued bytes over budget; closing peer", with the room, the
// enqueue path, the socket's queued bytes, this budget, the refused frame and the frame cap
// (enqueueLocked). It reaches Dispatch's own log because ygo logs through slog.Default() while
// Server.Logger is unset (provider/websocket/server.go:168-174), which cmd/dispatch sets at boot
// (its main); the drives of cmd/dispatch/write_queue_memory_test.go read those lines off the
// server's stderr.
const peerWriteQueueBytes = 60 << 20
