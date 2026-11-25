package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

const (
	newsRoot     = "LEGION-500"
	newsReviewed = "LEGION-501"
	newsTested   = "LEGION-502"
	newsAdmitted = "LEGION-503"
)

// newsRig is one tree on the record store of a database of its own, gated on its spec by the
// deployment and on its Verification document by its root's registration, and the daemon's answer
// to supervise's Deps.DesignNews over it (workflowRuntime.designNews).
type newsRig struct {
	t       *testing.T
	ctx     context.Context
	pool    *store.Pool
	records record.Store
	runtime *workflowRuntime
}

func newNewsRig(t *testing.T) *newsRig {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	r := &newsRig{t: t, ctx: context.Background(), pool: pool, records: records,
		runtime: &workflowRuntime{pool: pool, records: records, gatedSteps: []record.DesignStep{record.DesignStepSpec}}}
	root := newsRoot
	for i, member := range []struct {
		key   string
		phase phase.Phase
	}{{newsRoot, phase.Implementing}, {newsReviewed, phase.Reviewing}, {newsTested, phase.Testing}, {newsAdmitted, phase.Admitted}} {
		issue := record.Issue{Key: member.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
			Project: heldTaskProject, Tree: newsRoot, Title: member.key, Phase: member.phase, Generation: 1, Status: "in_progress",
			Rank: string(rune('A' + i)), LastDispatchSeq: 1}
		if member.key != newsRoot {
			issue.Parent = &root
		}
		putOutboxIssue(t, pool, records, issue)
	}
	// The spec stands approved at version 5, the version Dispatch recorded last, and the
	// Verification document at version 4.
	r.gate(record.DesignStepSpec, "artifact-spec", 5, 5)
	r.gate(record.DesignStepCriteria, "artifact-checks", 4, 4)
	r.inTx(func(tx pgx.Tx) error {
		for _, told := range []struct {
			issue    string
			audience record.DesignAudience
			versions map[record.DesignStep]int
		}{
			{newsRoot, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 3, record.DesignStepCriteria: 2}},
			{newsRoot, record.AudienceArchitect, map[record.DesignStep]int{record.DesignStepSpec: 3, record.DesignStepCriteria: 4}},
			{newsReviewed, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}},
			{newsTested, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 4}},
		} {
			if err := records.RaiseTold(r.ctx, tx, told.issue, told.audience, raisedTo(told.versions), time.Now()); err != nil {
				return err
			}
		}
		if err := records.SettleOpening(r.ctx, tx, newsRoot, record.AudienceHolder, true); err != nil {
			return err
		}
		// What changed in the spec from version 3 to 5 is recorded; from 4 to 5 is not yet.
		return records.PutDesignChanges(r.ctx, tx, record.DesignChanges{
			VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 3, To: 5}, State: record.DesignChangesRead,
			Sections: []record.SectionChange{{Heading: "Design", Change: "changed"}},
			Blocks:   []record.BlockChange{{ID: "r2", Type: "requirement", Change: "changed"}}, ReadAt: time.Now()})
	})
	return r
}

func (r *newsRig) inTx(write func(pgx.Tx) error) {
	r.t.Helper()
	if err := store.Transact(r.ctx, r.pool, func(_ context.Context, tx pgx.Tx) error { return write(tx) }); err != nil {
		r.t.Fatalf("write the record: %v", err)
	}
}

// gate writes the root's row of step: document artifact at version latest, approved at approved.
func (r *newsRig) gate(step record.DesignStep, artifact string, latest, approved int) {
	r.t.Helper()
	seq := int64(30)
	if step == record.DesignStepSpec {
		seq = 31
	}
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutGate(r.ctx, tx, record.DesignGate{Issue: newsRoot, Step: step, ArtifactID: artifact,
			LatestVersion: latest, ApprovedVersion: new(approved), ApprovedMark: record.EventMark{Seq: seq}, EverOpened: true})
	})
}

// send is the daemon's answer for a phase task to issue's claim of role carrying news.
func (r *newsRig) send(issue string, role claim.Role, news record.DeliveryNews) (string, *record.DeliveryNews) {
	r.t.Helper()
	return r.sendAs(issue, role, news, false)
}

// sendAlone is the daemon's answer for a task queued for news alone (supervise.Delivery.NewsOnly)
// to issue's claim of role.
func (r *newsRig) sendAlone(issue string, role claim.Role, news record.DeliveryNews) (string, *record.DeliveryNews) {
	r.t.Helper()
	return r.sendAs(issue, role, news, true)
}

// sendAs is the daemon's answer for a delivery to issue's claim of role carrying news, queued for
// that news alone where newsOnly says so.
func (r *newsRig) sendAs(issue string, role claim.Role, news record.DeliveryNews, newsOnly bool) (string, *record.DeliveryNews) {
	r.t.Helper()
	encoded, err := json.Marshal(news)
	if err != nil {
		r.t.Fatal(err)
	}
	c := supervise.Claim{Token: mustClaimToken(r.t, issue, role), Project: "legion", Tree: newsRoot, Issue: issue, Role: role}
	task, carried, err := r.runtime.designNews(r.ctx, c, supervise.Delivery{ID: "outbox:9", Task: "queued text", News: encoded, NewsOnly: newsOnly})
	if err != nil {
		r.t.Fatalf("the news the task to %s carries: %v", issue, err)
	}
	if carried == nil {
		return task, nil
	}
	var decoded record.DeliveryNews
	if err := json.Unmarshal(carried, &decoded); err != nil {
		r.t.Fatalf("decode the news the task to %s still carries: %v", issue, err)
	}
	return task, &decoded
}

// newsItem is the news of version to of step's document to issue's audience, which took from,
// rendered in the run every rig here records its issues in: generation 1 of the issue and of its
// tree's root.
func newsItem(issue string, audience record.DesignAudience, step record.DesignStep, artifact string, from, to int) record.DeliveryNewsItem {
	return record.DeliveryNewsItem{Issue: issue, Audience: audience, Step: step, Artifact: artifact, From: new(from), To: to,
		Generation: 1, TreeGeneration: 1}
}

// The text of a task carrying news is rendered at its send from the record as it stands then.
// An item stands while the gate's approved version of its step is still the version it tells and
// its audience has not taken it; it renders with the change set recorded for its pair. An item the
// gate approved past since is dropped, and so is one its audience already took, and one whose
// change set is not recorded yet, each from the text and from the news the delivery goes on
// carrying, and a send that drops one queues the reconcile of its tree's news once, which weighs
// again what the tree is owed now. A phase start left with no news is sent as its own text; a task
// queued for its news alone is left with nothing to say. And the gate's fields are said of the gate
// at the send: a task composed while the gate stood open and sent after a later version closed it
// says closed.
func TestTheNewsATaskCarriesIsWhatTheRecordSaysAtItsSend(t *testing.T) {
	t.Parallel()
	r := newNewsRig(t)
	const ownText = "Implement the plan. Issue: LEGION-500. Phase: implementing."
	standing := newsItem(newsRoot, record.AudienceHolder, record.DesignStepSpec, "artifact-spec", 3, 5)
	approvedPast := newsItem(newsRoot, record.AudienceHolder, record.DesignStepCriteria, "artifact-checks", 2, 3)
	implementer := record.DeliveryNews{Task: ownText, Items: []record.DeliveryNewsItem{standing, approvedPast},
		Openings: []record.DeliveryOpening{{Issue: newsRoot, Audience: record.AudienceHolder}}}

	task, carried := r.send(newsRoot, claim.RoleImplementer, implementer)

	if !strings.HasPrefix(task, ownText+"\n\n"+newsKindLine+"\n") {
		t.Fatalf("task = %q, want the start's own text, then the news record", task)
	}
	rec := oneNewsRecord(t, task)
	if got, want := rec.gate(), "open spec artifact-spec 5 merge=released"; got != want {
		t.Fatalf("the task's gate = %q, want %q:\n%s", got, want, task)
	}
	wantNewsLines(t, "items", rec.items(), []string{"spec artifact-spec 5 taken=3 listed"})
	wantNewsLines(t, "changes", rec.named(), []string{"0 Design changed", "0 requirement r2 changed"})
	if strings.Contains(task, "artifact-checks") {
		t.Fatalf("task = %q, want no word of the Verification document item the gate has approved past", task)
	}
	if carried == nil || !reflect.DeepEqual(carried.Items, []record.DeliveryNewsItem{standing}) || len(carried.Openings) != 1 {
		t.Fatalf("the task still carries %+v, want the standing spec item and the opening alone", carried)
	}
	var queued int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where kind = 'design_changes' and issue = $1`, newsRoot).Scan(&queued); err != nil {
		t.Fatalf("count the design_changes rows: %v", err)
	}
	if queued != 1 {
		t.Fatalf("design_changes rows after a send that dropped an item = %d, want one for the tree", queued)
	}

	// The reviewer's holder already took version 5, so a task queued for that news alone is left
	// with nothing to say.
	reviewer := record.DeliveryNews{
		Items: []record.DeliveryNewsItem{newsItem(newsReviewed, record.AudienceHolder, record.DesignStepSpec, "artifact-spec", 3, 5)}}
	if task, carried := r.sendAlone(newsReviewed, claim.RoleReviewer, reviewer); task != "" || carried != nil {
		t.Fatalf("the reviewer's news-only task = %q carrying %+v, want nothing left to say", task, carried)
	}

	// The tester's pair, from version 4 to 5, has no change set recorded yet: its phase start is
	// sent as its own text, carrying no news, and the item stays owed for a later start.
	const testerText = "Test the change. Issue: LEGION-502. Phase: testing."
	tester := record.DeliveryNews{Task: testerText,
		Items: []record.DeliveryNewsItem{newsItem(newsTested, record.AudienceHolder, record.DesignStepSpec, "artifact-spec", 4, 5)}}
	if task, carried := r.send(newsTested, claim.RoleTester, tester); task != testerText || carried != nil {
		t.Fatalf("the tester's task = %q carrying %+v, want its own text alone and no news", task, carried)
	}

	// The writer saves version 6 of the spec after the implementer's task was queued: the gate
	// closes, and the task sent now says so. The approval of version 5 still stands below it.
	r.gate(record.DesignStepSpec, "artifact-spec", 6, 5)

	task, carried = r.send(newsRoot, claim.RoleImplementer, implementer)

	if !strings.HasPrefix(task, ownText+"\n\n"+newsKindLine+"\n") {
		t.Fatalf("task = %q, want the start's own text, then the news record", task)
	}
	rec = oneNewsRecord(t, task)
	if got, want := rec.gate(), "closed spec artifact-spec 6 merge=held"; got != want {
		t.Fatalf("the task's gate = %q, want %q, said of the gate at the send:\n%s", got, want, task)
	}
	wantNewsLines(t, "items", rec.items(), []string{"spec artifact-spec 5 taken=3 listed"})
	if carried == nil || !reflect.DeepEqual(carried.Items, []record.DeliveryNewsItem{standing}) {
		t.Fatalf("the task still carries %+v, want the standing spec item", carried)
	}
	// Every later send dropped an item too, and none queued a second reconcile beside the one still
	// to run.
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where kind = 'design_changes'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("design_changes rows after every send = %d (%v), want the one still to run", queued, err)
	}
}

// An item whose told row knows no version of its step carries no earlier version. At its send, an
// item that carries no unread review history either (record.DeliveryNewsItem.HistoryUnread) is read
// as a start reads the record: it is dropped, as one whose change set is not recorded is, until the
// reconcile has recorded its review history unread (record.DesignHistoryUnread), and the send
// queues its tree's reconcile, which asks the history again. An unread row recorded while that
// reconcile is still to run tells nothing either, since the reconcile replaces it. Once the
// reconcile has run and the history stands unread, the item stands: the text says the version the
// holder last took could not be read, and why, never that the version is a first approval, and the
// delivery goes on carrying the item with no earlier version and that unread history.
func TestAnItemWhoseReviewHistoryIsRecordedUnreadIsSentSoAndOneNotYetReadIsDropped(t *testing.T) {
	t.Parallel()
	r := newNewsRig(t)
	r.inTx(func(tx pgx.Tx) error {
		_, err := tx.Exec(r.ctx, `update design_news_told set versions = '{}'::jsonb where issue = $1 and audience = 'holder'`, newsTested)
		return err
	})
	const testerText = "Test the change. Issue: LEGION-502. Phase: testing."
	unknown := record.DeliveryNewsItem{Issue: newsTested, Audience: record.AudienceHolder, Step: record.DesignStepSpec, Artifact: "artifact-spec", To: 5}
	tester := record.DeliveryNews{Task: testerText, Items: []record.DeliveryNewsItem{unknown}}

	if task, carried := r.send(newsTested, claim.RoleTester, tester); task != testerText || carried != nil {
		t.Fatalf("the tester's task with its history neither read nor recorded unread = %q carrying %+v, want its own text alone and no news", task, carried)
	}
	var queued int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where kind = 'design_changes' and issue = $1`, newsRoot).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("design_changes rows after the send dropped the item = %d (%v), want one for the tree", queued, err)
	}

	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutDesignHistoryUnread(r.ctx, tx, record.DesignHistoryUnread{
			DesignHistoryQuestion: record.DesignHistoryQuestion{ArtifactID: "artifact-spec", To: 5}, ReadAt: time.Now(),
			Reason: "Dispatch failed 3 reads of the document's review history, the last with: dispatch: 502 BAD_GATEWAY"})
	})
	if task, carried := r.send(newsTested, claim.RoleTester, tester); task != testerText || carried != nil {
		t.Fatalf("the tester's task with its history recorded unread and the tree's reconcile still to run = %q carrying %+v, want its own text alone and no news", task, carried)
	}

	// The reconcile runs and finishes, its last attempt leaving the history unread.
	if _, err := r.pool.Exec(r.ctx, `delete from outbox where kind = 'design_changes' and issue = $1`, newsRoot); err != nil {
		t.Fatalf("finish the tree's reconcile: %v", err)
	}
	task, carried := r.send(newsTested, claim.RoleTester, tester)
	if !strings.HasPrefix(task, testerText+"\n\n"+newsKindLine+"\n") {
		t.Fatalf("the tester's task with its history recorded unread = %q, want its own text, then the news record", task)
	}
	wantNewsLines(t, "items", oneNewsRecord(t, task).items(), []string{`spec artifact-spec 5 taken=null no_pair ` +
		`reason="Dispatch failed 3 reads of the document's review history, the last with: dispatch: 502 BAD_GATEWAY"`})
	if carried == nil || len(carried.Items) != 1 || carried.Items[0].From != nil || carried.Items[0].HistoryUnread == nil ||
		!strings.Contains(carried.Items[0].HistoryUnread.Reason, "502 BAD_GATEWAY") {
		t.Fatalf("the tester's task still carries %+v, want the item with no earlier version and the unread history it takes on", carried)
	}
}

// An item a delivery carries with its review history unread is told by what it carries at every
// send, never by whether the record still holds that history unread: a reconcile pending to read
// it again holds back what a start attaches, not what a delivery already carries, so a re-send
// tells the reason it was attached with. Once the told row has come to know a version of the step,
// the item is re-keyed to it; while that pair's change set is not recorded, the item is still told
// as unread, and once it is, as that pair, and the delivery goes on carrying the item from that
// version with no unread history.
func TestAResendTellsAnItemByTheUnreadHistoryItCarriesUntilItsPairIsRecorded(t *testing.T) {
	t.Parallel()
	r := newNewsRig(t)
	r.inTx(func(tx pgx.Tx) error {
		_, err := tx.Exec(r.ctx, `update design_news_told set versions = '{}'::jsonb where issue = $1 and audience = 'holder'`, newsTested)
		return err
	})
	const testerText = "Test the change. Issue: LEGION-502. Phase: testing."
	const reason = "Dispatch failed 3 reads of the document's review history, the last with: dispatch: 502 BAD_GATEWAY"
	attached := record.DeliveryNewsItem{Issue: newsTested, Audience: record.AudienceHolder, Step: record.DesignStepSpec, Artifact: "artifact-spec", To: 5,
		Generation: 1, TreeGeneration: 1,
		HistoryUnread: &record.DeliveryHistoryUnread{Reason: reason, ReadAt: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}}
	tester := record.DeliveryNews{Task: testerText, Items: []record.DeliveryNewsItem{attached}}
	unread := `spec artifact-spec 5 taken=null no_pair reason=` + strconv.Quote(reason)

	// A reconcile of the tree is queued, and the record holds no unread row: a start would attach
	// nothing, but the delivery carries the item as it was attached.
	r.inTx(func(tx pgx.Tx) error { return record.QueueDesignNews(r.ctx, r.records, tx, newsRoot, time.Now()) })
	task, carried := r.send(newsTested, claim.RoleTester, tester)
	if !strings.HasPrefix(task, testerText+"\n\n"+newsKindLine+"\n") {
		t.Fatalf("the re-sent task with a reconcile pending = %q, want its own text, then the news record", task)
	}
	wantNewsLines(t, "items with a reconcile pending", oneNewsRecord(t, task).items(), []string{unread})
	if carried == nil || !reflect.DeepEqual(carried.Items, []record.DeliveryNewsItem{attached}) {
		t.Fatalf("the re-sent task carries %+v, want the item as attached", carried)
	}

	// The reconcile reads the history: the told row takes version 4, whose pair is not recorded yet.
	r.inTx(func(tx pgx.Tx) error {
		return r.records.FillTold(r.ctx, tx, newsTested, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 4}, time.Now())
	})
	task, carried = r.send(newsTested, claim.RoleTester, tester)
	wantNewsLines(t, "items with the pair from version 4 not recorded", oneNewsRecord(t, task).items(), []string{unread})
	rekeyed := attached
	rekeyed.From = new(4)
	if carried == nil || !reflect.DeepEqual(carried.Items, []record.DeliveryNewsItem{rekeyed}) {
		t.Fatalf("the re-sent task carries %+v, want the item re-keyed to version 4, its unread history kept", carried)
	}

	// The pair from version 4 is recorded: the re-send tells it, and lets the unread history go.
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutDesignChanges(r.ctx, tx, record.DesignChanges{
			VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 4, To: 5}, State: record.DesignChangesRead,
			Sections: []record.SectionChange{{Heading: "Rollout", Change: "added"}}, ReadAt: time.Now()})
	})
	task, carried = r.send(newsTested, claim.RoleTester, *carried)
	wantNewsLines(t, "items with the pair from version 4 recorded", oneNewsRecord(t, task).items(), []string{"spec artifact-spec 5 taken=4 listed"})
	wantNewsLines(t, "changes with the pair from version 4 recorded", oneNewsRecord(t, task).named(), []string{"0 Rollout added"})
	if carried == nil || !reflect.DeepEqual(carried.Items, []record.DeliveryNewsItem{newsItem(newsTested, record.AudienceHolder, record.DesignStepSpec, "artifact-spec", 4, 5)}) {
		t.Fatalf("the re-sent task carries %+v, want the item from version 4 with no unread history", carried)
	}
}

// The root architect's summary names every member of its tree with its phase and what it is told
// as the record stands at the send: the root's implementer owed the spec and told by the next start
// of its phase, since no claim holds it; a reviewer told nothing because only the root's checks
// changed for it; a member not started yet; and a tester owed news whose change set is not recorded.
func TestTheRootArchitectsSummaryNamesEachMemberAsTheRecordStandsAtItsSend(t *testing.T) {
	t.Parallel()
	r := newNewsRig(t)
	const ownText = "A person approved a new version of the design of LEGION-500."
	summary := record.DeliveryNews{Task: ownText,
		Items: []record.DeliveryNewsItem{newsItem(newsRoot, record.AudienceArchitect, record.DesignStepSpec, "artifact-spec", 3, 5)}}

	task, carried := r.sendAlone(newsRoot, claim.RoleArchitect, summary)

	if !strings.HasPrefix(task, ownText+"\n\n"+newsKindLine+"\n") {
		t.Fatalf("summary = %q, want its own text, then the news record", task)
	}
	rec := newsRecordOf(t, task, "root_architect", newsRoot)
	wantNewsLines(t, "summary's items", rec.items(), []string{"spec artifact-spec 5 taken=3 yours=true listed"})
	wantNewsLines(t, "summary's members", rec.members(), []string{
		"LEGION-500 implementing told=at_next_start why= holder=implementer architect_of=",
		"LEGION-501 reviewing told=nothing why=root_checks_only holder= architect_of=",
		"LEGION-502 testing told=at_next_start why= holder=tester architect_of=",
		"LEGION-503 admitted told=nothing why=not_started holder= architect_of=",
	})
	// The tester's change set is not recorded, so it is owed news no item names yet.
	wantNewsLines(t, "summary's owed", rec.owed(), []string{"LEGION-500: spec artifact-spec 5 taken=3 yours=true listed"})
	if carried == nil || len(carried.Items) != 1 {
		t.Fatalf("the summary still carries %+v, want its one standing item", carried)
	}
}

// A member whose Dispatch status a person moved out of the workflow is owed nothing
// (record.UntoldOutOfWorkflow), whatever version its holder took, and the root architect's summary
// names it told nothing for that reason.
func TestTheRootArchitectsSummaryNamesAMemberOutOfTheWorkflowAsToldNothing(t *testing.T) {
	t.Parallel()
	r := newNewsRig(t)
	r.inTx(func(tx pgx.Tx) error {
		tested, err := r.records.Issue(r.ctx, tx, newsTested)
		if err != nil {
			return err
		}
		tested.Status = "backlog"
		return r.records.PutIssue(r.ctx, tx, *tested)
	})

	task, _ := r.sendAlone(newsRoot, claim.RoleArchitect, rootSummary())
	if got, want := newsRecordOf(t, task, "root_architect", newsRoot).member(newsTested),
		"LEGION-502 testing told=nothing why=out_of_workflow holder= architect_of="; got != want {
		t.Fatalf("the summary's member %s = %q, want %q:\n%s", newsTested, got, want, task)
	}
}

// A member whose owed news is decision blocks alone is told nothing, and the summary says so: the
// reconcile raises what its holder took and sends nothing. A member whose holder row also owes the
// opening is sent a task with it, which says nothing its issue holds changed, so the summary names
// that member as owed and how its holder is told, never as told nothing.
func TestTheRootArchitectsSummaryNamesADecisionOnlyMemberOwedTheOpeningAsTold(t *testing.T) {
	t.Parallel()
	summaryOfAMemberOwedNoWork(t, record.DesignChanges{
		VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 4, To: 5}, State: record.DesignChangesRead,
		DecisionOnly: true, ReadAt: time.Now()})
}

// A member whose owed version holds the same text as the one it took is told nothing, as a
// decision-only one is, and the summary says so in words true of it: the approved version holds
// the same text, and no decision block is said to have changed. A member whose holder row also owes
// the opening is named as owed and how its holder is told, as a decision-only one is.
func TestTheRootArchitectsSummaryNamesAnUnchangedMemberAsToldNothingAndOneOwedTheOpeningAsTold(t *testing.T) {
	t.Parallel()
	summaryOfAMemberOwedNoWork(t, record.DesignChanges{
		VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 4, To: 5}, State: record.DesignChangesRead,
		Unchanged: true, ReadAt: time.Now()})
}

// summaryOfAMemberOwedNoWork records changes, a pair from version 4 to 5 that changes nothing of
// the work (record.DesignChanges.WorkUnchanged), and reads the tester's row of the root
// architect's summary twice: owed no opening, it is told nothing, its work unchanged, which is
// true of either such pair; owed the opening, it is owed the version and its next start carries
// it, and the summary names the pair as changing nothing of the work, with no change row.
func summaryOfAMemberOwedNoWork(t *testing.T, changes record.DesignChanges) {
	t.Helper()
	r := newNewsRig(t)
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutDesignChanges(r.ctx, tx, changes)
	})

	task, _ := r.sendAlone(newsRoot, claim.RoleArchitect, rootSummary())
	rec := newsRecordOf(t, task, "root_architect", newsRoot)
	if got, want := rec.member(newsTested), "LEGION-502 testing told=nothing why=work_unchanged holder= architect_of="; got != want {
		t.Fatalf("the tester's row, owed no opening = %q, want %q", got, want)
	}

	r.inTx(func(tx pgx.Tx) error {
		return r.records.SettleOpening(r.ctx, tx, newsTested, record.AudienceHolder, true)
	})
	task, _ = r.sendAlone(newsRoot, claim.RoleArchitect, rootSummary())
	rec = newsRecordOf(t, task, "root_architect", newsRoot)
	if got, want := rec.member(newsTested), "LEGION-502 testing told=at_next_start why= holder=tester architect_of="; got != want {
		t.Fatalf("the tester's row, owed the opening = %q, want %q", got, want)
	}
	unchanged := texts.NewsChangesDecisionBlocksOnly
	if changes.Unchanged {
		unchanged = texts.NewsChangesSameText
	}
	pair := "spec artifact-spec 5 taken=4 yours=false " + unchanged
	if owed := rec.owed(); !slices.Contains(owed, "LEGION-502: "+pair) {
		t.Fatalf("the summary's owed rows = %q, want the tester owed the pair from version 4, %s", owed, pair)
	}
	for _, named := range rec.named() {
		if !strings.HasPrefix(named, "0 ") {
			t.Fatalf("the summary names a change of the pair from version 4, which changed nothing of the work: %s", named)
		}
	}
}

// rootSummary is the root architect's news-only summary of spec version 5, since version 3.
func rootSummary() record.DeliveryNews {
	return record.DeliveryNews{Task: "A person approved a new version of the design of LEGION-500.",
		Items: []record.DeliveryNewsItem{newsItem(newsRoot, record.AudienceArchitect, record.DesignStepSpec, "artifact-spec", 3, 5)}}
}

// A summary sent after a later version closed the gate again still tells the architect the
// approval it names, which stands below the new version, but no member it was owed to is told
// anything while the gate is closed, and no start will tell it: each such member is told nothing
// yet, and why, and no line says a task or a start carries its news.
func TestTheRootArchitectsSummarySentAfterTheGateClosedAgainTellsOwedMembersNothingYet(t *testing.T) {
	t.Parallel()
	r := newNewsRig(t)
	r.gate(record.DesignStepSpec, "artifact-spec", 6, 5)

	task, carried := r.sendAlone(newsRoot, claim.RoleArchitect, rootSummary())

	rec := newsRecordOf(t, task, "root_architect", newsRoot)
	wantNewsLines(t, "summary's items", rec.items(), []string{"spec artifact-spec 5 taken=3 yours=true listed"})
	wantNewsLines(t, "summary's members", rec.members(), []string{
		"LEGION-500 implementing told=not_yet why=gate_closed holder= architect_of=",
		"LEGION-501 reviewing told=nothing why=root_checks_only holder= architect_of=",
		"LEGION-502 testing told=not_yet why=gate_closed holder= architect_of=",
		"LEGION-503 admitted told=nothing why=not_started holder= architect_of=",
	})
	if carried == nil || len(carried.Items) != 1 {
		t.Fatalf("the summary still carries %+v, want its one standing item", carried)
	}
}

// Linger holds every member of a tree where it stood, and nothing goes to a lingering tree: a
// summary queued for its news alone and sent once the root lingers has nothing to say, and the
// machine retires it rather than sends it.
func TestALingeringTreesSummaryIsLeftWithNothingToSay(t *testing.T) {
	t.Parallel()
	r := newNewsRig(t)
	var root *record.Issue
	r.inTx(func(tx pgx.Tx) error {
		var err error
		root, err = r.records.Issue(r.ctx, tx, newsRoot)
		return err
	})
	lingers := time.Now().Add(time.Hour)
	root.LingerUntil = &lingers
	putOutboxIssue(t, r.pool, r.records, *root)

	if task, carried := r.sendAlone(newsRoot, claim.RoleArchitect, rootSummary()); task != "" || carried != nil {
		t.Fatalf("the lingering tree's summary = %q carrying %+v, want nothing left to say", task, carried)
	}
}
