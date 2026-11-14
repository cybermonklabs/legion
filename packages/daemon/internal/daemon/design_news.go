package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// designChangesAttempts is how many attempts of one design_changes row read a pair's change set
// from Dispatch before the pair is recorded unavailable with the last error: a failure Dispatch
// may stop giving is ridden out on the row's backoff, and a run of three, with about three seconds
// of backoff between the first and the last (retryBackoff), is no longer a moment's fault.
// Recording the pair is what lets the news go, naming the versions and why the change list could
// not be read, and what releases a merger held for it (mergerWaitsForTheDesignGate).
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
//     Dispatch. An item whose told row knows no version of its step takes its earlier version from
//     Dispatch's review history first (dispatch.Client.ApprovedVersionBefore, 0 where nothing was
//     approved before), and that version is written into the told row itself (step 3), so every
//     later reader names the same pair. A pair from version 0 has no earlier version to compare,
//     and Dispatch's route refuses it, so it is recorded read with no change listed, which the
//     news says as a first approval, and Dispatch is not asked.
//  3. In one write transaction, it records each change set it read (record.Store.PutDesignChanges)
//     and each version it resolved (record.Store.FillTold). A Dispatch without the changes route,
//     which answers 404 or 405 (dispatch.ErrNoChangesRoute), and a document Dispatch no longer
//     holds, which it answers 404 ARTIFACT_NOT_FOUND (dispatch.ErrDocumentNotFound), each have
//     their pair recorded unavailable at once, with Dispatch's refusal as the reason; any other
//     failed read returns its error, so the row retries on its backoff, and the attempt that is
//     the row's designChangesAttempts-th records the pair unavailable with the last error instead.
//     A failed read of the review history is bounded the same way: until that attempt it is
//     retried, and on it the step is filled with 0 and the pair from 0 recorded unavailable, its
//     reason naming the failed read, which the news says as a version the daemon could not read
//     (workflow.RenderNews) and never as a first approval. So every read this row makes ends
//     within its bounded attempts, and a merger held for the news is released then
//     (mergerWaitsForTheDesignGate).
//  4. In one transaction under the fact lock (intake.SerializeFacts), it settles each owed member
//     (settleNews).
//  5. In that same transaction, it settles the tree root's architect row, and queues the root
//     architect's news-only start when that row is owed.
//
// A root the record no longer holds owes nothing, and the row finishes. So does a tree owed
// nothing at the first step, with no lock taken: every write that leaves news owed queues a
// reconcile of its own (record.QueueDesignNews), and this row, running, does not stand in for it
// (record.Store.DesignNewsPending).
func (r *outbox) designChanges(ctx context.Context, row record.OutboxRow) error {
	if r.dispatch == nil {
		return errors.New("design_changes executor has no Dispatch client")
	}
	var root *record.Issue
	var owed record.TreeNews
	if err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
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
	if len(items) == 0 {
		return nil
	}
	fills, pairs, unread, historyErr := r.resolveBaselines(ctx, row, items)
	var recorded map[record.VersionPair]record.DesignChanges
	if err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		recorded, err = r.records.DesignChangesOf(ctx, tx, pairs)
		return err
	}); err != nil {
		return fmt.Errorf("read the recorded change sets of the tree of %s: %w", root.Tree, err)
	}
	read, readErr := r.readChangeSets(ctx, row, pairs, recorded, unread)
	if len(read) > 0 || len(fills) > 0 {
		at := r.now().UTC()
		if err := pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
			for _, changes := range read {
				if err := r.records.PutDesignChanges(ctx, tx, changes); err != nil {
					return err
				}
			}
			for key, versions := range fills {
				if err := r.records.FillTold(ctx, tx, key.issue, key.audience, versions, at); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return fmt.Errorf("record the change sets of the tree of %s: %w", root.Tree, err)
		}
	}
	if err := errors.Join(historyErr, readErr); err != nil {
		return err
	}
	return r.settleNews(ctx, row, root.Tree)
}

// resolveBaselines is the second step's first half (designChanges): for each item whose told row
// knows no version of its step, the version before its To that Dispatch's review history shows
// approved, by told row and step (fills), read once for each document and version; and every pair
// of versions the items name once those are known (pairs), each once. An item whose history read
// failed names no pair, and the failure is returned beside what was read, until the row's
// designChangesAttempts-th attempt: there the step is filled with 0, and its pair from 0 is named
// among unread with the reason it is recorded unavailable (readChangeSets), since nothing else can
// end a read Dispatch keeps failing.
func (r *outbox) resolveBaselines(ctx context.Context, row record.OutboxRow, items []owedNewsItem) (map[toldKey]map[record.DesignStep]int,
	[]record.VersionPair, map[record.VersionPair]string, error) {
	type question struct {
		artifact string
		to       int
	}
	type answer struct {
		version int
		err     error
	}
	// approvedBefore is Dispatch's answer to one question, read the first time it is asked
	// (fresh) and taken from the answers after; a failed read answers version 0.
	answers := map[question]answer{}
	approvedBefore := func(asked question) (answer, bool) {
		if got, read := answers[asked]; read {
			return got, false
		}
		version, err := r.dispatch.ApprovedVersionBefore(ctx, asked.artifact, asked.to)
		if err != nil {
			version = 0
		}
		answers[asked] = answer{version: version, err: err}
		return answers[asked], true
	}
	fills := map[toldKey]map[record.DesignStep]int{}
	unread := map[record.VersionPair]string{}
	var pairs []record.VersionPair
	var failed error
	for _, owed := range items {
		item := owed.item
		pair, known := item.Pair()
		if !known {
			got, fresh := approvedBefore(question{item.ArtifactID, item.To})
			switch {
			case got.err != nil && row.Attempts+1 < designChangesAttempts:
				if fresh {
					failed = errors.Join(failed, fmt.Errorf("read the version of %s approved before version %d: %w", item.ArtifactID, item.To, got.err))
				}
				continue
			case got.err != nil:
				unread[record.VersionPair{ArtifactID: item.ArtifactID, To: item.To}] =
					fmt.Sprintf("the version this issue last took could not be read from the review history: %v", got.err)
			}
			if fills[owed.told] == nil {
				fills[owed.told] = map[record.DesignStep]int{}
			}
			fills[owed.told][item.Step] = got.version
			pair = record.VersionPair{ArtifactID: item.ArtifactID, From: got.version, To: item.To}
		}
		if !slices.Contains(pairs, pair) {
			pairs = append(pairs, pair)
		}
	}
	return fills, pairs, unread, failed
}

// readChangeSets is the second step's second half (designChanges): the change set of each of
// pairs that recorded does not hold, as it is to be recorded. A pair unread names — from 0, for a
// version the review history could not give — is recorded unavailable with its reason. Any other
// pair from version 0 is recorded read with nothing listed, and Dispatch is not asked. A Dispatch
// without the changes route, and a document Dispatch no longer holds, have the pair recorded
// unavailable at once; any other failure is returned, beside every set that was read, until the
// row's designChangesAttempts-th attempt, which records the pair unavailable with that failure as
// its reason.
func (r *outbox) readChangeSets(ctx context.Context, row record.OutboxRow, pairs []record.VersionPair,
	recorded map[record.VersionPair]record.DesignChanges, unread map[record.VersionPair]string) ([]record.DesignChanges, error) {
	at := r.now().UTC()
	var read []record.DesignChanges
	var failed error
	for _, pair := range pairs {
		if _, known := recorded[pair]; known {
			continue
		}
		if reason, ok := unread[pair]; ok {
			read = append(read, record.DesignChanges{VersionPair: pair, State: record.DesignChangesUnavailable, Reason: reason, ReadAt: at})
			continue
		}
		if pair.From == 0 {
			read = append(read, record.DesignChanges{VersionPair: pair, State: record.DesignChangesRead, ReadAt: at})
			continue
		}
		answer, err := r.dispatch.ArtifactChanges(ctx, pair.ArtifactID, pair.From, pair.To)
		switch {
		case err == nil:
			read = append(read, changesFromDispatch(pair, answer, at))
		case errors.Is(err, dispatch.ErrNoChangesRoute), errors.Is(err, dispatch.ErrDocumentNotFound):
			read = append(read, record.DesignChanges{VersionPair: pair, State: record.DesignChangesUnavailable, Reason: err.Error(), ReadAt: at})
		case row.Attempts+1 >= designChangesAttempts:
			read = append(read, record.DesignChanges{VersionPair: pair, State: record.DesignChangesUnavailable, ReadAt: at,
				Reason: fmt.Sprintf("Dispatch failed %d reads of it, the last with: %v", row.Attempts+1, err)})
		default:
			failed = errors.Join(failed, fmt.Errorf("read what changed in %s from version %d to %d: %w", pair.ArtifactID, pair.From, pair.To, err))
		}
	}
	return read, failed
}

// changesFromDispatch is Dispatch's answer for pair as the record keeps it, read at at.
func changesFromDispatch(pair record.VersionPair, answer dispatch.ArtifactChanges, at time.Time) record.DesignChanges {
	changes := record.DesignChanges{VersionPair: pair, State: record.DesignChangesRead, DecisionOnly: answer.DecisionOnly, ReadAt: at}
	for _, section := range answer.Sections {
		changes.Sections = append(changes.Sections, record.SectionChange{Heading: section.Heading, Occurrence: section.Occurrence, Change: section.Change})
	}
	for _, block := range answer.Blocks {
		changes.Blocks = append(changes.Blocks, record.BlockChange{ID: block.ID, Type: block.Type, Change: block.Change})
	}
	return changes
}

// newsSettlement is what the reconcile does for one told row owed news (settlementOf).
type newsSettlement int

const (
	// settleLater leaves the news owed: a pair it names has no change set recorded yet, which
	// only a later approval or a later told version since the first step can cause, and the row
	// that approval queued, or the sweep, settles it then.
	settleLater newsSettlement = iota
	// settleRaise raises the told versions directly and sends nothing: every pair changed
	// decision blocks alone, which tells nobody anything of the work, and no opening is owed.
	settleRaise
	// settleSend sends the news, by a news-only start.
	settleSend
)

// settlementOf is what the reconcile does for one told row owed items, given the change sets
// recorded: settleLater while any pair is unrecorded; settleRaise, with the version of each step
// to raise to, when every pair changed decision blocks alone and the row owes no opening; and
// settleSend otherwise. A decision-only change the row owes the opening with is sent, not raised:
// the gate closed on that version, the holder was told it closed, and a person approved it later,
// so the holder is owed the opening as a task, with the words that nothing its issue holds changed
// and no change list (workflow.RenderNews), since no role message carried that opening.
func settlementOf(items []record.NewsItem, recorded map[record.VersionPair]record.DesignChanges, openingOwed bool) (newsSettlement, map[record.DesignStep]int) {
	versions := map[record.DesignStep]int{}
	decisionsOnly := true
	for _, item := range items {
		pair, known := item.Pair()
		if !known {
			return settleLater, nil
		}
		changes, recordedSet := recorded[pair]
		if !recordedSet {
			return settleLater, nil
		}
		decisionsOnly = decisionsOnly && changes.State == record.DesignChangesRead && changes.DecisionOnly
		versions[item.Step] = item.To
	}
	if decisionsOnly && !openingOwed {
		return settleRaise, versions
	}
	return settleSend, nil
}

// owedPairs is every pair of versions news names that has a known earlier version, each once.
func owedPairs(news record.TreeNews, root string) []record.VersionPair {
	var pairs []record.VersionPair
	for _, owed := range owedNewsItems(news, root) {
		if pair, known := owed.item.Pair(); known && !slices.Contains(pairs, pair) {
			pairs = append(pairs, pair)
		}
	}
	return pairs
}

// newsHolderStart is one news-only start the reconcile may queue: the member whose news it tells
// (for a holder) or whose issue the architect works (for an architect), and the claim it reaches.
type newsHolderStart struct {
	issue record.Issue
	role  claim.Role
	token claim.Token
}

// settleNews is the reconcile's fourth and fifth steps (designChanges): in one transaction under
// the fact lock, it reads again what tree owes and the change sets recorded, and settles each told
// row owed news (settlementOf):
//
//   - a row whose every pair changed decision blocks alone, and that owes no opening, has its
//     versions raised directly (record.Store.RaiseTold), and nothing is sent;
//   - otherwise a member's news goes to the holder of its phase by a news-only start
//     (record.SuperviseRequest.News), queued as a row of its own on the member's issue, unless one
//     is queued for that claim already, or the claim holds a task its worker has not taken yet
//     (holdsUntakenTask), or the claim is one a news-only start may not reach
//     (recordedClaimTakesNews): none, behind the entry its phases row names — owed its phase start,
//     which carries the news — retired, failed, or suspended by an operator, each of which the
//     start that revives the claim tells;
//   - a member in accepting is held by the architect that owns it (owningArchitect: a running
//     sub-architect, else the tree root's), and its news goes to that architect's claim, in the
//     holder form, by that claim's one news-only start;
//   - the tree root's architect row is settled the same way, and its news goes by the root
//     architect's news-only start, which also carries the holder form of each member in accepting
//     the root architect owns.
//
// A tree that lingers, a member out of the workflow, one that merged, or one whose phase has no
// holder is owed nothing (record.OwedNews), so nothing is queued for it. Each start reads again,
// when it runs, what it carries and whether it may reach its claim (the start executor's
// newsForStart and newsOnlyRefusal), since the record can move before then.
func (r *outbox) settleNews(ctx context.Context, row record.OutboxRow, tree string) error {
	now := r.now().UTC()
	var queued []newsHolderStart
	raised := 0
	if err := r.settleUnderFacts(ctx, func(ctx context.Context, tx pgx.Tx) error {
		queued, raised = nil, 0
		root, err := r.records.Issue(ctx, tx, tree)
		if err != nil || root == nil {
			return err
		}
		facts, err := record.TreeNewsFacts(ctx, r.records, tx, r.gatedSteps, root.Tree)
		if err != nil {
			return err
		}
		owed := record.OwedNews(facts)
		recorded, err := r.records.DesignChangesOf(ctx, tx, owedPairs(owed, root.Tree))
		if err != nil {
			return err
		}
		members := make(map[string]record.Issue, len(facts.Members))
		architects := make([]claim.Token, 0, len(facts.Members))
		// architectOf is the member whose issue each architect token names: a token carries the
		// issue's key lowercased (claim.NewToken), so the issue is read back from here.
		architectOf := make(map[claim.Token]record.Issue, len(facts.Members))
		for _, member := range facts.Members {
			members[member.Key] = member
			if token, err := claim.NewToken(r.project, member.Key, claim.RoleArchitect); err == nil {
				architects = append(architects, token)
				architectOf[token] = member
			}
		}
		held, err := r.records.RoleClaims(ctx, tx, architects)
		if err != nil {
			return err
		}
		runs := func(token claim.Token) bool { return recordedClaimRuns(held[token]) }
		var starts []newsHolderStart
		owners := map[claim.Token]bool{}
		for _, news := range owed.Members {
			member := members[news.Issue]
			settlement, versions := settlementOf(news.Items, recorded, record.ToldRow(facts.Told, news.Issue, record.AudienceHolder).OpeningOwed)
			switch settlement {
			case settleLater:
				continue
			case settleRaise:
				if err := r.records.RaiseTold(ctx, tx, news.Issue, record.AudienceHolder, versions, now); err != nil {
					return err
				}
				raised++
				continue
			}
			if !news.HeldByArchitect() {
				role := phase.RoleFor(member.Phase)
				token, err := claim.NewToken(r.project, member.Key, role)
				if err != nil {
					r.log.Warn("outbox design_changes cannot name the holder of a member owed news; it is told at its next start",
						"row", row.ID, "issue", member.Key, "role", role, "error", err)
					continue
				}
				starts = append(starts, newsHolderStart{issue: member, role: role, token: token})
				continue
			}
			owner, err := owningArchitect(r.project, members, member, "", runs)
			if err != nil {
				r.log.Warn("outbox design_changes cannot name the architect that owns a member owed news in accepting",
					"row", row.ID, "issue", member.Key, "error", err)
				continue
			}
			owners[owner] = true
		}
		rootArchitect, err := claim.NewToken(r.project, root.Key, claim.RoleArchitect)
		if err != nil {
			return err
		}
		if len(owed.Architect) > 0 {
			settlement, versions := settlementOf(owed.Architect, recorded, record.ToldRow(facts.Told, root.Key, record.AudienceArchitect).OpeningOwed)
			switch settlement {
			case settleRaise:
				if err := r.records.RaiseTold(ctx, tx, root.Key, record.AudienceArchitect, versions, now); err != nil {
					return err
				}
				raised++
			case settleSend:
				owners[rootArchitect] = true
			}
		}
		for _, owner := range slices.Sorted(maps.Keys(owners)) {
			if member, ok := architectOf[owner]; ok {
				starts = append(starts, newsHolderStart{issue: member, role: claim.RoleArchitect, token: owner})
			}
		}
		keys := make([]record.RoleRunKey, 0, len(starts))
		for _, start := range starts {
			keys = append(keys, record.RoleRunKey{Token: start.token, Issue: start.issue.Key, Role: start.role, Generation: start.issue.Generation})
		}
		roleRuns, err := r.records.RoleRuns(ctx, tx, keys)
		if err != nil {
			return err
		}
		for _, start := range starts {
			run := roleRuns[start.token]
			if newsStartQueued(run) || holdsUntakenTask(run) {
				continue
			}
			var entry int64
			request := record.SuperviseRequest{Op: "start", Tree: start.issue.Tree, Role: start.role, Generation: start.issue.Generation, News: true}
			if start.role != claim.RoleArchitect {
				rows, err := r.records.Phases(ctx, tx, start.issue.Key)
				if err != nil {
					return err
				}
				entry, request.Phase = record.PhaseRowOf(rows, start.role).Entry, start.issue.Phase
			}
			if !recordedClaimTakesNews(run.Claim, entry) {
				continue
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
		return nil
	}); err != nil {
		return fmt.Errorf("settle the news the tree of %s is owed: %w", tree, err)
	}
	for _, start := range queued {
		r.log.Info("outbox design_changes queued a news-only start", "row", row.ID, "tree", tree, "issue", start.issue.Key, "role", start.role)
	}
	if raised > 0 || len(queued) > 0 {
		r.log.Info("outbox design_changes settled the news its tree is owed", "row", row.ID, "tree", tree, "raised", raised, "starts", len(queued))
	}
	return nil
}

// newsStartQueued says whether run holds a news-only start still to run (record.SuperviseRequest.News):
// one is all a claim needs, since each reads what it carries when it runs.
func newsStartQueued(run record.RoleRun) bool {
	return slices.ContainsFunc(run.Queued, func(queued record.QueuedSupervise) bool {
		return queued.Request.Op == "start" && queued.Request.News
	})
}

// holdsUntakenTask says whether run's claim, as the record holds it, holds a task its worker has
// not taken yet: a pending delivery with no confirmation. A news-only start queued behind it
// would only wait, since the claim's machine takes no second task while one is pending
// (supervise.ErrDeliveryPending), and its start would retry on its backoff until a turn takes the
// first. So none is queued, and the news stays owed: the sweep queues the reconcile again
// (queueOwedDesignNews), which queues the start once that task is taken, and the next start of
// the phase carries the news itself.
func holdsUntakenTask(run record.RoleRun) bool {
	return run.Claim != nil && run.Claim.Pending != nil && run.Claim.Pending.ConfirmedAt.IsZero()
}

// newsClaimTakes is the one rule for whether a news-only start may hand its news to a claim the
// caller holds, over what says it: it was given the entry its role's phases row names (entry, 0 for
// an architect, which works no entry), since a claim behind that entry is owed its phase start,
// which carries the news; it has not ended (claimEnded), since a failed or retired claim is
// revived by its phase's own repair, whose start carries the news; and no operator holds it
// suspended (operatorHolds). A claim an idle pod release suspended takes the news, and the start
// that carries it wakes the claim as any start wakes one. Both readers state it through this: the
// reconcile and the architect's summary over the claim the record holds (recordedClaimTakesNews),
// and the start over the claim its machine holds (newsOnlyRefusal).
func newsClaimTakes(given, entry int64, state supervise.ClaimState, operatorHolds bool) bool {
	return given == entry && !claimEnded(state) && !operatorHolds
}

// recordedClaimTakesNews is newsClaimTakes over a claim as the record holds it (record.RoleClaim),
// nil for a role the record holds no claim of, which takes no news.
func recordedClaimTakesNews(held *record.RoleClaim, entry int64) bool {
	if held == nil {
		return false
	}
	operator := held.State == supervise.StateSuspended && held.SuspendedBy == supervise.SuspendedByOperator
	return newsClaimTakes(held.GivenEntry, entry, held.State, operator)
}

// newsOnlyRefusal is why a news-only start reaches no claim, as the claim's machine holds it when
// the start runs, and "" when it may: the guards the reconcile queued it under, read again
// (newsClaimTakes), since the claim can have moved since. entry is the entry the role's phases row
// names, 0 for an architect. The start's other guards are the executor's own, read before this:
// an earlier generation, a phase the issue has left, an issue out of the workflow and a lingering
// tree each finish it, as they finish any start.
func newsOnlyRefusal(machine *supervise.Machine, found bool, entry int64) string {
	if !found {
		return "no claim holds the role"
	}
	held := machine.Claim()
	if newsClaimTakes(held.GivenEntry, entry, held.State, held.RuleHolds()) {
		return ""
	}
	switch {
	case held.GivenEntry != entry:
		return fmt.Sprintf("the claim was given entry %d and its role's phases row names %d, so its phase start carries the news", held.GivenEntry, entry)
	case claimEnded(held.State):
		return fmt.Sprintf("the claim is %s, and the start that revives it carries the news", held.State)
	}
	return "an operator suspended the claim, and the start that resumes it carries the news"
}

// startNews is what a start carries of news of the approved design (newsForStart): the task it
// delivers — the start's own text, a blank line, then the news — the news itself as the delivery
// carries it (record.DeliveryNews, encoded; nil where nothing is attached), and whether the news
// says anything a task of its own is worth (newsAtSend.says).
type startNews struct {
	task string
	news []byte
	says bool
}

// newsForStart is the news a start of role on issue carries, read from the record in one read
// transaction when the start runs, with no Dispatch read: the items record.OwedNews owes the told
// rows the claim speaks for (owedToClaim), and the opening where its row owes it and the standing
// gate is open, each rendered as a send renders it (readNewsAtSend). An item whose change set is
// not recorded yet is left out and stays owed, for the reconcile or a later start to carry. The
// task is the start's own text (record.DeliveryNews.Task), a blank line, then the news; a start
// that carries nothing delivers its own text.
func (r *outbox) newsForStart(ctx context.Context, issue record.Issue, payload record.SuperviseRequest, token claim.Token) (startNews, error) {
	carried := record.DeliveryNews{Task: payload.Task, NewsOnly: payload.News}
	var sent newsAtSend
	if err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
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
		sent, err = readNewsAtSend(ctx, tx, r.records, r.gatedSteps, carried)
		return err
	}); err != nil {
		return startNews{}, fmt.Errorf("read the news the start of %s's %s carries: %w", issue.Key, payload.Role, err)
	}
	if sent.kept.Empty() {
		return startNews{task: payload.Task}, nil
	}
	news, err := json.Marshal(sent.kept)
	if err != nil {
		return startNews{}, fmt.Errorf("encode the news the start of %s's %s carries: %w", issue.Key, payload.Role, err)
	}
	return startNews{task: paragraphs(payload.Task, sent.text), news: news, says: sent.says}, nil
}

// owedToClaim is what facts owe the claim token, of role on issue, speaks for, as a delivery
// carries it: for a phase worker, the items its member's holder row is owed while the role is the
// one the member's phase names; for an architect, its own summary when issue is the tree root and
// its row is owed, and the holder form of each member in accepting it owns (owningArchitect, over
// the architect claims the record holds). Each told row it speaks for carries the opening too, where
// the row owes it and the standing gate is open. An architect is never handed a worker's news, and
// a worker never an architect's.
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
		if open && record.ToldRow(facts.Told, key, audience).OpeningOwed {
			openings = append(openings, record.DeliveryOpening{Issue: key, Audience: audience})
		}
	}
	if role != claim.RoleArchitect {
		for _, member := range owed.Members {
			if member.Issue == issue.Key && !member.HeldByArchitect() && phase.RoleFor(member.Phase) == role {
				tell(member.Issue, record.AudienceHolder, member.Items)
			}
		}
		return items, openings, nil
	}
	if claim.IsTreeRoot(issue.Key, issue.Tree) && len(owed.Architect) > 0 {
		tell(issue.Key, record.AudienceArchitect, owed.Architect)
	}
	members := make(map[string]record.Issue, len(facts.Members))
	architects := make([]claim.Token, 0, len(facts.Members))
	for _, member := range facts.Members {
		members[member.Key] = member
		if architect, err := claim.NewToken(r.project, member.Key, claim.RoleArchitect); err == nil {
			architects = append(architects, architect)
		}
	}
	held, err := r.records.RoleClaims(ctx, tx, architects)
	if err != nil {
		return nil, nil, err
	}
	runs := func(architect claim.Token) bool { return recordedClaimRuns(held[architect]) }
	for _, member := range owed.Members {
		if !member.HeldByArchitect() {
			continue
		}
		if owner, err := owningArchitect(r.project, members, members[member.Issue], "", runs); err == nil && owner == token {
			tell(member.Issue, record.AudienceHolder, member.Items)
		}
	}
	return items, openings, nil
}

// mergerNewsUnrecorded says whether issue's holder is owed news of the approved design that no
// start can carry yet: the reconcile would leave it owed (settlementOf's settleLater), since an
// owed item's earlier version is unknown to the record, or its change set is not recorded, read or
// unavailable. A merger released without it could merge before it reads what changed, so its start
// waits (mergerWaitsForTheDesignGate). The wait ends within the design_changes row's bounded
// attempts, since a pair Dispatch cannot read is recorded unavailable (designChangesAttempts).
func (r *outbox) mergerNewsUnrecorded(ctx context.Context, issue record.Issue) (bool, error) {
	var unrecorded bool
	if err := pgx.BeginTxFunc(ctx, r.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		facts, err := record.TreeNewsFacts(ctx, r.records, tx, r.gatedSteps, issue.Tree)
		if err != nil {
			return err
		}
		for _, member := range record.OwedNews(facts).Members {
			if member.Issue != issue.Key {
				continue
			}
			pairs := make([]record.VersionPair, 0, len(member.Items))
			for _, item := range member.Items {
				pair, known := item.Pair()
				if !known {
					unrecorded = true
					return nil
				}
				pairs = append(pairs, pair)
			}
			recorded, err := r.records.DesignChangesOf(ctx, tx, pairs)
			if err != nil {
				return err
			}
			settlement, _ := settlementOf(member.Items, recorded, false)
			unrecorded = settlement == settleLater
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
func (w *workflowRuntime) queueOwedDesignNews(ctx context.Context) error {
	var owed []string
	if err := pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		roots, err := w.records.GatedIssues(ctx, tx, w.dispatchProject)
		if err != nil {
			return err
		}
		owed = nil
		for _, root := range roots {
			facts, err := record.TreeNewsFacts(ctx, w.records, tx, w.gatedSteps, root)
			if err != nil {
				return err
			}
			if news := record.OwedNews(facts); len(news.Members) > 0 || len(news.Architect) > 0 {
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
	scoped, committed := intake.WithCommitHooks(ctx)
	if err := pgx.BeginFunc(scoped, w.pool, func(tx pgx.Tx) error {
		if err := intake.SerializeFacts(scoped, tx); err != nil {
			return fmt.Errorf("serialize the queue with every fact: %w", err)
		}
		for _, tree := range trees {
			if err := record.QueueDesignNews(scoped, w.records, tx, tree, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return fmt.Errorf("queue the reconcile of the news of %v: %w", trees, err)
	}
	committed()
	return nil
}
