package docs

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
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
		// The drop is the version the upload writes, the one after the latest before it: an
		// upload resets the server-owned attributes of the asks it keeps, so the settlement after
		// it writes back what a version keeps of each answered ask (keptAskState) as a version of
		// its own, which is no drop.
		number := latestVersionNumber(t, issue.service, issue.spec) + 1
		issue.replace(issue.spec, spec(kept...))
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
	heading := refs.BlockAddress(issue.refKey(decisions), sections[0].headingID())

	// The store census's dangling counter reads its returning set by the restore's own test of a
	// person's removal (ReturnDeclined): over the restore's work before the step runs, it declines
	// the returns of q-person and q-scoped, which the restore declines below, and not q-agent's,
	// which it writes back. q-marked's node holds its removal already, so it is no return.
	work, err := graphcensus.ReadRestoreWork(ctx, issue.database.Pool)
	if err != nil {
		t.Fatalf("read the restore's work: %v", err)
	}
	judged := map[string]bool{}
	for _, item := range work {
		if item.Shape != graphcensus.RestoreReturns {
			continue
		}
		declined, err := ReturnDeclined(ctx, issue.database.Pool, item)
		if err != nil {
			t.Fatalf("judge the return of %s: %v", name[item.Ask], err)
		}
		judged[name[item.Ask]] = declined
	}
	if want := map[string]bool{"q-person": true, "q-scoped": true, "q-agent": false}; !maps.Equal(judged, want) {
		t.Fatalf("the restore's test declines the returns %v, want %v", judged, want)
	}

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

// A person's deletion of an answered ask's block in the browser is that person's removal, as the
// same deletion over HTTP is, and it stays taken (R6). Alice answers q-own, which writes the ask's
// state into its block in her name, and her browser deletes the block before that answer's
// settlement runs: the one version the settlement writes, whose one author is alice, first holds
// her answer and leaves the block out, which is the version the restore reads as the answer's own
// (answerersVersion) and would write the block back over. The settlement records her removal on
// the ask's node; the finder reads no work for the ask; and a pass that read the ask as its work
// before the settlement declines it, so no version writes the block back.
func TestAPersonsBrowserDeletionOfAnAnsweredAskStaysThroughTheRestore(t *testing.T) {
	ctx := context.Background()
	const head = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"
	issue := newRestoreIssue(t, head+removedRestoreAsk("q-own", "Which store holds the corpus?"))
	before := latestVersionNumber(t, issue.service, issue.spec)
	ask := issue.answer(issue.spec, "q-own", "The database we run.")
	alice := model.Actor{Kind: "user", ID: "alice"}
	browserReplaceText(t, issue.service, issue.spec, head, alice)
	settleCurrentGeneration(t, issue.service, issue.spec)
	settled := latestVersionNumber(t, issue.service, issue.spec)
	var authors []model.Actor
	if err := issue.database.Pool.QueryRow(ctx, `select authors from artifact_versions where artifact_id = $1 and number = $2`,
		issue.spec, settled).Scan(&authors); err != nil || settled != before+1 || len(authors) != 1 || authors[0].ID != "alice" {
		t.Fatalf("the settlement wrote version %d after %d with the authors %+v (%v), want one version whose one author is alice", settled, before, authors, err)
	}
	if spec := issue.latest(issue.spec); strings.Contains(spec, "#q-own ") {
		t.Fatalf("the settled version holds q-own, which alice's browser deleted:\n%s", spec)
	}

	if held := heldRemovalsOf(t, issue, ask); len(held) != 1 || !held[0].Asserted || held[0].ToID != "alice" {
		t.Errorf("the ask %s holds the removals %+v, want one asserted to alice: her browser deleted its block", ask, held)
	}
	work, err := graphcensus.ReadRestoreWork(ctx, issue.database.Pool)
	if err != nil {
		t.Fatalf("read the restore's work: %v", err)
	}
	for _, item := range work {
		if item.Ask == ask {
			t.Errorf("the finder reads %+v as the restore's work, want none for the ask alice removed", item)
		}
	}
	result := issue.restoreWith([]graphcensus.RestoreWork{{
		Shape: graphcensus.RestoreReturns, Issue: "DOC-1", Ask: ask, BlockID: "q-own",
		Address: refs.BlockAddress(issue.refKey(issue.spec), "q-own"), Spec: issue.refKey(issue.spec),
	}}, false)
	if len(result.Restored) != 0 || result.SpecVersion != 0 {
		t.Fatalf("the restore over the ask's work did %+v, want nothing written: alice removed q-own", result)
	}
	if spec := issue.latest(issue.spec); strings.Contains(spec, "#q-own ") {
		t.Fatalf("the spec holds q-own again:\n%s", spec)
	}
}

// A person's deletion of an answered ask's block in the browser stays that person's removal when a
// write that versions the room without a settlement comes before its settlement (R6): an agent's
// upload, whose version credits the agent alone and clears the room's pending authors, and a
// snapshot of the room in an agent's name, whose version first holds the answer and so would
// excuse its answerer (answerersVersion). Alice answers q-own, which writes the ask's state into
// its block in her name, and her browser deletes the block; before its settlement runs, an agent's
// session uploads the spec with a word of its prose changed, or a snapshot versions the room. That
// version records her removal on the ask's node, the finder reads no work for the ask, and the
// decisions step writes no block of it back.
func TestAPersonsUnsettledBrowserDeletionOfAnAnsweredAskStaysThroughAnAgentsVersion(t *testing.T) {
	ctx := context.Background()
	const head = "# Spec\n\n:::requirement{#r1}\nThe store holds the corpus.\n:::\n\n"
	agent := model.Actor{Kind: "session", ID: "session-unsettled-removal"}
	for _, test := range []struct {
		name    string
		version func(t *testing.T, issue *restoreIssue)
	}{
		{"an agent's upload", func(t *testing.T, issue *restoreIssue) {
			issue.write(issue.spec, strings.Replace(head, "holds the corpus", "holds the whole corpus", 1), agent,
				func(ctx context.Context) context.Context { return ctx })
		}},
		{"a snapshot of the room in an agent's name", func(t *testing.T, issue *restoreIssue) {
			if err := joinedWrite(issue.service, func(ctx context.Context) error {
				written, err := issue.service.SnapshotVersion(ctx, issue.spec, agent)
				if err != nil {
					return err
				}
				tx, _ := txFromContext(ctx)
				return causeInWritePhase(ctx, tx, written.Blocks, graphstore.NoEvent)
			}); err != nil {
				t.Fatalf("snapshot the spec: %v", err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			issue := newRestoreIssue(t, head+removedRestoreAsk("q-own", "Which store holds the corpus?"))
			before := latestVersionNumber(t, issue.service, issue.spec)
			ask := issue.answer(issue.spec, "q-own", "The database we run.")
			alice := model.Actor{Kind: "user", ID: "alice"}
			browserReplaceText(t, issue.service, issue.spec, head, alice)
			test.version(t, issue)
			if written := latestVersionNumber(t, issue.service, issue.spec); written != before+1 {
				t.Fatalf("the spec stands at version %d after %d, want the agent's write the one version after it", written, before)
			}
			if spec := issue.latest(issue.spec); strings.Contains(spec, "#q-own ") {
				t.Fatalf("the agent's version holds q-own, which alice's browser deleted:\n%s", spec)
			}

			if held := heldRemovalsOf(t, issue, ask); len(held) != 1 || !held[0].Asserted || held[0].ToID != "alice" {
				t.Errorf("the ask %s holds the removals %+v, want one asserted to alice: her browser deleted its block", ask, held)
			}
			work, err := graphcensus.ReadRestoreWork(ctx, issue.database.Pool)
			if err != nil {
				t.Fatalf("read the restore's work: %v", err)
			}
			for _, item := range work {
				if item.Ask == ask {
					t.Errorf("the finder reads %+v as the restore's work, want none for the ask alice removed", item)
				}
			}
			// A pass that read the ask as its work before the agent's version, as the existing
			// browser test drives it, writes nothing back either.
			result := issue.restoreWith([]graphcensus.RestoreWork{{
				Shape: graphcensus.RestoreReturns, Issue: "DOC-1", Ask: ask, BlockID: "q-own",
				Address: refs.BlockAddress(issue.refKey(issue.spec), "q-own"), Spec: issue.refKey(issue.spec),
			}}, false)
			if len(result.Restored) != 0 || result.SpecVersion != 0 {
				t.Errorf("the restore over the ask's work did %+v, want nothing written: alice's browser deleted q-own before the agent's version", result)
			}
			if spec := issue.latest(issue.spec); strings.Contains(spec, "#q-own ") {
				t.Fatalf("the spec holds q-own again:\n%s", spec)
			}
		})
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
