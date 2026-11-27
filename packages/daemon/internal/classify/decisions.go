package classify

import (
	"sort"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Arrival is what a synchronize says of the move a head arrives by: Before, the head GitHub says
// the branch moved from, and Sender, the account whose push moved it (the webhook's sender), each
// empty when the fact names none - a reopen, a close at another head, the daemon's own read
// of the head, a listener that predates the field; and At, GitHub's clock of the synchronize (the
// pull request's updated_at), zero when unknown. Since is GitHub's clock of the head the record
// held, zero when unknown: the moves a skipped synchronize named between the two are this
// arrival's (arrivalPushes).
type Arrival struct {
	Before, Sender string
	At, Since      time.Time
}

// AdvancePullRequestHead moves the pull request to a new observed head (advance), arriving by the
// move arrival names. A head that arrives before every push of its
// move is recorded - no chain of recorded pushes leads to it from the recorded head
// (arrivalPushes), or its synchronize names a move the pushes cannot yet tell - opens a head run,
// and a head that arrives while one is open joins it: each push, and each move a late synchronize
// names (NameMove), replays the run (replayRun), so the moves made and the CI settlement that
// stands are those every push and synchronize arriving first would have given.
func AdvancePullRequestHead(pr record.PullRequest, arrival Arrival, headSHA string) record.PullRequest {
	if pr.Run == nil {
		if read := arrivalPushes(pr, arrival, headSHA, nil); (read.chained(pr.HeadSHA, headSHA) && !read.waits) || pr.HeadSHA == "" {
			pr, _, _ = advance(pr, arrival, headSHA, nil)
			return pr
		}
		pr.Run = &record.HeadRun{Anchor: judgementOf(pr), Pushes: clone(pr.Pushes), Since: pr.HeadUpdatedAt, Settled: clone(pr.SettledHeads)}
	}
	pr = runStep(pr, record.RunStep{Head: headSHA, Before: arrival.Before, Sender: arrival.Sender, SyncedAt: arrival.At})
	pr.Pushes = unspentPushes(pr.Pushes, pr.Moves, true)
	return pr
}

// MovesBy is whether a synchronize to head is a move of the head the record holds (pr.HeadSHA): a
// head other than it. A synchronize to the recorded head is no move, as the release before read
// it, whatever it names: a copy of the synchronize that reached the head (GitHub can send two for
// one push), or the branch back at the head after moves whose synchronizes were skipped - a loop in
// a skipped gap (the loop floor, unspentPushes), which the release before reads as no move and so
// does this one. It still names its move (NameMove).
func MovesBy(pr record.PullRequest, head string) bool {
	return head != "" && head != pr.HeadSHA
}

// NameMove records the move a synchronize names that moves no head - one older than the newest
// applied (LateLifecycle), or one that returned the branch to the recorded head - while a head run
// is open and an arrival in it waits for it (inAGap): which push, by its sender, the move from
// named.Before to named.SHA took. It is a fact about the branch, as a push is, so the run replays
// with it (replayRun), and an arrival whose synchronize the daemon skipped is judged by its own
// push. It moves no head and opens no run, and changes nothing - pr is returned as it was - with no
// run open, when no arrival of the run waits for it, when it was named already, or when it names no
// sender.
func NameMove(pr record.PullRequest, named record.Named) record.PullRequest {
	if pr.Run == nil || named.Before == "" || named.SHA == "" || named.Sender == "" || named.Before == named.SHA ||
		!inAGap(*pr.Run, named) || namedAlready(pr.Run.Names, named) {
		return pr
	}
	run := *pr.Run
	run.Names = append(clone(run.Names), named)
	if len(run.Names) > maxMoves {
		run.Names = run.Names[len(run.Names)-maxMoves:]
	}
	pr.Run = &run
	return replayRun(pr)
}

// namedAlready is whether names holds named (sameName): a synchronize delivered again names
// nothing new.
func namedAlready(names []record.Named, named record.Named) bool {
	for _, n := range names {
		if sameName(n, named) {
			return true
		}
	}
	return false
}

// inAGap is whether named can be a move of an arrival of run whose synchronize skipped it: a head
// step naming its move by its sender whose synchronize GitHub's clock puts named no later than, and
// no earlier than the head before it (record.HeadRun.Since for the first) - GitHub's clock is to
// the second, so a skipped move can share the second of either - unless named is that head's own
// move, at its own clock (inWindow). A clock unknown on either side fences nothing.
func inAGap(run record.HeadRun, named record.Named) bool {
	since, prev := run.Since, run.Anchor.HeadSHA
	for _, s := range run.Steps {
		if s.Head == "" {
			continue
		}
		if s.Before != "" && s.Sender != "" && !LateLifecycle(s.SyncedAt, named.At) && inWindow(named, prev, since) {
			return true
		}
		since, prev = s.SyncedAt, s.Head
	}
	return false
}

// inWindow is whether named comes no earlier than since, GitHub's clock of the head the window
// starts at (start), and is not that head's own move at that clock: a copy of the synchronize that
// reached it.
func inWindow(named record.Named, start string, since time.Time) bool {
	if named.At.IsZero() || since.IsZero() {
		return true
	}
	return !named.At.Before(since) && !(named.At.Equal(since) && named.SHA == start)
}

// advance moves the pull request to a new head, by the move arrival names, and says whether a
// chain of recorded pushes led to it from the recorded head (chained) with nothing left for it to
// wait for (arrivalPushes), and returns the named moves its judgement left (names, the run's). The
// recorded CI settlement stays, as CheckedHead's, but stands for nothing at the new head: a
// settlement stands only for the head it names, and the new head's own replaces it
// (SettlementFor). GitHub's own verdict on merging the head - Mergeability and MergeState - goes
// with the move, and the time that verdict was read (StateReadAt) with them, since any push may
// change the code GitHub answered about.
//
// The alternative was to record the head GitHub answered for beside the answer and read a verdict
// of another head as unread; this is the same rule with no second field to keep true, and a kept
// verdict of another head is exactly what moved an approval of a new head on the old head's
// `clean` (dispatch://INFRA-714 R2). It costs at most one read: the arrival opens a head run
// whose anchor keeps the verdict (record.Judgement), and the arrival is itself a fact that asks
// GitHub for the new head's state (workflow's askMergeState). The move and the chain's pushes
// join the moves made (madeMoves), and the pushes they spend go (unspentPushes). Nothing here
// judges the settlement: a CI result moves no issue (dispatch://INFRA-714/spec R1), so a head
// arrives counting nothing.
func advance(pr record.PullRequest, arrival Arrival, headSHA string, names []record.Named) (record.PullRequest, bool, []record.Named) {
	read := arrivalPushes(pr, arrival, headSHA, names)
	var chain []record.ClassifiedPush
	chained := read.path
	if read.path {
		chain = read.pushes
	} else {
		chain = chainOf(read.pushes, pr.HeadSHA, headSHA)
		chained = chain != nil
	}
	// GitHub's verdict is its answer about the head the record stands at, so a move to another
	// head leaves nothing to read: any push may change the code GitHub answered about. The time
	// it was read goes with it, since it is then the time of a read about the head the record has
	// left, and a stamp kept over a move would say this head had been read when it never was.
	if headSHA != pr.HeadSHA {
		pr.Mergeability, pr.MergeState, pr.StateReadAt = "", record.MergeStateUnread, time.Time{}
		pr.ReviewsRequested, pr.HeadChecks, pr.EvidenceReadAt = nil, record.HeadChecksUnread, time.Time{}
		pr.NullMergeableReads, pr.NullMergeableSince = 0, time.Time{}
	}
	if !read.waits {
		// An arrival that waits is judged for now by pushes that may not be its own (provisional),
		// so it makes no move and spends no push: each stays for the arrival whose own it is.
		pr.Moves = madeMoves(pr.Moves, pr.HeadSHA, headSHA, chain, pr.Pushes)
	}
	pr.Pushes = unspentPushes(pr.Pushes, pr.Moves, true)
	pr.HeadSHA = headSHA
	return pr, chained && !read.waits, read.names
}

// maxMoves bounds the moves a pull request remembers (record.PullRequest.Moves): a push for a move
// older than that arrives late enough to wait as a push not yet linked, until boundedPushes lets
// it go.
const maxMoves = 32

// madeMoves is moves with the move from previous to head and the moves of chain's pushes made,
// each pair of heads once and the newest last, at most maxMoves of them. A chain that ends at head
// has the move from previous to head as its last push's, and that move is made once. A move of the
// chain is pushed (record.Move.Pushed); the move from previous to head, when no push of its own
// made it, is pushed when a chain of recorded pushes made it, since GitHub sends no push of those
// two heads for it, and otherwise waits for its push.
//
// A move made again keeps the repeats its earlier record queued, less those the chain took
// (arrivalPushes); and every other push of recorded, the record's pushes, whose two heads a move
// now has - a later push of the same two heads than the one the chain took - is queued on that
// move (pushedMoves), in the order it was delivered.
func madeMoves(moves []record.Move, previous, head string, chain, recorded []record.ClassifiedPush) []record.Move {
	made := make([]record.Move, 0, len(chain)+1)
	for _, p := range chain {
		if moveIndex(made, p.Before, p.SHA) < 0 {
			made = append(made, record.Move{Before: p.Before, SHA: p.SHA, Pushed: true})
		}
	}
	if previous != "" && previous != head && moveIndex(made, previous, head) < 0 {
		made = append(made, record.Move{Before: previous, SHA: head, Pushed: chain != nil})
	}
	for i := range made {
		earlier := moveIndex(moves, made[i].Before, made[i].SHA)
		if earlier < 0 || len(moves[earlier].Repeats) == 0 {
			continue
		}
		if repeats := withoutCopies(moves[earlier].Repeats, chain); len(repeats) > 0 {
			made[i].Repeats = clone(repeats)
		}
	}
	kept := make([]record.Move, 0, len(moves)+len(made))
	for _, m := range moves {
		if moveIndex(made, m.Before, m.SHA) < 0 {
			kept = append(kept, m)
		}
	}
	kept = append(kept, made...)
	if len(kept) > maxMoves {
		kept = kept[len(kept)-maxMoves:]
	}
	taken := clone(chain)
	for _, p := range recorded {
		if k := indexOf(taken, p); k >= 0 {
			taken = append(taken[:k:k], taken[k+1:]...)
			continue
		}
		if moveIndex(kept, p.Before, p.SHA) >= 0 {
			kept, _ = pushedMoves(kept, p)
		}
	}
	return kept
}

// moveIndex is the index of the move from before to sha among moves, -1 when it is not there.
func moveIndex(moves []record.Move, before, sha string) int {
	for i, m := range moves {
		if m.Before == before && m.SHA == sha {
			return i
		}
	}
	return -1
}

// indexOf is the index of the first push of pushes equal to p, -1 when none is.
func indexOf(pushes []record.ClassifiedPush, p record.ClassifiedPush) int {
	for i, q := range pushes {
		if q == p {
			return i
		}
	}
	return -1
}

// maxRepeats bounds the repeats a move queues (record.Move.Repeats): past it the oldest goes, as
// the oldest push waiting for a link goes past maxWaitingPushes.
const maxRepeats = maxWaitingPushes

// pushedMoves is moves once push arrives, and the repeat the move let go past maxRepeats, if any:
// a move of push's two heads made before its push was recorded takes push as that push, and is
// pushed from then on; one already pushed queues push as its newest repeat (record.Move.Repeats) -
// the push delivered again, or the branch moving between the two heads again, which only the
// head's next arrival by that move tells apart. Either way the move spends push (unspentPushes).
func pushedMoves(moves []record.Move, push record.ClassifiedPush) ([]record.Move, *record.ClassifiedPush) {
	i := moveIndex(moves, push.Before, push.SHA)
	if i < 0 {
		return moves, nil
	}
	moves = clone(moves)
	if !moves[i].Pushed {
		moves[i].Pushed = true
		return moves, nil
	}
	repeats := append(clone(moves[i].Repeats), push)
	var gone *record.ClassifiedPush
	if len(repeats) > maxRepeats {
		gone, repeats = &repeats[0], repeats[1:]
	}
	moves[i].Repeats = repeats
	return moves, gone
}

// arrivalPushes is the pushes the arrival of head, by the move arrival names, is judged with;
// whether the arrival waits for what its synchronize names to be recorded (waits), which keeps a
// head run open; and the named moves of a run (names) its judgement leaves. Its three readings:
//
//   - A synchronize that names no move (no before) takes no repeat and the newest recorded push of
//     each two heads (newestOfEachMove), as the release before this one did, which kept the newest
//     push of each two heads alone: behind a listener that sends no before, the daemon judges a
//     history as that release did (see unspentPushes for the one change).
//   - One that names the head it moved from but not its sender takes the oldest recorded push of
//     each two heads (oldestOfEachMove) and, when no chain of them leads to head, a chain through
//     before that takes the oldest repeat of each move (beforeChain).
//   - One that names both takes the push of its own move by its sender (pushOf), and, when the
//     record's head is not before - the synchronizes of the moves between were skipped - the pushes
//     their own late synchronizes named (NameMove): the run's named moves GitHub's clock puts no
//     earlier than the record's head (Since, inWindow) and no later than this synchronize, in that
//     clock's order, which must lead one by one from the record's head to before (gapPath), each
//     push by its sender. Those named moves are spent by the
//     arrival. Until its own push, that path and each of its pushes are recorded it waits, judged
//     meanwhile by the pushes it has (provisional): a named move's push, and for two heads no
//     synchronize named, the newest recorded push (the release before's). A synchronize lost for
//     good leaves it so until the run's bound cuts it off.
func arrivalPushes(pr record.PullRequest, arrival Arrival, head string, names []record.Named) arrivalRead {
	before := arrival.Before
	if before == "" {
		return arrivalRead{pushes: newestOfEachMove(pr.Pushes), names: names}
	}
	if arrival.Sender == "" || pr.HeadSHA == "" {
		return arrivalRead{pushes: beforeChain(pr, before, head), names: names}
	}
	var required, optional []record.Named
	later := func(n record.Named) bool { return !arrival.At.IsZero() && !n.At.IsZero() && n.At.After(arrival.At) }
	for _, n := range names {
		switch {
		case later(n) || !inWindow(n, pr.HeadSHA, arrival.Since):
		case !n.At.IsZero() && (n.At.Equal(arrival.At) || n.At.Equal(arrival.Since)):
			optional = append(optional, n)
		default:
			required = append(required, n)
		}
	}
	seen := append(clone(required), optional...)
	waiting := func(own *record.ClassifiedPush) arrivalRead {
		return arrivalRead{pushes: provisional(pr, seen, own), waits: true, names: names}
	}
	own := pushOf(pr, before, head, arrival.Sender)
	if own == nil {
		return waiting(nil)
	}
	path, ok := gapPath(required, optional, pr.HeadSHA, before)
	if !ok {
		return waiting(own)
	}
	var gap []record.ClassifiedPush
	for _, n := range path {
		p := pushOf(pr, n.Before, n.SHA, n.Sender)
		if p == nil {
			return waiting(own)
		}
		gap = append(gap, *p)
	}
	// The arrival spends the moves on its path and every earlier one; a move of its own second it
	// did not take is the next arrival's, as is every later move.
	var left []record.Named
	for _, n := range names {
		if later(n) || (!arrival.At.IsZero() && n.At.Equal(arrival.At) && !namedAlready(path, n)) {
			left = append(left, n)
		}
	}
	// A path that reaches a head twice made a loop in the skipped gap; the loop is erased, as the
	// release before's shortest chain reads every move, so its pushes decide nothing (the loop
	// floor, unspentPushes). A move back to the record's head (MovesBy) is no loop.
	return arrivalRead{pushes: withoutLoops(pr.HeadSHA, append(gap, *own)), path: true, names: left}
}

// sameName is whether a and b name the same move by the same sender at the same clock.
func sameName(a, b record.Named) bool {
	return a.Before == b.Before && a.SHA == b.SHA && a.Sender == b.Sender && a.At.Equal(b.At)
}

// withoutLoops is path, the pushes of a move from from in the order the branch made them, with
// each loop erased: a push that reaches a head the path already reached drops the pushes since
// that head, itself with them, unless it is the path's last and reaches from - a move back to the
// head the move left.
func withoutLoops(from string, path []record.ClassifiedPush) []record.ClassifiedPush {
	kept := make([]record.ClassifiedPush, 0, len(path))
	reached := map[string]int{from: 0}
	for i, p := range path {
		if k, ok := reached[p.SHA]; ok && (i < len(path)-1 || p.SHA != from) {
			for _, q := range kept[k:] {
				delete(reached, q.SHA)
			}
			reached[p.SHA] = k
			kept = kept[:k]
			continue
		}
		kept = append(kept, p)
		reached[p.SHA] = len(kept)
	}
	return kept
}

// arrivalRead is what an arrival is judged with (arrivalPushes): pushes, which are the move's own
// path, in the order the branch made them, when path is set, and pushes to find a chain in
// otherwise; whether the arrival waits for a push or a named move still to be recorded (waits); and
// the named moves it leaves for the arrivals after it.
type arrivalRead struct {
	pushes      []record.ClassifiedPush
	path, waits bool
	names       []record.Named
}

// chained is whether read leads from previous to head: its path, or a chain of its pushes.
func (read arrivalRead) chained(previous, head string) bool {
	return read.path || chainOf(read.pushes, previous, head) != nil
}

// gapPath is the moves the branch made from from to before whose synchronizes were skipped, and
// whether they lead one by one from from to before - each move from the head the one before it
// reached - in the order GitHub's clock puts them: every move of required, and those of optional
// that lead there. Optional are the moves in the second the window starts or ends in, which can be
// the window's before them or after them: GitHub's clock is to the second. Of the paths that lead,
// the one taking the fewest optional moves is the path, its moves of one second in the one order
// of them that so leads; none, or more than one such path, leaves the path unled (as does trying
// more than maxTiedOrders). Named moves of unknown clock are
// taken as the shortest chain of the newest of each two heads. A move missing from the middle, the
// start or the end of the path leaves it unled, so the arrival waits for it; a loop the branch made
// inside the path, a head left and returned to, whose synchronizes are still to come, is not seen
// missing (TestAMoveMadeAgainInTheGapOfASkippedSynchronize).
func gapPath(required, optional []record.Named, from, before string) ([]record.Named, bool) {
	all := append(clone(required), optional...)
	for _, n := range all {
		if n.At.IsZero() {
			byKey := map[[2]string]record.Named{}
			var named []record.ClassifiedPush
			for _, m := range newestOfEachName(all) {
				byKey[[2]string{m.Before, m.SHA}] = m
				named = append(named, record.ClassifiedPush{Before: m.Before, SHA: m.SHA})
			}
			chain := chainOf(named, from, before)
			path := make([]record.Named, 0, len(chain))
			for _, p := range chain {
				path = append(path, byKey[pushKey(p)])
			}
			return path, chain != nil || from == before
		}
	}
	sorted := make([]candidate, 0, len(all))
	for i, n := range all {
		sorted = append(sorted, candidate{n, i >= len(required)})
	}
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].named.At.Before(sorted[j].named.At) })
	// Of the paths that lead, the one that takes the fewest optional moves: a move of the window's
	// own second it need not take is the neighbour window's, as it is when it is a second apart.
	var best []record.Named
	bestOptional, ties, tries := -1, 0, 0
	var walk func(start int, at string, acc []record.Named, optional int)
	walk = func(start int, at string, acc []record.Named, optional int) {
		if tries > maxTiedOrders || (bestOptional >= 0 && optional > bestOptional) {
			return
		}
		if start == len(sorted) {
			if at != before {
				return
			}
			if bestOptional < 0 || optional < bestOptional {
				best, bestOptional, ties = acc, optional, 1
			} else {
				ties++
			}
			return
		}
		end := start
		for end < len(sorted) && sorted[end].named.At.Equal(sorted[start].named.At) {
			end++
		}
		var order func(left []candidate, at string, acc []record.Named, optional int)
		order = func(left []candidate, at string, acc []record.Named, optional int) {
			if tries++; tries > maxTiedOrders {
				return
			}
			if onlyOptional(left) {
				walk(end, at, acc, optional)
			}
			for k, c := range left {
				if c.named.Before == at {
					rest := append(append(make([]candidate, 0, len(left)-1), left[:k]...), left[k+1:]...)
					took := optional
					if c.optional {
						took++
					}
					order(rest, c.named.SHA, append(clone(acc), c.named), took)
				}
			}
		}
		order(sorted[start:end], at, acc, optional)
	}
	walk(0, from, nil, 0)
	if bestOptional < 0 || ties != 1 || tries > maxTiedOrders {
		return nil, false
	}
	return best, true
}

// candidate is a move gapPath may put on a path: one the path must take, or an optional one.
type candidate struct {
	named    record.Named
	optional bool
}

// onlyOptional is whether every move of left is optional, so a path may leave them all.
func onlyOptional(left []candidate) bool {
	for _, c := range left {
		if !c.optional {
			return false
		}
	}
	return true
}

// maxTiedOrders bounds the orders gapPath tries of moves GitHub's clock puts in one second.
const maxTiedOrders = 4096

// provisional is the pushes an arrival that still waits is judged with: one push of each two heads
// recorded, waiting or a repeat - own for its own move when it is recorded, the push a named move
// took (seen) when it is recorded, and otherwise the newest recorded.
func provisional(pr record.PullRequest, seen []record.Named, own *record.ClassifiedPush) []record.ClassifiedPush {
	all := clone(pr.Pushes)
	for _, m := range pr.Moves {
		all = append(all, m.Repeats...)
	}
	pick := map[[2]string]record.ClassifiedPush{}
	for _, p := range all {
		pick[pushKey(p)] = p
	}
	for _, n := range newestOfEachName(seen) {
		if p := pushOf(pr, n.Before, n.SHA, n.Sender); p != nil {
			pick[pushKey(*p)] = *p
		}
	}
	if own != nil {
		pick[pushKey(*own)] = *own
	}
	kept := make([]record.ClassifiedPush, 0, len(pick))
	for _, p := range newestOfEachMove(all) {
		kept = append(kept, pick[pushKey(p)])
	}
	return kept
}

// beforeChain is the pushes an arrival by the move from before, its sender not named, is judged
// with: the oldest recorded push of each two heads (oldestOfEachMove), which a move takes before a
// later one; and, when no chain of them leads to head from pr's head, a chain that also takes the
// oldest repeat of each move (record.Move.Repeats) and passes through before: from pr's head to
// before - moves whose synchronizes the record missed - and on from before to head.
func beforeChain(pr record.PullRequest, before, head string) []record.ClassifiedPush {
	pushes := oldestOfEachMove(pr.Pushes)
	if pr.HeadSHA == "" || chainOf(pushes, pr.HeadSHA, head) != nil {
		return pushes
	}
	with := clone(pushes)
	for _, m := range pr.Moves {
		if len(m.Repeats) > 0 {
			with = append(with, m.Repeats[0])
		}
	}
	if len(with) == len(pushes) {
		return pushes
	}
	var first []record.ClassifiedPush
	if before != pr.HeadSHA {
		if first = chainOf(with, pr.HeadSHA, before); first == nil {
			return pushes
		}
	}
	second := chainOf(with, before, head)
	if second == nil {
		return pushes
	}
	return append(first, second...)
}

// pushOf is the recorded push of the move from before to head by sender: the oldest such push
// waiting, else the oldest such repeat of the move. A push that names no sender, recorded from a
// listener that predates the field, is taken for any sender's. Two pushes of the same two heads by
// the same sender are interchangeable. Nil when none is recorded: the push is on its way.
func pushOf(pr record.PullRequest, before, head, sender string) *record.ClassifiedPush {
	by := func(p record.ClassifiedPush) bool {
		return p.Before == before && p.SHA == head && (p.Sender == sender || p.Sender == "")
	}
	for _, p := range pr.Pushes {
		if by(p) {
			return &p
		}
	}
	if i := moveIndex(pr.Moves, before, head); i >= 0 {
		for _, p := range pr.Moves[i].Repeats {
			if by(p) {
				return &p
			}
		}
	}
	return nil
}

// newestOfEachName is names with only the newest by GitHub's clock, then by when it was recorded,
// of the names of each two heads: a move the branch made twice between two arrivals was made last
// by the newer.
func newestOfEachName(names []record.Named) []record.Named {
	newest := map[[2]string]int{}
	for i, n := range names {
		key := [2]string{n.Before, n.SHA}
		if j, ok := newest[key]; !ok || !names[j].At.After(n.At) {
			newest[key] = i
		}
	}
	kept := make([]record.Named, 0, len(newest))
	for i, n := range names {
		if newest[[2]string{n.Before, n.SHA}] == i {
			kept = append(kept, n)
		}
	}
	return kept
}

// oldestOfEachMove is pushes with only the first, in delivery order, of the pushes of each two
// heads: a move takes the oldest, and a later one is queued as its repeat (madeMoves).
func oldestOfEachMove(pushes []record.ClassifiedPush) []record.ClassifiedPush {
	seen := map[[2]string]bool{}
	kept := make([]record.ClassifiedPush, 0, len(pushes))
	for _, p := range pushes {
		if !seen[pushKey(p)] {
			seen[pushKey(p)] = true
			kept = append(kept, p)
		}
	}
	return kept
}

// newestOfEachMove is pushes with only the last, in delivery order, of the pushes of each two
// heads, each where it was delivered: what the release before this one recorded, which replaced a
// push with a later one of the same two heads.
func newestOfEachMove(pushes []record.ClassifiedPush) []record.ClassifiedPush {
	last := map[[2]string]int{}
	for i, p := range pushes {
		last[pushKey(p)] = i
	}
	kept := make([]record.ClassifiedPush, 0, len(last))
	for i, p := range pushes {
		if last[pushKey(p)] == i {
			kept = append(kept, p)
		}
	}
	return kept
}

// chainOf is the pushes of the shortest chain of recorded pushes from from to to, each of which
// said which head it replaced, oldest first; nil when no chain leads there.
func chainOf(pushes []record.ClassifiedPush, from, to string) []record.ClassifiedPush {
	if from == "" || from == to {
		return nil
	}
	type reach struct {
		head string
		via  []record.ClassifiedPush
	}
	reached := map[string]bool{to: true}
	for frontier := []reach{{head: to}}; len(frontier) > 0; {
		current := frontier[0]
		frontier = frontier[1:]
		for _, p := range pushes {
			if p.SHA != current.head || p.Before == "" || reached[p.Before] {
				continue
			}
			via := append([]record.ClassifiedPush{p}, current.via...)
			if p.Before == from {
				return via
			}
			reached[p.Before] = true
			frontier = append(frontier, reach{head: p.Before, via: via})
		}
	}
	return nil
}

// Settled says whether a CI settlement stands for the pull request's current head: one recorded
// for the head itself. A checked head is recorded only with a settlement (SettlementFor, then
// ApplySettlement), and cleared with it (a new generation, migration 0028). Whether every check
// GitHub reported on that head has concluded is a further question, which ChecksConcludedOnHead
// answers.
func Settled(pr record.PullRequest) bool {
	return pr.CheckedHead != "" && pr.CheckedHead == pr.HeadSHA
}

// ChecksConcludedOnHead says whether GitHub has reported a check run on the pull request's
// current head itself and every check run reported there has concluded. The fence holds the
// newest run of each check name with the head GitHub reported it on (record.AttemptRun), and a
// run reaches the fence only through a settlement, which the listener publishes for a commit only
// once every check it has seen there is terminal: so a run of the fence stamped with the current
// head is a run that concluded there, and its settlement is the account of every check GitHub
// had reported on that head having concluded.
//
// It is one of the conditions of MergeOutcomeOf, which says what each merge state the record holds
// comes to for an approved round. A check the base requires holds the head `blocked` while it
// runs, which says nothing about the round, and until the head's own checks have concluded the
// round waits rather than ending on, or naming as stuck, a state the daemon cannot tell apart from
// a check it has not been told of yet. It is evidence about the last settlement the daemon applied,
// not about the head now, so MergeOutcomeOf ends a round on `blocked` only beside the read's own
// account that no check of the head runs (record.PullRequest.HeadChecks). A head GitHub reports no
// check run on at all never satisfies this, so such a round neither ends on `blocked` nor is
// reported stuck, and waits on GitHub's next answer; that is the rule's cost, and it is the
// smaller one.
//
// A run stored before the fence carried the head it ran on names no head, so it is no run on the
// current head and such a record waits for the head's next settlement (record.AttemptRun).
func ChecksConcludedOnHead(pr record.PullRequest) bool {
	return len(HeadRuns(pr)) > 0
}

// HeadRuns is the runs of the pull request's fence that are the current head's own: runs GitHub
// reported on the head the pull request now stands at, in the fence's order; nil when the fence
// holds none. The fence holds the newest run of each check name with the head GitHub reported it
// on (record.AttemptRun), so these are one run per check name rather than every attempt, and a
// run stored before the fence carried its head names no head and is the run of no head at all.
//
// The fence holds only the runs a CI settlement brought, and the listener settles a head only once
// every check it has seen there is terminal. So no run here is one still going, and an empty
// answer does not say GitHub ran no check on the head: it is as much every head whose CI is still
// running as a head GitHub reported nothing on.
//
// It is the one place the test of a run's head is written, and every reader of the daemon that
// asks which runs are the current head's reads it, so none of them can drift from another:
// ChecksConcludedOnHead is this list being non-empty, the state build counts it for the number
// the issue's view carries (projection's mergeWaitView, api.MergeWaitView.CheckRuns), and the
// notice of a head GitHub will not merge names each of its runs with its conclusion (workflow's
// headChecks).
func HeadRuns(pr record.PullRequest) []record.AttemptRun {
	var runs []record.AttemptRun
	for _, run := range pr.CheckRuns {
		if run.Head != "" && run.Head == pr.HeadSHA {
			runs = append(runs, run)
		}
	}
	return runs
}

// SettlementFor says whether a CI settlement of candidate.Head may stand for the pull request's
// current head, and returns the pull request with the fence it is weighed against
// (ClassifySettlement) and applied to (ApplySettlement). It may only when candidate.Head is the
// current head: every other settlement - an earlier head's, a head a force push left - stands for
// nothing. Check runs, generations, snapshots and commit statuses are each head's own, so the
// fence is the head's: the one the pull request holds when it stands at the head, and otherwise
// the one it remembers for the head (record.PullRequest.SettledHeads), empty for a head it never
// settled or no longer remembers.
//
// The pull request holds another head's fence, or none, when the branch went to another head that
// settled and came back, when a reopen emptied the fence, and when a new generation's clear did
// (record.Store's ClearGeneration). What it remembers for the head is the fence as it stood
// before that, so a settlement after any of them is classified as it would have been had nothing
// reset the fence, and applied to the same fence. An earlier settlement of the head - an older
// run, fewer checks, an older commit status - delivered late, or replayed from a message the
// intake gave up on, however late, is then stale as it is against a fence that stands, and the
// newest one delivered again is the duplicate it is there (settle says what each does to a fence
// that was reset).
func SettlementFor(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	head := candidate.Head
	if head != pr.HeadSHA {
		return pr, false
	}
	if head == pr.CheckedHead {
		return pr, true
	}
	remembered := record.Settlement{Head: head}
	for _, settled := range pr.SettledHeads {
		if settled.Head == head {
			remembered = settled
		}
	}
	return withFence(pr, remembered), true
}

// fenceEntry is pr's check fence as an entry of what the pull request remembers of its settled
// heads (record.PullRequest.SettledHeads): the head it stands at and each field of the fence, every
// list copied, so the entry shares no list with pr.
func fenceEntry(pr record.PullRequest) record.Settlement {
	return record.Settlement{Head: pr.CheckedHead, CheckRuns: clone(pr.CheckRuns), Generation: pr.Generation, Snapshot: pr.Snapshot,
		Failing: clone(pr.Failing), Cancelled: clone(pr.Cancelled), Statuses: clone(pr.Statuses)}
}

// withFence is pr with entry as its check fence, standing at entry's head: each field of the entry,
// every list copied, so pr shares no list with the entry it remembers, and Failing and Cancelled
// lists even where the entry holds none, as a fence a settlement applied to holds them. fenceEntry
// is the copy the other way: these two copy the pull request's fence to and from what it
// remembers of its settled heads. They are not the only copies of a settlement's fields, so a
// field the fence gains reaches these two and each of the others: the settlement fact's candidate
// (workflow's settlementCandidate), a head run's settlement step and its replay
// (ApplyCheckSettlement's step, candidateOf, sameSettlement), the apply of a newer settlement
// (ApplySettlement), the reopen step that empties the fence (applyStep), the judgement a head run
// anchors and replays and the test of its change (judgementOf, withJudgement, JudgementChanged),
// and the store's columns and memory rows of the fence (record's PutPullRequest, scanPullRequest,
// storedFence and the clear of a new generation).
func withFence(pr record.PullRequest, entry record.Settlement) record.PullRequest {
	pr.CheckedHead = entry.Head
	pr.Failing, pr.Cancelled = append([]string{}, entry.Failing...), append([]string{}, entry.Cancelled...)
	pr.CheckRuns, pr.Statuses = clone(entry.CheckRuns), clone(entry.Statuses)
	pr.Generation, pr.Snapshot = entry.Generation, entry.Snapshot
	return pr
}

// maxSettledHeads bounds the heads a pull request remembers the fence of
// (record.PullRequest.SettledHeads), and so the rows of each kind pull_request_settled_heads keeps
// for it: at most this many heads, each with one run of each check name and one status of each
// context it reported. A branch comes back to a head it held after a push or two is undone, a
// reopen, or a new generation, each of which is a head or two back; sixteen cover that with room.
// A head pushed out of the memory loses only its fence: a settlement of it is weighed against an
// empty one, as one of a head the record never settled is.
const maxSettledHeads = 16

// rememberSettled is settled, what the pull request remembers of its settled heads
// (record.PullRequest.SettledHeads), with pr's fence as the entry of its checked head, the head a
// settlement just applied on: that head's entry replaced and moved to the end, where the head a
// settlement applied on last stands, and the oldest heads beyond maxSettledHeads gone. The fence
// was the remembered one, or the one that stood at the head, when the settlement was applied to it
// (SettlementFor), so the entry it replaces holds nothing the fence does not.
func rememberSettled(settled []record.Settlement, pr record.PullRequest) []record.Settlement {
	kept := make([]record.Settlement, 0, len(settled)+1)
	for _, entry := range settled {
		if entry.Head != pr.CheckedHead {
			kept = append(kept, entry)
		}
	}
	if drop := len(kept) - (maxSettledHeads - 1); drop > 0 {
		kept = kept[drop:]
	}
	return append(kept, fenceEntry(pr))
}

// HeadPushKnown says whether a push the record holds left head, so the record already knows of a
// push that took the branch there. Until one does, the push that left head is on its way or lost,
// and the daemon's read of the pull request rebuilds the move's pushes from GitHub's activity of
// the branch (daemon's headPushes).
func HeadPushKnown(pr record.PullRequest, head string) bool {
	for _, p := range pr.Pushes {
		if p.SHA == head {
			return true
		}
	}
	return false
}

// unspentPushes is what the moves made leave of pushes: every push whose own move, its two heads,
// the record has not made: a push still on its way, a link of a chain still to complete, or one
// that returns the branch to a commit it held before. A push is spent by its own move alone, never
// by the head it leaves: a branch reset to an earlier commit arrives there by a push of its own;
// the move keeps what it spent as its own push or a repeat (pushedMoves). A push that did not say
// which head it replaced is never a link: it stays on its way until a head arrives (arrival), and
// then goes.
//
// A push is matched to a move by its two heads, so the same two heads pushed again - h0..h1, a
// forced return h1..h0, then h0..h1 once more - are spent as the first push delivered twice is:
// neither is a push on its way (CodeOnItsWay), and neither plans anything until a head arrives by
// its move. The record keeps every push apart, in delivery order; a move takes the oldest of its
// two heads, and each later one is queued on the move (record.Move.Repeats, oldest first, at most
// maxRepeats). What an arrival takes for its push depends on what its synchronize names
// (arrivalPushes):
//
//   - Before and sender (the listener's synchronize carries both): the push of its own move by its
//     sender - a push of the same two heads by someone else is not it - and the moves between the
//     record's head and before, which late synchronizes name (NameMove), in GitHub's clock order,
//     each by its sender. Every move is judged by the push that made it, as a move to a fresh commit
//     is (TestAMoveMadeAgainIsJudgedByThePushThatMadeItAgain), and the two pushes of the same two
//     heads that only their order told apart (R1, below) are told apart by their senders
//     (TestASynchronizesSenderTellsTwoPushesOfTheSameTwoHeadsApart). GitHub's clock is to the
//     second, so a move in the second a window starts or ends in may be that window's or its
//     neighbour's (gapPath). A loop on the path - a head reached twice - is erased (withoutLoops),
//     and a synchronize to the recorded head is no move (MovesBy), each as the release before reads
//     it.
//   - Before alone: the oldest push of each two heads, and the oldest repeat on a chain through
//     before (beforeChain). R1 is the floor there: two pushes of the same two heads that differ -
//     in who pushed them - delivered in the reverse of the order they were made, which nothing but
//     the sender orders. The replay's shortest chain (chainOf) is a limit there too, shared with the
//     release before: pushes of another path delivered early can stand for a path the branch did
//     not take.
//   - Neither (an older listener, a reopen, a close at another head, the daemon's own read
//     of the head): no repeat and the newest push of each two heads, as the release before did
//     (TestAnArrivalThatNamesNoMoveIsJudgedAsTheReleaseBefore), a head run that a lost push opened
//     closing at its bound the one change; so this daemon may deploy before the listener.
//
// The rule over before and sender has three floors, each pinned by the fuzz of a sender's moves
// (TestEveryOrderOfASendersMovesIsJudgedAsTheOracleJudgesThem), which delivers every synchronize as
// the workflow does - late only when GitHub's clock puts it before the newest applied
// (LateLifecycle), exactly as the release before - and outside which every order of delivery is
// judged alike and as the fresh-commit oracle judges it. A synchronize is never dropped that the
// release before would apply.
//
//   - A loop in a skipped gap: the moves whose synchronizes are late reach a head twice (the record's
//     head, or one between it and before). The late names of a loop that ends where it began are
//     not missed by the path that leads from the record's head to before without them, so an
//     arrival can close its head run before they arrive, and a late synchronize opens no run; which
//     of them it saw then depends on delivery order. No rule avoids it: until a loop's facts arrive
//     they are the facts of a branch that made no loop, where the run must close. Inside this floor
//     the loop is read as the release before reads it - erased, and a return to the recorded head
//     no move - because the fresh-commit oracle judged by this rule and the same oracle judged by
//     the release before then agree that this rule is right where the release before is wrong more
//     often than the reverse; counting the loop's pushes, they did not agree. In the fuzz's long
//     mode about 119,700 of 200,000 histories have such a loop, and about 78,800 of those are judged
//     unlike their reference order or the oracle, or leave a run open.
//   - Two moves in one GitHub second whose synchronizes arrive reversed: the older is not late by
//     the clock, so it moves the record's head back, as on the release before, and the pull request
//     is judged from there. About 40,400 of 200,000 histories deliver two synchronizes of one
//     second reversed with no loop, and about 36,600 of those are judged unlike the oracle or leave
//     a run open. Three rules that read such a synchronize as late from what the record knew of its
//     heads each dropped a real move the release before applies; this floor is the release
//     before's. With no synchronize lost, about 108,400 of the 200,000 histories leave a head run
//     open to its bound, every one in this floor or the one before.
//   - A synchronize never delivered: the arrival after it waits for the move it names, judged for
//     now by the newest push of each two heads (provisional, the release before's), until the run's
//     bound cuts it off. About 169,600 to 170,600 of 200,000 histories with one lost leave a run
//     open to its bound.
//
// Outside those floors the rule is never wrong where the release before is right, by the
// fresh-commit oracle judged by this rule, in the gates' comparison (the undo and any generators,
// the reference, random and bounded orders, seeds 11, 20261028 and 4242, 40,000 histories each,
// synchronizes on time, late or never delivered, clocks a minute apart or in one second).
//
// A head whose push is lost for good is judged unchained and holds its head run open, and every
// head after it joins the run. The run drops exactly the pushes the record's own bound lets go and
// the repeat a move lets go past maxRepeats (ApplyPush), never one the record still holds, so each
// later head whose push arrives is judged with its chain, and the run closes at the step past its
// bound (maxRunSteps), which cuts the waiting head off (TestARunOneLostPushOpenedClosesAtItsBound).
func unspentPushes(pushes []record.ClassifiedPush, moves []record.Move, arrival bool) []record.ClassifiedPush {
	made := map[[2]string]bool{}
	for _, m := range moves {
		made[[2]string{m.Before, m.SHA}] = true
	}
	kept := make([]record.ClassifiedPush, 0, len(pushes))
	for _, p := range pushes {
		if (p.Before == "" && arrival) || made[pushKey(p)] {
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// ApplyPush records a push. Every push is recorded in delivery order beside any earlier push of the
// same two heads, a delivery being applied once (intake.ApplyFact). A push whose own move the
// record has made is spent as it arrives (unspentPushes), the move keeping it as its own push or as
// a repeat (pushedMoves); and the pushes waiting for a link are bounded (boundedPushes). A push
// delivered late, after its head arrived and the branch moved on, is that move's: it is taken for
// the same two heads pushed again only by a head's next arrival by that move (arrivalPushes). While
// a head run is open - a head arrived before every push of its move was recorded - the push joins
// the run's pushes, the run drops what the record's bound let go, and the run is replayed
// (replayRun): the moves made and the CI settlement that stands become those every push arriving
// before every head would have given. With no run open no head waits for a push of its move - or
// the run that held one was cut to its bound (maxRunSteps) - and a push changes no judgement until
// a head arrives.
func ApplyPush(pr record.PullRequest, push record.ClassifiedPush) record.PullRequest {
	pr, _ = ApplyPushDropping(pr, push)
	return pr
}

// Dropped is what a push lets go (ApplyPushDropping): Waiting, the pushes waiting for a link past
// the bound (boundedPushes), oldest first; Repeat, the repeat a move let go past maxRepeats, if
// any; and Copy, a push of two heads on their way past maxCopies of them, if any (cappedCopies).
type Dropped struct {
	Waiting []record.ClassifiedPush
	Repeat  *record.ClassifiedPush
	Copy    *record.ClassifiedPush
}

// ApplyPushDropping is ApplyPush, and what it let go (Dropped).
func ApplyPushDropping(pr record.PullRequest, push record.ClassifiedPush) (record.PullRequest, Dropped) {
	var dropped Dropped
	pr.Moves, dropped.Repeat = pushedMoves(pr.Moves, push)
	// push is recorded after every earlier push: pushes of the same two heads are each kept, in
	// delivery order.
	recorded := append(append(make([]record.ClassifiedPush, 0, len(pr.Pushes)+1), pr.Pushes...), push)
	unspent, copied := cappedCopies(unspentPushes(recorded, pr.Moves, false), push)
	dropped.Copy = copied
	var gone map[[2]string]bool
	pr.Pushes, gone = boundedPushes(unspent, pr.HeadSHA)
	dropped.Waiting = newestOfKeys(unspent, gone)
	if pr.Run == nil {
		return pr, dropped
	}
	run := *pr.Run
	run.Pushes = append(clone(run.Pushes), push)
	run.Pushes, _ = cappedCopies(withoutKeys(run.Pushes, gone), push)
	if dropped.Repeat != nil {
		run.Pushes = withoutCopies(run.Pushes, []record.ClassifiedPush{*dropped.Repeat})
	}
	pr.Run = &run
	return replayRun(pr), dropped
}

// maxCopies bounds the pushes of the same two heads a pull request keeps on their way, before a
// move of those heads is made: the one the move will take and the maxRepeats it will queue as
// repeats (madeMoves).
const maxCopies = maxRepeats + 1

// cappedCopies is pushes with at most maxCopies pushes of push's two heads, and the one it let go,
// if any: past the bound the oldest after the first goes, as the move made of them would keep the
// first for its own push and let the oldest repeat go (pushedMoves).
func cappedCopies(pushes []record.ClassifiedPush, push record.ClassifiedPush) ([]record.ClassifiedPush, *record.ClassifiedPush) {
	var at []int
	for i, p := range pushes {
		if pushKey(p) == pushKey(push) {
			at = append(at, i)
		}
	}
	if len(at) <= maxCopies {
		return pushes, nil
	}
	gone := pushes[at[1]]
	return append(append(make([]record.ClassifiedPush, 0, len(pushes)-1), pushes[:at[1]]...), pushes[at[1]+1:]...), &gone
}

// withoutCopies is pushes less gone, copy by copy: each push of gone takes away the oldest push of
// pushes equal to it that no earlier one took.
func withoutCopies(pushes, gone []record.ClassifiedPush) []record.ClassifiedPush {
	if len(gone) == 0 {
		return pushes
	}
	left := clone(gone)
	kept := make([]record.ClassifiedPush, 0, len(pushes))
	for _, p := range pushes {
		if k := indexOf(left, p); k >= 0 {
			left = append(left[:k:k], left[k+1:]...)
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// withoutKeys is pushes less every push of two heads gone holds.
func withoutKeys(pushes []record.ClassifiedPush, gone map[[2]string]bool) []record.ClassifiedPush {
	if len(gone) == 0 {
		return pushes
	}
	kept := make([]record.ClassifiedPush, 0, len(pushes))
	for _, p := range pushes {
		if !gone[pushKey(p)] {
			kept = append(kept, p)
		}
	}
	return kept
}

// newestOfKeys is, for each two heads keys holds, the newest push of pushes with them, in the
// order those newest pushes stand.
func newestOfKeys(pushes []record.ClassifiedPush, keys map[[2]string]bool) []record.ClassifiedPush {
	if len(keys) == 0 {
		return nil
	}
	var newest []record.ClassifiedPush
	for _, p := range newestOfEachMove(pushes) {
		if keys[pushKey(p)] {
			newest = append(newest, p)
		}
	}
	return newest
}

// maxWaitingPushes bounds the pushes a pull request keeps that wait for a link: a push that does
// not lie on the path forward from the head, and that no move spent (unspentPushes) - one of a
// line of commits the branch never takes, or one behind a webhook that never arrives. The bound
// counts the two heads of such pushes, each pair once however many of its pushes wait, as the
// release before kept one push of each two heads. Past the bound the pushes of the two heads whose
// newest push is oldest go, and a move they would have linked into is judged without them; an open
// head run lets the same pushes go (ApplyPush).
const maxWaitingPushes = 16

// boundedPushes is pushes with at most maxWaitingPushes pairs of heads of waiting pushes, and the
// pairs it let go, oldest first by their newest push.
func boundedPushes(pushes []record.ClassifiedPush, head string) ([]record.ClassifiedPush, map[[2]string]bool) {
	onPath := onPathFrom(pushes, head)
	newest := map[[2]string]int{}
	for i, p := range pushes {
		if !onPath[pushKey(p)] {
			newest[pushKey(p)] = i
		}
	}
	if len(newest) <= maxWaitingPushes {
		return pushes, nil
	}
	gone := map[[2]string]bool{}
	for i, p := range pushes {
		if newest[pushKey(p)] == i && !onPath[pushKey(p)] && len(newest)-len(gone) > maxWaitingPushes {
			gone[pushKey(p)] = true
		}
	}
	return withoutKeys(pushes, gone), gone
}

// ApplySettlement applies a listener CI settlement whenever ClassifySettlement says it is newer,
// merging its check runs into the fence per check name, and its commit statuses alone where it is
// newer for those alone. It returns false for stale, duplicate and conflicting observations.
//
// Each run is stamped with the head it was reported on and the conclusion the candidate gives it
// (stampedRuns) before it is merged, since the fence keeps the newest run of each name over
// several heads and those facts are that run's. What the pull request then reports failing and
// cancelled follows from the fence: each name the fence holds a concluded run for comes to that
// run's own conclusion, and a name with no run anywhere keeps what the lists say of it
// (concludedBy over EffectiveOutcome). So a check re-run and passed is no longer failing however
// the older run ended, and one that failed on the head whose settlement arrived last is failing
// even when the settlement applied after it reported nothing of that name.
//
// The listener's generation and snapshot number the settlements of one head's own record, so they
// are taken only from a settlement of the head the fence stands at (SettlementFor, which moves
// that head to the candidate's whenever the candidate is the nearer one to the pull request's
// head). A settlement of a head below it merges its runs and the conclusions of them, and leaves
// the recorded head's numbering as it was. The commit statuses are taken from the same
// settlements alone, and merged per context rather than replaced (mergedStatuses): for each
// context the status with the later created_at stands, so a settlement that names fewer statuses
// than the record keeps - the candidate's list is the whole list the head's CI record holds, which
// can be fewer than GitHub holds, as record.PullRequest.Statuses says - leaves each context it
// leaves out as it is, and an older settlement never puts back a status a newer one replaced. On
// an equal created_at the settlement's entry stands. A settlement this does not apply leaves the
// kept list as it is.
//
// A settlement that names a newer status and whose check runs are older than the fence's, or the
// same at a generation no higher than the kept one (SettlementStatusesOnly), changes the kept
// statuses alone, merged as above where it is of the checked head, but with the kept entry
// standing on an equal created_at, since the settlement is the older view: the fence, the failing
// and cancelled names, the generation and the snapshot stay as the newer settlements left them,
// so the newer settlement delivered again is the duplicate it was before (ClassifySettlement says
// why).
func ApplySettlement(pr record.PullRequest, candidate SettlementCandidate) (record.PullRequest, bool) {
	classified := ClassifySettlement(pr, candidate)
	if classified == SettlementStatusesOnly && candidate.Head == pr.CheckedHead {
		pr.Statuses = mergedStatuses(pr, candidate, SettlementStatusesOnly)
		return pr, true
	}
	if classified != SettlementNewer {
		return pr, false
	}
	fence := mergeAttemptSets(pr.CheckRuns, stampedRuns(candidate))
	outcome := concludedBy(fence, EffectiveOutcome(pr, candidate))
	pr.CheckRuns = fence
	if candidate.Head == pr.CheckedHead {
		pr.Generation = candidate.Generation
		pr.Snapshot = candidate.Snapshot
		pr.Statuses = mergedStatuses(pr, candidate, SettlementNewer)
	}
	pr.Failing = append([]string(nil), outcome.Failing...)
	pr.Cancelled = append([]string(nil), outcome.Cancelled...)
	return pr, true
}

// LateLifecycle reports whether a pull request lifecycle observation (opened, reopened,
// synchronize or closed) at observed is older than the newest one applied, at applied: a late
// redelivery, which must change nothing. Both are the pull request's updated_at. An equal clock is
// not late, since GitHub's clock is to the second and two real events can share one; a missing
// clock on either side fences nothing.
func LateLifecycle(observed, applied time.Time) bool {
	return !observed.IsZero() && !applied.IsZero() && observed.Before(applied)
}

// RepeatedClose reports whether a close at observed repeats the close already applied to pr: pr is
// recorded closed and the close's clock is not after the recorded one, or the close has none. A
// later close is a second one, whose reopen between the two has not been delivered yet. Unlike
// LateLifecycle, an equal clock is the same event here: a second close in the same second as the
// first goes unreported only when the reopen between them was never observed.
func RepeatedClose(pr record.PullRequest, observed time.Time) bool {
	return pr.State == record.PullRequestClosed && !observed.After(pr.HeadUpdatedAt)
}

// LatestClock is the clock a pull request keeps after applying an observation at observed: the
// later of the two. An observation with no clock is applied but never lowers the stored one, which
// would let an older observation redelivered after it pass LateLifecycle.
func LatestClock(applied, observed time.Time) time.Time {
	if observed.After(applied) {
		return observed
	}
	return applied
}

// pathFrom is the pushes that lie on the path forward from head: each that replaced head or the
// head a push already on the path left - also one that returns the branch to a head the path
// already passed, a push of its own whatever the head it leaves. With unplaced, a push that did
// not say which head it replaced is on the path too, the reading that never lets it pass unseen.
func pathFrom(pushes []record.ClassifiedPush, head string, unplaced bool) []record.ClassifiedPush {
	var path []record.ClassifiedPush
	reached, included := map[string]bool{head: true}, make([]bool, len(pushes))
	for grew := true; grew; {
		grew = false
		for i, p := range pushes {
			if !included[i] && ((unplaced && p.Before == "") || reached[p.Before]) {
				path = append(path, p)
				included[i], reached[p.SHA] = true, true
				grew = true
			}
		}
	}
	return path
}

// pushKey is a push's identity: the head it replaced and the head it left.
func pushKey(p record.ClassifiedPush) [2]string {
	return [2]string{p.Before, p.SHA}
}

// onPathFrom is the identities of the pushes on the path forward from head (pathFrom).
func onPathFrom(pushes []record.ClassifiedPush, head string) map[[2]string]bool {
	onPath := map[[2]string]bool{}
	for _, p := range pathFrom(pushes, head, false) {
		onPath[pushKey(p)] = true
	}
	return onPath
}

// ApprovalStands says whether an approval of reviewed approves the pull request's current head: it
// is an approval of the head itself, since any push may change code. Nothing stands while a push is
// on its way from the head (CodeOnItsWay): the push event can come first, and the approval is then
// of a head already on its way out.
func ApprovalStands(pr record.PullRequest, reviewed string) bool {
	return reviewed != "" && reviewed == pr.HeadSHA && !CodeOnItsWay(pr)
}

// ApprovalSetAside is the one spelling of the state an approval of reviewed is in when it neither
// stands at the pull request's head nor waits on anything the record already holds: it is no
// approval of the head itself (ApprovalStands), and no push is on its way to replace that head
// (CodeOnItsWay), whose arrival would decide it. The branch has moved past the code the reviewer
// read and nothing more is coming, so only a review of the head itself ends the round (workflow's
// stuckApprovedOtherCode).
func ApprovalSetAside(pr record.PullRequest, reviewed string) bool {
	return reviewed != "" && !ApprovalStands(pr, reviewed) && !CodeOnItsWay(pr)
}

// CodeOnItsWay says whether a push lies on the path forward from the pull request's current head,
// its new head not arrived yet: any push may change code. It is the one rule of what a recorded
// push does to an approval. A recorded push from the head is code on its way, and it takes back an
// approval of the head until its new head arrives. A push delivered late, after its new head
// arrived and the branch returned to this one, is spent by the move the record made
// (unspentPushes), so it is not on its way; the same two heads pushed again are spent as its repeat
// (record.Move.Repeats), and look the same until their synchronize moves the head. The daemon
// lands no merge meanwhile: the merger merges, and it merges the head its review approved.
//
// A push to no commit (record.NoCommit) is GitHub's deletion of the branch. It names no code, so
// it is never on its way, and the path does not run through it. A release before the deletion
// guard (intake's decodePush) stored such a push as it stored any push, so a record can hold one.
//
// A merged record keeps no pushes once its merge named the head, since that head is final
// (workflow's merged), and it records no push after the merge (workflow's push). So a merged
// record that still holds a push from its head is one whose merge did not name that head: a merge
// the release before recorded, or a merge that names no head. That push can be the code GitHub
// merged, so it stays on its way, and an approval of the head it left does not stand.
func CodeOnItsWay(pr record.PullRequest) bool {
	return len(pathFrom(withoutDeletions(pr.Pushes), pr.HeadSHA, true)) > 0
}

// withoutDeletions is pushes less GitHub's deletions of the branch, the pushes to no commit
// (record.NoCommit). It returns pushes itself, with no copy, when it holds none.
func withoutDeletions(pushes []record.ClassifiedPush) []record.ClassifiedPush {
	for i, p := range pushes {
		if p.SHA != record.NoCommit {
			continue
		}
		kept := append(make([]record.ClassifiedPush, 0, len(pushes)-1), pushes[:i]...)
		for _, q := range pushes[i+1:] {
			if q.SHA != record.NoCommit {
				kept = append(kept, q)
			}
		}
		return kept
	}
	return pushes
}

// RoundWaitsOnMergeState says whether a review round waits on GitHub's answer about merging the
// pull request's head: the issue is in reviewing, its pull request is open, its reviewer's review
// approved the head the pull request now stands at (ApprovalStands, which is the whole of that
// test: a review of another head approves nothing, and a push on its way takes an approval of this
// head back), the merge state the record holds for that head does not end the round
// (MergeOutcomeOf), the unread state among them, and the record does not hold that head in a
// confirmed conflict with its base (ConflictRound).
//
// Two readers ask it, and they must answer alike, which is the whole reason the rule is here
// rather than in either of them: the engine asks it to decide whether this round is one the
// daemon reads GitHub's merge state for (workflow's reviewRound, round.waitsOnMergeState, which
// the read behind daemon's readMergeState follows), and the state build asks it to decide whether
// the issue's view carries a mergeWait object (projection's mergeWaitView). Restated in either,
// the two would drift, and the state would then say the daemon waits on an answer it never asks
// for, or stay silent about a round it does read for.
//
// It holds for the round that waits quietly and for the stuck round alike (MergeOutcomeOf's two
// outcomes other than MergeEndsRound), because each of them is waiting on GitHub's next answer
// about the same head: the state a head is in changes under it, a base push clearing `behind` and
// the last check of the head concluding making `blocked` a state the round ends on, and a read is
// the only thing that would ever say so. What tells the waiting round from the stuck one is
// MergeOutcomeOf too, which workflow's reviewRound reads.
//
// It does not hold for a head the record holds in a confirmed conflict: that approval ends the
// round whatever the merge state (workflow's reviewRound), since nothing GitHub answers about the
// head will change until the base is merged forward. Where no ask of that merge was made before,
// the issue's move into merging is what asks the merger for it (cybermonklabs/legion#4979,
// record.ConflictMergeNext). Nor for a head whose state ends the round (MergeOutcomeOf). Read
// here, the state build shows no mergeWait for the round the engine has just ended.
//
// It holds only for an open pull request. A merged one has nothing left to merge. GitHub answers
// `unknown` for it for good, and the daemon never reads its state after the merge: workflow's
// mergeStateOwed refuses a pull request that is not open, so the state the record holds is the
// one it held before the merge. A closed one merges nothing until a reopen, and a reopen asks for
// a read of the head again (the opened handler, then Apply's askMergeState). Projection's
// mergeWaitView makes no state test of its own and relies on this one.
func RoundWaitsOnMergeState(issue record.Issue, reviewer record.PhaseRow, pr *record.PullRequest) bool {
	if issue.Phase != phase.Reviewing || pr == nil || reviewer.Decision == nil {
		return false
	}
	// The moment passed is the record's own read time, not a clock: only whether the round ends is
	// read here, and the moment decides nothing but which of the two waits a null `mergeable` comes
	// to (MergeOutcomeOf), so the stored read time answers this as the clock would.
	return pr.State == record.PullRequestOpen && reviewer.Decision.State == "approved" &&
		ApprovalStands(*pr, reviewer.Decision.Head) && MergeOutcomeOf(*pr, pr.StateReadAt) != MergeEndsRound && !ConflictRound(*pr)
}

// MergeOutcome is what the merge state the record holds for a pull request's head comes to for
// the review round of an approval that stands on that head (MergeOutcomeOf). The three told
// outcomes are one wait each, told apart by what holds the head (Hold), so a head told of for
// one hold is told again when another holds it.
type MergeOutcome int

const (
	// MergeEndsRound: the approval ends the round, and the issue moves on to its task's next step.
	MergeEndsRound MergeOutcome = iota + 1
	// MergeWaitsQuietly: the round waits on GitHub's next answer about the head, and nobody is
	// told: GitHub has given no answer the round can decide on yet, or a check of the head is
	// still running, and a later fact - a read, a settlement - brings the answer.
	MergeWaitsQuietly
	// MergeWaitsTold: the round waits on GitHub's next answer about the head, and the architect
	// and the reviewer are told once for the head and state (workflow's stuckMergeState): the
	// head's own checks are over and GitHub answers a state it will not merge from - `behind`,
	// `draft`, a `dirty` no second read confirmed - that only a push or a person will clear.
	MergeWaitsTold
	// MergeWaitsToldNullMergeable: told as MergeWaitsTold, of a `blocked` or `unknown` head whose
	// `mergeable` GitHub has answered null on a read at least NullMergeableFloor after the first
	// null read of the run, or whose first null read is NullMergeableTellAfter old by the clock:
	// its own computation has not finished (nullOutlived).
	MergeWaitsToldNullMergeable
	// MergeWaitsToldReviewRequested: told as MergeWaitsTold, of a `blocked`, MERGEABLE head on which
	// GitHub holds a review request open: only the person, team or code owner asked can give it.
	MergeWaitsToldReviewRequested
)

// Told says whether the outcome is one of the waits the architect and the reviewer are told of.
func (o MergeOutcome) Told() bool {
	return o == MergeWaitsTold || o == MergeWaitsToldNullMergeable || o == MergeWaitsToldReviewRequested
}

// Hold names what holds the head of a told outcome beside its merge state, for the mark that
// keeps the notice once for each head and finding (record.ReviewDecision.ToldStuck): a null
// `mergeable`, a review request, or none for every other outcome.
func (o MergeOutcome) Hold() record.StuckHold {
	switch o {
	case MergeWaitsToldNullMergeable:
		return record.StuckHoldMergeableNull
	case MergeWaitsToldReviewRequested:
		return record.StuckHoldReviewRequested
	}
	return ""
}

// EvidenceStands says whether the evidence the record keeps beside its merge state - the count of
// review requests, the rollup and the null reads - was written by the read that recorded that
// state (record.PullRequest.EvidenceReadAt): a release before migration 20261124130001 writes its
// own read time beside a new head or a new read of the same head and leaves the evidence where it
// stood, and a head move clears the read time. Evidence that does not stand is read as unread.
func EvidenceStands(pr record.PullRequest) bool {
	return pr.EvidenceReadAt.Equal(pr.StateReadAt)
}

// NullMergeableStanding says whether the record holds a run of reads of its head that answered
// `mergeable` null, written by the read that recorded the merge state (EvidenceStands): the run a
// next null read counts on from (workflow's mergeability), and the record a failed read asks the
// round again for, so the time bound fires when every read fails (daemon's readFailed).
func NullMergeableStanding(pr record.PullRequest) bool {
	return EvidenceStands(pr) && pr.NullMergeableReads > 0
}

// NullMergeableFloor is how long, read to read, a run of null `mergeable` reads of a head must
// span before it tells: a null read sooner than this after the run's first is the routine
// recompute, not a head stuck. GitHub computes mergeability lazily, on the first read after the
// base moves, so every waiting round's climb after a push to its base meets a null at least once,
// and the climb reads again 30 seconds later (mergeStateLadderWait).
//
// The value rests on a measurement (2026-11-25, 6 pushes to main in three repositories): 121 of
// 149 open pull requests read UNKNOWN on the first read after the push, and every one read again
// answered on the next read, 7.9 s to 14.9 s later (median 8.5 s), an upper bound on GitHub's
// computation. 90 s is six times that maximum. Measure again before changing it.
const NullMergeableFloor = 90 * time.Second

// NullMergeableTellAfter is how long a head may stand with GitHub answering `mergeable`
// null before the architect and the reviewer are told, whatever the count of reads that answered
// so: thirty minutes from the first such read. The base's follow reads read such a head again at
// most ten minutes apart, so three of them fit in the bound, and a null that outlives it is no
// computation still under way. The bound is wall-clock time, not a count, so reads that come far
// apart - a REST budget spent, a restart - cannot stretch the silence (MergeOutcomeOf).
const NullMergeableTellAfter = 30 * time.Minute

// MergeOutcomeOf is the one rule of what the merge state the record holds for pr's head comes to
// for an approved round, at the moment at of the fact that asks: RoundWaitsOnMergeState, which
// the engine and the state build read, and workflow's reviewRound, which tells the waiting round
// from the stuck one and names what holds the head from the outcome, both read it. at decides
// nothing but the clock's null-mergeable bound below; the floor is measured between the record's
// own reads.
//
//   - A state GitHub would merge from (record.MergeState.Mergeable) ends the round.
//   - No state read, a first conflicting read waiting for its second (FirstConflictReadWaits),
//     and a head with no concluded check run of its own (ChecksConcludedOnHead) wait quietly.
//   - `blocked` and `unknown` are decided on the evidence the read that recorded the state wrote
//     (EvidenceStands): the count of review requests, the rollup and the null reads of an older
//     read read as unread. Either waits quietly while the rollup says a check or a status of the
//     head is still running (record.HeadChecksRunning). While GitHub has not computed whether
//     the head merges (`mergeable` null, which it answers while a background job computes it,
//     routinely on the first read after a push, beside `blocked` or `unknown` alike), either
//     waits quietly until a null read of the run comes at least NullMergeableFloor after the
//     run's first, or the first is NullMergeableTellAfter old at at, and is told then
//     (MergeWaitsToldNullMergeable). Otherwise `unknown` waits quietly, and `blocked` waits
//     quietly while no read has given the rollup or the count (record.HeadChecksUnread, a nil
//     ReviewsRequested), waits and is told while GitHub holds a review request open
//     (MergeWaitsToldReviewRequested), and else - MERGEABLE, nothing of the head running, every
//     check run of the head concluded, no review asked for - ends the round. The readers read
//     each answer that waits quietly again (MergeAnswerIncomplete): a climb of reads, then the
//     base's follow reads, up to ten minutes apart, for as long as the head stands.
//   - Every other state GitHub would not merge from - `behind`, `draft`, a `dirty` no second read
//     confirmed - waits and is told (MergeWaitsTold).
//
// `blocked` ends the round because it is GitHub's answer to the reader, not its verdict on the
// merge: a rule about the reader holds a head `blocked` for one token and `clean` for another at
// the same moment (a token without the workflows permission reads `blocked` for every pull request
// that changes .github/workflows/), a check suite of another pull request on the same commit can
// hold a required check `expected` that passed on this one, and the daemon reads no ruleset, so it
// cannot tell such a hold from a required check or review that holds the head
// (dispatch://INFRA-4192/comment/1c89885e-5e89-42f7-8122-9cdc697d0ccd). A round that waited for
// `clean` on such a head would wait for good. The merger's merge is the call GitHub answers about
// the merge itself, accepted or refused with its reason, and the merger answers that refusal. The
// holds the daemon can read - a check of the head running, a review asked for - keep the round
// here, since the merger would meet them as a refusal already known. A required check nobody has
// reported (EXPECTED) is no check running: nothing is under way to report it, so waiting on it
// waits for good, and it is the merger's refusal to name. A
// confirmed conflict is no outcome of this rule: RoundWaitsOnMergeState ends that round whatever
// the merge state (ConflictRound).
func MergeOutcomeOf(pr record.PullRequest, at time.Time) MergeOutcome {
	switch {
	case pr.MergeState.Mergeable():
		return MergeEndsRound
	case pr.MergeState == record.MergeStateUnread, FirstConflictReadWaits(pr), !ChecksConcludedOnHead(pr):
		return MergeWaitsQuietly
	case pr.MergeState != record.MergeStateBlocked && pr.MergeState != record.MergeStateUnknown:
		return MergeWaitsTold
	}
	reviews, checks, nulls, since := pr.ReviewsRequested, pr.HeadChecks, pr.NullMergeableReads, pr.NullMergeableSince
	if !EvidenceStands(pr) {
		reviews, checks, nulls, since = nil, record.HeadChecksUnread, 0, time.Time{}
	}
	switch {
	case checks == record.HeadChecksRunning:
		return MergeWaitsQuietly
	case pr.Mergeability == record.MergeabilityUnknown && nullOutlived(nulls, since, pr.StateReadAt, at):
		return MergeWaitsToldNullMergeable
	case pr.MergeState == record.MergeStateUnknown, pr.Mergeability != record.MergeabilityMergeable,
		checks == record.HeadChecksUnread, reviews == nil:
		return MergeWaitsQuietly
	case *reviews > 0:
		return MergeWaitsToldReviewRequested
	}
	return MergeEndsRound
}

// nullOutlived says whether a run of nulls reads of a head's `mergeable`, the first read at since
// and the latest at last, has outlived GitHub's computation: a null read of the run at least
// NullMergeableFloor after the first, read to read, or the first NullMergeableTellAfter old at
// the moment at. The floor is measured between reads, so a read that failed adds nothing to it;
// the bound is measured by the clock, so reads that come far apart or fail cannot stretch it.
// Neither alone will do: a count alone tells the routine recompute after every push to the base,
// and a long bound alone keeps a head that is truly stuck untold for half an hour.
func nullOutlived(nulls int, since, last, at time.Time) bool {
	if nulls < 1 || since.IsZero() {
		return false
	}
	return nulls >= 2 && last.Sub(since) >= NullMergeableFloor || at.Sub(since) >= NullMergeableTellAfter
}

// MergeAnswerIncomplete says whether GitHub's answer about merging a head, as a read gives it, is
// one the readers read again before they leave the head (daemon's climbMergeState and the base's
// batch): `unknown`, which GitHub answers while it computes the merge state; and `blocked` with a
// part of the answer MergeOutcomeOf needs missing - mergeability not computed (`mergeable` null),
// or no rollup of the head's checks or no count of its review requests in the answer. Any other
// answer is one the round can decide on, and the readers stop at it.
func MergeAnswerIncomplete(answer record.PullRequest) bool {
	switch answer.MergeState {
	case record.MergeStateUnknown:
		return true
	case record.MergeStateBlocked:
		return answer.Mergeability != record.MergeabilityMergeable && answer.Mergeability != record.MergeabilityConflicting ||
			answer.HeadChecks == record.HeadChecksUnread || answer.ReviewsRequested == nil
	}
	return false
}

// SameCount says whether two counts of a pull request's open review requests
// (record.PullRequest.ReviewsRequested) are the same answer: both unread, or both read and equal.
func SameCount(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// FirstConflictReadWaits says whether a first read that found the pull request's current head
// conflicting with its base waits for the second read that confirms or releases it
// (record.PullRequest.ConflictRead, dispatch://INFRA-4374 R2). While it does, GitHub's answer for
// the head is no conflict the daemon holds, and an approval of the head waits for that second
// read (workflow's reviewRound).
func FirstConflictReadWaits(pr record.PullRequest) bool {
	return pr.ConflictRead != nil && pr.ConflictRead.Head == pr.HeadSHA
}

// MergeStateFact is the kind of fact that asks whether GitHub's merge state is read for a pull
// request (MergeStateReadOwed). Workflow's askMergeState names it from the fact it applied, and
// workflow's basePush names MergeStateFactBasePush for each pull request its walk recorded the
// push on.
type MergeStateFact int

const (
	// MergeStateFactOther is a fact that moves neither the head nor the base: a review, a CI
	// settlement, a push to the pull request's branch, whose synchronize is the fact that moves
	// the head.
	MergeStateFactOther MergeStateFact = iota
	// MergeStateFactHeadMove is a fact that moves the pull request's head or opens it at one: the
	// pull request opened or reopened, a synchronize.
	MergeStateFactHeadMove
	// MergeStateFactBasePush is a push to the base branch the pull request sits on.
	MergeStateFactBasePush
)

// MergeForwardStands says whether a merge forward is asked, and stands, where issue stands, on
// line, the phases its task walks (the workflow's Walk.Line: one for each step, then retro and
// merging; phase.Order for an issue with no plan rows): implementing, where the work already is,
// or any phase after implementing on that line (dispatch://INFRA-4374 R1). On the default steps
// that is testing, reviewing, accepting, retro and merging. Where the task places a planner step
// after the implementer, its planning is one too, since it works on a branch whose pull request is
// open. The issue keeps that phase while the role asked (record.MergeForwardAsker) merges. An
// issue that stands anywhere else - admitted, awaiting_person, a planning before the implementer,
// or out of the workflow - has no pull request work to merge forward and is asked for none. A hold
// counts as standing in the phase it is held from (record.StandsAt). Where the merge comes next
// before merging - retro, or accepting where no step of the task follows the acceptance
// (MergeComesNext) - nothing is asked (ConflictNeedsMerge): the merger merges the base forward once
// the issue reaches merging (cybermonklabs/legion#4979), and an ask made before stands there until
// it lands.
func MergeForwardStands(issue record.Issue, line []phase.Phase) bool {
	at := record.StandsAt(issue)
	implementing, here := -1, -1
	for i, p := range line {
		switch p {
		case phase.Implementing:
			implementing = i
		case at:
			here = i
		}
	}
	return at == phase.Implementing && implementing >= 0 || implementing >= 0 && here > implementing
}

// MergeStateReadOwed is the gate of the read of GitHub's merge state for an issue's open pull
// request (workflow's mergeStateOwed, which both a fact of one pull request reads by, to queue the
// ladder behind daemon's readMergeState, and a base push reads by for each pull request on the
// base, to queue that base's batch of reads, daemon's queueBaseMergeStates): whether a fact of
// kind fact asks for it, for an issue whose task walks line. Each of two reasons asks, and one is
// enough:
//
//   - The fact moves the head, and a merge forward stands where the issue stands
//     (MergeForwardStands, dispatch://INFRA-4374 R0): implementing, or a phase after it on the
//     task's line. GitHub runs no checks on a head that conflicts with its base, so without a read
//     a conflicting push stands with no CI and nobody is told; the read that finds the head
//     CONFLICTING is the first of the two reads that confirm a conflict (R2).
//   - The fact is a push to the base, and the issue stands in a phase after implementing on the
//     task's line. GitHub sends no event of the pull request while it computes whether the head
//     still merges into the moved base. In merging the merge is next (record.ConflictMergeNext),
//     so a conflict there is acted on at once (R0). Where the merge comes next before merging
//     (MergeComesNext: retro, or accepting where no step of the task follows the acceptance) it
//     is recorded for the move into merging (cybermonklabs/legion#4979). Elsewhere, without a
//     read, the record would keep the state the read before the push left: a record read
//     `blocked` while GitHub answered DIRTY, because no read followed the base push (INFRA-4315,
//     dispatch://INFRA-4374/spec#b-r4). A base push reads nothing for an issue in implementing or
//     before it: there the head the implementer pushes next is read, by the reason above.
//
// Every other fact asks nothing here, and so does a head move or a base push of an issue outside
// those phases. The third reason a read is owed is not the gate's: the issue's review round
// waiting on GitHub's answer about the head, whatever the fact - an approval that stands on a head
// GitHub has answered nothing for, or a state that does not end its round (RoundWaitsOnMergeState),
// or a head whose first conflicting read waits for its second (workflow's reviewRound,
// round.waitsOnMergeState). Only the engine works out the round, so workflow's mergeStateOwed adds
// that reason, and works the round out only for a fact the gate owes no read for.
func MergeStateReadOwed(issue record.Issue, line []phase.Phase, fact MergeStateFact) bool {
	switch fact {
	case MergeStateFactHeadMove:
		return MergeForwardStands(issue, line)
	case MergeStateFactBasePush:
		return phase.MovesBackOn(line, record.StandsAt(issue), phase.Implementing)
	}
	return false
}

// ConflictNeedsMerge is R0's decision on a confirmed conflict (dispatch://INFRA-4374 R0): why the
// head that conflicts with its base must be merged forward now, or "" when nothing needs that yet,
// and the conflict is recorded and nothing else is done. line is the phases the issue's task walks
// (the workflow's Walk.Line: one for each step, then retro and merging). The reasons are read in
// this order, and the first that holds is the answer:
//
//   - record.ConflictMergeNext: the issue stands in merging, or is held from it. The merge is the
//     merger's next step, GitHub merges no head that conflicts with its base, and the merger merges
//     the base forward itself (record.MergeForwardAsker, cybermonklabs/legion#4979).
//   - "": the merge comes next for the issue before merging (MergeComesNext): it stands in retro,
//     or in accepting where no step of its task follows the acceptance, or is held from one of
//     them. Every step of the task has run, so the conflict waits for merging, where the merger
//     resolves it (workflow's mergeForwardOnEntry), rather than going back to the implementer
//     (cybermonklabs/legion#4979). Neither the architect's acceptance nor retro needs a head that
//     merges, so nothing there waits on the merge forward. In accepting where a tester or a
//     reviewer step follows the acceptance, the reasons below decide, as in testing and
//     reviewing: that step still needs a head the base's checks run on.
//   - record.ConflictRulesUnread: the daemon could not read the base's rules (rules nil, or
//     rules.Unread set). It cannot tell whether every check the base requires passed, so it acts
//     at once, which is never later than a daemon that acted on every conflicting read. GitHub's
//     refusal of rulesets to a plan that has none is no failed read: it reads as no rules
//     (record.BaseRules).
//   - record.ConflictRequiresCurrent: a ruleset of the base requires a pull request to be current
//     with it (rules.Strict), so the head cannot merge whatever its checks say until the base is
//     merged into it.
//   - record.ConflictCheckNotGreen: a context the base requires (rules.Required) is not green on
//     the head (green, which is GreenContexts of the pull request). GitHub runs no checks on a
//     head that conflicts with its base, so a required check that has not passed there never
//     will: only a merge forward gives it a head to run on. A check still running reads as not
//     green too, since the record holds a run only once the head's CI settles, and R0 acts at
//     once rather than wait for it.
//
// Otherwise every check the base requires is green on the head and no rule requires the head to
// be current: the step the issue stands at goes on at that head, and the merge forward waits for
// the step that needs it, the issue's move into merging, where record.ConflictMergeNext holds.
//
// It is pure, so the engine, which acts on the answer (workflow's mergeability and mergeForward),
// and any reader of the record decide alike. green is the caller's, so the engine, which logs the
// contexts green on the head when nothing needs a merge yet, reads them once.
func ConflictNeedsMerge(issue record.Issue, line []phase.Phase, rules *record.BaseRules, green []string) record.ConflictWhy {
	switch {
	case record.StandsIn(issue, phase.Merging):
		return record.ConflictMergeNext
	case MergeComesNext(issue, line):
		return ""
	case rules == nil || rules.Unread != "":
		return record.ConflictRulesUnread
	case rules.Strict:
		return record.ConflictRequiresCurrent
	}
	for _, required := range rules.Required {
		if at := sort.SearchStrings(green, required); at == len(green) || green[at] != required {
			return record.ConflictCheckNotGreen
		}
	}
	return ""
}

// MergeComesNext says whether the merge is the next step for issue, whose task walks line (one
// phase for each step, then retro and merging): the issue stands in its retro or its merge, or in
// accepting where no step of the task follows the acceptance, or is held from one of them. In
// the default steps these are accepting, retro and merging. In merging the merger
// merges the base forward itself, and before it the conflict waits for merging
// (ConflictNeedsMerge, cybermonklabs/legion#4979). Where a step of the task follows the
// acceptance, that step runs at the head before the merge, as a tester or a reviewer does after
// the implementer. A line that holds no acceptance has no acceptance a step follows, so it answers
// false in accepting.
func MergeComesNext(issue record.Issue, line []phase.Phase) bool {
	switch record.StandsAt(issue) {
	case phase.Retro, phase.Merging:
		return true
	case phase.Accepting:
		for at, p := range line {
			if p == phase.Accepting {
				return at+1 < len(line) && line[at+1] == phase.Retro
			}
		}
	}
	return false
}

// GreenContexts is every context green on the pull request's current head, each once and sorted:
// the name of each check run GitHub reported on that head that concluded succeeded or skipped
// (HeadRuns, record.RunSucceeded, record.RunSkipped), and, while a settlement stands for the head
// (Settled), the context of each commit status the record keeps for the head
// (record.CommitStatus.StampedWith) whose state is success. A required context is matched by its
// name alone, as a check run's name or a status's context; GitHub's integration id is not
// compared. A status counts only on a settled head, since the record keeps the statuses of its
// checked head, and a status kept for another head says nothing of this one.
//
// A skipped run is green as a succeeded one is, since GitHub's branch protection takes a skipped
// required check as passing: so ConflictNeedsMerge answers for a skipped run exactly as for a
// succeeded one (dispatch://INFRA-4135 R2), and a required check whose run skipped does not by
// itself make the daemon merge the base forward at once (record.ConflictCheckNotGreen).
func GreenContexts(pr record.PullRequest) []string {
	var green []string
	for _, run := range HeadRuns(pr) {
		if run.Conclusion == record.RunSucceeded || run.Conclusion == record.RunSkipped {
			green = append(green, run.Name)
		}
	}
	if Settled(pr) {
		for _, status := range pr.Statuses {
			if status.StampedWith(pr.HeadSHA) && status.State == record.CommitStatusSuccess {
				green = append(green, status.Context)
			}
		}
	}
	sort.Strings(green)
	once := green[:0]
	for _, name := range green {
		if len(once) == 0 || once[len(once)-1] != name {
			once = append(once, name)
		}
	}
	return once
}

// ConflictRound says whether a backward move made now is a round a conflict with the base caused:
// the record holds the pull request's own recorded head in a confirmed conflict
// (record.PullRequest.ConflictingHead, which only the second CONFLICTING read of that head writes,
// workflow's mergeability), whatever fact is making the move. The exemption keys on that mark
// rather than on the cause, because every way back out of a conflicting head is the same round of
// work - a reviewer's request for changes, a tester's fail, a worker's backward move to any
// earlier step of its task - and what the conflict needs is the one forward merge, which the
// implementer makes. That merge moves
// the recorded head, the mark no longer equals it, and the next rejection counts, which is how the
// exemption ends without a write of its own. The same mark is what says a head needs merging
// forward before it can merge at all: an approval of it ends its review round (workflow's
// reviewRound), and a move into accepting, retro or merging at it asks for the merge forward
// (workflow's mergeForwardOnEntry).
func ConflictRound(pr record.PullRequest) bool {
	return pr.ConflictingHead != "" && pr.ConflictingHead == pr.HeadSHA
}

// DesignGateEventKind is the Dispatch artifact observation that changes a registered gate.
type DesignGateEventKind string

const (
	DesignGateApproved         DesignGateEventKind = "approved"
	DesignGateChangesRequested DesignGateEventKind = "changes_requested"
	DesignGateVersion          DesignGateEventKind = "version"
)

// ApplyDesignGateEvent updates a registered gate from one artifact observation, at mark: where
// Dispatch recorded the review, zero for a version event and for a read that gave none. Approval
// opens only when it names the latest known version, and the gate keeps that approval's mark; a
// newer document version and a changes request close it. A newer version keeps the approval it
// closes, with its mark, as the version a human approved below the latest; a changes request
// clears both. An approval also clears the changes request the architect was told of, which
// RecordChangesTold writes when it is told, so the approval and the told request never stand
// together on the gate. A newer version keeps the told record, at the version it was told at, so a
// late live event of that request is still recognised as told. An approval the gate already records
// at that version, read again (record.EventMark.Same), keeps the fuller of the two marks
// (record.EventMark.Merged), so a read that gave only its time never takes the sequence the live
// event gave.
//
// A step that stands open on an ancestor's approval, or waits on an ancestor's request
// (record.DesignGate.Basis), changes as a step a person approved at the version registered
// changes (R3 of dispatch://INFRA-3320): a newer version of the issue's own document, and a
// review of it at the version registered or a later one, end the basis (record.CloseBasis,
// record.EndPendingBasis) before the event applies as it does to any step. So a newer version
// closes the step and falls back to the person's own approval the basis kept, never to the
// version the basis opened; a changes request closes it and clears that approval too; and a
// person's approval opens the step on that approval and leaves no basis behind. A review of an
// earlier version, delivered late or twice, and a version the step already holds change nothing.
func ApplyDesignGateEvent(gate record.DesignGate, kind DesignGateEventKind, version int, mark record.EventMark) record.DesignGate {
	registered := gate.LatestVersion
	if version > gate.LatestVersion {
		gate.LatestVersion = version
	}
	if gate.Basis != nil {
		if version < registered || kind == DesignGateVersion && version == registered {
			return gate
		}
		gate = record.EndPendingBasis(record.CloseBasis(gate))
	}
	switch kind {
	case DesignGateApproved:
		if gate.ApprovedVersion != nil && *gate.ApprovedVersion == version && gate.ApprovedMark.Same(mark) {
			mark = mark.Merged(gate.ApprovedMark)
		}
		approved := version
		gate.ApprovedVersion, gate.ApprovedMark = &approved, mark
		gate = clearChangesTold(gate)
	case DesignGateChangesRequested:
		gate.ApprovedVersion, gate.ApprovedMark = nil, record.EventMark{}
	}
	return gate
}

func clearChangesTold(gate record.DesignGate) record.DesignGate {
	gate.ToldChangesVersion, gate.ToldChangesReason, gate.ToldChangesMark = nil, "", record.EventMark{}
	return gate
}

// LatestReview is the review a person gave of the issue's own document that the gate has applied
// last, and the document version it reviewed: the person's approval it records
// (record.DesignGate.ApprovedByPerson), else the changes request it records as told. The two never
// stand together (ApplyDesignGateEvent), so whichever is set is the gate's newest applied review.
// A step a basis opened records no review of the root's document by its approval: the person's
// approval it kept apart (KeptPerson) is read instead, never the basis's version or its mark,
// which is a place in the ancestor's event log. ok is false when the gate records no review at
// all.
func LatestReview(gate record.DesignGate) (record.EventMark, int, bool) {
	if approval := gate.ApprovedByPerson(); approval != nil {
		return approval.Mark, approval.Version, true
	}
	if gate.ToldChangesVersion != nil {
		return gate.ToldChangesMark, *gate.ToldChangesVersion, true
	}
	return record.EventMark{}, 0, false
}

// StaleReview reports whether a review of version at mark is older than the review the gate has
// already applied at that version (LatestReview): the person reviewed again after it, so this one
// changes no gate state. It covers an approval delivered after the changes request that
// superseded it, and a changes request delivered after a newer approval. A review Dispatch placed
// neither by sequence nor by time against the recorded one is not stale
// (record.EventMark.After), which keeps the earlier behaviour for marks a Dispatch did not give,
// and neither is the recorded review itself (record.EventMark.Same): a read that gave only its
// time and the live event that carries its sequence are one review, and the sequenced one
// completes what the gate records of it. On a step a basis opened, the review it is read against
// is the person's own (KeptPerson) or the told changes request, never the basis.
func StaleReview(gate record.DesignGate, version int, mark record.EventMark) bool {
	recorded, reviewed, ok := LatestReview(gate)
	return ok && reviewed == version && !mark.Same(recorded) && !mark.After(recorded)
}

// ReviewAfter reports whether the review the gate has applied at version was recorded after since.
// A registration whose read of Dispatch began at since is then older than that review: the person
// acted after the read, and the gate keeps what it already applied.
func ReviewAfter(gate record.DesignGate, version int, since record.EventMark) bool {
	recorded, reviewed, ok := LatestReview(gate)
	return ok && reviewed == version && recorded.After(since)
}

// DesignGateChangesTold reports whether the gate already records this changes request as one the
// architect was told of: the same document version, the same reason, and the same review in the
// issue's Dispatch event log (mark, record.EventMark.Same: its event's sequence, else Dispatch's
// time of it). A person who repeats a review's version and wording later has made a new review,
// with an event of its own, and is not this one. A read that carries neither the sequence nor the
// time is matched on version and reason alone.
func DesignGateChangesTold(gate record.DesignGate, version int, reason string, mark record.EventMark) bool {
	return gate.ToldChangesVersion != nil && *gate.ToldChangesVersion == version && gate.ToldChangesReason == reason &&
		gate.ToldChangesMark.Same(mark)
}

// RecordChangesTold records on the gate the changes request the architect is being told of, or has
// been, so a later read of the same review (a registration's or the gate seed's) tells it nothing
// again. A review the gate already records keeps its record, completed with what this read adds of
// its mark (record.EventMark.Merged), so the review's live event and a read that showed only its
// time are one review, recorded with both.
func RecordChangesTold(gate record.DesignGate, version int, reason string, mark record.EventMark) record.DesignGate {
	if DesignGateChangesTold(gate, version, reason, mark) {
		gate.ToldChangesMark = gate.ToldChangesMark.Merged(mark)
		return gate
	}
	told := version
	gate.ToldChangesVersion, gate.ToldChangesReason, gate.ToldChangesMark = &told, reason, mark
	return gate
}

// ApprovalFollows reports whether the gate records a person's approval given after a review of
// version at mark: an approval of a later version, or one of that version the review does not
// come after (record.EventMark.After). A changes request that such an approval followed was
// answered by the person's own approval, so nothing of it is left to tell. The approval read is
// the person's (record.DesignGate.ApprovedByPerson): a basis's version is no answer a person
// gave.
func ApprovalFollows(gate record.DesignGate, version int, mark record.EventMark) bool {
	approval := gate.ApprovedByPerson()
	if approval == nil {
		return false
	}
	return approval.Version > version || approval.Version == version && !mark.After(approval.Mark)
}

// DesignApprovalStands reports whether the approval a news item names, of version to of the
// document artifact, still stands: the gate's row for the item's step holds that document, and its
// approved version is to. That approval is either kind the row records there: a person's, or the
// ancestor's approval a basis opened the step on at the version the root registered
// (record.OpenOnBasis). It is the approval case of the question ApprovalFollows answers for a
// review — is this recorded decision still the word that stands on the version — asked when a task
// that names the decision is sent, not when it was composed. Where the step now records another
// document (a registration that named one since), a later approval has replaced the one the item
// names, a changes request has cleared it (ApplyDesignGateEvent), or a basis that opened it has
// ended (record.CloseBasis), the item would tell the holder of a version that no approval stands
// on any more. A new version that closed the gate since leaves a person's approval standing: the
// gate keeps it as the version a person approved below the latest. It is named for the design gate
// because ApprovalStands already answers the pull request's question of a review.
func DesignApprovalStands(gate record.DesignGate, artifact string, to int) bool {
	return gate.ArtifactID == artifact && gate.ApprovedVersion != nil && *gate.ApprovedVersion == to
}

// DesignGateOpen reports whether an approval stands on the gate's current document version: a
// human's, or the ancestor's approval a basis opened the step on. It answers one step's
// question; the deployment's whole list is DesignGatesOpen. It is record.GateOpen, where the rule
// lives because the record package needs it too and may not read this one, and it keeps its name
// here because every decision in this file is named this way.
func DesignGateOpen(gate record.DesignGate) bool {
	return record.GateOpen(gate)
}

// TreeSteps is the steps one tree is gated on: the steps the deployment gates, and criteria
// after them when the root holds a criteria row and the deployment gates spec but not criteria.
// `gates.design` names the steps every root must register, any step a root registers holds its
// tree, a criteria row exists only by the architect's registration, and a row kept after a switch
// to `gates.design: off` gates nothing. It is record.TreeSteps, where the rule lives because the
// record package needs it too and may not read this one, and it keeps its name here because every
// decision in this file is named this way.
func TreeSteps(configured []record.DesignStep, gates map[record.DesignStep]record.DesignGate) []record.DesignStep {
	return record.TreeSteps(configured, gates)
}

// DesignGatesOpen reports whether gates, every design gate an issue holds by its step, lets the
// tree's work run: no required step (record.TreeGate.Required) holds the work. A required
// step with no row holds the work, as an unregistered gate always has, and since a root always
// requires at least one step, an issue holding no row at all is held too. A row of a step the
// tree is not gated on is read by nothing here: it is kept as the record of a step gated before,
// and holds nothing.
//
// It is record.TreeGate.Open over the same steps and rows, where the rule lives because the
// record package needs it too and may not read this one, and it keeps its name here because
// every decision in this file is named this way. The same answer read the other way round is
// FirstClosedStep finding no closed step.
func DesignGatesOpen(configured []record.DesignStep, gates map[record.DesignStep]record.DesignGate) bool {
	return record.TreeGate{Steps: configured, Rows: gates}.Open()
}

// FirstClosedStep is the first required step (record.TreeGate.Required) whose gate is not
// open, in the configured order with a registered criteria row after the deployment's steps,
// with the row it holds — the zero row when it holds none, which is the step nobody has
// registered. ok is false when every required step is open, which is DesignGatesOpen.
//
// It answers which step holds the work now, and so which step a refusal or a hold is about: a
// step nobody has registered holds it, because nobody has said the document exists. Which step a
// person is being asked about is another question, since nobody can be asked about a document
// the record does not know, and the wait reads its own rule for it (StandingGate).
func FirstClosedStep(configured []record.DesignStep, gates map[record.DesignStep]record.DesignGate) (record.DesignStep, record.DesignGate, bool) {
	for _, step := range (record.TreeGate{Steps: configured, Rows: gates}).Required() {
		if gate, ok := gates[step]; !ok || !DesignGateOpen(gate) {
			return step, gate, true
		}
	}
	return "", record.DesignGate{}, false
}

// StandingGate is the gate a person is being asked about: the row of the first required step
// (record.TreeGate.Required), in the configured order with a registered criteria row after
// the deployment's steps, that is closed and has a row. That is a different question from which
// step holds the tree's work (FirstClosedStep), which a step nobody has registered also holds:
// such a step names no document, so nobody can be asked about it, and the wait it raised would
// be a wait on nothing. So the wait skips it and names the first registered unapproved step after
// it, while the work stays held. ok is false when no required step holds a closed row, which is
// every required step open or every closed one unregistered.
//
// A criteria row comes after the spec, so a root whose spec and Verification document are both
// with the person stands on its spec, the document one approval request names when it asks for
// both (`dispatch request-approval --include-verification`), and on its Verification document
// once the spec is open.
//
// A deployment that gates no step asks no person about anything, so the empty list stands on no
// gate: ok is false whatever rows the root holds. The empty list still holds the tree's work
// until the spec step is registered, which is FirstClosedStep's answer, not this one.
//
// Everything a root's wait is built from reads this one rule: the document its approval requests
// are counted on (workflow's gateRequestArtifact), the wait it records (waitSubject) and the step
// its resume speaks of (gateInQuestion).
func StandingGate(configured []record.DesignStep, gates map[record.DesignStep]record.DesignGate) (record.DesignGate, bool) {
	if len(configured) == 0 {
		return record.DesignGate{}, false
	}
	for _, step := range (record.TreeGate{Steps: configured, Rows: gates}).Required() {
		if gate, ok := gates[step]; ok && !DesignGateOpen(gate) {
			return gate, true
		}
	}
	return record.DesignGate{}, false
}
