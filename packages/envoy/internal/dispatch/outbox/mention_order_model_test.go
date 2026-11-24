package outbox

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The mention-copy order model: every interleaving, within small bounds, of one issue's events,
// the writes that move who its events reach, the outbox's batches and their records, and the two
// roads a mention's copy takes outside the outbox's chains - the API's send and the fallback pass -
// each of which asks the copy rule (model.MentionCopyHolderSQL) before it sends.
//
// What the model holds:
//   - issue T-1's events in id order: an earlier event, the comment that mentions session s1, and
//     the writes the program makes before and after it, each appending its event in the same
//     transaction;
//   - who an event reaches, read again at each batch as readBatchSQL reads it: the owner's topic
//     family, and the session the route, the claim and the followers name (s1, s2 or nobody);
//   - the outbox's batches, run with the package's own planChains, settleEvent and owedKeys on
//     the destinations the model gives them, every send succeeding or failing, a failure holding
//     the rest of its chain back as sendChains does; a batch reads and sends in one step and
//     records in a later one, so writes, copies and the other process's batches fall between;
//   - the record (recordBatch), which writes what a batch came to as its SQL does: reached
//     destinations and a publish whoever holds the publisher hold, a schedule and owed_keys (and
//     owed_seq, in the rule that has it) only while the hold is the batch's process's;
//   - two publishing processes whose hold moves between them, both of this build or one of the
//     previous build, whose record writes no owed_keys and no owed_seq;
//   - the comment's mention: the API's send, the lease that ends, and the fallback row a batch
//     makes past it when s1 is one of the comment's destinations, which the fallback pass sends.
//
// It checks two things.
//   - Order: when a copy reaches s1, no earlier event of T-1 is left that owes s1 a send under the
//     routing then, that s1 has not taken on any subject, and that is not about to be recorded
//     published (moModel.passed). Such an event reaches s1 after the copy on some path: s1 takes
//     the comment before it, which the test checks by carrying each counterexample on until s1
//     does. A write committed after the copy went may still make an earlier event reach s1 after
//     the copy; that write is ordered after the copy, as a concurrent write is, and no rule can
//     see it. A counterexample is in the window when an event it passed was last recorded by a
//     batch that read the owner before its last write (owed_seq below last_seq).
//   - Liveness: from every state the search reaches, with the program run to its end and the
//     outbox running fair batches - every send succeeding, the hold on a process of this build -
//     the copy goes, and the run never comes back to a state it left. That is run twice: once
//     with the API's send alone, and once with the lease ended first, so that the fallback row
//     carries the copy wherever the outbox made one.
//
// What it leaves out: role routes, whose holder each road reads when it asks
// (model.MentionCopyRolesSQL, TestARoleHolderChangeKeepsAMentionCopyBehindAnEarlierForward);
// a recipient read that fails (any-session, TestOwedKeysRecordWhatEachDestinationStillOwes);
// more than one comment; an owner window past batchSize; a record a fenced batch never makes,
// which writes nothing at all; and the previous build's own copy roads, which ask no rule at all,
// as at main.
//
// The rule is the one piece the model restates rather than calls, since it is SQL: the
// conformance test TestTheModelsCopyRuleIsTheSQLRule runs both on the same rows.

// The subjects of the model's one owner, T-1, as bits of a set.
const (
	moTopic uint8 = 1 << iota
	moS1
	moS2
)

var moBitsInOrder = [...]uint8{moTopic, moS1, moS2}

var moSubjects = map[uint8]string{
	moTopic: dispatchTopicPrefix + "issue.T-1.event",
	moS1:    contracts.LegacyAgentSubject("s1"),
	moS2:    contracts.LegacyAgentSubject("s2"),
}

var moShort = map[uint8]string{moTopic: "topic", moS1: "s1", moS2: "s2"}

func moBit(subject string) uint8 {
	for bit, s := range moSubjects {
		if s == subject {
			return bit
		}
	}
	panic("model: unknown subject " + subject)
}

func moBits(subjects []string) uint8 {
	var bits uint8
	for _, subject := range subjects {
		bits |= moBit(subject)
	}
	return bits
}

// moOwedBits reads owedKeys' answer back into subject bits: the topic family is spelled
// model.OwedTopicFamily, a session's subject as itself.
func moOwedBits(keys []string) uint8 {
	var bits uint8
	for _, key := range keys {
		if key == model.OwedTopicFamily {
			bits |= moTopic
			continue
		}
		bits |= moBit(key)
	}
	return bits
}

func moSet(bits uint8) string {
	var names []string
	for _, bit := range moBitsInOrder {
		if bits&bit != 0 {
			names = append(names, moShort[bit])
		}
	}
	return "[" + strings.Join(names, " ") + "]"
}

// moSession is a route's or a claim's value as a subject bit: nobody, s1 or s2.
func moSession(value uint8) uint8 {
	switch value {
	case 1:
		return moS1
	case 2:
		return moS2
	}
	return 0
}

var moSessionName = [...]string{"nobody", "s1", "s2"}

// moKind is what an event's destinations read beyond the owner's topic family.
type moKind uint8

const (
	// moNotifying reads the route, the claim and the followers: a person's event on an ask, which
	// notifies, takes the claimant route and the follower routes.
	moNotifying moKind = iota
	// moQuiet reads none of them: an agent's quiet event, the owner's topic family alone.
	moQuiet
	// moComment is the comment that mentions s1, notifying: its copy for s1 is the mention's,
	// left out of its list (destinations.leftTo).
	moComment
)

// moWrite is what a program step writes beside the event it appends, in one transaction.
type moWrite uint8

const (
	moNoWrite moWrite = iota
	moRoute
	moClaim
	moFollow
)

type moOp struct {
	kind  moKind
	write moWrite
	// to is the route's or claim's new session (1 s1, 2 s2, 0 nobody), or the follower added.
	to uint8
}

func (o moOp) label(id int) string {
	kind := map[moKind]string{moNotifying: "notifying", moQuiet: "quiet", moComment: "the comment, mentioning s1"}[o.kind]
	switch o.write {
	case moRoute:
		return fmt.Sprintf("route := %s, event %d (%s)", moSessionName[o.to], id, kind)
	case moClaim:
		return fmt.Sprintf("claim := %s, event %d (%s)", moSessionName[o.to], id, kind)
	case moFollow:
		return fmt.Sprintf("follower %s added, event %d (%s)", moSessionName[o.to], id, kind)
	}
	return fmt.Sprintf("event %d (%s)", id, kind)
}

const moMaxEvents = 5

type moEvent struct {
	kind      moKind
	published bool
	due       bool
	// reached is published_destinations; sent is every subject reaching s1 that NATS took for
	// the event, recorded or not.
	reached, sent uint8
	owedNull      bool
	owed          uint8
	// owedSeq is owed_seq, the owner's last_seq when the batch that wrote owed read it; -1 null.
	owedSeq int8
}

// Codes of outcomeState, kept small so a state is a comparable value.
const (
	moPublished uint8 = iota + 1
	moBackedOff
	moBlocked
	moReleased
	moOwedAgain
)

var moOutcomeCode = map[outcomeState]uint8{
	published: moPublished, backedOff: moBackedOff, blocked: moBlocked, released: moReleased, owedAgain: moOwedAgain,
}

var moOutcomeName = map[uint8]outcomeState{
	moPublished: published, moBackedOff: backedOff, moBlocked: blocked, moReleased: released, moOwedAgain: owedAgain,
}

type moOut struct {
	present bool
	state   uint8
	reached uint8
	owed    uint8
}

// moPending is a batch a process has sent and not recorded yet.
type moPending struct {
	active  bool
	readSeq int8
	out     [moMaxEvents]moOut
}

// moState is one state of the model; it is a comparable value, so the search keys on it.
type moState struct {
	n, c                 int8
	ev                   [moMaxEvents]moEvent
	route, claim, follow uint8
	pc                   int8
	hold, moves, fails   int8
	expired, row         bool
	// copied is the copy having reached s1, which ends the run; violated is that it passed an
	// earlier event (moModel.passed), and inWindow that one such event's last record that wrote
	// owed_keys read the owner before its last write (owed_seq below last_seq): the window a
	// stale record leaves open until a batch that read that write records the event again.
	copied, violated, inWindow bool
	pend                       [2]moPending
}

func (s *moState) anyDue(due bool) bool {
	for k := range int(s.n) {
		if !s.ev[k].published && s.ev[k].due == due {
			return true
		}
	}
	return false
}

// moVariant is a copy rule. Every record writes owed_seq beside owed_keys in every variant - the
// model's measure of a record older than the owner's last write (moState.inWindow) - and only the
// rule as built reads it.
type moVariant struct {
	name   string
	holder func(s *moState) int
}

func moOwesS1(e *moEvent) bool { return e.owed&(moTopic|moS1) != 0 }

// moRuleOwedKeysAlone is the rule that reads earlier events' owed_keys alone: an earlier event,
// unpublished, whose owed_keys are null or name the topic family or s1.
func moRuleOwedKeysAlone(s *moState) int {
	for k := range int(s.c) {
		if e := &s.ev[k]; !e.published && (e.owedNull || moOwesS1(e)) {
			return k
		}
	}
	return -1
}

// moRuleLaterNull is the hand-over's plan: the same, and any other event of the owner, later ones
// among them, whose owed_keys are null.
func moRuleLaterNull(s *moState) int {
	for k := range int(s.n) {
		if e := &s.ev[k]; k != int(s.c) && !e.published && (e.owedNull || (k < int(s.c) && moOwesS1(e))) {
			return k
		}
	}
	return -1
}

// moRuleOwnCopy holds the copy on the comment's own event too: the liveness control, since that
// event waits on the copy's mention.
func moRuleOwnCopy(s *moState) int {
	for k := range int(s.n) {
		if e := &s.ev[k]; !e.published && (e.owedNull || k <= int(s.c)) {
			return k
		}
	}
	return -1
}

// moRuleOwedSeq is the rule as built (model.MentionCopyHolderSQL): an earlier event, unpublished,
// whose owed_keys are null or name the topic family or s1, or whose record read the owner before
// its last event (owed_seq below the owner's last_seq) - a write since may have made it reach s1.
func moRuleOwedSeq(s *moState) int {
	for k := range int(s.c) {
		if e := &s.ev[k]; !e.published && (e.owedNull || e.owedSeq < s.n || moOwesS1(e)) {
			return k
		}
	}
	return -1
}

var (
	moVariantOwedKeysAlone = moVariant{name: "the rule that reads earlier events' owed_keys alone", holder: moRuleOwedKeysAlone}
	moVariantLaterNull     = moVariant{name: "hand-over plan: also any later event whose owed_keys are null", holder: moRuleLaterNull}
	moVariantOwnCopy       = moVariant{name: "liveness control: the comment's own event holds its copy", holder: moRuleOwnCopy}
	moVariantOwedSeq       = moVariant{name: "the rule as built: earlier events' owed_keys and owed_seq", holder: moRuleOwedSeq}
)

type moEnv struct {
	name      string
	processes int
	newBuild  [2]bool
	moves     int8
}

var (
	moOnePublisher  = moEnv{name: "one publisher", processes: 1, newBuild: [2]bool{true, true}}
	moHoldMoves     = moEnv{name: "two publishers of this build, the hold moving twice", processes: 2, newBuild: [2]bool{true, true}, moves: 2}
	moRollingDeploy = moEnv{name: "rolling deploy: B of the previous build, the hold moving twice", processes: 2, newBuild: [2]bool{true, false}, moves: 2}
)

type moScenario struct {
	route0  uint8
	follows bool
	program []moOp
}

func (sc moScenario) String() string {
	parts := []string{fmt.Sprintf("route %s", moSessionName[sc.route0])}
	if sc.follows {
		parts = append(parts, "s1 follows T-1's topic family")
	}
	for i, op := range sc.program {
		parts = append(parts, op.label(i+1))
	}
	return strings.Join(parts, "; ")
}

type moModel struct {
	v          moVariant
	env        moEnv
	sc         moScenario
	failBudget int8
	// verbose has each step name what it did, which only a counterexample's trace reads.
	verbose bool
}

// moStep is one step of a run: what it did, and the events it sent to a subject s1 takes, in
// the order sent.
type moStep struct {
	label string
	took  []int
}

var errModelSend = errors.New("the model's send fails")

var moNotDue = time.Unix(0, 0)

func (m *moModel) dests(s *moState, kind moKind) uint8 {
	d := moTopic
	if kind != moQuiet {
		d |= moSession(s.route) | moSession(s.claim)
		if s.follow&1 != 0 {
			d |= moS1
		}
		if s.follow&2 != 0 {
			d |= moS2
		}
	}
	return d
}

// reach is the subjects s1 takes: its own, and the topic family when it follows it.
func (m *moModel) reach() uint8 {
	if m.sc.follows {
		return moS1 | moTopic
	}
	return moS1
}

// batchEvents reads the owner's unpublished events as readBatchSQL and resolveDestinations do,
// with the routing as it stands now.
func (m *moModel) batchEvents(s *moState) []*batchEvent {
	var events []*batchEvent
	for k := range int(s.n) {
		e := &s.ev[k]
		if e.published {
			continue
		}
		be := &batchEvent{
			pendingEvent: pendingEvent{event: model.Event{ID: int64(k + 1)}, owner: "T-1", due: e.due},
			delivered:    map[string]struct{}{},
		}
		if !e.due {
			be.nextAttempt = &moNotDue
		}
		for _, bit := range moBitsInOrder {
			if e.reached&bit != 0 {
				be.delivered[moSubjects[bit]] = struct{}{}
			}
		}
		d := m.dests(s, e.kind)
		if e.kind == moComment && d&moS1 != 0 {
			// s1's copy is the mention's: left out of the list, and the event waits while the
			// mention is still being sent (destinations.leftTo).
			d &^= moS1
			be.mentionPending = !s.expired && !s.copied
		}
		for _, bit := range moBitsInOrder {
			if d&bit != 0 {
				be.destinationsOf = append(be.destinationsOf, destination{subject: moSubjects[bit]})
			}
		}
		events = append(events, be)
	}
	return events
}

// batch is process p reading a batch and sending it, every send succeeding or failing within the
// failure budget.
func (m *moModel) batch(s moState, p int, emit func(moStep, moState)) {
	if int(s.hold) != p || s.pend[p].active || !s.anyDue(true) {
		return
	}
	chains := planChains(m.batchEvents(&s))
	choice := make([]int, len(chains))
	var walk func(i int, fails int8)
	walk = func(i int, fails int8) {
		if i == len(chains) {
			emit(m.applyBatch(s, p, choice, fails))
			return
		}
		choice[i] = -1
		walk(i+1, fails)
		if fails < m.failBudget {
			for j := range chains[i].sends {
				choice[i] = j
				walk(i+1, fails+1)
			}
		}
	}
	walk(0, s.fails)
}

// applyBatch is one batch of process p, choice naming each chain's failed send (-1 for none).
func (m *moModel) applyBatch(s moState, p int, choice []int, fails int8) (moStep, moState) {
	next := s
	next.fails = fails
	events := m.batchEvents(&s)
	chains := planChains(events)
	reach := m.reach()
	var took []int
	var sends []string
	for i, c := range chains {
		for j, send := range c.sends {
			k := int(send.event.event.ID) - 1
			bit := moBit(send.dest.subject)
			switch {
			case choice[i] == -1 || j < choice[i]:
				send.state = reached
				if bit&reach != 0 {
					next.ev[k].sent |= bit
					took = append(took, k+1)
				}
				if m.verbose {
					sends = append(sends, fmt.Sprintf("%d→%s ok", k+1, moShort[bit]))
				}
			case j == choice[i]:
				send.state, send.err = failed, errModelSend
				if m.verbose {
					sends = append(sends, fmt.Sprintf("%d→%s fails", k+1, moShort[bit]))
				}
			default:
				send.state, send.heldBy = held, c.sends[choice[i]].event
			}
		}
	}
	if c := int(s.c); c >= 0 && !s.ev[c].published && m.dests(&s, moComment)&moS1 != 0 && s.expired && !s.copied && !s.row {
		// Past the mention's lease the batch holds s1's copy as a fallback row
		// (mentionComment.holdFallbackCopy).
		next.row = true
		sends = append(sends, "fallback row made")
	}
	pend := moPending{active: true, readSeq: s.n}
	var outs, read []string
	for _, be := range events {
		k := int(be.event.ID) - 1
		var o outcome
		if !be.due {
			o = outcome{id: be.event.ID, state: owedAgain, owed: owedKeys(be, nil)}
		} else {
			be.outcome = settleEvent(be, false)
			be.outcome.owed = owedKeys(be, be.outcome.reached)
			o = be.outcome
		}
		pend.out[k] = moOut{present: true, state: moOutcomeCode[o.state], reached: moBits(o.reached), owed: moOwedBits(o.owed)}
		if m.verbose {
			read = append(read, fmt.Sprint(k+1))
			outs = append(outs, fmt.Sprintf("%d %s owing %s", k+1, o.state, moSet(moOwedBits(o.owed))))
		}
	}
	next.pend[p] = pend
	var label string
	if m.verbose {
		label = fmt.Sprintf("%s reads [%s] (last_seq %d), sends {%s}, comes to {%s}", m.process(p), strings.Join(read, " "),
			s.n, strings.Join(sends, ", "), strings.Join(outs, "; "))
	}
	return moStep{label: label, took: took}, next
}

func (m *moModel) process(p int) string {
	name := []string{"A", "B"}[p]
	if !m.env.newBuild[p] {
		name += " (previous build)"
	}
	return name
}

// record is process p's record of its pending batch (recordBatch). It restates no owed_keys or
// owed_seq for an event the record publishes, under either hold, because no reader reads a
// published row: model.MentionCopyHolderSQL and model.MentionCopyRolesSQL filter published_at is
// null, and recordBatch writes only rows still unpublished.
func (m *moModel) record(s moState, p int) (moStep, moState, bool) {
	pend := s.pend[p]
	if !pend.active {
		return moStep{}, s, false
	}
	next := s
	next.pend[p] = moPending{}
	mine := int(s.hold) == p
	writesOwed := m.env.newBuild[p]
	var wrote []string
	note := func(format string, args ...any) {
		if m.verbose {
			wrote = append(wrote, fmt.Sprintf(format, args...))
		}
	}
	for k := range moMaxEvents {
		o := pend.out[k]
		if !o.present {
			continue
		}
		e := &next.ev[k]
		if e.published || !(o.state == moPublished || mine || o.reached != 0) {
			continue
		}
		e.reached |= o.reached
		if o.state == moPublished {
			e.published = true
		}
		if mine {
			switch o.state {
			case moBackedOff, moBlocked:
				e.due = false
			case moReleased:
				e.due = true
			}
		}
		switch {
		case e.published:
			note("%d published", k+1)
		case writesOwed && mine:
			e.owedNull, e.owed, e.owedSeq = false, o.owed, pend.readSeq
			note("%d %s owing %s, owed_seq %d", k+1, moOutcomeName[o.state], moSet(o.owed), pend.readSeq)
		default:
			note("%d reached %s, owed_keys left as they were", k+1, moSet(o.reached))
		}
	}
	var label string
	if m.verbose {
		how := "with the hold"
		if !mine {
			how = "after losing the hold"
		}
		label = fmt.Sprintf("%s records %s: {%s}", m.process(p), how, strings.Join(wrote, "; "))
	}
	return moStep{label: label}, next, true
}

func (m *moModel) client(s moState) (moStep, moState, bool) {
	if int(s.pc) >= len(m.sc.program) {
		return moStep{}, s, false
	}
	op := m.sc.program[s.pc]
	next := s
	next.pc++
	switch op.write {
	case moRoute:
		next.route = op.to
	case moClaim:
		next.claim = op.to
	case moFollow:
		next.follow |= op.to
	}
	k := next.n
	next.ev[k] = moEvent{kind: op.kind, due: true, owedNull: true, owedSeq: -1}
	next.n++
	if op.kind == moComment {
		next.c = k
	}
	return moStep{label: "write: " + op.label(int(k)+1)}, next, true
}

// passed is the earlier events a copy going now passes: unpublished, owing s1 a send under the
// routing now, not taken by s1 on any subject yet - a session that follows the topic family and
// is routed to as well takes an event on two subjects, and drops the second by its event id - and
// not recorded published by a batch a process holds.
func (m *moModel) passed(s *moState) uint8 {
	reach := m.reach()
	var passed uint8
	for k := range int(s.c) {
		e := &s.ev[k]
		if e.published || (e.sent|e.reached)&reach != 0 || m.dests(s, e.kind)&reach == 0 {
			continue
		}
		if (s.pend[0].active && s.pend[0].out[k].state == moPublished) || (s.pend[1].active && s.pend[1].out[k].state == moPublished) {
			continue
		}
		passed |= 1 << k
	}
	return passed
}

func (m *moModel) copyRoad(s moState, fallback bool) (moStep, moState, bool) {
	if s.c < 0 || s.copied || (fallback && !s.row) || m.v.holder(&s) >= 0 {
		return moStep{}, s, false
	}
	next := s
	next.copied = true
	passed := m.passed(&s)
	next.violated = passed != 0
	for k := range int(s.c) {
		if passed&(1<<k) != 0 && s.ev[k].owedSeq < s.n {
			next.inWindow = true
		}
	}
	var label string
	if m.verbose {
		road := "the API's send"
		if fallback {
			road = "the fallback pass"
		}
		label = fmt.Sprintf("%s asks the rule: nothing holds the copy, and it goes", road)
		if passed != 0 {
			var ids []string
			for k := range int(s.c) {
				if passed&(1<<k) != 0 {
					ids = append(ids, fmt.Sprint(k+1))
				}
			}
			label += fmt.Sprintf(", while event %s still owes s1 a send", strings.Join(ids, ", "))
		}
	}
	return moStep{label: label, took: []int{int(s.c) + 1}}, next, true
}

func (m *moModel) tick(s moState) (moStep, moState, bool) {
	if !s.anyDue(false) {
		return moStep{}, s, false
	}
	next := s
	for k := range int(s.n) {
		next.ev[k].due = true
	}
	return moStep{label: "time passes: every back-off falls due"}, next, true
}

func (m *moModel) expire(s moState) (moStep, moState, bool) {
	if s.c < 0 || s.expired || s.copied {
		return moStep{}, s, false
	}
	next := s
	next.expired = true
	return moStep{label: "the mention's lease ends"}, next, true
}

func (m *moModel) moveHold(s moState) (moStep, moState, bool) {
	if m.env.processes < 2 || s.moves >= m.env.moves {
		return moStep{}, s, false
	}
	next := s
	next.hold = 1 - s.hold
	next.moves++
	var label string
	if m.verbose {
		label = fmt.Sprintf("the publisher hold moves to %s", m.process(int(next.hold)))
	}
	return moStep{label: label}, next, true
}

// successors is every step the model can take from s. A state whose copy went has none.
func (m *moModel) successors(s moState, emit func(moStep, moState)) {
	if s.copied {
		return
	}
	for _, f := range []func(moState) (moStep, moState, bool){
		m.client,
		func(s moState) (moStep, moState, bool) { return m.copyRoad(s, false) },
		func(s moState) (moStep, moState, bool) { return m.copyRoad(s, true) },
	} {
		if step, next, ok := f(s); ok {
			emit(step, next)
		}
	}
	for p := range m.env.processes {
		if step, next, ok := m.record(s, p); ok {
			emit(step, next)
		}
		m.batch(s, p, emit)
	}
	for _, f := range []func(moState) (moStep, moState, bool){m.tick, m.moveHold, m.expire} {
		if step, next, ok := f(s); ok {
			emit(step, next)
		}
	}
}

func (m *moModel) start() moState {
	return moState{c: -1, route: m.sc.route0}
}

// moFull is whether ENVOY_MENTION_ORDER_MODEL=full asks for the whole model: every scenario of
// moScenarios, and two failed sends with two publishers as with one. The run the pull request's
// table and its verification row record is the full one, about 1.65 billion states over every
// rule and control, some eleven minutes on 16 workers. Without it the test runs the part a CI
// runner can afford in seconds: route s2, s1 not following the topic family, at most one write,
// and one failed send with two publishers - which keeps the reviewer's scenario and every kind of
// write, before the comment and after it.
func moFull() bool { return os.Getenv("ENVOY_MENTION_ORDER_MODEL") == "full" }

// moScenarios is every program of the model: an earlier notifying event, the comment, and up to
// two writes before and after it, from route s2 or nobody, with s1 following the topic family or
// not; or, without moFull, the part of them named there.
func moScenarios() []moScenario {
	writes := []moOp{
		{kind: moQuiet, write: moRoute, to: 1},
		{kind: moNotifying, write: moRoute, to: 1},
		{kind: moQuiet, write: moClaim, to: 1},
		{kind: moQuiet, write: moFollow, to: 1},
		{kind: moQuiet, write: moRoute, to: 2},
	}
	earlier := moOp{kind: moNotifying}
	comment := moOp{kind: moComment}
	var programs [][]moOp
	add := func(before, after []moOp) {
		program := []moOp{earlier}
		program = append(program, before...)
		program = append(program, comment)
		programs = append(programs, append(program, after...))
	}
	add(nil, nil)
	for _, a := range writes {
		add(nil, []moOp{a})
		add([]moOp{a}, nil)
		for _, b := range writes {
			add(nil, []moOp{a, b})
			add([]moOp{a}, []moOp{b})
		}
	}
	var scenarios []moScenario
	for _, route0 := range []uint8{2, 0} {
		for _, follows := range []bool{false, true} {
			for _, program := range programs {
				if !moFull() && (route0 != 2 || follows || len(program) > 3) {
					continue
				}
				scenarios = append(scenarios, moScenario{route0: route0, follows: follows, program: program})
			}
		}
	}
	return scenarios
}

type moRow struct {
	variant                                      moVariant
	env                                          moEnv
	failBudget                                   int8
	scenarios, states, violations, inWindow      int
	counterexampleScenarios, liveFailures, steps int
	// unobserved counts the scenarios whose first counterexample, carried on with every send
	// succeeding, never has s1 take the event the copy passed: the model's own check of its
	// order check, which must count none.
	unobserved int
	first      *moCounterexample
	liveFirst  string
	duration   time.Duration
}

// moFailBudget is how many sends of one run may fail: two with one publisher, and one with two
// publishers, whose runs reach some seventy times the states, unless moFull.
func moFailBudget(env moEnv) int8 {
	if env.processes == 1 || moFull() {
		return 2
	}
	return 1
}

// moRun explores every scenario under v and env, the scenarios spread over at most 16 workers.
func moRun(v moVariant, env moEnv, scenarios []moScenario) moRow {
	row := moRow{variant: v, env: env, failBudget: moFailBudget(env), scenarios: len(scenarios)}
	began := time.Now()
	results := make([]moResult, len(scenarios))
	work := make(chan int)
	var workers sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), 16) {
		workers.Go(func() {
			for i := range work {
				m := &moModel{v: v, env: env, sc: scenarios[i], failBudget: row.failBudget}
				results[i] = m.explore()
			}
		})
	}
	for i := range scenarios {
		work <- i
	}
	close(work)
	workers.Wait()
	for _, result := range results {
		row.states += result.states
		row.violations += result.violations
		row.inWindow += result.inWindow
		row.liveFailures += result.liveFailures
		if result.first != nil {
			row.counterexampleScenarios++
			if !result.first.observed() {
				row.unobserved++
			}
			if row.first == nil || len(result.first.trace) < len(row.first.trace) {
				row.first = result.first
			}
		}
		if row.liveFirst == "" {
			row.liveFirst = result.liveFirst
		}
		row.steps = max(row.steps, result.maxSteps)
	}
	row.duration = time.Since(began)
	return row
}

func (r moRow) String() string {
	return fmt.Sprintf("| %s | %s | %d | %d | %d | %d (%d scenarios; %d in the window) | %d (longest fair run %d steps) | %s |",
		r.variant.name, r.env.name, r.failBudget, r.scenarios, r.states, r.violations, r.counterexampleScenarios, r.inWindow,
		r.liveFailures, r.steps, r.duration.Round(time.Millisecond))
}

// The model run: the rule that reads owed_keys alone must reproduce the reviewer's counterexample,
// a route moved after the comment letting the copy pass the earlier event (s1 takes [2 1]); the
// hand-over's plan must show the model sees what a second publisher does; the liveness control
// must show the model sees a copy that never goes; and the rule as built must find nothing
// anywhere. Every counterexample is carried on until s1 takes the event the copy passed
// (moRow.unobserved).
func TestTheMentionCopyOrderModel(t *testing.T) {
	quiet := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(quiet) })
	t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
	scenarios := moScenarios()
	t.Logf("full model (ENVOY_MENTION_ORDER_MODEL=full): %v; %d scenarios", moFull(), len(scenarios))
	t.Logf("| rule | publishers | failed sends | scenarios | states | copies that passed an earlier event | copies that never went | time |")
	t.Logf("| --- | --- | --- | --- | --- | --- | --- | --- |")
	report := func(row moRow) moRow {
		t.Logf("%s", row)
		if row.unobserved != 0 {
			t.Errorf("the model's order check is wrong: %d counterexamples of %q under %q never have s1 take the event the copy passed, first:\n%s",
				row.unobserved, row.variant.name, row.env.name, row.first)
		}
		return row
	}

	reviewer := moScenario{route0: 2, program: []moOp{{kind: moNotifying}, {kind: moComment}, {kind: moQuiet, write: moRoute, to: 1}}}
	control := report(moRun(moVariantOwedKeysAlone, moOnePublisher, []moScenario{reviewer}))
	if control.first == nil || !slices.Equal(control.first.took(), []int{2, 1}) {
		t.Fatalf("the model is blind: the rule that reads owed_keys alone did not reproduce s1 taking [2 1] in the reviewer's scenario; first counterexample: %v", control.first)
	}
	t.Logf("the rule that reads owed_keys alone, the reviewer's scenario:\n%s", control.first)
	allOwedKeysAlone := report(moRun(moVariantOwedKeysAlone, moOnePublisher, scenarios))
	if allOwedKeysAlone.violations == 0 {
		t.Fatalf("the model is blind: the rule that reads owed_keys alone passed every scenario")
	}

	for _, env := range []moEnv{moOnePublisher, moHoldMoves, moRollingDeploy} {
		row := report(moRun(moVariantLaterNull, env, scenarios))
		switch {
		case env.processes == 1 && row.violations != 0:
			t.Errorf("the hand-over's plan let a copy pass with one publisher:\n%s", row.first)
		case env.processes == 2 && row.violations == 0:
			t.Errorf("the model is blind to a second publisher: the hand-over's plan passed every scenario of %q", env.name)
		case env.processes == 2 && row.inWindow != row.violations:
			t.Errorf("the hand-over's plan let a copy pass outside the window of %q:\n%s", env.name, row.first)
		case env.processes == 2:
			t.Logf("the hand-over's plan, %s:\n%s", env.name, row.first)
		}
	}

	ownCopy := report(moRun(moVariantOwnCopy, moOnePublisher, scenarios))
	if ownCopy.liveFailures == 0 {
		t.Fatalf("the model is blind to a copy that never goes: the comment's own event held its copy and every run delivered it")
	}
	t.Logf("liveness control: %s", ownCopy.liveFirst)

	for _, env := range []moEnv{moOnePublisher, moHoldMoves, moRollingDeploy} {
		row := report(moRun(moVariantOwedSeq, env, scenarios))
		if row.violations != 0 {
			t.Errorf("the rule as built let a copy pass an earlier event (%s):\n%s", env.name, row.first)
		}
		if row.liveFailures != 0 {
			t.Errorf("the rule as built left a copy that never goes (%s): %s", env.name, row.liveFirst)
		}
	}
	t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
}
