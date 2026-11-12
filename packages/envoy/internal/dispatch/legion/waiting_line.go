package legion

// waiting_line.go holds the daemon's waiting line as the readers of a card read it: the place
// each issue holds in the line, the issues a blocker link holds, which issues carry a card at
// all, and which card counts an issue that carries none. Two readers ask those questions — the
// board route, which draws the cards (internal/dispatch/api/legion_board.go), and the per-issue
// facts, which answer the waiting rule for an issue with no card to hand
// (internal/dispatch/legionfacts/facts.go) — and a line built twice is two lines: the card and
// the stored row of one issue then disagree about why that issue waits.
//
// The line reads plain values, as the rules beside it do (board_rules.go): what the daemon
// reports of one issue, and what the tree walk reads of one (TreeChain). Neither reader's own
// population comes here, and nothing here reads a database.

// LineIssue is what the line reads of one issue the daemon reports: the phase the daemon puts it
// in and the slot it holds of its own. Both readers take them from the newest observation of each
// field, by the one decode of that field (ParseSlot).
type LineIssue struct {
	Phase string
	Slot  *int
}

// WaitingLineInput is what a reader gives the line.
//
// Admission is the daemon's newest admission block, nil while no read has recorded one, which the
// column rule answers differently from a recorded block that names the issue nowhere (Queued).
//
// Issue answers what the daemon reports of one issue, and false for an issue it reports nothing
// of: the board draws no card for such an issue, and the line numbers it in no place.
//
// Tree answers what the tree walk reads of one issue (TreeNode), for every issue of the project
// and not only for the ones the daemon reports, because the walk climbs through an issue that
// carries Legion phase work alone. Both functions are read as the line is built and as it is
// asked, so a reader holds them for as long as it holds the line.
type WaitingLineInput struct {
	Admission *Admission
	Issue     func(key string) (LineIssue, bool)
	Tree      func(key string) (TreeNode, bool)
}

// WaitingLine is one project's waiting line, built once by each reader of a board. It answers
// what the daemon's state says of one issue — its place and its blockers — and what the board
// makes of that: the column the issue stands in, or no column at all for an issue the board
// draws no card for.
type WaitingLine struct {
	input WaitingLineInput
	// read is whether a read recorded an admission block at all (QueuedUnread).
	read bool
	// order is the issues the line names, in the daemon's own order, each one once; places is
	// the place each of them holds, from 1; blockers is the issues a blocker link holds each
	// issue on.
	order    []string
	places   map[string]int
	blockers map[string][]string
	// columns keeps each answer of column once it is known, which also ends a tree chain that
	// loops back on itself.
	columns map[string]string
}

// NewWaitingLine numbers the line and takes the blocker links. A key the daemon reports nothing
// of takes no place: the board draws no card for it, so no reader has a card to put a place on.
func NewWaitingLine(input WaitingLineInput) *WaitingLine {
	line := &WaitingLine{
		input:    input,
		places:   map[string]int{},
		blockers: map[string][]string{},
		columns:  map[string]string{},
	}
	if input.Admission == nil {
		return line
	}
	line.read = true
	for _, key := range input.Admission.Waiting {
		if _, reported := input.Issue(key); !reported {
			continue
		}
		if _, numbered := line.places[key]; numbered {
			continue
		}
		line.order = append(line.order, key)
		line.places[key] = len(line.places) + 1
	}
	for _, blocked := range input.Admission.Blocked {
		line.blockers[blocked.Issue] = blocked.BlockedBy
	}
	return line
}

// queued is what this line knows of the issue, in the shape the column rule reads (Queued):
// nothing at all where no read recorded an admission block, and otherwise whether the line names
// the issue or a blocker link holds it.
func (l *WaitingLine) queued(key string) Queued {
	switch {
	case !l.read:
		return QueuedUnread
	case l.places[key] > 0 || len(l.blockers[key]) > 0:
		return QueuedYes
	default:
		return QueuedNo
	}
}

// Place is the issue's place in the line, from 1, and nil for an issue the line does not name.
// Each call answers a pointer of its own, which a caller may keep.
func (l *WaitingLine) Place(key string) *int {
	place, named := l.places[key]
	if !named {
		return nil
	}
	return &place
}

// BlockedBy is the issues a blocker link holds this one on, and nil where none does.
func (l *WaitingLine) BlockedBy(key string) []string {
	return l.blockers[key]
}

// Column is the board column the issue stands in, worked out once and kept, and "" for an issue
// the board draws no card for: an issue the daemon reports nothing of, an issue whose phase no
// column holds, and a child its tree has not started with a card above it in that tree, which
// counts the child instead (CountedOn). Every other issue stands in the column of its phase, and
// an issue the daemon's state queues stands in one whatever its phase (Column, board_rules.go).
//
// It is the one place the rule is read: whether the board draws a card for an issue is whether
// this answers a column of its own, so the two can never differ.
//
// The issue is kept as standing in no column before the climb above it, so a tree chain that
// loops back on itself ends rather than running for ever.
func (l *WaitingLine) Column(key string) string {
	if held, known := l.columns[key]; known {
		return held
	}
	l.columns[key] = ""
	issue, reported := l.input.Issue(key)
	if !reported {
		return ""
	}
	if l.unstarted(key, issue) && l.nearestCard(key) != "" {
		return ""
	}
	held := Column(issue.Phase, issue.Slot, l.queued(key))
	l.columns[key] = held
	return held
}

// CountedOn is the card that counts this issue, and "" where no card does. An `admitted` issue
// that holds no slot and that the daemon's state does not queue has not been started: the board
// draws no card for it and counts it on the nearest card above it in its tree. Every other issue
// the board draws no card for is counted on nothing, because the reader of a card sees nothing of
// the phase it is in.
func (l *WaitingLine) CountedOn(key string) string {
	issue, reported := l.input.Issue(key)
	if !reported || !l.unstarted(key, issue) {
		return ""
	}
	return l.nearestCard(key)
}

// Queue is the Queue column's keys in the board's order: the issues the line names, in the
// daemon's own order, which the view does not re-sort, and then the rest in the order the reader
// draws them in. keys is every key the reader may draw, in that order, which is Dispatch's rank
// order on the board. With a block recorded the rest are the issues a blocker link holds, and
// with none it is every unstarted issue that carries a card, by the fallback the column rule
// states (QueuedUnread).
func (l *WaitingLine) Queue(keys []string) []string {
	queue := make([]string, 0, len(l.order)+len(l.blockers))
	for _, key := range l.order {
		if l.Column(key) == ColumnQueue {
			queue = append(queue, key)
		}
	}
	for _, key := range keys {
		if _, named := l.places[key]; named {
			continue
		}
		if l.Column(key) == ColumnQueue {
			queue = append(queue, key)
		}
	}
	return queue
}

// unstarted is whether the issue is a child its tree has not started: the daemon reports it
// `admitted`, it holds no slot of its own (waitingForASlot), and the daemon's state does not
// queue it. An issue the line names or a blocker link holds has been admitted to the queue,
// which is a place of its own and not a count on another issue's card.
func (l *WaitingLine) unstarted(key string, issue LineIssue) bool {
	return waitingForASlot(issue.Phase, issue.Slot) && l.queued(key) != QueuedYes
}

// nearestCard is the nearest issue above key in its tree that carries a card, and "" where none
// does. It climbs the one chain the tree rule names (TreeChain), so a count never leaves the tree
// the walk puts the issue in.
func (l *WaitingLine) nearestCard(key string) string {
	for _, above := range TreeChain(key, l.input.Tree)[1:] {
		if l.Column(above) != "" {
			return above
		}
	}
	return ""
}
