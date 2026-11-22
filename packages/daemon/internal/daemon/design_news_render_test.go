package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A prompt's recorded news is computed from the text it sends, by the function that renders it
// (renderNews), whichever builder writes the task: a phase entry's start, a start queued for news
// alone, the root architect's entry start, the send of a held task again, and the narrowing of a
// root architect's held task whose decision left the tree. For each, the news the task carries is
// exactly the news its text names: every item it carries is told in the text by its pair, an
// opening it carries says the gate opened, and nothing else is told. The fixed point says the same
// thing whole: rendering the news a builder recorded, as a send would, gives back that very text
// and that very news, so no builder kept a news field beside a text it did not render with it.
func TestEveryBuilderRecordsExactlyTheNewsItsTextNames(t *testing.T) {
	r := newNewsReconcileRig(t)
	root := r.member(reconcileRoot, "", phase.Reviewing)
	child := r.member(reconcileChild, reconcileRoot, phase.Implementing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.told(reconcileRoot, record.AudienceArchitect, 3)
	r.owesOpening(reconcileChild, record.AudienceHolder)
	r.record(record.VersionPair{ArtifactID: reconcileDoc, From: 4, To: 5}, "")
	r.record(record.VersionPair{ArtifactID: reconcileDoc, From: 3, To: 5}, "")
	childToken := r.holder(reconcileChild, claim.RoleImplementer, supervise.StateIdle, "", 1)
	rootToken := mustClaimToken(t, reconcileRoot, claim.RoleArchitect)
	childClaim := supervise.Claim{Token: childToken, Project: "legion", Tree: reconcileRoot, Issue: reconcileChild, Role: claim.RoleImplementer}
	rootClaim := supervise.Claim{Token: rootToken, Project: "legion", Tree: reconcileRoot, Issue: reconcileRoot, Role: claim.RoleArchitect}
	sender := &workflowRuntime{pool: r.pool, records: r.records, gatedSteps: config.DesignGateRootIssues.Steps(),
		dispatchProject: "LEGION", log: quietLogger()}

	start := func(issue record.Issue, token claim.Token, request record.SuperviseRequest) startNews {
		t.Helper()
		started, err := r.runner.newsForStart(r.ctx, issue, request, token)
		if err != nil {
			t.Fatalf("compute the news of %s's start: %v", issue.Key, err)
		}
		return started
	}
	entry := start(child, childToken, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleImplementer,
		Phase: phase.Implementing, Generation: 1, Entry: 2, Task: "Implement the plan. Issue: LEGION-601. Phase: implementing."})
	alone := start(child, childToken, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleImplementer,
		Phase: phase.Implementing, Generation: 1, News: true})

	// The root architect's entry start names two of the tree's kept decisions; the second's row is
	// no longer this delivery's (a newer decision replaced it), so the narrowing drops it.
	at := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	kept := record.DecisionMark{Issue: reconcileRoot, Ref: "dispatch://LEGION-600/ask/ask-0001", Decision: record.DecisionAskAnswered,
		Actor: "user:alice", At: at}
	replaced := record.DecisionMark{Issue: reconcileRoot, Ref: "dispatch://LEGION-600/ask/ask-0002", Decision: record.DecisionAskAnswered,
		Actor: "user:alice", At: at.Add(time.Minute)}
	if _, err := r.pool.Exec(r.ctx, `insert into parked_decisions (issue, ref, decision, actor, at, held_by_claim, held_by_delivery)
		values ($1, $2, $3, $4, $5, $6, 'outbox:7'), ($1, $7, $3, $4, $8, null, null)`,
		reconcileRoot, kept.Ref, string(kept.Decision), kept.Actor, kept.At, string(rootToken), replaced.Ref, at.Add(time.Hour)); err != nil {
		t.Fatalf("keep the root's decisions, the first held by the architect's task: %v", err)
	}
	named, err := json.Marshal(record.KeptDecisions{Decisions: []record.DecisionMark{kept, replaced}})
	if err != nil {
		t.Fatal(err)
	}
	architect := start(root, rootToken, record.SuperviseRequest{Op: "start", Tree: reconcileRoot, Role: claim.RoleArchitect,
		Generation: 1, Entry: 1, Task: "A person decided on the tree while nobody could hear them: " + kept.Ref + " and " + replaced.Ref + "."})
	if !strings.Contains(architect.task, replaced.Ref) || len(architect.news) == 0 {
		t.Fatalf("the architect's entry start = %q carrying %s, want it naming both decisions and carrying news: the case's premise",
			architect.task, architect.news)
	}

	resentTask, resentNews, err := sender.designNews(r.ctx, childClaim, supervise.Delivery{ID: "outbox:2", Task: entry.task,
		Phase: phase.Implementing, Generation: 1, Entry: 2, News: entry.news})
	if err != nil {
		t.Fatalf("send the entry task again: %v", err)
	}
	narrowedTask, _, narrowedNews, err := sender.rootTask(r.ctx, rootClaim, supervise.Delivery{ID: "outbox:7", Task: architect.task,
		Generation: 1, Entry: 1, Kept: named, News: architect.news})
	if err != nil {
		t.Fatalf("narrow the architect's held task: %v", err)
	}
	if strings.Contains(narrowedTask, replaced.Ref) {
		t.Fatalf("the narrowed task %q still names %s, which left this delivery", narrowedTask, replaced.Ref)
	}

	for _, built := range []struct {
		name  string
		claim supervise.Claim
		task  string
		news  []byte
	}{
		{"a phase entry's start", childClaim, entry.task, entry.news},
		{"a start queued for news alone", childClaim, alone.task, alone.news},
		{"the root architect's entry start", rootClaim, architect.task, architect.news},
		{"the send of a held task again", childClaim, resentTask, resentNews},
		{"the narrowing of a root architect's held task", rootClaim, narrowedTask, narrowedNews},
	} {
		t.Run(built.name, func(t *testing.T) {
			wantTextNamesExactly(t, built.task, built.news)
			again, carried, err := sender.designNews(r.ctx, built.claim, supervise.Delivery{ID: "outbox:9", Task: "stale text", News: built.news})
			if err != nil {
				t.Fatalf("render the recorded news again: %v", err)
			}
			if again != built.task || !bytes.Equal(carried, built.news) {
				t.Fatalf("rendering the recorded news again gives %q carrying %s, want the builder's own text %q and news %s",
					again, carried, built.task, built.news)
			}
		})
	}
}

// wantTextNamesExactly fails the test unless text carries news and names exactly that news: the
// start's own text first, each item told by its pair, the opened gate where an opening it carries
// says the gate opened, and no item told that the news does not carry.
func wantTextNamesExactly(t *testing.T, text string, encoded []byte) {
	t.Helper()
	if len(encoded) == 0 {
		t.Fatalf("the task %q carries no news, want news", text)
	}
	var news record.DeliveryNews
	if err := json.Unmarshal(encoded, &news); err != nil {
		t.Fatalf("decode the news %s: %v", encoded, err)
	}
	if !strings.HasPrefix(text, news.Task) {
		t.Fatalf("the task %q does not open with the start's own text the news carries, %q", text, news.Task)
	}
	told := text[len(news.Task):]
	var pairs []string
	for _, item := range news.Items {
		if item.From == nil {
			t.Fatalf("the news %s carries an item with no earlier version, which this case never owes", encoded)
		}
		pair := fmt.Sprintf("version %d, since version %d, which", item.To, *item.From)
		if !strings.Contains(told, pair) {
			t.Fatalf("the task %q does not tell %+v, which the news it carries names", text, item)
		}
		pairs = append(pairs, pair)
	}
	if got, want := strings.Count(told, ", which you last took")+strings.Count(told, ", which your phase last took"), len(pairs); got != want {
		t.Fatalf("the task %q tells %d items by their pair, want the %d the news carries", text, got, want)
	}
	opened := false
	for _, opening := range news.Openings {
		opened = opened || !opening.Closed
	}
	if saysOpened := strings.Contains(told, "opened:"); saysOpened != opened {
		t.Fatalf("the task %q says the gate opened: %t, want %t, as the openings it carries say (%+v)",
			text, saysOpened, opened, news.Openings)
	}
}
