package asof_test

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asof"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Two writes change a field with no event that dates the change: the restore of a decided ask's
// wording (docs' decidedWordingRestore), and the orphaning of a comment's anchor that a 409 of an
// accept or a reject wrote with no event, before such a 409 appended one, and that a delete of a
// suggestion writes. The statements that tell a read at a cut where such a field is unknown read
// the present log and rows; these tests hold each case of their rules.

// The wording of an ask at a cut is unknown where the wording just after the cut, by the record,
// is not the wording of the ask's newest state event at the cut: the previous wording of its
// first edit after the cut, the own wording of any other first event after the cut, or the
// present row's wording when no event is later. Such an ask counts as changed after the cut. A
// payload's options carry part ids that the row's and an edit's previous options do not; the
// comparison reads none of them.
func TestAnAskWhoseWordingNoRecordDatesIsChangedWithItsWordingUnknown(t *testing.T) {
	w := newWorld(t)
	made := when(t, "2026-11-10T09:00:00Z")
	w.project("ASOF", "carol", made.Add(-time.Hour))
	issue := newIssue("ASOF-1", "ASOF", 1, "Asks", made)
	w.insertIssue(issue)
	w.issueEvent("issue.created", issue, made)
	key := issue.Key

	decided := model.AskEditPrevious{Question: "Ship it?", Options: []model.AskOption{{Label: "yes"}, {Label: "no", Description: "Not yet."}}, Urgency: "med"}
	reworded := model.AskEditPrevious{Question: "Ship it today?", Options: []model.AskOption{{Label: "yes"}, {Label: "no"}, {Label: "later"}}, Multiple: true, Urgency: "high"}
	rewordedAgain := model.AskEditPrevious{Question: "Ship it this week?", Options: decided.Options, Urgency: "low"}
	asks := map[string]string{}
	// ask writes an ask whose row holds row's wording, and answers its id.
	ask := func(name string, row model.AskEditPrevious) string {
		id := "00000000-0000-4000-8000-0000000000" + name
		asks[id] = name
		w.exec(`insert into asks (id, issue_key, author, question, options, multiple, urgency, created_at) values ($1, $2, $3, $4, $5, $6, $7, $8)`,
			id, key, w.encode(alice), row.Question, w.encode(row.Options), row.Multiple, row.Urgency, made)
		return id
	}
	// worded is the ask id in state with wording, as an event's payload holds it: its options carry
	// part ids, as a payload read back from a hydrated ask carries them.
	worded := func(id, state string, wording model.AskEditPrevious) model.Ask {
		withIDs := make([]model.AskOption, len(wording.Options))
		for index, option := range wording.Options {
			option.ID = id[len(id)-2:] + "-o" + string(rune('1'+index))
			withIDs[index] = option
		}
		return model.Ask{ID: id, IssueKey: &key, Author: alice, Kind: "question", Question: wording.Question,
			Options: withIDs, Multiple: wording.Multiple, Urgency: wording.Urgency, State: state, CreatedAt: made}
	}
	opened := func(id string, wording model.AskEditPrevious, at string) {
		w.event(issueOwned(key), "ask.opened", alice, model.NewAskEventPayload(worded(id, "open", wording), model.ReferenceChanges{}), when(t, at))
	}
	answered := func(id string, wording model.AskEditPrevious, at string) {
		w.event(issueOwned(key), "ask.answered", alice, model.NewAskEventPayload(worded(id, "answered", wording), model.ReferenceChanges{}), when(t, at))
	}
	edit := func(id, state string, wording, previous model.AskEditPrevious, at string) {
		w.event(issueOwned(key), "ask.edited", alice,
			model.NewAskEditEventPayload(worded(id, state, wording), previous, alice, model.ReferenceChanges{}), when(t, at))
	}

	// A decided ask an earlier release reworded after its decision, then restored with no event.
	restored := ask("a1", decided)
	opened(restored, decided, "2026-11-10T09:00:00Z")
	answered(restored, decided, "2026-11-10T09:10:00Z")
	edit(restored, "answered", reworded, decided, "2026-11-10T09:20:00Z")

	// An ask edited after the cut, whose edit recorded the wording at the cut as its previous.
	editedLater := ask("a2", reworded)
	opened(editedLater, decided, "2026-11-10T09:00:00Z")
	edit(editedLater, "open", reworded, decided, "2026-11-10T09:40:00Z")

	// An ask restored with no event and then edited again: the second edit's previous wording is
	// the restored one, not the wording of the first edit.
	editedAfterRestore := ask("a3", rewordedAgain)
	opened(editedAfterRestore, decided, "2026-11-10T09:00:00Z")
	edit(editedAfterRestore, "open", reworded, decided, "2026-11-10T09:20:00Z")
	edit(editedAfterRestore, "open", rewordedAgain, decided, "2026-11-10T09:40:00Z")

	// An ask answered after the cut, which does not change its wording.
	answeredLater := ask("a4", decided)
	opened(answeredLater, decided, "2026-11-10T09:00:00Z")
	answered(answeredLater, decided, "2026-11-10T09:40:00Z")

	// An ask edited after the cut by an edit whose previous wording records only the question:
	// each part it does not record counts as the wording at the cut.
	questionOnly := ask("a5", reworded)
	opened(questionOnly, decided, "2026-11-10T09:00:00Z")
	var payload map[string]any
	if err := json.Unmarshal(w.encode(model.NewAskEditEventPayload(worded(questionOnly, "open", reworded), decided, alice, model.ReferenceChanges{})), &payload); err != nil {
		t.Fatalf("decode the edit's payload: %v", err)
	}
	payload["previous"] = map[string]any{"question": decided.Question}
	w.event(issueOwned(key), "ask.edited", alice, payload, when(t, "2026-11-10T09:40:00Z"))

	// An ask opened after the cut.
	openedLater := ask("a6", decided)
	opened(openedLater, decided, "2026-11-10T09:40:00Z")

	// An ask no write changed since the cut.
	unchanged := ask("a7", decided)
	opened(unchanged, decided, "2026-11-10T09:00:00Z")

	read := func(cut string) map[string]string {
		t.Helper()
		rows, err := w.database.Pool.Query(context.Background(), asof.AsksChangedAfterStatement,
			slices.Sorted(maps.Keys(asks)), mustCut(t, cut).Time())
		got := map[string]string{}
		for _, row := range scanAll(t, rows, err) {
			verdict := "changed"
			if row["wording_unknown"].(bool) {
				verdict = "changed, wording unknown"
			}
			got[asks[row["id"].(string)]] = verdict
		}
		return got
	}
	for _, row := range []struct {
		cut  string
		want map[string]string
	}{
		{"2026-11-10T09:30:00Z", map[string]string{
			"a1": "changed, wording unknown",
			"a2": "changed",
			"a3": "changed, wording unknown",
			"a4": "changed",
			"a5": "changed",
			"a6": "changed",
		}},
		// Before the rewording, the edit's previous wording is the wording at the cut.
		{"2026-11-10T09:15:00Z", map[string]string{
			"a1": "changed",
			"a2": "changed",
			"a3": "changed",
			"a4": "changed",
			"a5": "changed",
			"a6": "changed",
		}},
		{"2026-11-10T09:50:00Z", map[string]string{
			"a1": "changed, wording unknown",
		}},
	} {
		if got := read(row.cut); !maps.Equal(got, row.want) {
			t.Errorf("at %s the asks changed after the cut are %v, want %v", row.cut, got, row.want)
		}
	}
}

// A comment's anchor at a cut is its newest state event's. Only a comment.anchor_refreshed dates
// an orphaning: an accept or a reject before its 409 appended one, and a delete of a suggestion,
// set the flag with no event that dates it. So the flag at a cut where the newest event gives it
// false is unknown where, by the record, it was true just before the comment's first state event
// after the cut: the present row's flag with no later event; false before a refresh that orphans
// the anchor; true before a refresh that found the mark again with the quote it had; the later
// event's own flag before any other event.
func TestACommentAnchorOrphanedWithNoEventThatDatesItIsUnknownAtTheCut(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	made := when(t, "2026-11-10T09:00:00Z")
	w.project("ASOF", "carol", made.Add(-time.Hour))
	issue := newIssue("ASOF-1", "ASOF", 1, "Anchors", made)
	w.insertIssue(issue)
	w.issueEvent("issue.created", issue, made)
	key := issue.Key
	var spec string
	if err := w.database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by, created_at)
		values ($1, 'ASOF', 'spec', 'spec.md', 'doc', true, $2, $3) returning id::text`, key, w.encode(alice), made).Scan(&spec); err != nil {
		t.Fatalf("insert the spec: %v", err)
	}
	w.exec(`insert into artifact_versions (artifact_id, number, markdown, created_at) values ($1, 1, 'The quick brown fox.', $2)`, spec, made)

	comments := map[string]string{}
	// comment writes a comment on quote "brown", created at made, whose row's anchor holds the
	// flag orphaned and whose events are the kinds of later, each with its anchor, after the cut
	// at 10:00 unless its time says otherwise.
	type later struct {
		kind, at, quote string
		orphaned        bool
	}
	comment := func(name string, orphaned bool, events ...later) {
		id := "00000000-0000-4000-8000-0000000000" + name
		comments[id] = name
		posted := newComment(id, key, alice, "Use red.", made)
		posted.Anchor = &model.Anchor{ArtifactID: spec, MarkID: "m-" + name, Quote: "brown"}
		row := *posted.Anchor
		row.Orphaned = orphaned
		w.exec(`insert into comments (id, issue_key, author, body, anchor, created_at) values ($1, $2, $3, $4, $5, $6)`,
			id, key, w.encode(alice), posted.Body, w.encode(row), made)
		w.event(issueOwned(key), "comment.created", alice, commentPayload(posted), made)
		for _, event := range events {
			written := posted
			anchor := *posted.Anchor
			anchor.Quote, anchor.Orphaned = event.quote, event.orphaned
			written.Anchor = &anchor
			w.event(issueOwned(key), event.kind, alice, commentPayload(written), when(t, event.at))
		}
	}
	const after = "2026-11-10T10:30:00Z"
	comment("c1", true)
	comment("c2", true, later{"comment.deleted", after, "brown", true})
	comment("c3", true, later{"comment.anchor_refreshed", after, "brown", true})
	comment("c4", true, later{"comment.resolved", after, "brown", false})
	comment("c5", false, later{"comment.anchor_refreshed", after, "brown", false})
	comment("c6", true, later{"comment.anchor_refreshed", "2026-11-10T09:30:00Z", "brown", true})
	comment("c7", false, later{"comment.anchor_refreshed", after, "quick brown", false})
	comment("c8", false)

	rows, err := w.database.Pool.Query(ctx, asof.CommentAnchorsOrphanedNotGivenStatement,
		slices.Sorted(maps.Keys(comments)), mustCut(t, "2026-11-10T10:00:00Z").Time())
	var got []string
	for _, row := range scanAll(t, rows, err) {
		got = append(got, comments[row["id"].(string)])
	}
	slices.Sort(got)
	// c1: an old 409, and no event since. c2: an old 409, then a delete, which does not date it.
	// c5: a refresh that found the mark again, which it had lost with no event.
	if want := []string{"c1", "c2", "c5"}; !slices.Equal(got, want) {
		t.Errorf("the comments whose anchor's flag no record gives at the cut are %v, want %v "+
			"(c3 a refresh orphaned, c4 a resolve read the flag false, c6 orphaned at the cut, c7 a quote move, c8 never orphaned)", got, want)
	}
}
