package replay

// design_news.go is the replay's run of the daemon's news of the approved design: the reconcile a
// design_changes row runs (internal/daemon's design_news.go), with the replay's copy of Dispatch
// (the shadow) in Dispatch's place, over the daemon's own rule for which claim holds a told row's
// news (record.NewsClaims, record.ReadOwedHolders), and what a start reads of that news
// (internal/daemon's startNewsOf).

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// designChangesUnreadable is the reason the stub records the change set of a pair from a version
// above 0 under, unavailable (record.DesignChangesUnavailable), as the daemon records a pair
// Dispatch could not answer: the shadow holds each document's versions and verdicts, never its
// text, so it cannot say what changed between two versions.
const designChangesUnreadable = "the replay's copy of Dispatch holds no document's text, so it cannot say what changed between two of its versions"

// designChangesRow runs a design_changes row as the daemon's executor does (internal/daemon's
// designChanges), the shadow answering what the executor reads of Dispatch. It reads what the
// row's tree owes (record.OwedNews). For each item whose told row knows no version of its step, it
// reads from the shadow the version a person approved before the item's To (shadow.approvedBefore,
// Dispatch's review history), and writes that version into the told row (record.Store.FillTold),
// so the item names its pair. It records the change set of each pair the record does not hold: read
// with nothing listed for a pair from version 0, which the executor records so without asking
// Dispatch, and unavailable for every other pair (designChangesUnreadable). Last, it settles each
// told row owed news (settleNews). A root the record no longer holds, and a tree owed nothing,
// finish with nothing written.
//
// The shadow answers every question of a review history, so the stub records none unread
// (record.DesignHistoryUnread), and an item is one the record can tell once its pair's change set
// is recorded. A merger's start its news holds (mergerNewsUnrecorded) waits on its backoff, as in
// the daemon, and starts at its first try after this row, which waits on no read of Dispatch.
func (d *driver) designChangesRow(row record.OutboxRow) error {
	var root *record.Issue
	var owed record.TreeNews
	if err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if root, err = d.records.Issue(ctx, tx, row.Issue); err != nil || root == nil {
			return err
		}
		facts, err := record.TreeNewsFacts(ctx, d.records, tx, d.window.GateSteps, root.Tree)
		owed = record.OwedNews(facts)
		return err
	}); err != nil {
		return fmt.Errorf("read what the tree of %s owes of its approved design: %w", row.Issue, err)
	}
	if root == nil || len(owed.Members) == 0 && !owed.ArchitectOwed() {
		return d.finish(row)
	}
	var filled []record.ToldKey
	fills := map[record.ToldKey]map[record.DesignStep]int{}
	var pairs []record.VersionPair
	for _, owedItem := range record.OwedNewsItems(owed, root.Tree) {
		item := owedItem.Item
		pair, known := item.Pair()
		if !known {
			if fills[owedItem.Told] == nil {
				fills[owedItem.Told] = map[record.DesignStep]int{}
				filled = append(filled, owedItem.Told)
			}
			from := d.shadow.approvedBefore(item.ArtifactID, item.To)
			fills[owedItem.Told][item.Step] = from
			pair = record.VersionPair{ArtifactID: item.ArtifactID, From: from, To: item.To}
		}
		if !slices.Contains(pairs, pair) {
			pairs = append(pairs, pair)
		}
	}
	at := d.now().UTC()
	if err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		recorded, err := d.records.DesignChangesOf(ctx, tx, pairs)
		if err != nil {
			return err
		}
		for _, pair := range pairs {
			if _, known := recorded[pair]; known {
				continue
			}
			changes := record.DesignChanges{VersionPair: pair, State: record.DesignChangesRead, ReadAt: at}
			if pair.From != 0 {
				changes.State, changes.Reason = record.DesignChangesUnavailable, designChangesUnreadable
			}
			if err := d.records.PutDesignChanges(ctx, tx, changes); err != nil {
				return err
			}
		}
		for _, key := range filled {
			if err := d.records.FillTold(ctx, tx, key.Issue, key.Audience, fills[key], at); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("record what the reconcile of the tree of %s read from the replay's copy of Dispatch: %w", root.Tree, err)
	}
	if err := d.settleNews(row, root.Tree); err != nil {
		return err
	}
	return d.finish(row)
}

// newsSettlement is what the reconcile does for one told row owed news (settlementOf).
type newsSettlement int

const (
	// settleLater leaves the news owed: an item the record cannot tell yet.
	settleLater newsSettlement = iota
	// settleRaise raises the told versions directly and sends nothing: no pair changed anything of
	// the work, and no opening is owed.
	settleRaise
	// settleSend sends the news, by a news-only start.
	settleSend
)

// settlementOf is what the reconcile does for one told row owed items (internal/daemon's
// settlementOf), given the change sets recorded: settleLater while an item names no pair whose
// change set is recorded; settleRaise when every pair changed nothing of the work
// (record.DesignChanges.WorkUnchanged) and the row owes no opening; settleSend otherwise. The stub
// records no review history unread, so an item whose told row knows no version is one the record
// cannot tell yet, as the daemon reads such an item with no unread row.
func settlementOf(items []record.NewsItem, changes map[record.VersionPair]record.DesignChanges, openingOwed bool) newsSettlement {
	workUnchanged := true
	for _, item := range items {
		pair, known := item.Pair()
		recorded, told := changes[pair]
		if !known || !told {
			return settleLater
		}
		workUnchanged = workUnchanged && recorded.WorkUnchanged()
	}
	if workUnchanged && !openingOwed {
		return settleRaise
	}
	return settleSend
}

// changesOf is the recorded change set of each pair items name, read in tx
// (record.Store.DesignChangesOf).
func (d *driver) changesOf(tx pgx.Tx, items []record.NewsItem) (map[record.VersionPair]record.DesignChanges, error) {
	var pairs []record.VersionPair
	for _, item := range items {
		if pair, known := item.Pair(); known && !slices.Contains(pairs, pair) {
			pairs = append(pairs, pair)
		}
	}
	return d.records.DesignChangesOf(d.ctx, tx, pairs)
}

// settleNews is the reconcile's settling of tree's news (internal/daemon's settleNews), in one
// transaction: it reads again what tree owes and the change sets recorded, and settles each told
// row owed news (settlementOf). A row whose every pair changed nothing of the work, owing no
// opening, has its versions raised (record.Store.RaiseTold). The news of every other row the record
// can tell goes to the claim that holds it (record.ReadOwedHolders) by a news-only start
// (record.SuperviseRequest.News), queued as a row of its own on that claim's issue: a phase
// worker's, then each architect's once, in token order. None is queued for a claim that has one
// queued already (record.NewsStartQueued), that holds a task its worker has not taken
// (record.HoldsUntakenTask), or that a news-only start may not reach
// (record.RecordedClaimTakesNews), nor for a row whose holder cannot be named; each of those is
// told at its next start, as in the daemon. Each start queued is logged as the daemon logs it,
// once the transaction commits (`outbox design_changes queued a news-only start`), naming row, the
// design_changes row that settled it.
func (d *driver) settleNews(row record.OutboxRow, tree string) error {
	now := d.now().UTC()
	var queued []record.OwedHolder
	err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		queued = nil
		root, err := d.records.Issue(ctx, tx, tree)
		if err != nil || root == nil {
			return err
		}
		facts, err := record.TreeNewsFacts(ctx, d.records, tx, d.window.GateSteps, root.Tree)
		if err != nil {
			return err
		}
		owed := record.OwedNews(facts)
		var items []record.NewsItem
		for _, owedItem := range record.OwedNewsItems(owed, root.Tree) {
			items = append(items, owedItem.Item)
		}
		changes, err := d.changesOf(tx, items)
		if err != nil {
			return err
		}
		var sendTo []record.ToldKey
		for _, news := range owed.Members {
			switch settlementOf(news.Items, changes, record.ToldRow(facts.Told, news.Issue, record.AudienceHolder).OpeningOwed) {
			case settleLater:
				continue
			case settleRaise:
				if err := d.records.RaiseTold(ctx, tx, news.Issue, record.AudienceHolder, news.Items, now); err != nil {
					return err
				}
				continue
			}
			sendTo = append(sendTo, record.ToldKey{Issue: news.Issue, Audience: record.AudienceHolder})
		}
		if owed.ArchitectOwed() {
			switch settlementOf(owed.Architect, changes, record.ToldRow(facts.Told, root.Key, record.AudienceArchitect).OpeningOwed) {
			case settleRaise:
				if err := d.records.RaiseTold(ctx, tx, root.Key, record.AudienceArchitect, owed.Architect, now); err != nil {
					return err
				}
			case settleSend:
				sendTo = append(sendTo, record.ToldKey{Issue: root.Key, Audience: record.AudienceArchitect})
			}
		}
		holders, err := record.ReadOwedHolders(ctx, tx, d.records, record.NewNewsClaims(*root, &facts), sendTo)
		if err != nil {
			return err
		}
		var starts []record.OwedHolder
		owners := map[claim.Token]record.OwedHolder{}
		for _, key := range sendTo {
			holder, held := holders[key]
			switch {
			case !held:
			case holder.Role == claim.RoleArchitect:
				owners[holder.Token] = holder
			default:
				starts = append(starts, holder)
			}
		}
		for _, owner := range slices.Sorted(maps.Keys(owners)) {
			starts = append(starts, owners[owner])
		}
		for _, start := range starts {
			if record.NewsStartQueued(start.Run) || record.HoldsUntakenTask(start.Run.Claim) || !record.RecordedClaimTakesNews(start.Run.Claim, start.Entry) {
				continue
			}
			request := record.SuperviseRequest{Op: "start", Tree: start.Issue.Tree, Role: start.Role, Generation: start.Issue.Generation, News: true}
			if start.Role != claim.RoleArchitect {
				request.Phase = start.Issue.Phase
			}
			queue, err := record.NewOutboxRow(start.Issue.Key, request, now)
			if err != nil {
				return err
			}
			if err := d.records.Enqueue(ctx, tx, queue); err != nil {
				return err
			}
			queued = append(queued, start)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("settle the news the tree of %s is owed: %w", tree, err)
	}
	for _, start := range queued {
		d.log.Info("outbox design_changes queued a news-only start", "row", row.ID, "tree", tree, "issue", start.Issue.Key, "role", start.Role)
	}
	return nil
}

// newsOnlyReaches says whether a news-only start reaches c, the claim the stub holds of its role
// (found where it holds one), as internal/daemon's newsOnlyRefusal reads the claim when the start
// runs: one that exists and record.NewsClaimTakes takes, entry the entry the role's phases row
// names.
func newsOnlyReaches(c supervise.Claim, found bool, entry int64) bool {
	return found && record.NewsClaimTakes(c.GivenEntry, entry, c.State, c.RuleHolds())
}

// mergerNewsUnrecorded says whether issue's holder is owed news of the approved design that no
// start can carry yet (internal/daemon's mergerNewsUnrecorded): the reconcile would leave it owed
// (settlementOf's settleLater). The merger's start waits while it is (start), as it waits while the
// design gate is closed.
func (d *driver) mergerNewsUnrecorded(issue record.Issue) (bool, error) {
	unrecorded := false
	err := store.TransactWith(d.ctx, d.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		facts, err := record.TreeNewsFacts(ctx, d.records, tx, d.window.GateSteps, issue.Tree)
		if err != nil {
			return err
		}
		member, owes := record.OwedNews(facts).Member(issue.Key)
		if !owes {
			return nil
		}
		changes, err := d.changesOf(tx, member.Items)
		unrecorded = settlementOf(member.Items, changes, false) == settleLater
		return err
	})
	if err != nil {
		return false, fmt.Errorf("read whether the merger of %s is owed news no start can carry yet: %w", issue.Key, err)
	}
	return unrecorded, nil
}

// creditNews is what a task given to the claim token, of role on issue, carries of news of the
// approved design (internal/daemon's newsForStart, over the told rows internal/daemon's
// owedToClaim names), and, where credit says so, the credit of that task, written as the daemon's
// claim store writes it once the worker's turn has taken the task and run (store's
// settleDesignNews): each item owed a told row the claim speaks for whose change the record can
// tell raises that row past it (record.Store.RaiseTold, a credit counted from the item's From), and
// each opening owed such a row while the standing gate is open is told (record.Store.SettleOpening).
// An item the record cannot tell yet is left owed, as the daemon's start leaves it. The replay's
// workers take each task at its start (assumption 30), so the credit is written then. It answers
// whether the task carries anything (internal/daemon's newsAtSend.says), on which a start queued
// for news alone finishes.
//
// The rows a claim speaks for are, for a phase worker, its own member's holder row while the role
// is the one the member's phase names, and, for an architect, the tree root's architect row where
// the claim is the root's, and the holder row of each member in accepting it owns
// (record.NewsClaims.HolderOf). A tree whose root the record does not hold carries nothing.
func (d *driver) creditNews(issue record.Issue, role claim.Role, token claim.Token, credit bool) (bool, error) {
	says := false
	now := d.now().UTC()
	err := store.Transact(d.ctx, d.pool, func(ctx context.Context, tx pgx.Tx) error {
		says = false
		facts, err := record.TreeNewsFacts(ctx, d.records, tx, d.window.GateSteps, issue.Tree)
		if err != nil {
			return err
		}
		rootAt := slices.IndexFunc(facts.Members, func(member record.Issue) bool { return claim.IsTreeRoot(member.Key, member.Tree) })
		if rootAt < 0 {
			return nil
		}
		owed := record.OwedNews(facts)
		open := record.TreeGate{Steps: facts.Steps, Rows: facts.Gates}.Open()
		var rows []record.ToldKey
		carried := map[record.ToldKey][]record.NewsItem{}
		tell := func(key record.ToldKey, news []record.NewsItem) {
			rows = append(rows, key)
			carried[key] = news
		}
		root := facts.Members[rootAt]
		claims := record.NewNewsClaims(root, &facts)
		if holder, held := claims.RootArchitect(); held && holder.Token == token && owed.ArchitectOwed() {
			tell(record.ToldKey{Issue: root.Key, Audience: record.AudienceArchitect}, owed.Architect)
		}
		for _, news := range owed.Members {
			if role != claim.RoleArchitect && news.Issue != issue.Key {
				continue
			}
			member, err := claims.Member(ctx, tx, d.records, news.Issue)
			if err != nil {
				return err
			}
			holder, held, err := claims.HolderOf(ctx, tx, d.records, member)
			if err != nil {
				return err
			}
			if held && holder.Token == token {
				tell(record.ToldKey{Issue: news.Issue, Audience: record.AudienceHolder}, news.Items)
			}
		}
		var items []record.NewsItem
		for _, key := range rows {
			items = append(items, carried[key]...)
		}
		changes, err := d.changesOf(tx, items)
		if err != nil {
			return err
		}
		for _, key := range rows {
			var told []record.NewsItem
			for _, item := range carried[key] {
				if pair, known := item.Pair(); known {
					if _, recorded := changes[pair]; recorded {
						told = append(told, item)
					}
				}
			}
			opening := open && record.ToldRow(facts.Told, key.Issue, key.Audience).OpeningOwed
			says = says || len(told) > 0 || opening
			if !credit {
				continue
			}
			if len(told) > 0 {
				if err := d.records.RaiseTold(ctx, tx, key.Issue, key.Audience, told, now); err != nil {
					return err
				}
			}
			if opening {
				if err := d.records.SettleOpening(ctx, tx, key.Issue, key.Audience, false); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("credit the news the start of %s's %s carries: %w", issue.Key, role, err)
	}
	return says, nil
}
