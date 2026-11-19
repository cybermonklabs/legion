package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// designNews answers supervise's Deps.DesignNews: what a task that carries news of the approved
// design says now, at the moment it is about to be sent. The start that queued the task read the
// record when it ran, and the task can wait behind a turn, a busy agent or a provider's error long
// enough for the record to move: a later version can close the gate the news says opened, a later
// approval or a changes request can leave an item naming a version that is no longer the person's
// word, and another delivery's confirmation can tell it first. A task composed from the record
// then and sent as it was would be the copy dispatch://INFRA-3136 forbids, so the text is rendered
// here again, from the record as one read transaction finds it, by the one render
// (workflow.RenderNews):
//
//   - the gate sentence is said of the tree root's gate rows as they stand now, so a task composed
//     while the gate stood open and sent after a later version closed it says closed;
//   - an item stands while the gate's row of its step still holds the document it names, approved
//     at its To (classify.DesignApprovalStands), and the told row of its audience still holds a
//     version below To or none it knows; every other item is dropped from the text and from the
//     news the delivery carries, so its confirmation credits only what was told;
//   - an item or an opening told to the holder audience of a member stands only while the claim
//     the task is held for holds that member's phase now (newsClaims.holderOf): the claim of the
//     role its phase names, or, for a member in accepting, the architect that owns it. The
//     confirmation credits the member's holder row, which records what the session holding its
//     phase took. So an architect's task does not tell a member that its architect signed off
//     while the task waited: the member's next holder is told it;
//   - an opening stands while the told row of its audience still owes it and the standing gate is
//     open, under the opening number that row holds now (record.DesignTold.Opening), which the
//     task carries with it (record.DeliveryOpening.Seq). A row that owes the opening while the gate
//     stands open is told it by every task that speaks to that row, whether or not the start
//     carried it. The gate can close and open again while a task waits, and the opening then
//     finds the holder owed news, so no role message tells it (workflow's openingDelivery), and
//     the reconcile queues no news-only start beside the task the holder has not taken
//     (holdsUntakenTask). The task is then the one delivery of that opening, and the credit of the
//     prompt that carried it tells it, and no earlier or later one. A task that carried an
//     opening and is sent after a later version closed the gate again says the gate is closed, and
//     carries the opening marked closed (record.DeliveryOpening.Closed), which clears nothing;
//   - an item is re-keyed to the version its audience's told row holds of its step now, whenever
//     the item carries another (rekeyed): the row can have come to know a version, another
//     delivery's credit can have told part of the news, or a credit the item was rendered against
//     can have been given back, and the pair the send names is the pair the audience is owed now;
//   - every item and opening it keeps is marked with the run its issue stands in now (inRun): the
//     issue's generation and its tree root's. The credit of the prompt that carries it counts only
//     while the record still holds that run (the SQL function design_news_current), so what a
//     prompt sent before a clear of a generation told counts nothing for the run after it, and a
//     send after the clear tells the next run what that run is owed;
//   - an item that names its pair (From) is told with that pair's change set, and is dropped as well
//     where none is recorded yet: nothing else can say what changed, and the item stays owed for a
//     later start, or the reconcile, to carry;
//   - an item with no earlier version (From nil, its told row knowing none) is decided on what the
//     delivery carries, never on whether the record still holds its history unread
//     (carriedChange). Where the start attached it with its review history unread
//     (record.DeliveryNewsItem.HistoryUnread), it is told as that history, with the reason it was
//     attached with, never as a first approval: a reconcile that has run or is pending since
//     changes nothing a start already attached, so a re-send never loses it. An item carrying
//     neither is read as a start reads the record (readNewsRecorded) and takes the unread history
//     on;
//   - the root architect's summary of a tree that lingers is dropped whole: linger holds every
//     member where it stood, record.OwedNews owes such a tree nothing, and nothing goes to it.
//
// The task text is the start's own text (record.DeliveryNews.Task), a blank line, and the news,
// rendered by the one render every task of news goes through (renderNews). A phase start whose
// news is all dropped is sent as its own text alone, carrying no news; a task queued for its news
// alone (supervise.Delivery.NewsOnly) with nothing left to say — no item standing, and no opening
// owed while the gate stands open — answers "", which the machine retires rather than sends. A task
// whose news all stands as it is carried is answered with the bytes it carries, so a send of a task
// the record has not moved under writes nothing; one whose items the send dropped, re-keyed or told
// by another state than they carry, or whose openings it dropped, took on, numbered anew or said
// closed, is answered with its news encoded again, so any later send reads what this send told, and
// so does the copy this prompt's acknowledgement records (supervise.Delivery.Carried), which is
// what the confirmation of this prompt's turn credits.
//
// An item dropped from the text and the delivery queues one design_changes row for its tree
// (record.QueueDesignNews) where a reconcile has something to send it: the item no longer stands,
// the record could not tell it yet, or its holder is another claim now, and the reconcile weighs
// again what that tree's holders are owed and records what changed. An item whose issue or tree
// the record does not hold, and one to the architect of a lingering tree, queue none, since no
// reconcile can send them anything (readNewsAtSend).
//
// It is the machine that asks, because the machine owns the pending delivery: it holds its own
// lock and asks this in a short read-only transaction of its own, as rootHears is asked. A send
// that dropped an item then queues the reconcile in a second transaction, serialized with every
// fact (intake.SerializeFacts), as rootTask's write is, since every writer of that row takes the
// fact lock so that none queues a second beside one still to run; it takes no lock of the daemon's.
func (w *workflowRuntime) designNews(ctx context.Context, c supervise.Claim, d supervise.Delivery) (string, []byte, error) {
	if len(d.News) == 0 {
		return d.Task, d.News, nil
	}
	var carried record.DeliveryNews
	if err := json.Unmarshal(d.News, &carried); err != nil {
		return "", nil, fmt.Errorf("decode the news the task held for %s carries: %w", c.Token, err)
	}
	var sent renderedNews
	if err := pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		sent, err = renderNews(ctx, tx, w.records, w.gatedSteps, c.Token, carried, d.News, d.RoundNamed, time.Now)
		return err
	}); err != nil {
		return "", nil, fmt.Errorf("read what the news the task held for %s carries says now: %w", c.Token, err)
	}
	if len(sent.dropped) > 0 {
		if err := w.queueDesignNews(ctx, sent.dropped); err != nil {
			return "", nil, fmt.Errorf("queue the reconcile of the news the task held for %s dropped: %w", c.Token, err)
		}
	}
	if d.NewsOnly && !sent.says {
		return "", nil, nil
	}
	return sent.task, sent.news, nil
}

// renderedNews is a task of news as the one render says it (renderNews): its text, the news the
// delivery carries with it (nil for none), whether any of that news is worth a task of its own
// (newsAtSend.says), and the trees whose reconcile the caller queues (newsAtSend.dropped).
type renderedNews struct {
	task    string
	news    []byte
	says    bool
	dropped []string
}

// renderNews is the one render of a task that carries news of the approved design, and every
// builder of such a task goes through it: a start's task and a start queued for news alone
// (newsForStart), each send of a held task (designNews), and the narrowing of a root architect's
// held task (rootTask, through designNews). So the news a delivery carries is computed from the
// text it sends, by the function that renders that text, and nothing keeps a news field beside a
// text it did not render. It reads carried as the record stands in the caller's transaction, for
// the claim whose token is reader (readNewsAtSend), and answers the text — carried's own Task, a
// blank line, then the news — and exactly the news that text names: none where nothing stands;
// encoded, the bytes carried was decoded from, where it keeps carried as it was; and carried
// encoded again otherwise. A start has no bytes to keep, and passes nil. A narrowing passes the
// news it encoded again under the narrowed text (narrowedNews, through designNews), so news it
// keeps as it was is answered as those bytes, under that text.
//
// A reviewer's task that ends with the daemon's two sentences of its review round says so by a
// fact (roundNamed: record.SuperviseRequest.RoundNamed at a start, supervise.Delivery.RoundNamed at
// a send), and those two stay the last two of the text the reviewer is sent, which is where its
// prompt reads them: the text is carried's Task cut before them (reviewround.Split), a blank line,
// the news, and then the two sentences. A task that does not end with them as Sentences writes
// them is rendered whole, as any other.
func renderNews(ctx context.Context, tx pgx.Tx, records record.Store, steps []record.DesignStep, reader claim.Token,
	carried record.DeliveryNews, encoded []byte, roundNamed bool, now func() time.Time) (renderedNews, error) {
	sent, err := readNewsAtSend(ctx, tx, records, steps, reader, carried, now)
	if err != nil {
		return renderedNews{}, err
	}
	body, round := carried.Task, ""
	if roundNamed {
		if cut, sentences, named := reviewround.Split(carried.Task); named {
			body, round = cut, sentences
		}
	}
	rendered := renderedNews{task: paragraphs(body, sent.text) + round, says: sent.says, dropped: sent.dropped}
	switch {
	case sent.kept.Empty():
	case encoded != nil && !sent.rewritten && len(sent.kept.Items) == len(carried.Items) &&
		len(sent.kept.Openings) == len(carried.Openings):
		rendered.news = encoded
	default:
		if rendered.news, err = json.Marshal(sent.kept); err != nil {
			return renderedNews{}, fmt.Errorf("encode the news %s's task carries: %w", reader, err)
		}
	}
	return rendered, nil
}

// newsAtSend is what a delivery's news says at its send: the text that follows the start's own
// (text), what of the news still stands (kept, which keeps the start's own text), whether any of
// it is worth a task of its own (says): an item that stands, or an opening owed while the gate
// stands open, whether it keeps the news otherwise than it was carried (rewritten: an item
// re-keyed to the version its told row holds now, marked with another run (inRun), or with its
// unread review history taken on or let go, carriedChange, or an opening the delivery did not
// carry, one it carried and no longer tells, one it carries under another opening number or
// another run, or one it now says is closed), and the tree of each item it dropped that a
// reconcile can still send (dropped), each once, whose reconcile the send queues: not the tree of
// an item dropped because the record does not hold its issue or its tree, or because it is to the
// architect of a lingering tree (readNewsAtSend).
type newsAtSend struct {
	text      string
	kept      record.DeliveryNews
	says      bool
	rewritten bool
	dropped   []string
}

// newsListener is one audience of one issue a delivery speaks to: the items it tells that
// audience, by their place in the delivery's news, and the opening it carries it, nil for none.
type newsListener struct {
	issue    string
	audience record.DesignAudience
	items    []int
	opening  *record.DeliveryOpening
}

// listenersOf is each audience of each issue news speaks to, in the order the news first names it.
func listenersOf(news record.DeliveryNews) []*newsListener {
	var listeners []*newsListener
	find := func(issue string, audience record.DesignAudience) *newsListener {
		for _, listener := range listeners {
			if listener.issue == issue && listener.audience == audience {
				return listener
			}
		}
		listener := &newsListener{issue: issue, audience: audience}
		listeners = append(listeners, listener)
		return listener
	}
	for i, item := range news.Items {
		listener := find(item.Issue, item.Audience)
		listener.items = append(listener.items, i)
	}
	for _, opening := range news.Openings {
		find(opening.Issue, opening.Audience).opening = &opening
	}
	return listeners
}

// newsTree is what the send reads of one tree: its root's record, the root's gate rows, whether
// the standing gate is open (record.TreeGate.Open), the told rows of its issues, and what names the
// holder of each member's phase (newsClaims), which reads the tree's architect claims only the
// first time the owner of a member in accepting is asked.
type newsTree struct {
	root   record.Issue
	gates  map[record.DesignStep]record.DesignGate
	open   bool
	told   []record.DesignTold
	claims *newsClaims
}

// newsListened is one listener as the record stands at the send: its issue's record and tree, its
// told row, the items of it that stand, the opening the send says to it (nil for none), and, for
// the tree root's architect, the members its summary names.
type newsListened struct {
	listener *newsListener
	issue    record.Issue
	tree     *newsTree
	told     record.DesignTold
	items    []int
	opening  *record.DeliveryOpening
	members  []memberDraft
}

// readNewsAtSend reads, in the caller's one transaction, what carried says as the record stands at
// the moment the clock now gives, read for the claim whose token is reader, and renders it
// (designNews). An item, an opening or a listener whose issue the record does not hold is dropped:
// nothing can say what the gate of a tree the record does not know stands at. So is the architect
// of a tree that lingers, items, opening and summary alike. A holder listener whose member's phase
// reader does not hold now (newsClaims.holderOf) is dropped too, items and opening: its told row is
// what the session holding that phase took, and that session is another claim now, whose own task
// tells it. An item dropped as an item of such a listener, as one that no longer stands, or as one
// the record cannot tell yet names its tree among the dropped, for the reconcile its caller queues.
// An item dropped because the record does not hold its issue or its tree, or because it is to the
// architect of a lingering tree, names none, since no reconcile has anything to send it. An item
// that stands is decided on what the delivery carries (rekeyed, carriedChange); the summary's
// members are read as a start reads the record (memberDraft.told). The gate's state is said to a
// listener as openingAtSend decides.
func readNewsAtSend(ctx context.Context, tx pgx.Tx, records record.Store, steps []record.DesignStep, reader claim.Token,
	carried record.DeliveryNews, now func() time.Time) (newsAtSend, error) {
	trees := map[string]*newsTree{}
	var listened []newsListened
	var asks newsAsks
	// items is each carried item as this send reads it: re-keyed against its told row, then as the
	// delivery goes on carrying it once told (carriedChange).
	items := slices.Clone(carried.Items)
	for _, listener := range listenersOf(carried) {
		issue, err := records.Issue(ctx, tx, listener.issue)
		if err != nil {
			return newsAtSend{}, err
		}
		if issue == nil {
			continue
		}
		tree, err := readNewsTree(ctx, tx, records, steps, trees, issue.Tree)
		if err != nil {
			return newsAtSend{}, err
		}
		if tree == nil {
			continue
		}
		if listener.audience == record.AudienceArchitect && tree.root.Lingers() {
			continue
		}
		heard := newsListened{listener: listener, issue: *issue, tree: tree,
			told: record.ToldRow(tree.told, listener.issue, listener.audience)}
		if listener.audience == record.AudienceHolder {
			holder, held, err := tree.claims.holderOf(ctx, tx, records, *issue)
			if err != nil {
				return newsAtSend{}, err
			}
			if !held || holder.token != reader {
				listened = append(listened, heard)
				continue
			}
		}
		for _, i := range listener.items {
			item := carried.Items[i]
			if !itemStands(item, tree.gates[item.Step], heard.told) {
				continue
			}
			items[i] = inRun(rekeyed(item, heard.told), issue.Generation, tree.root.Generation)
			heard.items = append(heard.items, i)
			// An item told by the unread history it carries asks the record nothing of it.
			if items[i].From != nil || items[i].HistoryUnread == nil {
				asks.add(issue.Tree, items[i].NewsItem())
			}
		}
		heard.opening = heard.openingAtSend()
		if listener.audience == record.AudienceArchitect && claim.IsTreeRoot(issue.Key, issue.Tree) {
			if heard.members, err = readTreeMembers(ctx, tx, records, steps, tree.claims); err != nil {
				return newsAtSend{}, err
			}
			for _, member := range heard.members {
				asks.add(issue.Tree, member.owed...)
			}
		}
		listened = append(listened, heard)
	}
	recorded, err := readNewsRecorded(ctx, tx, records, asks, now)
	if err != nil {
		return newsAtSend{}, err
	}
	sent := newsAtSend{kept: record.DeliveryNews{Task: carried.Task}}
	keep := map[int]bool{}
	var texts []string
	for _, heard := range listened {
		news := workflow.News{Audience: heard.listener.audience, Tree: heard.issue.Tree, Issue: heard.issue.Key,
			Phase: heard.issue.Phase, Steps: steps, Gates: heard.tree.gates, OpeningOwed: heard.opening != nil}
		for _, i := range heard.items {
			change, kept, ok := recorded.carriedChange(items[i])
			if !ok {
				continue
			}
			items[i] = kept
			keep[i] = true
			news.Items = append(news.Items, change)
		}
		for _, member := range heard.members {
			news.Members = append(news.Members, member.told(recorded))
		}
		if heard.opening != nil {
			sent.kept.Openings = append(sent.kept.Openings, *heard.opening)
		}
		sent.rewritten = sent.rewritten || !sameOpening(heard.opening, heard.listener.opening)
		if len(news.Items) == 0 && heard.opening == nil {
			continue
		}
		sent.says = sent.says || len(news.Items) > 0 || (heard.opening != nil && !heard.opening.Closed)
		texts = append(texts, workflow.RenderNews(news))
	}
	for i, item := range items {
		if keep[i] {
			sent.kept.Items = append(sent.kept.Items, item)
			was := carried.Items[i]
			sent.rewritten = sent.rewritten || item.From != was.From || item.HistoryUnread != was.HistoryUnread ||
				item.Generation != was.Generation || item.TreeGeneration != was.TreeGeneration
		}
	}
	for _, heard := range listened {
		for _, i := range heard.listener.items {
			if !keep[i] && !slices.Contains(sent.dropped, heard.issue.Tree) {
				sent.dropped = append(sent.dropped, heard.issue.Tree)
			}
		}
	}
	sent.text = paragraphs(texts...)
	return sent, nil
}

// openingAtSend is the gate's state a send says to heard's listener, whose told row is heard.told,
// while the standing gate of heard's tree is open or not, as the opening the send carries (nil for
// none), marked with the run of heard's issue and of its tree's root (inRun):
//
//   - while the gate stands open and the row owes the opening, the opening, under the number the
//     row holds for it now (record.DesignTold.Opening), whether or not the delivery carried it: the
//     gate can close and open again while the task waits, and the opening that the reopening owes
//     is a new one, which only a prompt carrying its number tells (the claim store's credit of a
//     delivery counts no other, and design_news_opening_told clears no other);
//   - while the gate stands closed and the delivery carried an opening, the close: a task composed
//     while the gate stood open and sent after a later version closed it says closed (RenderNews),
//     and carries the opening marked closed, which the confirmation clears nothing for;
//   - otherwise nothing: the row owes no opening, so the opened role message or a confirmation
//     told it, or the gate is closed and the delivery carried none.
func (heard *newsListened) openingAtSend() *record.DeliveryOpening {
	listener, open := heard.listener, heard.tree.open
	generation, treeGeneration := heard.issue.Generation, heard.tree.root.Generation
	switch {
	case open && heard.told.OpeningOwed:
		return &record.DeliveryOpening{Issue: listener.issue, Audience: listener.audience, Seq: heard.told.Opening,
			Generation: generation, TreeGeneration: treeGeneration}
	case !open && listener.opening != nil:
		closed := *listener.opening
		closed.Closed, closed.Generation, closed.TreeGeneration = true, generation, treeGeneration
		return &closed
	}
	return nil
}

// sameOpening says whether two openings a delivery carries, each nil for none, are the same: the
// same audience told the same opening, opened or closed, so a send that says it as carried writes
// the news back unchanged.
func sameOpening(a, b *record.DeliveryOpening) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// readNewsTree is the tree whose root is root, gated on steps as the deployment names them, read
// once for every listener of it; nil when the record holds no such root.
func readNewsTree(ctx context.Context, tx pgx.Tx, records record.Store, steps []record.DesignStep, trees map[string]*newsTree,
	root string) (*newsTree, error) {
	if tree, read := trees[root]; read {
		return tree, nil
	}
	issue, err := records.Issue(ctx, tx, root)
	if err != nil || issue == nil {
		trees[root] = nil
		return nil, err
	}
	gates, err := records.GatesOf(ctx, tx, root)
	if err != nil {
		return nil, err
	}
	told, err := records.TreeTold(ctx, tx, root)
	if err != nil {
		return nil, err
	}
	trees[root] = &newsTree{root: *issue, gates: gates, open: record.TreeGate{Steps: steps, Rows: gates}.Open(), told: told,
		claims: newNewsClaims(*issue, nil)}
	return trees[root], nil
}

// itemStands says whether an item a delivery carries is still news its audience is owed: the gate
// row of its step holds the document it names, approved at the version it tells
// (classify.DesignApprovalStands), and the audience's told row holds a version of that step below
// it, or none it knows.
func itemStands(item record.DeliveryNewsItem, gate record.DesignGate, told record.DesignTold) bool {
	if !classify.DesignApprovalStands(gate, item.Artifact, item.To) {
		return false
	}
	took, known := told.Took(item.Step)
	return !known || took < item.To
}

// rekeyed is a carried item as this send reads it against its audience's told row now: its earlier
// version (From) is the version the row holds of its step, whenever the two differ, so the send
// names the pair from what the audience took now. The row can have come to know a version since
// the item was rendered (a reconcile filled it from the review history), or another delivery's
// credit since can have told part of the news, or a credit the item was rendered against can have
// been given back, which leaves the row below the From it carried: told as carried, that item would
// leave the news of the pair before it untold, and its credit, counted from a version the row does
// not hold, would reach nothing (the SQL function design_news_known). An item whose re-keyed pair
// has no change set recorded yet is dropped and its tree's reconcile queued (carriedChange,
// readNewsAtSend). A row that knows no version of the step leaves the item with none, which is
// told as its unread review history says, or dropped until that history is recorded. An item whose
// From is already the row's is returned as carried, the same value under the same pointer, which is
// what says the send kept it unchanged.
func rekeyed(item record.DeliveryNewsItem, told record.DesignTold) record.DeliveryNewsItem {
	took, known := told.Took(item.Step)
	switch {
	case known && (item.From == nil || *item.From != took):
		item.From = &took
	case !known && item.From != nil:
		item.From = nil
	}
	return item
}

// inRun is item marked with the run the send renders it in: generation, the generation of the
// item's issue, and treeGeneration, that of the root of the tree the issue stands in
// (record.DeliveryNewsItem.Generation and TreeGeneration), as the transaction reads them. The
// credit of the prompt that carries it counts only while the record still holds that run (the SQL
// function design_news_current), so a send after a clear of a generation marks the item with the
// next run, and tells that run what it is owed now.
func inRun(item record.DeliveryNewsItem, generation, treeGeneration uint64) record.DeliveryNewsItem {
	item.Generation, item.TreeGeneration = generation, treeGeneration
	return item
}

// carriedChange is what a send tells of item, a carried item as re-keyed against its told row
// (rekeyed), the item as the delivery goes on carrying it, and whether it stands. An item naming a
// pair whose change set is recorded is told with it, and lets go any unread history it carried.
// Otherwise an item carrying an unread review history is told by that history, as the start
// attached it, whatever the record has done since: readNewsRecorded's rule that a pending
// reconcile holds an unread row back is for what a start attaches, never for what a delivery
// already carries, whose re-send must not lose it. An item carrying neither is read as a start
// reads the record (change): one naming a pair not recorded does not stand, and one naming none
// stands only where its history stands unread, which it then takes on.
func (n newsRecorded) carriedChange(item record.DeliveryNewsItem) (workflow.NewsChange, record.DeliveryNewsItem, bool) {
	if pair, known := item.NewsItem().Pair(); known {
		if changes, recorded := n.changes[pair]; recorded {
			item.HistoryUnread = nil
			return workflow.NewsChange{Step: item.Step, Changes: changes}, item, true
		}
	}
	if unread := item.Unread(); unread != nil {
		return workflow.NewsChange{Step: item.Step, HistoryUnread: unread}, item, true
	}
	change, told := n.change(item.NewsItem())
	if told && change.HistoryUnread != nil {
		item.HistoryUnread = &record.DeliveryHistoryUnread{Reason: change.HistoryUnread.Reason, ReadAt: change.HistoryUnread.ReadAt}
	}
	return change, item, told
}

// memberDraft is one member of a tree as the root architect's summary reads it before the change
// sets are read: its record, how it is told (memberTelling: its state, and the issue whose
// architect holds it in accepting where that is not the tree root's), and, for a member owed news,
// the items its holder is owed now (record.OwedNews) and whether its holder row owes the opening.
type memberDraft struct {
	memberTelling
	issue       record.Issue
	owed        []record.NewsItem
	openingOwed bool
}

// told is the member as the summary names it, given what is recorded: its owed items with their
// change sets, or with the review history that could not say which version its holder took, or
// told nothing, for that reason, where the reconcile raises what its holder took and sends nothing
// (settlementOf's settleRaise): every item it is owed is recorded as changing nothing of the work
// (record.DesignChanges.WorkUnchanged: decision blocks alone, or nothing at all), and its holder
// row owes no opening. A member whose row owes the opening is sent a task with it, so it is named
// with its items and how its holder is told. An owed item the record cannot tell yet
// (newsRecorded.change) is named by none.
func (m memberDraft) told(recorded newsRecorded) workflow.NewsMember {
	member := workflow.NewsMember{Issue: m.issue.Key, Phase: m.issue.Phase, State: m.state, Architect: m.architect}
	if len(m.owed) > 0 && settlementOf(m.owed, recorded, m.openingOwed) == settleRaise {
		member.State = workflow.NewsWorkUnchanged
		return member
	}
	for _, item := range m.owed {
		if change, ok := recorded.change(item); ok {
			member.Items = append(member.Items, change)
		}
	}
	return member
}

// readTreeMembers is every member of the tree whose holders claims names, the root first, as its
// root architect's summary names it: what OwedNews owes each now, read from the facts that rule
// reads (record.TreeNewsFacts), and what each is told. claims is the send's own for the tree
// (newsTree.claims), so a holder item of the same send and this read share one read of the tree's
// architect claims, and each member is read through it (newsClaims.member).
//
// A member owed news is told by a task now (workflow.NewsSentNow) when its holder's claim was
// given its phase's current entry, runs and holds no task its worker has not taken yet — the claim
// the reconcile queues a news-only start for (holderTold). A claim that a news-only start reaches
// but that holds such a task gets no start until a turn takes that task, so the member's news
// waits for it (workflow.NewsWaitsForTask). Otherwise the next start of its phase tells it
// (workflow.NewsAtNextStart): a claim owed its phase start (workflow.OwedEntry), none at all, or
// one retired, failed or suspended by an operator. A member in accepting is answered by the
// architect that owns it (newsClaims.holderOf, the owner walk): the root architect itself
// (workflow.NewsYouAnswer), or a child's architect, whose claim is told as any holder's is
// (holderTold) and which the summary names as the one told (memberTelling.architect). Who holds
// each member is the one answer the reconcile and the start read too (readOwedHolders).
//
// A member owed nothing is told nothing, for the reason OwedNews gives (record.TreeNews.Untold),
// as the summary says it (untoldReason). A member that reason says OwedNews holds back
// (record.UntoldHeldBack) is one whose news the standing gate holds, closed again before the
// summary was sent: OwedNews owes no member anything while it is closed, so no task carries any
// member's news and no start will, and each such member is told nothing yet, for that reason
// (workflow.NewsGateClosed), rather than said to be told now or at its next start. The summary of
// a lingering tree is never sent.
func readTreeMembers(ctx context.Context, tx pgx.Tx, records record.Store, steps []record.DesignStep, claims *newsClaims) ([]memberDraft, error) {
	facts, err := record.TreeNewsFacts(ctx, records, tx, steps, claims.root.Key)
	if err != nil {
		return nil, err
	}
	// The facts read every member of the tree in this transaction, so the claims that name each
	// member's holder take them from here rather than read the tree's issues a second time.
	claims.seed(&facts)
	members := facts.Members
	slices.SortStableFunc(members, func(a, b record.Issue) int {
		return boolOrder(claim.IsTreeRoot(b.Key, b.Tree)) - boolOrder(claim.IsTreeRoot(a.Key, a.Tree))
	})
	owed := record.OwedNews(facts)
	told, err := readMemberClaims(ctx, tx, records, claims, owed)
	if err != nil {
		return nil, err
	}
	drafts := make([]memberDraft, 0, len(members))
	for _, member := range members {
		draft := memberDraft{issue: member, openingOwed: record.ToldRow(facts.Told, member.Key, record.AudienceHolder).OpeningOwed}
		if news, owes := owed.Member(member.Key); owes {
			draft.owed, draft.memberTelling = news.Items, told[member.Key]
		} else {
			draft.state = untoldReason(owed.Untold[member.Key])
		}
		drafts = append(drafts, draft)
	}
	return drafts, nil
}

// memberTelling is how one member owed news is told (readMemberClaims): the state the summary
// names, and, for a member in accepting that an architect other than the tree root's holds, the
// key of the issue whose architect that is (architect), which the summary names as the one told.
// Every other member has none.
type memberTelling struct {
	state     workflow.NewsState
	architect string
}

// readMemberClaims is how each member owed news is told, by its key, from the claim that holds its
// phase as the reconcile names it (readOwedHolders): a member the root architect holds, in
// accepting, is answered by the root architect itself, and any other holder's claim is told as the
// reconcile treats it (holderTold), a child's architect that owns a member in accepting included,
// which is named with it. A member whose holder cannot be named is told at its next start
// (newsClaims.holderOf).
func readMemberClaims(ctx context.Context, tx pgx.Tx, records record.Store, claims *newsClaims, owed record.TreeNews) (map[string]memberTelling, error) {
	keys := make([]toldKey, 0, len(owed.Members))
	for _, news := range owed.Members {
		keys = append(keys, toldKey{news.Issue, record.AudienceHolder})
	}
	holders, err := readOwedHolders(ctx, tx, records, claims, keys)
	if err != nil {
		return nil, err
	}
	rootArchitect, _ := claims.rootArchitect()
	told := make(map[string]memberTelling, len(owed.Members))
	for _, news := range owed.Members {
		holder, held := holders[toldKey{news.Issue, record.AudienceHolder}]
		switch {
		case !held:
			told[news.Issue] = memberTelling{state: workflow.NewsAtNextStart}
		case holder.token == rootArchitect.token:
			told[news.Issue] = memberTelling{state: workflow.NewsYouAnswer}
		case holder.role == claim.RoleArchitect:
			told[news.Issue] = memberTelling{state: holderTold(holder.run, holder.entry), architect: holder.issue.Key}
		default:
			told[news.Issue] = memberTelling{state: holderTold(holder.run, holder.entry)}
		}
	}
	return told, nil
}

// untoldReason is how the root architect's summary says why OwedNews owes a member nothing
// (record.UntoldReason, decided there once; readTreeMembers). A member OwedNews holds back is one
// whose news the standing gate holds, closed again (workflow.NewsGateClosed): a lingering tree, the
// rule's other hold, gets no summary at all (readNewsAtSend).
func untoldReason(reason record.UntoldReason) workflow.NewsState {
	switch reason {
	case record.UntoldTaken:
		return workflow.NewsAlreadyTaken
	case record.UntoldNotConcerned:
		return workflow.NewsNotConcerned
	case record.UntoldMerged:
		return workflow.NewsMerged
	case record.UntoldDone:
		return workflow.NewsDone
	case record.UntoldOutOfWorkflow:
		return workflow.NewsOutOfWorkflow
	case record.UntoldAdmitted:
		return workflow.NewsAdmitted
	case record.UntoldAwaitingPerson:
		return workflow.NewsAwaitingPerson
	case record.UntoldHeld:
		return workflow.NewsStopped
	}
	return workflow.NewsGateClosed
}

// boolOrder is 1 for true and 0 for false, to order by a predicate.
func boolOrder(b bool) int {
	if b {
		return 1
	}
	return 0
}

// paragraphs is each non-empty text in turn, a blank line between each two: the start's own text
// and the news after it, or the news of each audience a delivery speaks to.
func paragraphs(texts ...string) string {
	said := make([]string, 0, len(texts))
	for _, text := range texts {
		if text != "" {
			said = append(said, text)
		}
	}
	return strings.Join(said, "\n\n")
}
