package supervise

import "context"

// The count of a claim's process's turns (turnCount) and what a take keeps while that count is
// not known (takeAnchor): what tells which turn a turn-outcome report is of, and whether a turn
// has started since the one the daemon gave a take. The report's decision in turnoutcome.go reads
// it (reportedTask, notTheTakesTurn).

// inTurn is whether a turn of the claim's process is running: the claim is busy, and not for a
// compaction alone (Machine.compacting, whose inTurn is a turn started under it). It is the one
// reading of a running turn on the machine, and a let-go reads it through endState (cutsTurn).
func (m *Machine) inTurn() bool {
	return m.busy() && (m.compacting == nil || m.compacting.inTurn)
}

// busy is whether the claim is working: a turn of its process runs (inTurn), or its session is
// being compacted with no turn started under it (Machine.compacting). Working is the daemon's one
// record of a busy agent (compactStart), so this is the stored state read as the machine's word
// for it. Work that meets a busy claim waits for the end of what keeps it busy, a turn's or a
// compaction's (holdSuspension, holdStaleRelaunch, finishAtTurnsEnd), and the start frame of a
// turn reported while the claim is busy has come or is given no take (startedUnseen,
// turnStartedWhileBusy). What asks whether a turn runs, as against a compaction alone, asks
// inTurn: the phase of the wait for a take's report (reportWaitNow), and what a let-go cuts off
// (cutsTurn).
func (m *Machine) busy() bool { return m.claim.State == StateWorking }

// turnsStarted is how many turns of the claim's process the machine has seen start in the launch
// generation it counts (turnCount): every turn whose end it counted, and the one running, if one
// runs (inTurn). A compaction alone is no turn: the producer numbers the turns that end, and a
// compaction with no turn started under it ends none. It reads the count as it stands, so its
// callers hold the claim's current generation's (counted).
func (m *Machine) turnsStarted() uint64 {
	if m.inTurn() {
		return m.turns.ended + 1
	}
	return m.turns.ended
}

// startedSince is whether a turn has started since the given one, the given-th the machine saw
// start as its count numbers them: the one rule every reading of "a later turn started" goes by
// — the move of a take's turn to the run after the one it was given (notTheTakesTurn), the
// producer's word read against the turns since the given one (sinceGiven), and the placement of a
// count and its check (learnTurns, checkPlacedCount). A compaction after the given turn's end is
// no later turn, for each of them alike: it starts no turn the producer numbers, so the turn after
// the given one, if it comes, is still the next start, and a placement that stands through a
// compaction is no more off by one than one that stands through an idle claim.
func (m *Machine) startedSince(given uint64) bool {
	return m.turnsStarted() > given
}

// heldPassedOverLine is the line of a turn's start the daemon gives no take, at Info, with the
// task's number, because the producer said that take's text was held unrun through that turn
// (heldUnrun, turnStarted).
const heldPassedOverLine = "supervise: a turn started that the producer said ran with the accepted take's text held; the turn is no task's, and the take awaits its turn"

// heldUnrun is a report read while the claim's count of its process's turns is not known
// (notTheTakesTurn), for what the producer said of the pending take: the take's text was still
// held unrun at the end of the report's turn (RequestTurnOutcome.Held). The producer holds the
// text from the take's answer until the text starts a turn of its own — pi-shared's take holds it
// through the run going when the answer came (task-take.ts, Accepted "held"), the Claude bridge
// through the turn running then (take.go, bridge.held) — and a report sent before the answer
// reached it names nothing held; the Claude bridge names the take held, too, for a turn it began
// with the text that Claude Code never took, a text it writes again only at the value's next
// revision (take.go). So every report naming the take held is of a turn that ended before the
// take's own turn began, and none is of a turn that ran the text, whatever the count says or
// whenever the report comes: that is what makes the word exact, where a count lost to a restart
// is a guess.
//
// What the word does not say is whether the take's own turn has started by the time the report
// comes: a start the machine saw after the reported turn's end may be it, and that turn's own
// report then settles the task, while the take, put back, would await a turn it already had
// (notTheTakesTurn says what that costs). So the word is applied only where no start has followed
// the reported turn's end, and is otherwise moot. The frames carry no sequence, so the machine
// reads that from what it kept at the take's accept (takeAnchor), where the take is the first of
// its number this process made while the count was not known (anchorTake) and its connection is
// still the claim's (onAnchoredConn):
//
//   - the turns the producer said held the text are the ones that ended from the take's answer
//     to the take's own turn, consecutive in the producer's count, and each started after the
//     accept (one that started before it would have had the claim working, and the take refused
//     in-turn), so the turn of the first start after the accept is at most the smallest sequence
//     such a report named (takeAnchor.ceiling);
//   - on one connection no frame is dropped, so the starts the machine sees from that first one
//     on are of consecutive turns, and the turn of the last start seen is at most the ceiling plus
//     the starts seen after the one the daemon gave the take;
//   - where that is at most the highest sequence a report named held (takeAnchor.held), the last
//     start seen is of a turn that held the text, so the take's own turn, which follows every such
//     turn, has not started: the take is put back, and the next start is given to it. Anything
//     else is moot.
//
// A report that comes before the start of the turn given the take is kept (takeAnchor.heldReport)
// and read at that start, which is given no take where it is of a turn that held the text
// (turnStarted). A take of a number taken before — one a provider error's ladder or
// the second wait gave back, taken again — has no anchor, so a report its earlier take's hold
// made, sent late, reaches no later take; nor does a report naming another number than the
// pending take's, a report at or below the sequences the anchor already accounts for
// (takeAnchor.reported), or one of a take whose connection the claim no longer has, where a frame
// may have been dropped. Each of them is read as a report without the word.
//
// A take the daemon before this one accepted, which a daemon start restored, is anchored where
// the store shows that daemon can have handled no turn start after the one it gave the take
// (anchorRestored), and its connection is the one the first hello after the start opened, where
// that hello shows the connection carries every frame its shim held meanwhile
// (bindRestoredAnchor); a report that comes before that hello is kept, and read at it. With no
// task pending it puts nothing back.
func (m *Machine) heldUnrun(ctx context.Context, r RequestTurnOutcome) error {
	p := m.claim.Pending
	a := &m.turns.anchor
	unbound := a.restored && a.conn == 0
	if p == nil || r.Held == 0 || r.Held != p.Number || a.number != p.Number || r.Sequence <= a.reported ||
		(!unbound && !m.onAnchoredConn()) {
		return nil
	}
	a.heldReport(r.Sequence)
	since, given := m.sinceGiven()
	if !given || unbound {
		return nil
	}
	if !a.unrun(since) {
		m.log.Info("supervise: a turn's report says the accepted take's text was held through it, but a turn may have started after it; the take keeps its turn",
			"task", p.Number, "sequence", r.Sequence)
		return nil
	}
	a.pass(since)
	return m.putBack(ctx, "held")
}

// sinceGiven is how many turn starts the machine has seen after the one it gave the pending take
// (takeAnchor.given), and whether that take has a given turn standing: its turn started, and a
// start of it the anchor placed. A turn running counts as started, a compaction alone does not
// (turnsStarted, startedSince).
func (m *Machine) sinceGiven() (uint64, bool) {
	p, a := m.claim.Pending, m.turns.anchor
	if p == nil || !p.startRecorded() || a.given == 0 {
		return 0, false
	}
	seen := m.turnsStarted()
	if seen < a.given {
		return 0, false
	}
	return seen - a.given, true
}

// onAnchoredConn is whether the claim's connection is the one the anchored take was accepted on
// (takeAnchor.conn), or, for a take a daemon start restored, the one its first hello opened.
func (m *Machine) onAnchoredConn() bool {
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	return connected && conn.Sequence() == m.turns.anchor.conn
}

// anchorRestored anchors the take of the pending task a daemon start restored (NewMachine), for
// a report saying that take's text was held to give it its turn back (heldUnrun) as one this
// daemon accepted would be, where that can never put back a take whose own turn ran. The
// producer's word is as exact as ever; what this daemon cannot see is the starts the daemon
// before it handled, whose frames reached that daemon and no other, so it anchors the take only
// where the store shows that daemon handled none after the turn it gave the take:
//
//   - a take awaiting its turn: that daemon handled no start after the accept, or after the report
//     that put the take back (a start it handled then would have been given the take), so every
//     start after that point reaches this daemon, behind the first hello, from the shim's backlog
//     — where the hello shows that backlog dropped none of them (bindRestoredAnchor) — or as the
//     agent sends it, and the first one it sees is the one it gives the take (turnStarted), as for
//     a take it accepted itself; a run of the host's own whose setup spans the restart is such a
//     start;
//   - a take whose turn that daemon gave, where the claim is stored working and records no turn's
//     end after the take's accept (Claim.LastTurnEnd, Delivery.ConfirmedAt): that daemon handled
//     the given turn's start and nothing after it, since it writes every turn's end and start it
//     handles, and frames reach a machine in the order the shim sent them. The given turn counts
//     as the start running at this machine's build (takeAnchor.given 1).
//
// Every other restored take is refused the word: one whose given turn's end that daemon handled
// may have had its own turn start, run and end under that daemon as well, its report still on its
// way, and the store tells that from a take whose own turn is still to come by nothing. Put back
// there, the take's own report would then read it as a turn whose start is still to come
// (startedUnseen), whose mark names it in the next task's start, and a second wait that ran out
// before that report landed would take back a task whose turn ran. So its run of the host's own
// is given back by no report, as before this word, and that is the one take a restart leaves
// unbounded: the store would need the time of the given turn's start to tell the two apart.
//
// A take whose report the store owes (owedReport) is one whose own turn ran, and is not anchored.
// Reports at or below the newest the restored window holds were applied by that daemon or are of
// an earlier take of the number (takeAnchor.reported), and none of them is read. What this leaves
// is a daemon that received frames it never recorded — a write the store refused before it
// stopped, or frames it read and never handled — and frames the shim wrote to that daemon's
// connection as it closed, which no daemon read (shimwire.Hello2.BacklogWhole): no restart can
// see either.
func (m *Machine) anchorRestored() {
	p := m.claim.Pending
	if p == nil || !p.taken() || p.TakenText == "" || m.takePhase() == waitFinish || m.ladderOwed != nil {
		return
	}
	count := m.counted()
	a := takeAnchor{number: p.Number, reported: m.claim.newestReported(), restored: true}
	switch {
	case !p.TurnStarted:
	case m.busy() && !m.claim.LastTurnEnd.After(p.ConfirmedAt):
		a.given = count.ended + 1
	default:
		return
	}
	count.anchor = a
}

// restoredAnchorRefusedLine is the line of a restored take's anchor the first hello after the
// daemon start does not bind (bindRestoredAnchor), at Info, with the take's number and why: no
// report gives that take its turn back.
const restoredAnchorRefusedLine = "supervise: the first hello after the daemon start does not show its connection carries every frame its shim held; no report gives the restored take its turn back"

// bindRestoredAnchor gives a restored take's anchor (anchorRestored) the connection of the claim's
// first hello after the daemon start, and reads the reports kept before it (heldUnrun). The anchor
// reads the starts that connection carries as every start since the point the store shows, and the
// shim replays them from the backlog it held while no daemon was connected, which drops its oldest
// frames past its bound. A replay that lost the given run's end and the take's own turn's start
// would have a report saying the given run held the text put back a take whose own turn runs; that
// turn's report would then read it as a turn whose start is still to come (startedUnseen), and the
// mark it leaves would take the next task's start. So the anchor goes, and no report gives the
// take its turn back, as for a take anchorRestored refuses, wherever the hello cannot show the
// connection carries every frame:
//
//   - the hello does not say its shim's backlog is whole (StreamHello.BacklogWhole): the shim
//     dropped frames since its last connection, or it is of a release that does not say, which
//     may have;
//   - the agent does not answer a get_state on the connection within the RPC bound: a shim
//     replays its backlog on a connection, and so answers on it, only where the backlog dropped
//     nothing since the hello that said so — one that dropped frames while that hello waited for
//     its ack closes the connection with nothing replayed on it — so the answer is the one sign,
//     at the hello, that the word still held when the shim attached. The ask is the anchor's own,
//     logged by this refusal's line alone, not one of the asks the hello's row makes (streaming);
//   - the connection is gone already: frames may have been lost with it.
//
// Over a take a handoff protects (handoffGuard) the probe is not made: Oh My Pi answers no ask
// behind the handoff, so a probe bounded by the RPC timeout would discard the anchor for an answer
// that only comes later, or hold the hello behind the handoff. The anchor stays unbound, which
// gives no report the take's turn back meanwhile (heldUnrun), and the answer to the one ask the
// hello makes (askProtected) is the evidence, on this connection, that binds it
// (bindAnchorAtAnswer).
func (m *Machine) bindRestoredAnchor(ctx context.Context, hello StreamHello) error {
	a := &m.turns.anchor
	if !a.restored || a.conn != 0 || m.turns.generation != m.claim.Generation {
		return nil
	}
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	if !connected {
		m.turns.anchor = takeAnchor{}
		return nil
	}
	if !hello.BacklogWhole {
		m.log.Info(restoredAnchorRefusedLine, "task", a.number, "why", "the hello does not say its shim's backlog is whole")
		m.turns.anchor = takeAnchor{}
		return nil
	}
	if _, protected := m.handoffUntil(); protected {
		m.handoff.anchorConn = conn.Sequence()
		return nil
	}
	asking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	_, err := conn.GetState(asking)
	cancel()
	if err != nil {
		m.log.Info(restoredAnchorRefusedLine, "task", a.number, "why", "the agent did not answer on the connection", "error", err)
		m.turns.anchor = takeAnchor{}
		return nil
	}
	a.conn = conn.Sequence()
	if since, given := m.sinceGiven(); given && a.unrun(since) {
		a.pass(since)
		return m.putBack(ctx, "held")
	}
	return nil
}

// bindAnchorAtAnswer binds a restored take's anchor that a protected hello left to the answer of
// its ask (bindRestoredAnchor): the answer reached the machine on that connection, behind every
// frame its shim replayed, which is the evidence the probe gives. The reports kept before it are
// then read as at the probe.
func (m *Machine) bindAnchorAtAnswer(ctx context.Context) error {
	g, a := m.handoff, &m.turns.anchor
	if g == nil || g.anchorConn == 0 || !a.restored || a.conn != 0 || m.turns.generation != m.claim.Generation {
		return nil
	}
	a.conn, g.anchorConn = g.anchorConn, 0
	if since, given := m.sinceGiven(); given && a.unrun(since) {
		a.pass(since)
		return m.putBack(ctx, "held")
	}
	return nil
}

// backlogCutLine is the line of a hello that does not say its shim's backlog is whole
// (readBacklog), at Info: the count of the process's turns is not known until a report places it.
const backlogCutLine = "supervise: the shim's hello does not say its backlog is whole; the count of its process's turns is no longer known"

// readBacklog reads what a hello of the claim's ready, idle or working agent says of its shim's
// backlog (StreamHello.BacklogWhole; reconnected, reconnectedMidTurn). A backlog that is not whole
// dropped its oldest frames while no daemon was connected, or its shim cannot say it dropped none,
// and a trim of it can drop a whole turn, start and end. What the machine read off the frames it
// saw then stops standing for the turns that ran. This is the one list of what the supervisor does
// with such a hello (shimwire.Hello2.BacklogWhole and internal/api's task-take entry,
// `contract_20261125171504_task-take.go`, point here):
//
//   - the count of the process's turns is lost (turnCount.lose), and the hello marks it as cut
//     (turnCount.cut). Kept, it would be short by every turn the backlog dropped whole, so the turn
//     the daemon gives the next take would carry a dropped turn's sequence, and that turn's late
//     report, naming no task, would put back a take whose own turn is running (notTheTakesTurn).
//     It is known again only where a report places it (learnTurns);
//   - a report's mark of its turn's start as still to come (Machine.reportedAhead) goes: that
//     start may be among the frames dropped, and kept, the mark would take the next start, the
//     next task's own among them. While the count is not known after such a hello no report
//     leaves one (startedUnseen), and a start of the reported turn that does come is read as any
//     other start: given to a take that awaits its turn, or no task's;
//   - a take a daemon start restored has its anchor refused by the same word
//     (bindRestoredAnchor), so no producer's word that its text was held gives that take its turn
//     back;
//   - a stale-plugin relaunch decided behind such a replay, at the agent's answer of no turn
//     (turnAnswered) or at the reconnect deadline with no answer (reconnectDeadline), takes a take
//     whose turn no frame showed back as a turn that may have run (markRestoredTurnUnseen): any
//     take awaiting its turn then, one the daemon start restored or one accepted since.
func (m *Machine) readBacklog(hello StreamHello) {
	if hello.BacklogWhole {
		return
	}
	m.log.Info(backlogCutLine)
	count := m.counted()
	count.lose()
	count.cut = true
	m.reportedAhead = aheadReport{}
}

// aheadReport is the turn whose report ran ahead of its start frame (Machine.reportedAhead): the
// task the report named, 0 for none, and the report's sequence.
type aheadReport struct {
	task, sequence uint64
}

// turnCount is a process's turns as this machine counts them from the worker stream, which is
// what tells which turn a turn-outcome report is of (RequestTurnOutcome.Sequence): the producer
// numbers the turns of a launch generation from 1 in the order they end, and the end of every turn
// that ends once its agent is ready reaches the machine as an end frame — of a turn it saw start
// (turnEnded), of one it did not (blindTurnEnded), or of one under a compaction — before the next
// one starts. So the turn starting now is the one after the ends counted, and a report whose
// sequence is at or below that count is of a turn whose start was seen.
//
// generation is the launch generation counted, ended the ends of its turns seen, and take the
// sequence of the turn the daemon gave the pending task's accepted take (turnStarted), or the one
// a report of that take's number named first (startedUnseen); 0 for none. known is whether ended
// is every end of the generation's turns: a machine a daemon start builds over a live process
// missed the turns that ended under the daemon before it (NewMachine), a start that arrives with a
// turn still running says an end was missed (turnStartedWhileBusy), a turn that ends before
// the agent said ready is one the producer may or may not have numbered (turnEndedBeforeReady),
// and a hello whose shim's backlog is not whole may follow a turn dropped whole (readBacklog).
// A count not known goes on counting every end it sees, so it is the producer's count less the
// ends it missed, and take is read only while the count is known. It reads a report's turn start
// as unseen wherever the claim is not working (startedUnseen), as the machine read every report
// before it counted, and gives a take its turn back only on the producer's word that the take's
// text was held unrun through the report's turn (notTheTakesTurn, heldUnrun); anchor is what a
// take accepted meanwhile saw (takeAnchor), which that word is read against, and the report of
// that take's own turn makes the count known again where it places that turn among the ones the
// machine saw (learnTurns), placed until the agent's answer to the ask that placement makes has
// checked it (placedCount, checkPlacedCount). The next launch counts its generation afresh, known
// (counted).
//
// cut is whether a hello of the generation said its shim's backlog is not whole (readBacklog):
// a turn's start may be gone with the frames it dropped, so while the count is not known no
// report marks its turn's start as still to come (startedUnseen). It stands for the generation,
// since what was dropped stays dropped whatever a later hello says; a count known again reads a
// report's sequence instead.
type turnCount struct {
	generation, ended, take uint64
	known, cut              bool
	anchor                  takeAnchor
	placed                  placedCount
}

// lose is the count no longer known: what a placement still unchecked added to the ends counted
// goes back, so they are the ends seen again, and a take's anchor goes with it.
func (c *turnCount) lose() {
	c.ended -= c.placed.offset
	c.known, c.anchor, c.placed = false, takeAnchor{}, placedCount{}
}

// counted is the claim's count of its process's turns, begun afresh, known, for a launch
// generation it has not counted yet (turnCount). A count of an earlier generation goes whole,
// known or not, its take's anchor with it: the turns it counted were another process's.
func (m *Machine) counted() *turnCount {
	if m.turns.generation != m.claim.Generation {
		m.turns = turnCount{generation: m.claim.Generation, known: true}
	}
	return &m.turns
}

// takeAnchor is what the machine saw when it accepted the pending task's take while its count of
// the process's turns was not known (anchorTake), for that take's report to place the count
// (learnTurns) and for a report saying its text was held to give it its turn back (heldUnrun):
// number, the number taken, 0 for no anchor; reported, the newest sequence of the claim's
// generation its window of reports held then (Claim.newestReported), or the highest a report
// said held when a report or a start passed the given turn over (pass); conn, the sequence of the
// claim's connection then (runtime.Conn.Sequence); and given, the turn the daemon gave the take as
// the count numbers it (turnStarted: the ends it had counted, plus one), 0 while it has given
// none.
//
// ceiling and held are what the reports of the take's turns said (heldReport): ceiling, an upper
// bound on the producer's sequence of the turn the daemon gave the take, or of the next start
// while it gave none, 0 while none is known; held, the highest sequence of a report that said the
// take's text was held unrun at its turn's end, 0 for none. passed is whether a turn given the
// take, or a start, was passed over since the accept (pass): ceiling then counts on from that
// turn, and a report's sequence no longer lowers it.
//
// restored is a take the daemon before this one accepted, which a daemon start restored
// (anchorRestored): its accept is the point the store shows that daemon handled no start after,
// conn is 0 until the first hello after the start opens one (bindRestoredAnchor), and it places
// no count (learnTurns), since what that daemon counted is not this machine's.
type takeAnchor struct {
	number, reported, conn, given uint64
	ceiling, held                 uint64
	passed, restored              bool
}

// heldReport takes the sequence of a report that said the take's text was held unrun at its
// turn's end. Every such turn started after the accept, so until a turn is passed over the
// smallest such sequence bounds the turn of the first start after the accept (heldUnrun); after
// it, a report that comes late is of a turn that may have started before the one passed over, and
// lowers nothing.
func (a *takeAnchor) heldReport(sequence uint64) {
	if !a.passed && (a.ceiling == 0 || sequence < a.ceiling) {
		a.ceiling = sequence
	}
	a.held = max(a.held, sequence)
}

// unrun is whether the last start the machine has seen, since starts after the one it gave the
// take (or the start being decided, for since 0 with none given), is of a turn the producer said
// held the text: its turn is at most the ceiling plus since, and that is at most the highest
// sequence a report named held.
func (a takeAnchor) unrun(since uint64) bool {
	return a.held != 0 && a.ceiling != 0 && a.ceiling+since <= a.held
}

// pass is the turn the daemon gave the take, and since starts after it, passed over as turns that
// held the text: the next start is of a turn at most one past the last of them, and is the one
// given the take (turnStarted). Every turn up to the highest a report named held has ended, since
// its report came, so the take's own turn's report is right after that one where it follows them
// (learnTurns).
func (a *takeAnchor) pass(since uint64) {
	a.ceiling += since + 1
	a.given, a.passed = 0, true
	a.reported = max(a.reported, a.held)
}

// placedCount is a count of the process's turns a take's report placed (learnTurns) that the
// agent's answer to the ask the placement made has still to check (checkPlacedCount): ask, that
// ask's id (runtime.ConnState.Ask), "" for none; sequence, the turn the daemon gave the take, as
// the placed count numbers it; offset, what the placement added to the ends counted; and conn,
// the sequence of the connection the take was accepted on.
type placedCount struct {
	ask                    string
	sequence, offset, conn uint64
}

// turnsKnownAgainLine is the line of a count of the process's turns a take's report made known
// again (learnTurns), at Info, with the report's sequence and the ends the count now holds.
const turnsKnownAgainLine = "supervise: the report of a take's own turn places the turn the daemon gave it; the count of its process's turns is known again"

// turnsPlacementTakenBackLine is the line of a placed count the agent's answer took back, or that
// the agent did not answer the ask for (learnTurns, checkPlacedCount), at Info, with the sequence
// the count gave the turn given the take and why.
const turnsPlacementTakenBackLine = "supervise: the agent's answer does not bear out the count of its process's turns a take's report placed; the count is not known again"

// anchorTake keeps, for a take just accepted while the claim's count of its process's turns is
// not known, what learnTurns needs to place that take's turn (takeAnchor), and drops what an
// earlier take kept. first is whether the claim had never taken the take's number before
// (Claim.TaskTaken, the number of its last accepted take: numbers rise, and a pending number a
// take holds is the last one taken). A number taken before — a take a provider error or the
// second wait gave back, then taken again — keeps nothing, and so does a take
// accepted while the claim has no connection: see learnTurns.
func (m *Machine) anchorTake(first bool) {
	count := m.counted()
	if count.known {
		return
	}
	count.anchor = takeAnchor{}
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	if !first || !connected {
		return
	}
	count.anchor = takeAnchor{number: m.claim.Pending.Number, reported: m.claim.newestReported(), conn: conn.Sequence()}
}

// learnTurns makes the claim's count of its process's turns known again from the report r of the
// turn the daemon gave the pending task's take (reportedTask), where the report places that turn
// among the turns the machine saw for certain. The frames carry no sequence, so a count lost —
// to a daemon restarted under the process (NewMachine), a turn ended before ready
// (turnEndedBeforeReady), a start over a running turn (turnStartedWhileBusy) — is learned from a
// report or not at all; and a report placed one turn off would leave the count one too high or
// one too low, and either lets the report of another turn give back a take whose own turn is
// running or has run (notTheTakesTurn). A sequence taken as the count whenever the claim is not
// working undercounts wherever a report comes late: a report sent again after a restart, or the
// report of one turn landing after the next one's end.
//
// The report places the turn where all of these hold, and each is why no other turn can be it:
//
//   - it names the number of the take the machine accepted while the count was not known, a number
//     the claim had not taken before (anchorTake): the report is then of the one turn that ran
//     that take's text, which the worker started on the take's answer, after the take — no
//     earlier take of the number left a turn whose late report names it, and no turn that ended
//     before the restart did;
//   - its sequence is the one right after the newest the window held at the take
//     (takeAnchor.reported): every turn up to that one had ended by then, its report having reached
//     the daemon, so the report's turn is the first of the process to start after them. A run of
//     the host's own that started first — the run given a take that notTheTakesTurn is for —
//     leaves the take's own turn a later sequence than this one, whether or not its report has come,
//     but where a report said such a run held the take's text and the turn it was given was passed
//     over (heldUnrun, takeAnchor.pass), reported is the highest such report's sequence, and the
//     take's own turn, which follows every turn that held its text, is the one right after it;
//   - the connection the take was accepted on is still the claim's (takeAnchor.conn): every frame
//     from the take on reached the machine in the order the shim wrote it, none dropped from the
//     backlog of a shim with no daemon to send to, so of the turns that started after the take,
//     or after the last turn passed over, that first turn's start is the first the machine sees;
//   - no start has come since the one the daemon gave the take (takeAnchor.given, startedSince),
//     at the report and again at the agent's answer to the ask the placement makes
//     (checkPlacedCount): the claim works the given turn, its end still to come, or that end has
//     come and nothing after it but, perhaps, a compaction, which starts no turn the producer
//     numbers.
//
// The last is what tells the given turn from a start that reached the machine late, of a turn
// that ended before the take: one a shim replays behind its hello, of a turn that ran while no
// daemon heard it, or one queued behind the claim's earlier decisions while the report and the
// take came over HTTP, which nothing orders with the frames. The machine cannot tell such a start
// from any other — no frame says it is replayed, and its events come in the order of a live
// turn's — and behind it the take's own turn is the one after it, whose report can come before
// its start does. But the shim sends a turn's start before that turn's report can be sent, so
// before the agent answers an ask made at the report, and the worker stream hands the machine
// that answer behind every frame the shim sent before it, naming the ask it answers
// (StreamTurnAnswered.Ask), so no answer to an earlier ask stands in for it: by then the take's
// own turn's start has come, after the one given, and the placement goes back (lose). An agent
// that does not answer leaves nothing to check by, and the placement goes back at once.
//
// The producer then numbered the given turn r.Sequence where the count numbered it given, so
// every count moves by the difference: the ends seen, and the take's turn. The count stands from
// the report on, since the report of the run given the next take may come before the answer does.
// Until the answer, every start the machine handles is one the shim sent before it, so a take a
// wrong count would give back is one accepted after the report and given such a start, and the
// report that gives it back is of a later turn than that start, which has to run and be reported
// before the answer reaches the machine: the stream behind the reports by a whole run more, under
// which the first start after a take is not that take's turn however the turns are counted
// (turnStarted). Anything that fails a check leaves the count not known, the take's anchor spent
// either way, and the report of a later take's turn may place it.
func (m *Machine) learnTurns(ctx context.Context, r RequestTurnOutcome) {
	count := m.counted()
	anchor := count.anchor
	count.anchor = takeAnchor{}
	if count.known || anchor.number == 0 || anchor.number != r.Task || anchor.given == 0 || anchor.restored {
		return
	}
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	if !connected || conn.Sequence() != anchor.conn || m.startedSince(anchor.given) || r.Sequence != anchor.reported+1 || r.Sequence < anchor.given {
		return
	}
	offset := r.Sequence - anchor.given
	count.ended += offset
	count.take, count.known = r.Sequence, true
	count.placed = placedCount{sequence: r.Sequence, offset: offset, conn: anchor.conn}
	m.log.Info(turnsKnownAgainLine, "sequence", r.Sequence, "ended", count.ended)
	state, answered := m.streaming(ctx, conn)
	if !answered || state.Ask == "" {
		m.log.Info(turnsPlacementTakenBackLine, "sequence", r.Sequence, "why", "the agent did not answer whether it is in a turn")
		count.lose()
		return
	}
	count.placed.ask = state.Ask
}

// checkPlacedCount checks a count a take's report placed (placedCount) at the worker stream's
// delivery of the agent's answer to the ask the placement made, which comes behind every frame the
// shim sent before that answer (StreamTurnAnswered); an answer to any other ask checks nothing.
// The count stands where no start has come since the turn the daemon gave the take and the
// take's connection is still the claim's; anything else takes it back (lose). A count of a
// generation the claim has left is the next count's to drop (counted).
func (m *Machine) checkPlacedCount(answer StreamTurnAnswered) {
	count := &m.turns
	placed := count.placed
	if placed.ask == "" || answer.Ask != placed.ask || count.generation != m.claim.Generation {
		return
	}
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	if !connected || conn.Sequence() != placed.conn {
		m.log.Info(turnsPlacementTakenBackLine, "sequence", placed.sequence, "why", "the take's connection is no longer the claim's")
		count.lose()
		return
	}
	if m.startedSince(placed.sequence) {
		m.log.Info(turnsPlacementTakenBackLine, "sequence", placed.sequence, "why", "a turn started after the one given the take")
		count.lose()
		return
	}
	count.placed = placedCount{}
}

// newestReported is the newest sequence of the claim's generation its window of reports holds,
// and 0 for none: every turn of its process up to that one has ended, since its report came. The
// window keeps the newest reports whatever its bound drops (recordTurnOutcome), in key order.
func (c Claim) newestReported() uint64 {
	if n := len(c.TurnOutcomes); n > 0 && c.TurnOutcomes[n-1].Generation == c.Generation {
		return c.TurnOutcomes[n-1].Sequence
	}
	return 0
}
