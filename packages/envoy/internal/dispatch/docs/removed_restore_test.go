package docs

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

// removedRestoreAsk is an ask block of the fixture of TestTheRestoreTakesBackOnlyWhatNoPersonRemoved.
func removedRestoreAsk(id, question string) string {
	return ":::ask{#" + id + " urgency=\"med\" multiple=\"false\"}\n" + question + "\n:::\n\n"
}

// The restore takes back only an ask whose block left its spec in a version no person wrote, and
// never writes again an answered ask a person removed (R6). Four answered asks leave the spec, each
// in a version of its own: the first in a version whose authors name a person, the second in one
// whose authors name an agent whose act holds the person scope, the third in one whose authors name
// a session with no act - an agent's drop, which the restore exists to undo - and the fourth through
// this build's own write by a person, which leaves an asserted removed_by on its node. The first
// two versions are recorded as the release before recorded them: authors and an artifact.version
// event, and no removal. The decisions document holds a section that cites the first ask and
// nothing else, and an ask block an agent's session put there.
//
// Three passes of the step restore the third ask alone and move the second shape's block, decline
// the first two once each - a removed_by to that person, under the event of the version that left
// the block out - and touch the fourth, whose one removed_by stays the one the person's write left.
// No pass after the first writes anything. The section that cites the first ask is deleted by R9's
// rule for it: its heading's one moved_to reaches the ask's own node, and its words are written
// nowhere in the spec; the decisions document is retired, and the step owes nothing more.
func TestTheRestoreTakesBackOnlyWhatNoPersonRemoved(t *testing.T) {
	ctx := context.Background()
	const requirement = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"
	blocks := map[string]string{
		"q-person": removedRestoreAsk("q-person", "Which store holds the corpus?"),
		"q-scoped": removedRestoreAsk("q-scoped", "Which reader reads it first?"),
		"q-agent":  removedRestoreAsk("q-agent", "Which colour does the census print in?"),
		"q-marked": removedRestoreAsk("q-marked", "Which build seeds the corpus?"),
	}
	order := []string{"q-person", "q-scoped", "q-agent", "q-marked"}
	spec := func(kept ...string) string {
		markdown := requirement
		for _, id := range order {
			if slices.Contains(kept, id) {
				markdown += blocks[id]
			}
		}
		return markdown
	}
	issue := newRestoreIssue(t, spec(order...))
	asked := map[string]string{}
	for _, id := range order {
		asked[id] = issue.answer(issue.spec, id, "The answer to "+id+".")
	}
	name := map[string]string{}
	for id, ask := range asked {
		name[ask] = id
	}

	// The three drops of the release before, each a version of its own recorded as that release
	// recorded it, and the person's drop through this build's own write.
	carol := model.Actor{Kind: "user", ID: "carol"}
	owner := "dave"
	scoped := model.Actor{Kind: "session", ID: "ses-scoped", Owner: &owner, Proven: true,
		Act: &model.ActorAct{Session: "ses-scoped", SessionProven: true, Scopes: model.Scopes}}
	versionEvents := map[string]int64{}
	dropAs := func(id string, authors *model.Actor, kept ...string) {
		t.Helper()
		issue.replace(issue.spec, spec(kept...))
		number := latestVersionNumber(t, issue.service, issue.spec)
		if authors != nil {
			if _, err := issue.database.Pool.Exec(ctx, `update artifact_versions set authors = jsonb_build_array($3::jsonb) where artifact_id = $1 and number = $2`,
				issue.spec, number, actorJSON(t, *authors)); err != nil {
				t.Fatalf("record version %d as the release before recorded it: %v", number, err)
			}
		}
		versionEvents[id] = asThePreviousBuild(t, issue.database, "DOC-1", func(context.Context, pgx.Tx) (model.Event, error) {
			return model.Event{Type: "artifact.version", Actor: agentOfTheReleaseBefore, Payload: map[string]any{
				"artifact_id": issue.spec, "name": "spec.md", "version": map[string]any{"number": number},
			}}, nil
		})
	}
	dropAs("q-person", &carol, "q-scoped", "q-agent", "q-marked")
	dropAs("q-scoped", &scoped, "q-agent", "q-marked")
	dropAs("q-agent", nil, "q-marked")
	issue.replaceAsPerson(issue.spec, spec())
	marked := heldRemovalsOf(t, issue, asked["q-marked"])
	if len(marked) != 1 || !marked[0].Asserted || marked[0].ToID != "alice" {
		t.Fatalf("the person's own drop of q-marked left the removals %+v, want one asserted to alice", marked)
	}

	// The decisions document: a section that cites the first ask and nothing else, and an ask block
	// an agent's session put in it.
	decisions := issue.document("decisions-md", "decisions.md", false, "decisions",
		"# Decisions\n\n## Which store holds the corpus\n\nThe person chose the database. Decided in dispatch://DOC-1/ask/"+asked["q-person"]+".\n")
	issue.replace(decisions, "# Decisions\n\n## Which store holds the corpus\n\nThe person chose the database. Decided in dispatch://DOC-1/ask/"+asked["q-person"]+".\n\n"+
		"## Which way the census reads\n\n"+removedRestoreAsk("q-moved", "Which way does the census read?"))
	moved := blockAskOn(t, issue.service, decisions, "q-moved").ID
	name[moved] = "q-moved"
	sections := decisionSectionsOf(liveTree(t, issue.service, decisions))
	if len(sections) != 2 {
		t.Fatalf("the decisions document holds %d sections, want two", len(sections))
	}
	heading := storeBlockAddress(issue.refKey(decisions), sections[0].headingID())

	step := NewDecisionStep(issue.database, "", nil)
	step.UseDocuments(issue.service)
	var versions int
	for pass := 1; pass <= 3; pass++ {
		done, failed, err := step.RestoreAndConvert(ctx, "DOC", true, false)
		if err != nil || len(failed) > 0 {
			t.Fatalf("pass %d answered %v with failures %+v", pass, err, failed)
		}
		if pass > 1 {
			for _, result := range done {
				if len(result.Restored)+len(result.Declined) > 0 || result.SpecVersion > 0 {
					t.Fatalf("pass %d did %+v, want nothing: the first pass did the issue's work", pass, result)
				}
			}
			if now := latestVersionNumber(t, issue.service, issue.spec); now != versions {
				t.Fatalf("pass %d left the spec at version %d, want %d: no pass after the first writes", pass, now, versions)
			}
			continue
		}
		if len(done) != 1 || done[0].Issue != "DOC-1" {
			t.Fatalf("the first pass touched %+v, want DOC-1 alone", done)
		}
		result := done[0]
		var restored []string
		for _, ask := range result.Restored {
			restored = append(restored, name[ask.Ask])
			if name[ask.Ask] == "q-agent" && ask.Read != restoreReadNoPerson {
				t.Errorf("the restore read %q of the version that dropped q-agent, want %q", ask.Read, restoreReadNoPerson)
			}
		}
		slices.Sort(restored)
		if !slices.Equal(restored, []string{"q-agent", "q-moved"}) {
			t.Fatalf("the restore wrote back %v, want q-agent and q-moved alone: q-person and q-scoped left the spec in a version a person wrote, and q-marked in a person's own write", restored)
		}
		declined := map[string]DeclinedAsk{}
		for _, ask := range result.Declined {
			declined[name[ask.Ask]] = ask
		}
		if len(declined) != 2 || declined["q-person"].Person != "carol" || declined["q-scoped"].Person != "dave" {
			t.Fatalf("the restore declined %+v, want q-person to carol and q-scoped to dave", result.Declined)
		}
		if result.Removed != 1 || result.Left != 0 || result.Readings != 0 || !result.Retired {
			t.Fatalf("the conversion did %+v, want the section citing q-person deleted by its rule, none left, no reading, the document retired", result)
		}
		versions = latestVersionNumber(t, issue.service, issue.spec)
	}

	for id, person := range map[string]string{"q-person": "carol", "q-scoped": "dave", "q-marked": "alice"} {
		held := heldRemovalsOf(t, issue, asked[id])
		if len(held) != 1 || !held[0].Asserted || held[0].ToKind != graphstore.KindPerson || held[0].ToID != person {
			t.Errorf("ask %s holds the removals %+v, want one asserted to %s", id, held, person)
			continue
		}
		if event, recorded := versionEvents[id]; recorded && held[0].SourceEvent != event {
			t.Errorf("the removal of ask %s carries the event %d, want %d, the event of the version that dropped it", id, held[0].SourceEvent, event)
		}
	}
	if held := heldRemovalsOf(t, issue, asked["q-agent"]); len(held) != 0 {
		t.Errorf("ask q-agent holds the removals %+v, want none: a session with no act dropped it", held)
	}
	latest := issue.latest(issue.spec)
	for _, id := range []string{"q-person", "q-scoped", "q-marked"} {
		if strings.Contains(latest, "#"+id+" ") {
			t.Errorf("the spec holds the block of %s, which a person removed:\n%s", id, latest)
		}
	}
	for _, id := range []string{"q-agent", "q-moved"} {
		if !strings.Contains(latest, "#"+id+" ") {
			t.Errorf("the spec holds no block of %s, which the restore returns:\n%s", id, latest)
		}
	}
	if strings.Contains(latest, "The person chose the database") {
		t.Errorf("the spec holds the deleted section's words, which no rule writes:\n%s", latest)
	}

	var to []graphstore.Tuple
	tuples, err := graphstore.History(ctx, issue.database.Pool, refs.BlockNodeKind, heading, graphstore.TupleMovedTo)
	if err != nil {
		t.Fatalf("read the moves of the section's heading: %v", err)
	}
	to = tuples
	if len(to) != 1 || to[0].ToKind != graphstore.KindAsk || to[0].ToID != asked["q-person"] || !to[0].Asserted {
		t.Fatalf("the section's heading %s holds the moves %+v, want one to the ask q-person %s", heading, to, asked["q-person"])
	}
	reached, ok, err := graphstore.MovedTo(ctx, issue.database.Pool, heading)
	if want := "dispatch://DOC-1/ask/" + asked["q-person"]; err != nil || !ok || reached.Ask != asked["q-person"] || reached.Address != want {
		t.Fatalf("MovedTo(%s) = %+v, %t (%v), want the ask's own address %s", heading, reached, ok, err, want)
	}
	var retired bool
	if err := issue.database.Pool.QueryRow(ctx, `select retired_at is not null from artifacts where id = $1`, decisions).Scan(&retired); err != nil || !retired {
		t.Fatalf("the decisions document reads retired %t (%v), want it retired", retired, err)
	}
	owed, err := countDecisionStepOwed(ctx, issue.database.Pool)
	if err != nil || owed.DecisionsDocuments != 0 || owed.AnswersLeftSpec != 0 {
		t.Fatalf("the step owes %+v (%v), want no decisions document and no answer whose block left its spec", owed, err)
	}
}

// heldRemovalsOf is every removed_by tuple of ask's node, in the order they were appended.
func heldRemovalsOf(t *testing.T, issue *restoreIssue, ask string) []graphstore.Tuple {
	t.Helper()
	held, err := graphstore.History(context.Background(), issue.database.Pool, graphstore.KindAsk, ask, graphstore.TupleRemovedBy)
	if err != nil {
		t.Fatalf("read the removals of ask %s: %v", ask, err)
	}
	return held
}

// The answerer of an ask is no remover in the one version that first holds her answer, and in
// every later version she counts as any person does (R6). Alice answers q-own; an agent's version
// then changes another part of the spec, the first version after her answer; and a later version
// whose one author is alice drops the block, recorded as the release before recorded it, with no
// removal of this build's own. The step declines q-own: it writes no block of it and appends one
// removed_by to alice, under the event of the version that dropped it.
func TestAnAnswerersOwnLaterDropOfHerAnsweredAskStays(t *testing.T) {
	ctx := context.Background()
	const head = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"
	block := removedRestoreAsk("q-own", "Which store holds the corpus?")
	issue := newRestoreIssue(t, head+block)
	ask := issue.answer(issue.spec, "q-own", "The database we run.")
	issue.replace(issue.spec, strings.Replace(head, "holds the corpus", "holds the whole corpus", 1)+block)
	issue.replace(issue.spec, strings.Replace(head, "holds the corpus", "holds the whole corpus", 1))
	dropped := latestVersionNumber(t, issue.service, issue.spec)
	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := issue.database.Pool.Exec(ctx, `update artifact_versions set authors = jsonb_build_array($3::jsonb) where artifact_id = $1 and number = $2`,
		issue.spec, dropped, actorJSON(t, alice)); err != nil {
		t.Fatalf("record version %d as alice's: %v", dropped, err)
	}
	event := asThePreviousBuild(t, issue.database, "DOC-1", func(context.Context, pgx.Tx) (model.Event, error) {
		return model.Event{Type: "artifact.version", Actor: alice, Payload: map[string]any{
			"artifact_id": issue.spec, "name": "spec.md", "version": map[string]any{"number": dropped},
		}}, nil
	})

	result := issue.run(false)
	for _, restored := range result.Restored {
		if restored.Ask == ask {
			t.Fatalf("the restore wrote back the ask %s (q-own), which its answerer alice dropped in version %d, a version after the one that first held her answer", ask, dropped)
		}
	}
	if len(result.Declined) != 1 || result.Declined[0] != (DeclinedAsk{Ask: ask, Person: "alice", Version: dropped}) {
		t.Fatalf("the restore declined %+v, want the ask %s to alice at version %d", result.Declined, ask, dropped)
	}
	if held := heldRemovalsOf(t, issue, ask); len(held) != 1 || !held[0].Asserted || held[0].ToID != "alice" || held[0].SourceEvent != event {
		t.Fatalf("the ask %s holds the removals %+v, want one asserted to alice under the event %d", ask, held, event)
	}
	if spec := issue.latest(issue.spec); strings.Contains(spec, "#q-own ") {
		t.Fatalf("the spec holds q-own again:\n%s", spec)
	}
}

// actorJSON is actor as a stored actor's JSON.
func actorJSON(t *testing.T, actor model.Actor) string {
	t.Helper()
	encoded, err := json.Marshal(actor)
	if err != nil {
		t.Fatalf("encode an actor: %v", err)
	}
	return string(encoded)
}
