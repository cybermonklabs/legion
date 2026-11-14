package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
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
			if err := records.RaiseTold(r.ctx, tx, told.issue, told.audience, told.versions, time.Now()); err != nil {
				return err
			}
		}
		if err := records.SetOpeningOwed(r.ctx, tx, newsRoot, record.AudienceHolder, true); err != nil {
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
	if err := pgx.BeginFunc(r.ctx, r.pool, write); err != nil {
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

// send is the daemon's answer for a delivery to issue's claim of role carrying news.
func (r *newsRig) send(issue string, role claim.Role, news record.DeliveryNews) (string, *record.DeliveryNews) {
	r.t.Helper()
	encoded, err := json.Marshal(news)
	if err != nil {
		r.t.Fatal(err)
	}
	c := supervise.Claim{Token: mustClaimToken(r.t, issue, role), Project: "legion", Tree: newsRoot, Issue: issue, Role: role}
	task, carried, err := r.runtime.designNews(r.ctx, c, supervise.Delivery{ID: "outbox:9", Task: "queued text", News: encoded})
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

// newsItem is the news of version to of step's document to issue's audience, which took from.
func newsItem(issue string, audience record.DesignAudience, step record.DesignStep, artifact string, from, to int) record.DeliveryNewsItem {
	return record.DeliveryNewsItem{Issue: issue, Audience: audience, Step: step, Artifact: artifact, From: new(from), To: to}
}

// The text of a task carrying news is rendered at its send from the record as it stands then.
// An item stands while the gate's approved version of its step is still the version it tells and
// its audience has not taken it; it renders with the change set recorded for its pair. An item the
// gate approved past since is dropped, and so is one its audience already took, and one whose
// change set is not recorded yet, each from the text and from the news the delivery goes on
// carrying, and a send that drops one queues the reconcile of its tree's news once, which weighs
// again what the tree is owed now. A phase start left with no news is sent as its own text; a task
// queued for its news alone is left with nothing to say. And the gate sentence is said of the gate
// at the send: a task composed while the gate stood open and sent after a later version closed it
// says closed.
func TestTheNewsATaskCarriesIsWhatTheRecordSaysAtItsSend(t *testing.T) {
	r := newNewsRig(t)
	const ownText = "Implement the plan. Issue: LEGION-500. Phase: implementing."
	standing := newsItem(newsRoot, record.AudienceHolder, record.DesignStepSpec, "artifact-spec", 3, 5)
	approvedPast := newsItem(newsRoot, record.AudienceHolder, record.DesignStepCriteria, "artifact-checks", 2, 3)
	implementer := record.DeliveryNews{Task: ownText, Items: []record.DeliveryNewsItem{standing, approvedPast},
		Openings: []record.DeliveryOpening{{Issue: newsRoot, Audience: record.AudienceHolder}}}

	task, carried := r.send(newsRoot, claim.RoleImplementer, implementer)

	if !strings.HasPrefix(task, ownText+"\n\nThe design gate of LEGION-500 opened: a person approved version 5 of its spec document (artifact-spec)") {
		t.Fatalf("task = %q, want the start's own text, a blank line, then the opened gate sentence", task)
	}
	for _, want := range []string{"version 5, since version 3, which your phase last took", `"Design" changed`, "r2 changed"} {
		if !strings.Contains(task, want) {
			t.Fatalf("task = %q, want the standing spec item rendered with its recorded change set (%q)", task, want)
		}
	}
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
	reviewer := record.DeliveryNews{NewsOnly: true,
		Items: []record.DeliveryNewsItem{newsItem(newsReviewed, record.AudienceHolder, record.DesignStepSpec, "artifact-spec", 3, 5)}}
	if task, carried := r.send(newsReviewed, claim.RoleReviewer, reviewer); task != "" || carried != nil {
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

	if !strings.HasPrefix(task, ownText+"\n\nThe design gate of LEGION-500 closed: version 6 of its spec document (artifact-spec) is with its reviewer") {
		t.Fatalf("task = %q, want the closed gate sentence, said of the gate at the send", task)
	}
	if strings.Contains(task, "opened") || !strings.Contains(task, "version 5, since version 3") {
		t.Fatalf("task = %q, want no opened sentence and the standing spec item still told", task)
	}
	if carried == nil || !reflect.DeepEqual(carried.Items, []record.DeliveryNewsItem{standing}) {
		t.Fatalf("the task still carries %+v, want the standing spec item", carried)
	}
	// Every later send dropped an item too, and none queued a second reconcile beside the one still
	// to run.
	if err := r.pool.QueryRow(r.ctx, `select count(*) from outbox where kind = 'design_changes'`).Scan(&queued); err != nil || queued != 1 {
		t.Fatalf("design_changes rows after every send = %d (%v), want the one still to run", queued, err)
	}
}

// The root architect's summary names every member of its tree with its phase and what it is told
// as the record stands at the send: the root's implementer owed the spec and told by the next start
// of its phase, since no claim holds it; a reviewer told nothing because only the root's checks
// changed for it; a member not started yet; and a tester owed news whose change set is not recorded.
func TestTheRootArchitectsSummaryNamesEachMemberAsTheRecordStandsAtItsSend(t *testing.T) {
	r := newNewsRig(t)
	const ownText = "A person approved a new version of the design of LEGION-500."
	summary := record.DeliveryNews{Task: ownText, NewsOnly: true,
		Items: []record.DeliveryNewsItem{newsItem(newsRoot, record.AudienceArchitect, record.DesignStepSpec, "artifact-spec", 3, 5)}}

	task, carried := r.send(newsRoot, claim.RoleArchitect, summary)

	for _, want := range []string{
		ownText + "\n\nA person approved a new version of the design of LEGION-500. What changed",
		"version 5, since version 3, which you last took",
		"- LEGION-500, in implementing: owed version 5 of the spec document, since version 3, which its phase last took; " +
			"the next start of its implementer carries it.",
		"- LEGION-501, in reviewing: told nothing: only the root's checks changed",
		"- LEGION-502, in testing: owed news of the approved design; the next start of its tester carries it.",
		"- LEGION-503, in admitted: told nothing: it has not started",
	} {
		if !strings.Contains(task, want) {
			t.Fatalf("summary = %q, want it to say %q", task, want)
		}
	}
	if carried == nil || len(carried.Items) != 1 || !carried.NewsOnly {
		t.Fatalf("the summary still carries %+v, want its one standing item", carried)
	}
}

// A member whose owed news is decision blocks alone is told nothing, and the summary says so: the
// reconcile raises what its holder took and sends nothing. A member whose holder row also owes the
// opening is sent a task with it, which says nothing its issue holds changed, so the summary names
// that member as owed and how its holder is told, never as told nothing.
func TestTheRootArchitectsSummaryNamesADecisionOnlyMemberOwedTheOpeningAsTold(t *testing.T) {
	r := newNewsRig(t)
	r.inTx(func(tx pgx.Tx) error {
		return r.records.PutDesignChanges(r.ctx, tx, record.DesignChanges{
			VersionPair: record.VersionPair{ArtifactID: "artifact-spec", From: 4, To: 5}, State: record.DesignChangesRead,
			DecisionOnly: true, ReadAt: time.Now()})
	})
	const tested = "- LEGION-502, in testing: "
	line := func(task string) string {
		t.Helper()
		for _, line := range strings.Split(task, "\n") {
			if strings.HasPrefix(line, tested) {
				return line
			}
		}
		t.Fatalf("summary = %q, want a line for LEGION-502", task)
		return ""
	}

	task, _ := r.send(newsRoot, claim.RoleArchitect, rootSummary())
	if got, want := line(task), tested+"told nothing: the approved version differs from the one it took only in decision blocks."; got != want {
		t.Fatalf("the tester's line, owed no opening = %q, want %q", got, want)
	}

	r.inTx(func(tx pgx.Tx) error {
		return r.records.SetOpeningOwed(r.ctx, tx, newsTested, record.AudienceHolder, true)
	})
	task, _ = r.send(newsRoot, claim.RoleArchitect, rootSummary())
	if got, want := line(task), tested+"owed version 5 of the spec document, since version 4, which its phase last took; "+
		"the next start of its tester carries it."; got != want {
		t.Fatalf("the tester's line, owed the opening = %q, want %q", got, want)
	}
}

// rootSummary is the root architect's news-only summary of spec version 5, since version 3.
func rootSummary() record.DeliveryNews {
	return record.DeliveryNews{Task: "A person approved a new version of the design of LEGION-500.", NewsOnly: true,
		Items: []record.DeliveryNewsItem{newsItem(newsRoot, record.AudienceArchitect, record.DesignStepSpec, "artifact-spec", 3, 5)}}
}

// A summary sent after a later version closed the gate again still tells the architect the
// approval it names, which stands below the new version, but no member it was owed to is told
// anything while the gate is closed, and no start will tell it: each such member is told nothing
// yet, and why, and no line says a task or a start carries its news.
func TestTheRootArchitectsSummarySentAfterTheGateClosedAgainTellsOwedMembersNothingYet(t *testing.T) {
	r := newNewsRig(t)
	r.gate(record.DesignStepSpec, "artifact-spec", 6, 5)

	task, carried := r.send(newsRoot, claim.RoleArchitect, rootSummary())

	const nothingYet = "told nothing yet: the gate closed again before this was sent; what it is owed goes once a person approves " +
		"the version now with its reviewer."
	for _, want := range []string{
		"version 5, since version 3, which you last took",
		"- LEGION-500, in implementing: " + nothingYet,
		"- LEGION-502, in testing: " + nothingYet,
		"- LEGION-501, in reviewing: told nothing: only the root's checks changed",
		"- LEGION-503, in admitted: told nothing: it has not started",
	} {
		if !strings.Contains(task, want) {
			t.Fatalf("summary = %q, want it to say %q", task, want)
		}
	}
	for _, never := range []string{"the next start of", "a task tells"} {
		if strings.Contains(task, never) {
			t.Fatalf("summary = %q, want no member said to be told by %q while the gate is closed", task, never)
		}
	}
	if carried == nil || len(carried.Items) != 1 {
		t.Fatalf("the summary still carries %+v, want its one standing item", carried)
	}
}

// Linger holds every member of a tree where it stood, and nothing goes to a lingering tree: a
// summary queued for its news alone and sent once the root lingers has nothing to say, and the
// machine retires it rather than sends it.
func TestALingeringTreesSummaryIsLeftWithNothingToSay(t *testing.T) {
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

	if task, carried := r.send(newsRoot, claim.RoleArchitect, rootSummary()); task != "" || carried != nil {
		t.Fatalf("the lingering tree's summary = %q carrying %+v, want nothing left to say", task, carried)
	}
}
