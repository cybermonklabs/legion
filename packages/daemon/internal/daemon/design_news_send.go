package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
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
//   - an item stands while the gate's approved version of its step is still its To
//     (classify.DesignApprovalStands), of the document it names, and the told row of its audience
//     still holds a version below To or none it knows; every other item is dropped from the text
//     and from the news the delivery carries, so its confirmation raises only what was told;
//   - an opening stands while the told row of its audience still owes it;
//   - an item whose change set is not recorded yet is dropped as well: nothing else can say what
//     changed, and the item stays owed for a later start, or the reconcile, to carry;
//   - the root architect's summary of a tree that lingers is dropped whole: linger holds every
//     member where it stood, record.OwedNews owes such a tree nothing, and nothing goes to it.
//
// The task text is the start's own text (record.DeliveryNews.Task), a blank line, and the news. A
// phase start whose news is all dropped is sent as its own text alone, carrying no news; a task
// queued for its news alone with nothing left to say — no item standing, and no opening owed while
// the gate stands open — answers "", which the machine retires rather than sends. A task whose news
// all stands is answered with the bytes it carries, so a send of a task the record has not moved
// under writes nothing.
//
// It is the machine that asks, because the machine owns the pending delivery: it holds its own
// lock and asks this in a short read-only transaction of its own, as rootHears is asked.
func (w *workflowRuntime) designNews(ctx context.Context, c supervise.Claim, d supervise.Delivery) (string, []byte, error) {
	if len(d.News) == 0 {
		return d.Task, d.News, nil
	}
	var carried record.DeliveryNews
	if err := json.Unmarshal(d.News, &carried); err != nil {
		return "", nil, fmt.Errorf("decode the news the task held for %s carries: %w", c.Token, err)
	}
	var sent newsAtSend
	if err := pgx.BeginTxFunc(ctx, w.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		var err error
		sent, err = readNewsAtSend(ctx, tx, w.records, w.gatedSteps, carried)
		return err
	}); err != nil {
		return "", nil, fmt.Errorf("read what the news the task held for %s carries says now: %w", c.Token, err)
	}
	if carried.NewsOnly && !sent.says {
		return "", nil, nil
	}
	task := paragraphs(carried.Task, sent.text)
	switch {
	case len(sent.kept.Items) == len(carried.Items) && len(sent.kept.Openings) == len(carried.Openings):
		return task, d.News, nil
	case len(sent.kept.Items) == 0 && len(sent.kept.Openings) == 0:
		return task, nil, nil
	}
	news, err := json.Marshal(sent.kept)
	if err != nil {
		return "", nil, fmt.Errorf("encode what the news the task held for %s still carries: %w", c.Token, err)
	}
	return task, news, nil
}

// newsAtSend is what a delivery's news says at its send: the text that follows the start's own
// (text), what of the news still stands (kept, which keeps the start's own text and whether it is
// a news-only start), and whether any of it is worth a task of its own (says): an item that
// stands, or an opening owed while the gate stands open.
type newsAtSend struct {
	text string
	kept record.DeliveryNews
	says bool
}

// newsListener is one audience of one issue a delivery speaks to: the items it tells that
// audience, by their place in the delivery's news, and whether it carries it the opening.
type newsListener struct {
	issue    string
	audience record.DesignAudience
	items    []int
	opening  bool
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
		find(opening.Issue, opening.Audience).opening = true
	}
	return listeners
}

// newsTree is what the send reads of one tree: its root's record, the root's gate rows and the
// told rows of its issues.
type newsTree struct {
	root  record.Issue
	gates map[record.DesignStep]record.DesignGate
	told  []record.DesignTold
}

// newsListened is one listener as the record stands at the send: its issue's record and tree, its
// told row, the items of it that stand, whether its opening stands, and, for the tree root's
// architect, the members its summary names.
type newsListened struct {
	listener *newsListener
	issue    record.Issue
	tree     *newsTree
	told     record.DesignTold
	items    []int
	opening  bool
	members  []memberDraft
}

// readNewsAtSend reads, in the caller's one transaction, what carried says as the record stands,
// and renders it (designNews). An item, an opening or a listener whose issue the record does not
// hold is dropped: nothing can say what the gate of a tree the record does not know stands at. So
// is the architect of a tree that lingers, items, opening and summary alike.
func readNewsAtSend(ctx context.Context, tx pgx.Tx, records record.Store, steps []record.DesignStep, carried record.DeliveryNews) (newsAtSend, error) {
	trees := map[string]*newsTree{}
	var listened []newsListened
	var pairs []record.VersionPair
	for _, listener := range listenersOf(carried) {
		issue, err := records.Issue(ctx, tx, listener.issue)
		if err != nil {
			return newsAtSend{}, err
		}
		if issue == nil {
			continue
		}
		tree, err := readNewsTree(ctx, tx, records, trees, issue.Tree)
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
			told: toldRow(tree.told, listener.issue, listener.audience)}
		for _, i := range listener.items {
			item := carried.Items[i]
			if !itemStands(item, tree.gates[item.Step], heard.told) {
				continue
			}
			if pair, ok := item.Pair(); ok {
				heard.items = append(heard.items, i)
				pairs = append(pairs, pair)
			}
		}
		heard.opening = listener.opening && heard.told.OpeningOwed
		if listener.audience == record.AudienceArchitect && claim.IsTreeRoot(issue.Key, issue.Tree) {
			if heard.members, err = readTreeMembers(ctx, tx, records, steps, tree); err != nil {
				return newsAtSend{}, err
			}
			for _, member := range heard.members {
				pairs = append(pairs, member.pairs()...)
			}
		}
		listened = append(listened, heard)
	}
	recorded, err := records.DesignChangesOf(ctx, tx, pairs)
	if err != nil {
		return newsAtSend{}, err
	}
	sent := newsAtSend{kept: record.DeliveryNews{Task: carried.Task, NewsOnly: carried.NewsOnly}}
	keep := map[int]bool{}
	var texts []string
	for _, heard := range listened {
		news := workflow.News{Audience: heard.listener.audience, Tree: heard.issue.Tree, Issue: heard.issue.Key,
			Phase: heard.issue.Phase, Steps: steps, Gates: heard.tree.gates, OpeningOwed: heard.opening}
		for _, i := range heard.items {
			item := carried.Items[i]
			pair, _ := item.Pair()
			changes, ok := recorded[pair]
			if !ok {
				continue
			}
			keep[i] = true
			news.Items = append(news.Items, workflow.NewsChange{Step: item.Step, Changes: changes})
		}
		for _, member := range heard.members {
			news.Members = append(news.Members, member.told(recorded))
		}
		if heard.opening {
			sent.kept.Openings = append(sent.kept.Openings, record.DeliveryOpening{Issue: heard.listener.issue, Audience: heard.listener.audience})
		}
		if len(news.Items) == 0 && !heard.opening {
			continue
		}
		open := record.TreeGate{Steps: steps, Rows: heard.tree.gates}.Open()
		sent.says = sent.says || len(news.Items) > 0 || (heard.opening && open)
		texts = append(texts, workflow.RenderNews(news))
	}
	for i, item := range carried.Items {
		if keep[i] {
			sent.kept.Items = append(sent.kept.Items, item)
		}
	}
	sent.text = paragraphs(texts...)
	return sent, nil
}

// readNewsTree is the tree whose root is root, read once for every listener of it; nil when the
// record holds no such root.
func readNewsTree(ctx context.Context, tx pgx.Tx, records record.Store, trees map[string]*newsTree, root string) (*newsTree, error) {
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
	trees[root] = &newsTree{root: *issue, gates: gates, told: told}
	return trees[root], nil
}

// toldRow is issue's audience row among told, the zero row, which knows no version and owes no
// opening, where there is none.
func toldRow(told []record.DesignTold, issue string, audience record.DesignAudience) record.DesignTold {
	for _, row := range told {
		if row.Issue == issue && row.Audience == audience {
			return row
		}
	}
	return record.DesignTold{Issue: issue, Audience: audience}
}

// itemStands says whether an item a delivery carries is still news its audience is owed: the gate
// row of its step holds the document it names, approved at the version it tells
// (classify.DesignApprovalStands), and the audience's told row holds a version of that step below
// it, or none it knows.
func itemStands(item record.DeliveryNewsItem, gate record.DesignGate, told record.DesignTold) bool {
	if gate.ArtifactID != item.Artifact || !classify.DesignApprovalStands(gate, item.To) {
		return false
	}
	took, known := told.Took(item.Step)
	return !known || took < item.To
}

// memberDraft is one member of a tree as the root architect's summary reads it before the change
// sets are read: its record, what it is told, and, for a member owed news, the items its holder is
// owed now (record.OwedNews).
type memberDraft struct {
	issue record.Issue
	state workflow.NewsState
	owed  []record.NewsItem
}

// pairs is each change set the member's owed items name, leaving out an item whose From the
// record does not know, which names no pair.
func (m memberDraft) pairs() []record.VersionPair {
	var pairs []record.VersionPair
	for _, item := range m.owed {
		if item.From != nil {
			pairs = append(pairs, record.VersionPair{ArtifactID: item.ArtifactID, From: *item.From, To: item.To})
		}
	}
	return pairs
}

// told is the member as the summary names it, given the change sets recorded: its owed items with
// their change sets, or, where every item it is owed is recorded as decision blocks alone, told
// nothing for that reason. An owed item whose change set is not recorded is named by none.
func (m memberDraft) told(recorded map[record.VersionPair]record.DesignChanges) workflow.NewsMember {
	member := workflow.NewsMember{Issue: m.issue.Key, Phase: m.issue.Phase, State: m.state}
	decisionsOnly := len(m.owed) > 0
	for _, item := range m.owed {
		var changes record.DesignChanges
		var ok bool
		if item.From != nil {
			changes, ok = recorded[record.VersionPair{ArtifactID: item.ArtifactID, From: *item.From, To: item.To}]
		}
		decisionsOnly = decisionsOnly && ok && changes.DecisionOnly
		if ok {
			member.Items = append(member.Items, workflow.NewsChange{Step: item.Step, Changes: changes})
		}
	}
	if decisionsOnly {
		member.State, member.Items = workflow.NewsDecisionOnly, nil
	}
	return member
}

// readTreeMembers is every member of tree, its root first, as its root architect's summary names
// it: what OwedNews owes each now, read from the facts that rule reads, and what each is told.
//
// A member owed news is told by a task now (workflow.NewsSentNow) when its holder's claim was
// given its phase's current entry and runs — the claim a news-only start reaches — and by the next
// start of its phase otherwise (workflow.NewsAtNextStart): a claim owed its phase start
// (workflow.OwedEntry), none at all, one retired, failed or suspended by an operator. A member in
// accepting is answered by the architect that owns it (owningArchitect): the root architect
// itself (workflow.NewsYouAnswer), or a sub-architect, whose task tells it now.
//
// A member owed nothing is told nothing, and the reason is the first that holds of: its phase
// already took every approved version that concerns it (NewsAlreadyTaken), or every one it has not
// taken is criteria, which concerns the tree root alone (NewsNotConcerned); its pull request
// merged; its work is done; its status takes it out of the workflow; it has not started; it waits
// on a person; it is stopped. A member owed something none of those explain is one OwedNews holds
// back because the standing gate closed again before the summary was sent: OwedNews owes no
// member anything while it is closed, so no task carries any member's news and no start will, and
// each such member is told nothing yet, for that reason (workflow.NewsGateClosed), rather than
// said to be told now or at its next start. The summary of a lingering tree is never sent.
func readTreeMembers(ctx context.Context, tx pgx.Tx, records record.Store, steps []record.DesignStep, tree *newsTree) ([]memberDraft, error) {
	members, err := records.TreeIssues(ctx, tx, tree.root.Key)
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(members, func(a, b record.Issue) int {
		return boolOrder(claim.IsTreeRoot(b.Key, b.Tree)) - boolOrder(claim.IsTreeRoot(a.Key, a.Tree))
	})
	merged := map[string]bool{}
	for _, member := range members {
		pr, err := records.PullRequest(ctx, tx, member.Key)
		if err != nil {
			return nil, err
		}
		merged[member.Key] = pr != nil && pr.State == record.PullRequestMerged
	}
	owed := record.OwedNews(record.NewsFacts{Steps: steps, Gates: tree.gates, Members: members, Told: tree.told, Merged: merged})
	owedOf := make(map[string]record.MemberNews, len(owed.Members))
	for _, member := range owed.Members {
		owedOf[member.Issue] = member
	}
	holders, owners, err := readMemberClaims(ctx, tx, records, tree.root, members, owedOf)
	if err != nil {
		return nil, err
	}
	drafts := make([]memberDraft, 0, len(members))
	for _, member := range members {
		draft := memberDraft{issue: member}
		news, isOwed := owedOf[member.Key]
		switch {
		case !isOwed:
			draft.state = untoldReason(member, merged[member.Key], steps, tree.gates, toldRow(tree.told, member.Key, record.AudienceHolder))
		case news.HeldByArchitect:
			draft.owed, draft.state = news.Items, owners[member.Key]
		default:
			draft.owed, draft.state = news.Items, holders[member.Key]
		}
		drafts = append(drafts, draft)
	}
	return drafts, nil
}

// readMemberClaims reads what says how each member owed news is told: for a member whose phase a
// worker holds, whether its claim is the one a news-only start reaches now; for one in accepting,
// which architect owns it, by the walk notices take (owningArchitect) over the sub-architect claims
// the record holds. A member whose claim token cannot be named is told at its next start.
func readMemberClaims(ctx context.Context, tx pgx.Tx, records record.Store, root record.Issue, members []record.Issue,
	owed map[string]record.MemberNews) (holders, owners map[string]workflow.NewsState, err error) {
	holders, owners = map[string]workflow.NewsState{}, map[string]workflow.NewsState{}
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		return nil, nil, err
	}
	tree := make(map[string]record.Issue, len(members))
	var keys []record.RoleRunKey
	var architects []claim.Token
	entries := map[claim.Token]int64{}
	for _, member := range members {
		tree[member.Key] = member
		if token, err := claim.NewToken(project, member.Key, claim.RoleArchitect); err == nil {
			architects = append(architects, token)
		}
		news, isOwed := owed[member.Key]
		if !isOwed || news.HeldByArchitect {
			continue
		}
		holders[member.Key] = workflow.NewsAtNextStart
		role := phase.RoleFor(member.Phase)
		token, err := claim.NewToken(project, member.Key, role)
		if err != nil {
			continue
		}
		rows, err := records.Phases(ctx, tx, member.Key)
		if err != nil {
			return nil, nil, err
		}
		entries[token] = record.PhaseRowOf(rows, role).Entry
		keys = append(keys, record.RoleRunKey{Token: token, Issue: member.Key, Role: role, Generation: member.Generation})
	}
	runs, err := records.RoleRuns(ctx, tx, keys)
	if err != nil {
		return nil, nil, err
	}
	for _, key := range keys {
		holders[key.Issue] = holderTold(runs[key.Token], root, entries[key.Token])
	}
	held, err := records.RoleClaims(ctx, tx, architects)
	if err != nil {
		return nil, nil, err
	}
	architectRuns := func(token claim.Token) bool { return recordedClaimRuns(held[token]) }
	rootArchitect, err := claim.NewToken(project, root.Key, claim.RoleArchitect)
	if err != nil {
		return nil, nil, err
	}
	for key, news := range owed {
		if !news.HeldByArchitect {
			continue
		}
		owners[key] = workflow.NewsSentNow
		if owner, err := owningArchitect(project, tree, tree[key], "", architectRuns); err != nil || owner == rootArchitect {
			owners[key] = workflow.NewsYouAnswer
		}
	}
	return holders, owners, nil
}

// holderTold is how the holder of a member owed news is told, from its role's run: by a task now
// when its claim was given its phase's current entry and runs, the claim a news-only start
// reaches; by the next start of its phase when the claim is owed that entry's start
// (workflow.OwedEntry), or there is no claim, or it retired, failed, or an operator suspended it,
// since the start that revives it carries the news.
func holderTold(run record.RoleRun, root record.Issue, entry int64) workflow.NewsState {
	held := run.Claim
	switch {
	case workflow.OwedEntry(run, root, entry), held == nil, claimEnded(held.State),
		held.State == supervise.StateSuspended && held.SuspendedBy == supervise.SuspendedByOperator:
		return workflow.NewsAtNextStart
	}
	return workflow.NewsSentNow
}

// recordedClaimRuns is claimRuns over a claim as the record holds it: it has not ended, and it is
// not suspended, but for a released claim, which keeps its role while its pod is released
// (holdsRole). A token the record holds no claim of runs nothing.
func recordedClaimRuns(held *record.RoleClaim) bool {
	if held == nil {
		return false
	}
	if held.State == supervise.StateSuspended {
		return held.SuspendedBy == supervise.SuspendedByReleased
	}
	return !claimEnded(held.State)
}

// untoldReason is why a member OwedNews owes nothing is told nothing (readTreeMembers), read from
// its record, whether its pull request merged, and its holder's told row against the gate. A
// member none of its reasons explain is one OwedNews holds back for the standing gate, which is
// closed: a lingering tree, the rule's other hold, gets no summary at all (readNewsAtSend).
func untoldReason(member record.Issue, merged bool, steps []record.DesignStep, gates map[record.DesignStep]record.DesignGate, told record.DesignTold) workflow.NewsState {
	concerned, other := false, false
	for _, step := range record.TreeSteps(steps, gates) {
		gate := gates[step]
		if gate.ApprovedVersion == nil {
			continue
		}
		if took, known := told.Took(step); known && took >= *gate.ApprovedVersion {
			continue
		}
		if step != record.DesignStepCriteria || claim.IsTreeRoot(member.Key, member.Tree) {
			concerned = true
		} else {
			other = true
		}
	}
	switch {
	case !concerned && other:
		return workflow.NewsNotConcerned
	case !concerned:
		return workflow.NewsAlreadyTaken
	case merged:
		return workflow.NewsMerged
	case member.Phase == phase.Done:
		return workflow.NewsDone
	case !record.InWorkflow(member):
		return workflow.NewsOutOfWorkflow
	case member.Phase == phase.Admitted:
		return workflow.NewsAdmitted
	case member.Phase == phase.AwaitingPerson:
		return workflow.NewsAwaitingPerson
	case member.Phase == phase.Held:
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
