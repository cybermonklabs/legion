package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// designChangesAttempts is how many attempts of one design_changes row read from Dispatch before
// the reconcile gives up on a read and records why: a pair's change set is recorded unavailable
// with the last error, and a question of the review history is recorded unread with it
// (record.DesignHistoryUnread). A failure Dispatch may stop giving is ridden out on the row's
// backoff, and a run of three, with about three seconds of backoff between the first and the last
// (record.RetryBackoff), is no longer a moment's fault. Recording it is what lets the news go,
// naming why the change list, or the version the holder last took, could not be read, and what
// releases a merger held for it (mergerWaitsForTheDesignGate). An unread history is still asked
// again by every later reconcile, since a read that failed says nothing about the document, and
// nothing is told from it while such a reconcile is pending (readNewsRecorded).
const designChangesAttempts = 3

// toldKey names one told row: an audience of one issue (record.DesignTold).
type toldKey struct {
	issue    string
	audience record.DesignAudience
}

// owedNewsItem is one item of a tree's owed news with the told row it is owed to.
type owedNewsItem struct {
	told toldKey
	item record.NewsItem
}

// owedNewsItems is every item news owes, each with its told row: each member's to its holder row,
// and the architect's to the tree root's architect row.
func owedNewsItems(news record.TreeNews, root string) []owedNewsItem {
	var items []owedNewsItem
	for _, member := range news.Members {
		for _, item := range member.Items {
			items = append(items, owedNewsItem{told: toldKey{member.Issue, record.AudienceHolder}, item: item})
		}
	}
	for _, item := range news.Architect {
		items = append(items, owedNewsItem{told: toldKey{root, record.AudienceArchitect}, item: item})
	}
	return items
}

// designChanges runs one design_changes row: the reconcile of its tree's news of the approved
// design (record.DesignNewsReconcile), whose issue is the tree's root. It does five things, in
// order, each against the record as it stands when that step runs:
//
//  1. In one read transaction, it reads what the tree owes (record.OwedNews over
//     record.TreeNewsFacts).
//  2. For each pair of versions an owed item needs whose change set is not recorded, it reads
//     Dispatch. An item whose told row knows no version of its step first asks Dispatch's review
//     history which version a person approved before its To (dispatch.Client.ApprovedVersionBefore,
//     0 where none was), at every reconcile, whatever an earlier one recorded, and the version it
//     answers is written into the told row itself (step 3), so every later reader names the same
//     pair. A pair from version 0 has no earlier version to compare, and Dispatch's route refuses
//     it, so it is recorded read with no change listed, which the news says as a first approval,
//     and Dispatch is not asked.
//  3. In one write transaction, it records each change set it read (record.Store.PutDesignChanges),
//     each version the review history answered (record.Store.FillTold), clearing what the record
//     held of that history as unread (record.Store.ClearDesignHistoryUnread), and each history it
//     gave up on (record.Store.PutDesignHistoryUnread). A failed read is recorded unavailable or
//     unread, or returned for the row to retry, as giveUpReason answers. The told row of a
//     question recorded unread is left knowing no version. A read that failed says nothing about
//     the document, so it is never written as version 0, which says no version was approved
//     before: the news says the version could not be read, and why (workflow.RenderNews), and the
//     next reconcile asks the history again, nothing being told from the unread row while that
//     one is pending (readNewsRecorded). So every read this row makes ends within its bounded
//     attempts, and a merger held for the news is released then (mergerWaitsForTheDesignGate).
//  4. In one transaction under the fact lock (intake.SerializeFacts), it settles each owed member
//     (settleNews).
//  5. In that same transaction, it settles the tree root's architect row, and queues the root
//     architect's news-only start when that row is owed.
//
// A root the record no longer holds owes nothing, and the row finishes. So does a tree owed
// nothing at the first step, with no lock taken: every write that leaves news owed queues a
// reconcile of its own (record.QueueDesignNews), and this row, running, does not stand in for it
// (record.Store.DesignNewsPending). A tree owed an opening alone (record.OwedNews) names no pair
// of versions, so it reads nothing of Dispatch or of the recorded change sets, and goes straight
// to the settling.
func (r *outbox) designChanges(ctx context.Context, row record.OutboxRow) error {
	if r.dispatch == nil {
		return errors.New("design_changes executor has no Dispatch client")
	}
	var root *record.Issue
	var owed record.TreeNews
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if root, err = r.records.Issue(ctx, tx, row.Issue); err != nil || root == nil {
			return err
		}
		facts, err := record.TreeNewsFacts(ctx, r.records, tx, r.gatedSteps, root.Tree)
		owed = record.OwedNews(facts)
		return err
	}); err != nil {
		return fmt.Errorf("read what the tree of %s owes of its approved design: %w", row.Issue, err)
	}
	if root == nil {
		r.log.Info("outbox design_changes of a tree the record does not hold; finished without acting", "row", row.ID, "issue", row.Issue)
		return nil
	}
	items := owedNewsItems(owed, root.Tree)
	if len(owed.Members) == 0 && !owed.ArchitectOwed() {
		return nil
	}
	baselines, historyErr := r.resolveBaselines(ctx, row, items)
	// A tree that names no pair, such as one owed an opening alone, has no change set to read.
	var recorded map[record.VersionPair]record.DesignChanges
	if len(baselines.pairs) > 0 {
		if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			recorded, err = r.records.DesignChangesOf(ctx, tx, baselines.pairs)
			return err
		}); err != nil {
			return fmt.Errorf("read the recorded change sets of the tree of %s: %w", root.Tree, err)
		}
	}
	read, readErr := r.readChangeSets(ctx, row, baselines.pairs, recorded)
	if len(read) > 0 || len(baselines.fills) > 0 || len(baselines.unread) > 0 {
		at := r.now().UTC()
		if err := store.Transact(ctx, r.pool, func(ctx context.Context, tx pgx.Tx) error {
			for _, changes := range read {
				if err := r.records.PutDesignChanges(ctx, tx, changes); err != nil {
					return err
				}
			}
			for key, versions := range baselines.fills {
				if err := r.records.FillTold(ctx, tx, key.issue, key.audience, versions, at); err != nil {
					return err
				}
			}
			for _, question := range baselines.answered {
				if err := r.records.ClearDesignHistoryUnread(ctx, tx, question); err != nil {
					return err
				}
			}
			for _, unread := range baselines.unread {
				if err := r.records.PutDesignHistoryUnread(ctx, tx, unread); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("record what the reconcile of the tree of %s read from Dispatch: %w", root.Tree, err)
		}
	}
	if err := errors.Join(historyErr, readErr); err != nil {
		return err
	}
	return r.settleNews(ctx, row, root.Tree)
}

// resolvedBaselines is what the second step's first half resolved (resolveBaselines): the version
// each told row is filled with, by row and step (fills); each question of the review history that
// answered (answered), whose unread row, where the record holds one, goes in the same write; each
// question the reconcile gave up on, to be recorded unread (unread); and every pair of versions the
// items name once their versions are known (pairs), each once.
type resolvedBaselines struct {
	fills    map[toldKey]map[record.DesignStep]int
	answered []record.DesignHistoryQuestion
	unread   []record.DesignHistoryUnread
	pairs    []record.VersionPair
}

// resolveBaselines is the second step's first half (designChanges): for each item whose told row
// knows no version of its step, it asks Dispatch's review history which version a person approved
// before its To, once for each document and version, and at every reconcile, whatever an earlier
// one recorded of that question; and it names every pair of versions the items name once those
// are known. A read that answered fills the step, with 0 where no version was approved before. An
// item whose read failed names no pair and fills nothing. A failed read records its question
// unread, or is returned beside what was read, as giveUpReason answers, and a failure is never
// written as a version.
func (r *outbox) resolveBaselines(ctx context.Context, row record.OutboxRow, items []owedNewsItem) (resolvedBaselines, error) {
	type answer struct {
		version int
		err     error
	}
	answers := map[record.DesignHistoryQuestion]answer{}
	resolved := resolvedBaselines{fills: map[toldKey]map[record.DesignStep]int{}}
	at := r.now().UTC()
	var failed error
	for _, owed := range items {
		item := owed.item
		pair, known := item.Pair()
		if !known {
			question := item.Question()
			got, asked := answers[question]
			if !asked {
				got.version, got.err = r.dispatch.ApprovedVersionBefore(ctx, question.ArtifactID, question.To)
				answers[question] = got
				if got.err == nil {
					resolved.answered = append(resolved.answered, question)
				} else if reason, giveUp := r.giveUpReason(row, got.err, "the document's review history"); giveUp {
					resolved.unread = append(resolved.unread, record.DesignHistoryUnread{DesignHistoryQuestion: question, ReadAt: at,
						Reason: reason})
				} else {
					failed = errors.Join(failed, fmt.Errorf("read the version of %s approved before version %d: %w", question.ArtifactID, question.To, got.err))
				}
			}
			if got.err != nil {
				continue
			}
			if resolved.fills[owed.told] == nil {
				resolved.fills[owed.told] = map[record.DesignStep]int{}
			}
			resolved.fills[owed.told][item.Step] = got.version
			pair = record.VersionPair{ArtifactID: item.ArtifactID, From: got.version, To: item.To}
		}
		if !slices.Contains(resolved.pairs, pair) {
			resolved.pairs = append(resolved.pairs, pair)
		}
	}
	return resolved, failed
}

// readChangeSets is the second step's second half (designChanges): the change set of each of
// pairs that recorded does not hold, as it is to be recorded. A pair from version 0, which only a
// told row or a review history that answered names, is recorded read with nothing listed, and
// Dispatch is not asked. A failed read records the pair unavailable, or is returned beside every
// set that was read, as giveUpReason answers.
func (r *outbox) readChangeSets(ctx context.Context, row record.OutboxRow, pairs []record.VersionPair,
	recorded map[record.VersionPair]record.DesignChanges) ([]record.DesignChanges, error) {
	at := r.now().UTC()
	var read []record.DesignChanges
	var failed error
	for _, pair := range pairs {
		if _, known := recorded[pair]; known {
			continue
		}
		if pair.From == 0 {
			read = append(read, record.DesignChanges{VersionPair: pair, State: record.DesignChangesRead, ReadAt: at})
			continue
		}
		answer, err := r.dispatch.ArtifactChanges(ctx, pair.ArtifactID, pair.From, pair.To)
		if err == nil {
			read = append(read, changesFromDispatch(pair, answer, at))
		} else if reason, giveUp := r.giveUpReason(row, err, "it"); giveUp {
			read = append(read, record.DesignChanges{VersionPair: pair, State: record.DesignChangesUnavailable, Reason: reason, ReadAt: at})
		} else {
			failed = errors.Join(failed, fmt.Errorf("read what changed in %s from version %d to %d: %w", pair.ArtifactID, pair.From, pair.To, err))
		}
	}
	return read, failed
}

// designReadErrorInFull is the message of the lines that carry, in parts, the whole error of a
// Dispatch read the reconcile gave up on (giveUpReason, storetext.LogWhole), where the reason the
// record keeps holds only its start.
const designReadErrorInFull = "outbox design_changes: the error of a Dispatch read given up on, in full; the recorded reason keeps its start"

// giveUpReason is the one rule for giving up on a Dispatch read of the design_changes row row
// that failed with err (resolveBaselines, readChangeSets): whether the reconcile gives up, and the
// reason it records, in which what names what was read. A document Dispatch no longer holds
// (dispatch.ErrDocumentNotFound) is given up on at once, with Dispatch's refusal as the reason,
// since a deleted document does not come back with a wait. Any other failure is given up on at the
// row's designChangesAttempts-th attempt, with the count and the last failure as the reason,
// since nothing else ends a read Dispatch keeps failing. A Dispatch build without the changes
// route (dispatch.ErrNoChangesRoute) is one such failure: while Dispatch rolls out, the next
// attempt can reach the build that has it. Before then giveUp is false, and the caller returns the
// failure for the row to retry on its backoff, whose settle logs it.
//
// Dispatch's error is a far side's text, so the reason holds it as the daemon keeps any far side's
// error text (storetext.FarSideError): valid UTF-8 with no NUL, no URL's credentials, and cut to
// its start. The record, the news and every task that carries the item then hold one text, which
// the store keeps as it is written. What the cut leaves out is in the log: the whole error is
// logged here, once for the read given up on (storetext.LogWhole).
func (r *outbox) giveUpReason(row record.OutboxRow, err error, what string) (reason string, giveUp bool) {
	switch {
	case errors.Is(err, dispatch.ErrDocumentNotFound):
		reason = storetext.FarSideError(err.Error())
	case row.Attempts+1 >= designChangesAttempts:
		reason = fmt.Sprintf("Dispatch failed %d reads of %s, the last with: %s", row.Attempts+1, what, storetext.FarSideError(err.Error()))
	default:
		return "", false
	}
	storetext.LogWhole(r.log, designReadErrorInFull, err.Error(), "row", row.ID, "kind", row.Kind, "issue", row.Issue)
	return reason, true
}

// changesFromDispatch is Dispatch's answer for pair as the record keeps it, read at at: whether the
// decision blocks were the whole change, whether nothing changed, and each changed unit and block,
// a run of the text outside every section with the place Dispatch named it at.
func changesFromDispatch(pair record.VersionPair, answer dispatch.ArtifactChanges, at time.Time) record.DesignChanges {
	changes := record.DesignChanges{VersionPair: pair, State: record.DesignChangesRead, DecisionOnly: answer.DecisionOnly,
		Unchanged: answer.Unchanged, ReadAt: at}
	for _, section := range answer.Sections {
		changes.Sections = append(changes.Sections, record.SectionChange{Heading: section.Heading, Occurrence: section.Occurrence,
			Change: section.Change, Place: section.Place, AfterHeading: section.AfterHeading, AfterOccurrence: section.AfterOccurrence})
	}
	for _, block := range answer.Blocks {
		changes.Blocks = append(changes.Blocks, record.BlockChange{ID: block.ID, Type: block.Type, Change: block.Change})
	}
	return changes
}

// newsSettlement is what the reconcile does for one told row owed news (settlementOf).
type newsSettlement int

const (
	// settleLater leaves the news owed: an item the record cannot tell yet, its pair's change set
	// not recorded, or its earlier version neither resolved nor recorded unread with no reconcile of
	// its tree pending (readNewsRecorded). A later approval, a later told version since the first
	// step, or a reconcile queued since causes it, and the row that queued it, or the sweep,
	// settles it then.
	settleLater newsSettlement = iota
	// settleRaise raises the told versions directly and sends nothing: no pair changed anything of
	// the work, each changing decision blocks alone or nothing at all, which tells nobody anything,
	// and no opening is owed. Each pair is raised over as a credit counted from its From
	// (record.Store.RaiseTold), so a raise read off a version an unfinal delivery's credit gave the
	// row reaches nothing once that credit is given back, and the news before it stays owed.
	settleRaise
	// settleSend sends the news, by a news-only start.
	settleSend
)

// settlementOf is what the reconcile does for one told row owed items, given what is recorded:
// settleLater while the record cannot yet tell some item (newsRecorded.change); settleRaise, which
// raises the row past items, when no pair changed anything of the work
// (record.DesignChanges.WorkUnchanged: decision blocks alone, or nothing at all) and the row owes
// no opening; and settleSend otherwise. An item whose review history could not be read is sent,
// never raised: nothing says what changed since the version its holder took. A pair that changed
// no work, owed with the opening, is sent, not raised: the gate closed on that version, the holder
// was told it closed, and a person approved it later, so the holder is owed the opening as a task,
// with the words that nothing its issue holds changed and no change list (workflow.RenderNews),
// since no role message carried that opening.
func settlementOf(items []record.NewsItem, recorded newsRecorded, openingOwed bool) newsSettlement {
	workUnchanged := true
	for _, item := range items {
		change, told := recorded.change(item)
		if !told {
			return settleLater
		}
		workUnchanged = workUnchanged && change.HistoryUnread == nil && change.Changes.WorkUnchanged()
	}
	if workUnchanged && !openingOwed {
		return settleRaise
	}
	return settleSend
}

// newsAsks is what of the record a set of owed items is told from, each once: the pair of versions
// each item names, and the question of the review history each item whose told row knows no
// version of its step asks (record.NewsItem.Question), with each tree whose item asks it.
type newsAsks struct {
	pairs     []record.VersionPair
	questions map[record.DesignHistoryQuestion][]string
}

// add adds what each of items, items of tree, is told from.
func (a *newsAsks) add(tree string, items ...record.NewsItem) {
	for _, item := range items {
		if pair, known := item.Pair(); known {
			if !slices.Contains(a.pairs, pair) {
				a.pairs = append(a.pairs, pair)
			}
			continue
		}
		if a.questions == nil {
			a.questions = map[record.DesignHistoryQuestion][]string{}
		}
		if question := item.Question(); !slices.Contains(a.questions[question], tree) {
			a.questions[question] = append(a.questions[question], tree)
		}
	}
}

// newsRecorded is what the record holds that owed news is told from: the change set of each pair
// recorded (record.Store.DesignChangesOf), and each question of the review history recorded unread
// (record.Store.DesignHistoryUnreadOf) that still stands (readNewsRecorded).
type newsRecorded struct {
	changes map[record.VersionPair]record.DesignChanges
	unread  map[record.DesignHistoryQuestion]record.DesignHistoryUnread
}

// readNewsRecorded is what the record holds of asks, read in the caller's transaction, at the
// moment the clock now gives.
//
// A question's unread row stands only while no reconcile of a tree that asks it is pending
// (record.Store.DesignNewsPending: queued, backing off or retrying, under no live lease). A pending
// reconcile asks the history again, and what it reads replaces the row: a fill of the told row with
// the row cleared, or, on its last attempt, a fresh unread row. Telling the row before then would
// keep the failure it records, since the confirmation of a task that tells it credits the told
// version past the item: so until that reconcile has run, the item is not recorded, the reconcile
// leaves it owed, a merger waits for it, the summary names it by nothing, and a start attaches
// nothing of it. The rule is for what is built anew: an item a delivery already carries with its
// history unread is told by what it carries (carriedChange), so a re-send never loses it. The
// reconcile that runs holds a live lease on its own row, so the row it records on its last attempt
// stands for its own settle. Only a set that asks a question reads the clock and whether a
// reconcile is pending.
func readNewsRecorded(ctx context.Context, tx pgx.Tx, records record.Store, asks newsAsks, now func() time.Time) (newsRecorded, error) {
	changes, err := records.DesignChangesOf(ctx, tx, asks.pairs)
	if err != nil {
		return newsRecorded{}, err
	}
	var at time.Time
	var pending map[string]bool
	var standing []record.DesignHistoryQuestion
	for question, trees := range asks.questions {
		stands := true
		for _, tree := range trees {
			held, read := pending[tree]
			if !read {
				if at.IsZero() {
					at = now().UTC()
				}
				if held, err = records.DesignNewsPending(ctx, tx, tree, at); err != nil {
					return newsRecorded{}, err
				}
				if pending == nil {
					pending = map[string]bool{}
				}
				pending[tree] = held
			}
			stands = stands && !held
		}
		if stands {
			standing = append(standing, question)
		}
	}
	unread, err := records.DesignHistoryUnreadOf(ctx, tx, standing)
	if err != nil {
		return newsRecorded{}, err
	}
	return newsRecorded{changes: changes, unread: unread}, nil
}

// change is what news tells of item, and whether the record can tell it yet: the change set of
// its pair where its told row knows the version it took, and, where the row knows none, the review
// history that could not say which (workflow.NewsChange.HistoryUnread). An item whose change set
// is not recorded, or whose row knows no version and whose history has no unread row that stands
// (readNewsRecorded), cannot be told yet: the reconcile has not read it, or reads its history
// again.
func (n newsRecorded) change(item record.NewsItem) (workflow.NewsChange, bool) {
	if pair, known := item.Pair(); known {
		changes, recorded := n.changes[pair]
		return workflow.NewsChange{Step: item.Step, Changes: changes}, recorded
	}
	unread, recorded := n.unread[item.Question()]
	if !recorded {
		return workflow.NewsChange{}, false
	}
	return workflow.NewsChange{Step: item.Step, HistoryUnread: &unread}, true
}

// settleNews is the reconcile's fourth and fifth steps (designChanges): in one transaction under
// the fact lock, it reads again what tree owes, the change sets recorded and the review histories
// recorded unread that stand (readNewsRecorded), and settles each told row owed news (settlementOf):
//
//   - a row whose every pair changed decision blocks alone or nothing at all, and that owes no
//     opening, has its versions raised directly (record.Store.RaiseTold), and nothing is sent;
//   - otherwise a member's news goes to the holder of its phase by a news-only start
//     (record.SuperviseRequest.News), queued as a row of its own on the member's issue, unless one
//     is queued for that claim already, or the claim holds a task its worker has not taken yet
//     (holdsUntakenTask), or the claim is one a news-only start may not reach
//     (recordedClaimTakesNews): none, behind the entry its phases row names — owed its phase start,
//     which carries the news — retired, failed, or one a suspension that holds (the operator's or
//     the controller's) is over, each of which the start that brings its agent back tells;
//   - a member in accepting is held by the architect that owns it (newsClaims.holderOf, the owner
//     walk over the architect claims the record holds: its own architect while that claim holds its
//     role, else the nearest one above it whose claim does, else the tree root's), and its news
//     goes to that architect's claim, in the holder form, by that claim's one news-only start;
//   - a member no claim can be named for (newsClaims.holderOf) is logged and told at its next
//     start;
//   - the tree root's architect row is settled the same way, and its news goes by the root
//     architect's news-only start, which also carries the holder form of each member in accepting
//     the root architect owns;
//   - a row that owes the opening alone, while the gate stands open (record.OwedNews: the prompt
//     whose turn confirmed its task was rendered before the opening became owed, or carried an
//     earlier opening that a close and a reopening replaced), is sent the same way: its task leads
//     with the opening and says nothing more.
//
// A tree that lingers, a member out of the workflow, one that merged, or one whose phase has no
// holder is owed nothing (record.OwedNews), so nothing is queued for it. Each start reads again,
// when it runs, what it carries and whether it may reach its claim (the start executor's
// newsForStart and newsOnlyRefusal), since the record can move before then. The lines that say
// what the settlement queued and raised are hooks of its commit (store.OnCommit), so a settlement
// that rolls back says nothing, and one whose commit reply is lost says it once the store says it
// committed.
func (r *outbox) settleNews(ctx context.Context, row record.OutboxRow, tree string) error {
	now := r.now().UTC()
	if err := r.settleUnderFacts(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var queued []owedHolder
		raised := 0
		root, err := r.records.Issue(ctx, tx, tree)
		if err != nil || root == nil {
			return err
		}
		facts, err := record.TreeNewsFacts(ctx, r.records, tx, r.gatedSteps, root.Tree)
		if err != nil {
			return err
		}
		owed := record.OwedNews(facts)
		var asks newsAsks
		for _, item := range owedNewsItems(owed, root.Tree) {
			asks.add(root.Tree, item.item)
		}
		recorded, err := readNewsRecorded(ctx, tx, r.records, asks, r.now)
		if err != nil {
			return err
		}
		// sendTo is each told row whose news goes by a news-only start, in the order the rows are
		// settled. A row raised or left for later queues no start, so its holder is never read.
		var sendTo []toldKey
		for _, news := range owed.Members {
			switch settlementOf(news.Items, recorded, record.ToldRow(facts.Told, news.Issue, record.AudienceHolder).OpeningOwed) {
			case settleLater:
				continue
			case settleRaise:
				if err := r.records.RaiseTold(ctx, tx, news.Issue, record.AudienceHolder, news.Items, now); err != nil {
					return err
				}
				raised++
				continue
			}
			sendTo = append(sendTo, toldKey{news.Issue, record.AudienceHolder})
		}
		if owed.ArchitectOwed() {
			switch settlementOf(owed.Architect, recorded, record.ToldRow(facts.Told, root.Key, record.AudienceArchitect).OpeningOwed) {
			case settleRaise:
				if err := r.records.RaiseTold(ctx, tx, root.Key, record.AudienceArchitect, owed.Architect, now); err != nil {
					return err
				}
				raised++
			case settleSend:
				sendTo = append(sendTo, toldKey{root.Key, record.AudienceArchitect})
			}
		}
		holders, err := readOwedHolders(ctx, tx, r.records, newNewsClaims(*root, &facts), sendTo)
		if err != nil {
			return err
		}
		var starts []owedHolder
		owners := map[claim.Token]owedHolder{}
		for _, key := range sendTo {
			holder, held := holders[key]
			switch {
			case !held:
				r.log.Warn("outbox design_changes cannot name the claim that holds a told row owed news; it is told at its next start",
					"row", row.ID, "issue", key.issue, "audience", key.audience)
			case holder.role == claim.RoleArchitect:
				owners[holder.token] = holder
			default:
				starts = append(starts, holder)
			}
		}
		for _, owner := range slices.Sorted(maps.Keys(owners)) {
			starts = append(starts, owners[owner])
		}
		for _, start := range starts {
			if newsStartQueued(start.run) || holdsUntakenTask(start.run.Claim) || !recordedClaimTakesNews(start.run.Claim, start.entry) {
				continue
			}
			request := record.SuperviseRequest{Op: "start", Tree: start.issue.Tree, Role: start.role, Generation: start.issue.Generation, News: true}
			if start.role != claim.RoleArchitect {
				request.Phase = start.issue.Phase
				rows, err := r.records.Phases(ctx, tx, start.issue.Key)
				if err != nil {
					return err
				}
				if request.Branch, err = workflow.StartBranch(ctx, tx, r.records, start.issue, request, rows); err != nil {
					return err
				}
			}
			queue, err := record.NewOutboxRow(start.issue.Key, request, now)
			if err != nil {
				return err
			}
			if err := r.records.Enqueue(ctx, tx, queue); err != nil {
				return err
			}
			queued = append(queued, start)
		}
		store.OnCommit(ctx, func() {
			for _, start := range queued {
				r.log.Info("outbox design_changes queued a news-only start", "row", row.ID, "tree", tree, "issue", start.issue.Key, "role", start.role)
			}
			if raised > 0 || len(queued) > 0 {
				r.log.Info("outbox design_changes settled the news its tree is owed", "row", row.ID, "tree", tree, "raised", raised, "starts", len(queued))
			}
		})
		return nil
	}); err != nil {
		return fmt.Errorf("settle the news the tree of %s is owed: %w", tree, err)
	}
	return nil
}

// startNews is what a start carries of news of the approved design (newsForStart): the task it
// delivers — the start's own text, a blank line, then the news — the news itself as the delivery
// carries it (record.DeliveryNews, encoded; nil where nothing is attached), whether the news says
// anything a task of its own is worth (newsAtSend.says), and whether the task ends with the
// daemon's two sentences of a reviewer's review round (roundNamed, which the delivery carries as
// supervise.RequestDeliver.RoundNamed).
type startNews struct {
	task       string
	news       []byte
	says       bool
	roundNamed bool
}

// newsForStart is the news a start of role on issue carries, read from the record in one read
// transaction when the start runs, with no Dispatch read: the items record.OwedNews owes the told
// rows the claim speaks for (owedToClaim), and the opening where its row owes it and the standing
// gate is open, rendered by the one render every task of news goes through (renderNews), so the
// news the start carries is exactly what its task names. An item the record cannot tell yet — its
// change set not recorded, or, where its told row knows no version, that version neither read from
// the review history nor recorded unread with no reconcile of its tree pending (readNewsRecorded)
// — is left out and stays owed, for the reconcile or a later start to carry. An item it attaches
// with its review history unread carries that history's reason and time
// (record.DeliveryNewsItem.HistoryUnread), so every later send of the task tells it as attached,
// whatever a reconcile does before then (carriedChange). The task is the start's own text
// (record.DeliveryNews.Task), a blank line, then the news; a start that carries nothing delivers
// its own text.
//
// A start of the reviewer queued for news alone carries no task, so the workflow's queue named
// no round on it. Where it carries news, its task is named here, in the same read: the daemon's
// two sentences of the round, read from the phase the issue stands in and the reviewer's row
// (workflow.ReviewerTask), which the news goes before and which the delivery then carries as
// named, so the claim's send adds no second pair (supervise's Delivery.message). A record that
// holds no reviewed commit or no entry's task names `none` for it, as the queue does.
func (r *outbox) newsForStart(ctx context.Context, issue record.Issue, payload record.SuperviseRequest, token claim.Token) (startNews, error) {
	carried := record.DeliveryNews{Task: payload.Task}
	sent := renderedNews{task: payload.Task}
	roundNamed := payload.RoundNamed
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		facts, err := record.TreeNewsFacts(ctx, r.records, tx, r.gatedSteps, issue.Tree)
		if err != nil {
			return err
		}
		if carried.Items, carried.Openings, err = r.owedToClaim(ctx, tx, facts, issue, payload.Role, token); err != nil {
			return err
		}
		if carried.Empty() {
			return nil
		}
		if payload.News && payload.Role == claim.RoleReviewer && !roundNamed {
			if carried.Task, err = workflow.ReviewerTask(ctx, tx, r.records, issue.Key, carried.Task); err != nil {
				return err
			}
			roundNamed = true
		}
		sent, err = renderNews(ctx, tx, r.records, r.gatedSteps, token, carried, nil, roundNamed, r.now)
		return err
	}); err != nil {
		return startNews{}, fmt.Errorf("read the news the start of %s's %s carries: %w", issue.Key, payload.Role, err)
	}
	return startNews{task: sent.task, news: sent.news, says: sent.says, roundNamed: roundNamed}, nil
}

// owedToClaim is what facts owe the claim token, of role on issue, speaks for, as a delivery
// carries it: each told row owed news whose holder is that claim (newsClaims.holderOf, and
// rootArchitect for the tree root's architect row). For a phase worker that is its member's holder
// row while the role is the one the member's phase names; for an architect, its own summary when
// issue is the tree root and its row is owed, and the holder form of each member in accepting it
// owns. Which architect owns such a member is the owner walk over the machine's live view
// (withLiveView, the outbox's holdsItsRole), as the outbox routes a notice of the member, so a
// start carries a member's news to no architect a decision in flight is stopping. Each told row it
// speaks for carries the opening too, where the row owes it and the standing gate is open. An
// architect is never handed a worker's news, and a worker never an architect's. Where the tree's
// root is not among its issues, nothing is carried, since every holder is named over the root's
// record (newsClaims).
func (r *outbox) owedToClaim(ctx context.Context, tx pgx.Tx, facts record.NewsFacts, issue record.Issue, role claim.Role,
	token claim.Token) ([]record.DeliveryNewsItem, []record.DeliveryOpening, error) {
	owed := record.OwedNews(facts)
	open := record.TreeGate{Steps: facts.Steps, Rows: facts.Gates}.Open()
	var items []record.DeliveryNewsItem
	var openings []record.DeliveryOpening
	tell := func(key string, audience record.DesignAudience, news []record.NewsItem) {
		for _, item := range news {
			items = append(items, record.DeliveryNewsItem{Issue: key, Audience: audience, Step: item.Step, Artifact: item.ArtifactID,
				From: item.From, To: item.To})
		}
		if told := record.ToldRow(facts.Told, key, audience); open && told.OpeningOwed {
			openings = append(openings, record.DeliveryOpening{Issue: key, Audience: audience, Seq: told.Opening})
		}
	}
	rootAt := slices.IndexFunc(facts.Members, func(member record.Issue) bool { return claim.IsTreeRoot(member.Key, member.Tree) })
	if rootAt < 0 {
		return nil, nil, nil
	}
	root := facts.Members[rootAt]
	claims := newNewsClaims(root, &facts).withLiveView(r.holdsItsRole)
	if holder, held := claims.rootArchitect(); held && holder.token == token && owed.ArchitectOwed() {
		tell(root.Key, record.AudienceArchitect, owed.Architect)
	}
	for _, news := range owed.Members {
		// A worker's claim holds its own issue's phase alone, so no other member's holder is read
		// for it.
		if role != claim.RoleArchitect && news.Issue != issue.Key {
			continue
		}
		member, err := claims.member(ctx, tx, r.records, news.Issue)
		if err != nil {
			return nil, nil, err
		}
		holder, held, err := claims.holderOf(ctx, tx, r.records, member)
		if err != nil {
			return nil, nil, err
		}
		if held && holder.token == token {
			tell(news.Issue, record.AudienceHolder, news.Items)
		}
	}
	return items, openings, nil
}

// mergerNewsUnrecorded says whether issue's holder is owed news of the approved design that no
// start can carry yet: the reconcile would leave it owed (settlementOf's settleLater), since an
// owed item's change set is not recorded, read or unavailable, or its earlier version is unknown to
// the record and its review history has no unread row that stands: none is recorded, or a
// reconcile of the tree is pending, which reads that history again (readNewsRecorded). A merger
// released without it could merge before it reads what changed, or be told a failure the pending
// reconcile would replace, so its start waits (mergerWaitsForTheDesignGate). The wait ends within
// the design_changes row's bounded attempts, since a pair Dispatch cannot read is recorded
// unavailable, and a review history it cannot read is recorded unread (designChangesAttempts).
func (r *outbox) mergerNewsUnrecorded(ctx context.Context, issue record.Issue) (bool, error) {
	var unrecorded bool
	if err := store.TransactWith(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		facts, err := record.TreeNewsFacts(ctx, r.records, tx, r.gatedSteps, issue.Tree)
		if err != nil {
			return err
		}
		if member, owes := record.OwedNews(facts).Member(issue.Key); owes {
			var asks newsAsks
			asks.add(issue.Tree, member.Items...)
			recorded, err := readNewsRecorded(ctx, tx, r.records, asks, r.now)
			if err != nil {
				return err
			}
			unrecorded = settlementOf(member.Items, recorded, false) == settleLater
		}
		return nil
	}); err != nil {
		return false, fmt.Errorf("read whether the merger of %s is owed news no start can carry yet: %w", issue.Key, err)
	}
	return unrecorded, nil
}

// queueOwedDesignNews is the sweep's backstop for news of the approved design, run on the entry
// sweep's tick (tickEntrySweep): every tree of the project with a gate (record.Store.GatedIssues)
// that record.OwedNews owes anything gets a design_changes row, unless one is queued and not yet
// running (record.QueueDesignNews). It catches what nothing else queued a row for: a fact the
// daemon missed while it was down, an item a send dropped, a restart's task a claim kept rather
// than took a new one (the outbox's keepsStartTask), and a told row that moved under a running
// reconcile. What each tree owes is read in one read transaction outside the fact lock; the rows
// are queued in one transaction that takes it, as every other writer of the row does.
//
// While the project is paused it queues nothing, as the entry sweep's gate answers no then
// (admit's OwesEntryWork, decision 11 of dispatch://INFRA-3759). The pause holds every start a
// reconcile queues (record's pauseHolds), so a tree owed news stays owed until the resume, and
// the backstop would queue a reconcile of each such tree at every tick of the pause, each of
// which asks Dispatch again what an earlier one could not read, to send nothing. A reconcile a
// fact queues still runs while the project is paused: it records what changed, and its news-only
// starts wait with the other held rows. The first tick after the resume asks again. The pause is
// read in the same read transaction (store.OpenOrLastPauseTx), the table and the key the
// outbox's own hold reads (record's outboxPauseFree), as admit's readStallFloor reads it.
func (w *workflowRuntime) queueOwedDesignNews(ctx context.Context) error {
	var owed []string
	if err := store.TransactWith(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		owed = nil
		pause, found, err := store.OpenOrLastPauseTx(ctx, tx, w.dispatchProject)
		if err != nil || (found && !pause.Ended()) {
			return err
		}
		roots, err := w.records.GatedIssues(ctx, tx, w.dispatchProject)
		if err != nil {
			return err
		}
		for _, root := range roots {
			facts, err := record.TreeNewsFacts(ctx, w.records, tx, w.gatedSteps, root)
			if err != nil {
				return err
			}
			if news := record.OwedNews(facts); len(news.Members) > 0 || news.ArchitectOwed() {
				owed = append(owed, root)
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("read which trees of %s are owed news of their approved design: %w", w.dispatchProject, err)
	}
	if len(owed) == 0 {
		return nil
	}
	return w.queueDesignNews(ctx, owed)
}

// queueDesignNews queues the reconcile of each of trees' news (record.QueueDesignNews), in one
// transaction serialized with every fact (intake.SerializeFacts), which every writer of that row
// takes so that none queues a second beside one still to run. The lanes are woken as it commits.
func (w *workflowRuntime) queueDesignNews(ctx context.Context, trees []string) error {
	now := time.Now().UTC()
	if err := store.Transact(ctx, w.pool, func(ctx context.Context, tx pgx.Tx) error {
		if err := intake.SerializeFacts(ctx, tx); err != nil {
			return fmt.Errorf("serialize the queue with every fact: %w", err)
		}
		for _, tree := range trees {
			if err := record.QueueDesignNews(ctx, w.records, tx, tree, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("queue the reconcile of the news of %v: %w", trees, err)
	}
	return nil
}
