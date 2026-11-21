package docs

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A browser's update does not show in its bytes what it does to the document's rendering. Where
// each item lands, and what a deleted format item leaves formatted, depend on what the document
// already holds, and the renderer writes some of what it holds many times over: measured on this
// branch before this check, a 35-byte frame setting an ordered list's start number to 999,999,999
// over a 100,000-line code block grew the rendering by 800,024 bytes, since every line of the
// block is indented by the marker's width; a 7-byte delete of an inline code mark's two format
// items over 560,000 characters of `~a` grew it by 2,799,998, since the characters the code span
// had held literal then each take an escape; a 43-byte frame of table spans grew it by 72,027;
// escapes render up to 3.6 times the bytes of the characters they escape, and a newline typed
// into a code block nested in quotes up to about 540 times. The hook's byte bound weighs what an
// update carries (peerApplyAllowance), so none of those was refused.
//
// So the document bound is held where it can be measured: on what the update leaves, before it
// commits. The room's document carries an apply check (crdt.WithApplyCheck, installed by
// keepReplica through newRenderCheck), which ygo runs under the document's write lock once a
// remote apply has integrated its structs and applied its delete set and before its transaction
// commits, and again on what a drain of the pending queue lands. The check renders the document
// as the apply left it, through the apply's view (crdt.ApplyView), and refuses a browser's apply
// whose rendering would be past the document bound and its creep
// (maxPeerRenderingBytes) and larger than the rendering the apply found; ygo then rolls the whole
// apply back, byte for byte, and fires no observer (crdt.ErrApplyCheck). An apply that leaves the
// rendering no larger is never refused by it, so a document already past the bound can still be
// shortened. A refused drain is discarded and the update that filled its gap stands
// (crdt.ErrApplyCheckDrain, leaveParkedOut), and the browsers whose frames it held are closed
// with the check's close for what the drain would land, DOC_TOO_LARGE for a rendering past the
// bound (closeDiscardedSenders). What it would have landed is gone from the room, and
// never reached its replica: the commit's update leaves it out, and the replica's document refuses
// every drain of its own that lands anything, so it integrates only what each commit's update
// carries (renderedReplica, holdParked).
//
// It judges the hook's own applies - those the hook reserved an allowance for
// (roomApplies.inFlight), which is every update and whole-state message a read-write browser
// sends - and the drain of a server write's apply, and passes the rest: the room's load decodes
// its stored state before the check is set, the update of a catch-up's stored rows and of a
// published live write is weighed by rules of its own (growth.go), so a stored document already
// past the bound loads and is caught up as it was, and every other write the server makes is a
// local transaction, which ygo does not check. What a server write's drain lands is not the
// write's own: it is browser content another apply parked behind the clocks the write supplied.
// So that drain is held to what a browser's drain is held to, and one the check refuses is
// discarded while the write stands, its senders closed as a browser's refused drain closes
// them (check, serverApplyError).
//
// One render pays for the update. The check's rendering is the one the update observer then
// keeps and publishes for the commit (takeChecked), so the observer renders nothing of its own for
// a browser's update, as it renders nothing for a published live write whose operation rendered
// the state it leaves (publishedRendering); the render has moved from after the commit, outside
// the document's lock, to before it, inside. And it follows the observer's own three paths
// (updateChangesMarkdown): from the room's rendering as the apply found it, splicing in just the
// top-level blocks the apply touched (spliceIndexed, through the same pmdoc.Layout.Splice the
// observer's spliceBlocks takes), and whole only where that path would have been whole.
//
// That needs the room's rendering as the apply found it, which the check cannot read from the
// replica: the replica's lock is taken by holders that then take the document's (atPoint), so the
// check, which holds the document's, may not wait for it. So every rendering is placed at a
// commit position - how many of the room's commits since its load the state it renders holds,
// counted by the room's update log (loggedCommits) - and the check takes the rendering that stands
// at the position the apply began at (peerApply.startAt): its own of the last browser's apply it
// passed (roomApplies.checked), or the replica's of the last commit it took
// (roomWeighing.rendered). Either is exact there: no commit lies between the state it renders and
// the apply. Where neither stands there - a server write, a catch-up or ygo's format cleanup
// committed since, and the replica has not taken it yet - the apply is rendered whole, and the
// rendering it is weighed against is the most recent one placed (base says what that admits).
//
// A render that errors or panics refuses that apply alone, DOC_UPDATE_REFUSED: the update leaves
// a document this build cannot render, which the socket path refuses to open at all
// (refuseOutsideSchema) and no editor of this build writes. The next update is weighed afresh,
// against the room as the refusal left it. Where the document rendered nothing before the apply
// either - it left the schema through a write of the server's own while browsers were in it - the
// apply is passed, as the hook has always passed updates into a room it cannot measure, since
// refusing it would lock every editor out and the rendering bounds nothing there.
//
// The check's cost is one render a browser's update, which the observer used to pay outside the
// document's lock and the check now pays inside it, where every other apply to the room waits for
// it (BenchmarkFiveEditorsTyping measures what that does under load).

// maxPeerRenderingBytes is the most a browser's apply may leave the document's rendering at and
// grow it: the document bound and the creep the hook's byte rule lets the editor's own derived
// writes and ordinary typing take past it (maxPeerByteCreep). The byte rule admits an update by
// what it carries and the check by what it leaves, so the two agree on where growth stops.
const maxPeerRenderingBytes = pmdoc.MaxDocumentBytes + maxPeerByteCreep

// roomRendered is the room's rendering as of one of its commits: at, the commit position the
// state it renders stands at (loggedCommits), bytes, the rendering's length, sizeUnmeasured for a
// state that renders no markdown, and rendering itself where the holder keeps it, nil where it
// does not - a room whose load handed the replica no copy publishes the load rendering's size
// alone, so as to hold nothing of it resident (keepReplica). A value is never written in place.
type roomRendered struct {
	at        uint64
	bytes     int64
	rendering *replicaRendering
}

// renderedSize is rendering's length, and sizeUnmeasured for no rendering.
func renderedSize(rendering *replicaRendering) int64 {
	if rendering == nil {
		return sizeUnmeasured
	}
	return int64(len(rendering.markdown))
}

// began places the hook's apply with origin at the commit position the room stands at as it
// begins (peerApply.startAt), which the room's apply check weighs the apply against
// (renderCheck.base). ygo asks for the apply's budget under the document's lock as the apply
// begins, before the apply changes anything, so the room's document asks this there, beside the
// budget (newRoomApplies). An apply of any other origin is placed nowhere for the check. Every
// remote apply's position is kept for its report besides (begunAt), which tells by it whether the
// apply logged a commit (noteApply).
func (a *roomApplies) began(origin any, logged *loggedCommits) {
	at, counted := logged.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.begunAt, a.begunCounted = at, counted
	if running, reserved := a.inFlight[origin]; reserved {
		running.startAt, running.counted = at, counted
	}
}

// checking is the record of the hook's apply with origin that the room's apply check is judging,
// nil for an apply of any other origin, which the check passes, all but the drain of a server
// write's apply (renderCheck.check). The caller holds the document's lock.
func (a *roomApplies) checking(origin any) *peerApply {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.inFlight[origin]
}

// passed records that a check of what running's apply left has passed, with rendering, that
// check's rendering of it, nil for a state that renders nothing: the later of an update's check
// and its drain's is the one that stands, since a drain the check refuses is taken back and leaves
// what the update's own check rendered (noteChecked). The caller holds the document's lock.
func (a *roomApplies) passed(running *peerApply, rendering *replicaRendering) {
	a.mu.Lock()
	defer a.mu.Unlock()
	running.checked, running.rendering = true, rendering
}

// noteChecked keeps, as the room's record of its rendering (checked), the rendering running's
// commit left the room at: the one the room's apply check made of what the apply left, where a
// check passed it, at at, the room's commit position once the commit is logged. An apply no check
// ran for changed nothing the check reads - ygo checks every apply that changes the document -
// so a record standing at the position the apply began at is carried to at unchanged. A position
// that cannot be counted now (counted false) places nothing, and the record keeps the position
// it had. It answers what the commit's report carries (madeReport): the record it made, nil where
// no check rendered the apply or none could be placed. The caller holds the document's lock and
// mu.
func (a *roomApplies) noteChecked(running *peerApply, at uint64, counted bool) *roomRendered {
	held := a.checked.Load()
	switch {
	case !counted:
		return nil
	case !running.checked:
		if held != nil && running.counted && held.at == running.startAt {
			a.checked.Store(&roomRendered{at: at, bytes: held.bytes, rendering: held.rendering})
		}
		return nil
	}
	made := &roomRendered{at: at, bytes: renderedSize(running.rendering), rendering: running.rendering}
	a.checked.Store(made)
	if running.rendering == nil {
		return nil
	}
	return made
}

// forgetChecked drops the room's record of its rendering (checked) where it stands before seq, a
// commit position whose rendering the replica has published (publishRendered): a commit past the
// record has landed, and the replica's rendering says what the room renders now. A record standing
// at seq itself stays: it is of the same state as the replica's and shares its markdown
// (takeChecked keeps the check's string), and it carries the index of the room's own blocks the
// next apply's check splices by (blockIndex), which the replica's rendering, of its own document's
// blocks, cannot.
func (a *roomApplies) forgetChecked(seq uint64) {
	if a == nil {
		return
	}
	if held := a.checked.Load(); held != nil && held.at < seq {
		a.checked.CompareAndSwap(held, nil)
	}
}

// loggedCommits counts a room's commits by its update log: every commit that changes the room's
// document or parks something in it is appended to the log under the document's lock, before any
// observer runs (crdt.UpdateLog), so the count of what the log has handed over and what it still
// holds is the room's commit position, exact under that lock. The replica drains the log and its
// recover resets it, and both go through here (drain, reset), so the count follows every update
// the log hands over; a room no replica drains - a test's - counts by a log of its own, which the
// count drains itself (owned).
type loggedCommits struct {
	mu  sync.Mutex
	log *crdt.UpdateLog
	// owned says the log is the count's own, which nothing else drains.
	owned bool
	// taken is how many updates the log has handed over, through drains and resets.
	taken uint64
	// resetting is set while a reset runs: ygo's reset takes the document's lock and hands over
	// what the log held, so between its start and the count of what it handed over the position
	// cannot be read.
	resetting bool
}

// newLoggedCommits is the count of live's commits by log, the log a replica drains, or by a log
// of its own opened on live where log is nil. Either way it is opened before anything commits on
// the room, so the replica's commit positions (renderedReplica.seq) and the count are one count.
func newLoggedCommits(live *crdt.Doc, log *crdt.UpdateLog) *loggedCommits {
	if log == nil {
		return &loggedCommits{log: live.OpenUpdateLog(), owned: true}
	}
	return &loggedCommits{log: log}
}

// drain is the log's Drain, counted: the replica's drain of the room's commits (drainLog).
func (c *loggedCommits) drain() (base []byte, updates []crdt.LoggedUpdate) {
	c.mu.Lock()
	defer c.mu.Unlock()
	base, updates = c.log.Drain()
	c.taken += uint64(len(updates))
	return base, updates
}

// reset is the log's Reset, counted: the replica's recover (renderedReplica.recover). The reset
// takes the document's lock, which the check holds while it reads the position, so it runs
// outside mu with the position marked unreadable until what it handed over is counted.
func (c *loggedCommits) reset() []crdt.LoggedUpdate {
	c.mu.Lock()
	c.resetting = true
	c.mu.Unlock()
	updates := c.log.Reset()
	c.mu.Lock()
	c.taken += uint64(len(updates))
	c.resetting = false
	c.mu.Unlock()
	return updates
}

// now is the room's commit position, and false while a reset makes it unreadable. It is exact
// only under the document's lock, which no commit is logged without: ygo asks for an apply's
// budget, runs its check and makes its report under that lock (roomApplies.budget, renderCheck,
// roomApplies.noteApply).
func (c *loggedCommits) now() (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resetting {
		return 0, false
	}
	if c.owned {
		_, updates := c.log.Drain()
		c.taken += uint64(len(updates))
		return c.taken, true
	}
	return c.taken + uint64(c.log.Pending()), true
}

// renderCheck is the room's apply check (crdt.WithApplyCheck): what the room's document reads of
// its own rendering before a browser's apply commits, and before the drain of a server write's
// apply commits what it lands (check). fragment is the room document's fragment, resolved before
// any apply, since a view resolves no root (crdt.ApplyView). panicsForTest makes every whole
// render the check makes panic, so a test can hold it to its recovery (check).
type renderCheck struct {
	weighing      *roomWeighing
	fragment      *crdt.YXmlFragment
	paths         *renderPathCounts
	panicsForTest atomic.Bool
}

// newRenderCheck sets live, a room's document, to check every browser's apply and the drain of
// every server write's apply by what it leaves the document rendering (renderCheck.check),
// weighing against what weighing records of the room. Like the room's other apply callbacks it
// is set as the room is listed, before ygo hands the document to anyone, so every remote apply
// from then on is checked.
func newRenderCheck(live *crdt.Doc, weighing *roomWeighing, paths *renderPathCounts) *renderCheck {
	check := &renderCheck{weighing: weighing, fragment: live.GetXmlFragment(fragmentName), paths: paths}
	live.SetApplyCheck(check.check)
	return check
}

// check is ygo's apply check for the room's document: nil for an apply it passes, and a
// *renderRefusal for one it refuses (renderCheck says what it judges). ygo calls it under the
// document's write lock, at most twice on one apply: on the update, before the drain of the
// pending queue, where the update changed the document, and on the drain, where the drain landed
// something an earlier apply parked. view says which (crdt.ApplyView.Drained): false on the
// update's run and true on the drain's, so the one run of an apply whose update changed nothing,
// because the room held all of it or its structs only parked, is the drain's. The check reads the
// document only through view and the reads that take no lock, and calls nothing that takes that
// lock.
//
// A server write's apply (writtenApplyOf) is passed on its update's run and judged on its drain's.
// That drain lands what browsers parked behind the clocks the write supplied: a frame typed after
// another process's edit, which a catch-up brings, or one naming the room's own client at its
// next clock, which only the server writes, in a transaction of the room's own that drains
// nothing, so the next remote apply's drain lands it, whatever that apply's update does. The
// write's update is not rendered here, so what it left is not known, and the drain is rendered
// whole and weighed as an apply whose starting rendering is not known (judge): passed where it
// leaves the rendering within maxPeerRenderingBytes, and refused for now past it
// (renderRefusal.unsure). Its senders are then closed with the transient DOC_BUSY, and each frame
// sent again is weighed as its browser's own apply, its gap now filled. checks counts the runs
// passed, so a drain refused, which lands nothing, is not counted (writtenApply.landedBeyond).
//
// A browser's drain is weighed against what the apply's update left where the update had a run of
// its own (running.checked), which that run rendered. Where it had none, the update changed
// nothing, so the drain's run is the apply's first and nothing committed since the apply began,
// which holds the document's lock from its budget to its check: the drain is weighed against the
// room's rendering as the apply began (base), as an update's run is.
func (c *renderCheck) check(origin any, view *crdt.ApplyView) error {
	if write := writtenApplyOf(origin); write != nil {
		if view.Drained() {
			before, _ := c.weighing.size()
			if _, err := c.judge(view, before, nil, false); err != nil {
				return err
			}
		}
		write.checks.Add(1)
		return nil
	}
	running := c.weighing.applies().checking(origin)
	if running == nil {
		return nil
	}
	base, exact := c.base(running)
	before, from := base.bytes, base.rendering
	if !exact {
		from = nil
	}
	if view.Drained() && running.checked {
		// The update the drain follows stands whatever this answers, so the drain is weighed
		// against what the update left, which that update's check rendered.
		before, exact = renderedSize(running.rendering), true
	}
	after, err := c.judge(view, before, from, exact)
	if err == nil {
		c.weighing.applies().passed(running, after)
	}
	return err
}

// judge renders what the apply the check is running on leaves, from from (render), and answers
// that rendering where the apply passes: one within maxPeerRenderingBytes, or past it and no
// larger than before, the rendering it is weighed against. before is exact where it is what the
// apply's starting state renders; where it is not, an apply leaving the rendering past
// maxPeerRenderingBytes is refused for now (renderRefusal.unsure), since whether it grew the
// rendering is not known. A render that errors refuses the apply, unless before rendered nothing
// either, which passes it with no rendering. A panic in the render is recovered here and refuses
// the apply as a render error does, rather than reaching the peer's read loop.
func (c *renderCheck) judge(view *crdt.ApplyView, before int64, from *replicaRendering, exact bool) (after *replicaRendering, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			after, err = nil, &renderRefusal{before: before, after: sizeUnmeasured, cause: fmt.Errorf("%w: %v", errRenderPanicked, recovered)}
		}
	}()
	after, err = c.render(view, from)
	switch {
	case err != nil && before == sizeUnmeasured:
		return nil, nil
	case err != nil:
		return nil, &renderRefusal{before: before, after: sizeUnmeasured, cause: err}
	}
	size := renderedSize(after)
	switch {
	case size <= maxPeerRenderingBytes:
	case !exact:
		return nil, &renderRefusal{before: before, after: size, unsure: true}
	case size > before:
		return nil, &renderRefusal{before: before, after: size}
	}
	return after, nil
}

// base is the room's rendering as the apply running found it, and whether it is exact: the
// rendering placed at the commit position the apply began at, the check's own of the last
// browser's apply it passed (roomApplies.checked) or the replica's of the last commit it took
// (roomWeighing.rendered), one that holds its rendering first, since only that one can be spliced
// into. Nothing commits between that position and the apply, which holds the document's lock
// from its budget to its check, so the rendering there is the apply's starting state exactly.
//
// Where neither stands there it is the most recent rendering placed, which is not exact: commits
// the check did not render - a server write, a catch-up's stored rows, ygo's format cleanup -
// landed since, and the replica has not taken them yet. The apply is then rendered whole, and
// what it found is not known: a server write may have shortened a document already past the
// bound, and weighed against the size before it, an update growing the shortened document back
// would pass for one that shrank. So there the rule is the safe half of itself: an apply that
// leaves the rendering within maxPeerRenderingBytes is passed, as it would be whatever it found,
// and one that leaves it past is refused for now (renderRefusal.unsure) - including one that
// shortens a document already past the bound, which is the one apply the exact rule would pass
// and this does not, until the replica has taken the commits between and the rendering at the
// apply's start is known again. A room with no rendering placed at all - one whose replica is in
// its recover state before anything was checked - is weighed against the measurement it last
// published (roomWeighing.size), not exact either.
func (c *renderCheck) base(running *peerApply) (*roomRendered, bool) {
	checked, published := c.weighing.applies().checked.Load(), c.weighing.rendered.Load()
	if running.counted {
		var standing *roomRendered
		for _, at := range []*roomRendered{checked, published} {
			if at == nil || at.at != running.startAt {
				continue
			}
			if at.rendering != nil {
				return at, true
			}
			if standing == nil {
				standing = at
			}
		}
		if standing != nil {
			return standing, true
		}
	}
	switch {
	case checked != nil && (published == nil || checked.at >= published.at):
		return checked, false
	case published != nil:
		return published, false
	}
	bytes, _ := c.weighing.size()
	return &roomRendered{bytes: bytes}, false
}

// render is the room's rendering as the apply view reads has left it, from from, the room's
// rendering as the apply found it, nil where that is not known: from itself where the apply
// touched no top-level block and not the fragment (untouched), from with the blocks it touched
// rendered again where it touched some and not the fragment (incremental, spliceIndexed), and a
// whole render otherwise - the update observer's three paths (updateChangesMarkdown), taken on
// the room's own document through the view rather than on the replica. What the apply touched is
// what view names as changed (crdt.ApplyView.ChangedTypes), placed in the fragment's top-level
// blocks as the replica places a transaction's (noteChanged): in a drain's check that is what the
// update and the drain changed together, so from, the state before the update, is spliced with
// both.
//
// A splice costs what the change costs, not what the document does: the touched blocks are found
// in the index the rendering carries of the room's top-level blocks (blockIndex) rather than by a
// walk of the fragment's children, which walks every item the fragment ever held, tombstones
// among them. A rendering with no index - the replica's, which is of another document's elements
// - is spliced after one walk that makes its index, and every rendering the check makes carries
// one from then on.
func (c *renderCheck) render(view *crdt.ApplyView, from *replicaRendering) (*replicaRendering, error) {
	var touched touchedBlocks
	noteChanged(view.ChangedTypes(), c.fragment, &touched)
	if from != nil && !touched.whole {
		if len(touched.blocks) == 0 {
			c.paths.checkUntouched.Add(1)
			return from, nil
		}
		index := from.blocks
		if index == nil {
			index = newBlockIndex(c.fragment.Children())
		}
		read := func(block *crdt.YXmlElement) (*pmdoc.Node, error) { return pmdoc.ReadBlockInView(view, block) }
		if spliced, ok := spliceIndexed(from, index, touched.blocks, read); ok {
			c.paths.checkIncremental.Add(1)
			return spliced, nil
		}
	}
	c.paths.checkRendered.Add(1)
	if c.panicsForTest.Load() {
		panic("the render check's test hook panicked the render")
	}
	tree, err := pmdoc.ReadInView(view, c.fragment)
	if err != nil {
		return nil, documentSchemaError(err)
	}
	markdown, _, layout, err := documentRenderingWithLayout(tree)
	if err != nil {
		return nil, err
	}
	if from != nil && markdown == from.markdown {
		markdown = from.markdown
	}
	return &replicaRendering{markdown: markdown, layout: layout, blocks: newBlockIndex(c.fragment.Children())}, nil
}

// blockIndex is the top-level blocks of the room's live document a rendering the room's apply
// check made is of, in order, and each block's position: what the check splices a later apply's
// touched blocks by (spliceIndexed). It is valid for every state whose fragment holds the same
// blocks in the same order, which is every state a splice is made of: an apply that inserts,
// deletes or moves a top-level block changes the fragment, and the check renders that apply whole,
// which makes a fresh index. It is shared by the renderings that follow one another by splices,
// and never written once made.
type blockIndex struct {
	elements []*crdt.YXmlElement
	at       map[crdt.SharedType]int
}

// newBlockIndex is the index of children, the fragment's children, or nil where one of them is no
// element - a fragment the layout cannot splice either.
func newBlockIndex[N any](children []N) *blockIndex {
	index := &blockIndex{elements: make([]*crdt.YXmlElement, len(children)), at: make(map[crdt.SharedType]int, len(children))}
	for position, child := range children {
		element, isElement := any(child).(*crdt.YXmlElement)
		if !isElement {
			return nil
		}
		index.elements[position] = element
		index.at[element] = position
	}
	return index
}

// spliceIndexed is from with the top-level blocks blocks names rendered again into its markdown
// (pmdoc.Layout.Splice), each found by its position in index, or ok false where the document must
// be rendered whole: an index of another number of blocks than from's layout records, or a touched
// block the index does not hold. Splice checks the rest itself. The rendering it answers carries
// index on, since the blocks are the same.
func spliceIndexed(from *replicaRendering, index *blockIndex, blocks []crdt.SharedType, read func(*crdt.YXmlElement) (*pmdoc.Node, error)) (*replicaRendering, bool) {
	if index == nil || len(index.elements) != from.layout.Blocks() {
		return nil, false
	}
	touched := make([]int, 0, len(blocks))
	for _, block := range blocks {
		position, held := index.at[block]
		if !held {
			return nil, false
		}
		touched = append(touched, position)
	}
	slices.Sort(touched)
	markdown, layout, ok := from.layout.Splice(from.markdown, touched, func(position int) (*pmdoc.Node, error) {
		return read(index.elements[position])
	})
	if !ok {
		return nil, false
	}
	if markdown == from.markdown {
		markdown = from.markdown
	}
	return &replicaRendering{markdown: markdown, layout: layout, blocks: index}, true
}

// errRenderPanicked is why the check refused an apply whose render panicked (renderCheck.check).
var errRenderPanicked = errors.New("rendering the document the update leaves panicked")

// renderRefusal is the room's apply check's refusal of a browser's apply, or of the drain of a
// server write's (renderCheck.check): what it would leave renders after bytes, past
// maxPeerRenderingBytes and more than the before it found, or past it where what it found is not
// known (unsure), or does not render at all where it rendered before, cause saying why. ygo wraps
// it inside the apply's crdt.RefusedUpdateError, beside crdt.ErrApplyCheck, and the hook reads it
// back to close that browser and log why (refusedByApply); a drain's refusal comes back as the
// cause of a crdt.DrainDiscardedError instead, which leaves the update standing and closes the
// browsers whose parked frames the drain held with this refusal's close (closeDiscardedSenders).
type renderRefusal struct {
	before, after int64
	unsure        bool
	cause         error
}

func (r *renderRefusal) Error() string {
	switch {
	case r.cause != nil:
		return fmt.Sprintf("the document the update leaves does not render (%d bytes before it): %v", r.before, r.cause)
	case r.unsure:
		return fmt.Sprintf("the update leaves the document rendering %d bytes, past the %d a browser may grow it to, and what it found is not known yet", r.after, maxPeerRenderingBytes)
	}
	return fmt.Sprintf("the update leaves the document rendering %d bytes, past the %d a browser may grow it to, from %d", r.after, maxPeerRenderingBytes, r.before)
}

func (r *renderRefusal) Unwrap() error { return r.cause }

// close is the close the browser whose apply this refused is given: the document's own
// DOC_TOO_LARGE for a rendering past the bound, which the editor drops the edit for and says so;
// DOC_UPDATE_REFUSED for one that does not render, which the editor drops the edit for as one
// holding content the document does not take; and, for one refused because what it found was not
// known (unsure), the transient DOC_BUSY, which the editor keeps the edit through and sends again
// on its backoff - by then the room's rendering at the commit it starts from is known, and an
// edit that shortens the document is taken. The senders of a server write's drain refused for
// now are closed with it too, and the frame each sends again, its gap filled, is weighed as that
// browser's own apply.
func (r *renderRefusal) close() *websocket.PeerClose {
	switch {
	case r.cause != nil:
		return documentUpdateRefused()
	case r.unsure:
		return documentBusy()
	}
	return documentTooLarge()
}

// why is the refusal in plain words, for the one line it leaves (logRefusal).
func (r *renderRefusal) why() string {
	switch {
	case r.cause != nil:
		return "dispatch: refused a browser's update whose document this build cannot render"
	case r.unsure:
		return "dispatch: refused for now a browser's update whose document would render past its byte bound, against a rendering of the room the replica has not caught up to"
	}
	return "dispatch: refused a browser's update whose document would render past its byte bound"
}
