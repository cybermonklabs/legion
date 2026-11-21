package record

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

func putRoot(t *testing.T, ctx context.Context, records Store, tx pgx.Tx, key string) {
	t.Helper()
	must(t, records.PutIssue(ctx, tx, Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: key, Project: "LEGION", Title: key, Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"}))
}

// An ask's mirror row round-trips, a write older than the stored row on any of its fences or
// person's acts changes nothing, a closed row keeps the turn it last had, and the batched read
// groups every issue's rows.
func TestARootAskMirrorKeepsItsNewestState(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		putRoot(t, ctx, records, tx, "LEGION-1")
		putRoot(t, ctx, records, tx, "LEGION-2")
		newer := RootAsk{Issue: "LEGION-1", Ask: "ask-q", Kind: AskApproval, Artifact: "artifact-1", Open: false, Seq: 12, WaitingOn: "human",
			WaitingOnSeq: 11, ClosedBy: ClosedByPerson, ClosedSeq: 12, ByTree: true, PersonSeq: 9, PersonAnswerSeq: 12}
		must(t, records.PutRootAsk(ctx, tx, newer))
		for _, older := range []RootAsk{
			{Issue: "LEGION-1", Ask: "ask-q", Kind: AskQuestion, Open: true, Seq: 10, WaitingOn: "human", WaitingOnSeq: 10, ByTree: true},
			{Issue: "LEGION-1", Ask: "ask-q", Seq: 13, WaitingOn: "agent", WaitingOnSeq: 10, ClosedBy: ClosedByPerson, ClosedSeq: 12, PersonSeq: 9, PersonAnswerSeq: 12},
			{Issue: "LEGION-1", Ask: "ask-q", Seq: 13, WaitingOn: "human", WaitingOnSeq: 11, ClosedBy: ClosedByOther, ClosedSeq: 12, PersonSeq: 8, PersonAnswerSeq: 12},
		} {
			must(t, records.PutRootAsk(ctx, tx, older))
		}
		got, err := records.RootAsk(ctx, tx, "LEGION-1", "ask-q")
		must(t, err)
		if got == nil || *got != newer {
			t.Fatalf("ask after older writes = %#v, want %#v kept", got, newer)
		}
		unknown := RootAsk{Issue: "LEGION-2", Ask: "ask-r", Open: true, Seq: 4, WaitingOn: "agent", WaitingOnSeq: 4}
		must(t, records.PutRootAsk(ctx, tx, unknown))
		all, err := records.RootAsks(ctx, tx, []string{"LEGION-1", "LEGION-2", "LEGION-3"})
		must(t, err)
		if want := map[string][]RootAsk{"LEGION-1": {newer}, "LEGION-2": {unknown}}; !reflect.DeepEqual(all, want) {
			t.Fatalf("asks = %#v, want %#v", all, want)
		}
		missing, err := records.RootAsk(ctx, tx, "LEGION-2", "ask-none")
		must(t, err)
		if missing != nil {
			t.Fatalf("an ask never mirrored = %#v, want none", missing)
		}
	})
}

// A person's decline round-trips as the one optional value it is: the row keeps it in three
// columns, and the declined one alone says there is a decline to read, so a decline the person
// wrote no note with reads back as a decline with an empty note. An ask nobody declined reads
// back with none.
func TestARootAskCarriesAPersonsDecline(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	inTx(t, st, func(tx pgx.Tx) {
		putRoot(t, ctx, records, tx, "LEGION-1")
		words := "The person chose not to answer this question. Decide the next step without an answer."
		declines := map[string]*AskDecline{
			"ask-noted":    {Note: "The vendor owns that date; I do not.", Words: words},
			"ask-noteless": {Words: words},
			"ask-plain":    nil,
		}
		for ask, decline := range declines {
			must(t, records.PutRootAsk(ctx, tx, RootAsk{Issue: "LEGION-1", Ask: ask, Kind: AskQuestion, Seq: 8, WaitingOn: "human",
				WaitingOnSeq: 7, ClosedBy: ClosedByPerson, ClosedSeq: 8, ByTree: true, PersonSeq: 8, Decline: decline}))
		}
		for ask, want := range declines {
			got, err := records.RootAsk(ctx, tx, "LEGION-1", ask)
			must(t, err)
			if got == nil {
				t.Fatalf("%s has no mirror row", ask)
			}
			if !reflect.DeepEqual(got.Decline, want) {
				t.Fatalf("%s reads back the decline %+v, want %+v", ask, got.Decline, want)
			}
		}
	})
}

// An approval request's answer is a review of its document: it answers no root waiting on the
// person when that document is the root's design gate's, and does for any other document or for a
// question. PersonSeq's acts count whatever the document.
func TestAnAnswerOfTheGatesDocumentIsNoPersonsAct(t *testing.T) {
	request := RootAsk{Kind: AskApproval, Artifact: "artifact-1", PersonSeq: 4, PersonAnswerSeq: 9}
	if !request.GateRequestOf("artifact-1") || request.PersonActSeq("artifact-1") != 4 {
		t.Fatalf("the gate's request = %t, act %d; want its answer left out, its other act at 4", request.GateRequestOf("artifact-1"), request.PersonActSeq("artifact-1"))
	}
	if request.GateRequestOf("artifact-2") || request.PersonActSeq("artifact-2") != 9 || request.PersonActSeq("") != 9 {
		t.Fatalf("another document's request acts at %d (no gate: %d), want its answer at 9", request.PersonActSeq("artifact-2"), request.PersonActSeq(""))
	}
	question := RootAsk{Kind: AskQuestion, PersonAnswerSeq: 9}
	if question.GateRequestOf("") || question.PersonActSeq("") != 9 {
		t.Fatalf("a question's answer acts at %d, want 9", question.PersonActSeq(""))
	}
}

// The words of a person's acts on an ask round-trip through Postgres whole: an answer's text with
// the options it chose and a reply's body, each with who wrote it, when, and whether more follows
// than its event carried, at the sequence it goes with. Nothing cuts them where they are kept, so
// a body of 5,000 UTF-16 units reads back as it was written. A write older on either sequence
// changes neither words, a newer act's words replace those of the act before it, and an ask with
// no person's act reads back with none.
func TestARootAskKeepsTheWordsOfEachActItsSequencesName(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	at := time.Date(2026, 11, 18, 9, 30, 0, 0, time.UTC)
	long := strings.Repeat("a", 4991) + "</reply>\n"
	answer := &ActWords{Text: "Ship it on Friday.", Options: []string{"ship", "tell the vendor"}, By: "user:alice", At: at}
	reply := &ActWords{Text: long, By: "user:alice", At: at.Add(time.Minute), More: true}
	inTx(t, st, func(tx pgx.Tx) {
		putRoot(t, ctx, records, tx, "LEGION-1")
		kept := RootAsk{Issue: "LEGION-1", Ask: "ask-q", Kind: AskQuestion, Open: true, Seq: 12, WaitingOn: "agent", WaitingOnSeq: 12,
			ClosedBy: ClosedByPerson, ClosedSeq: 9, ByTree: true, PersonAnswerSeq: 9, PersonAnswerWords: answer, PersonSeq: 12, PersonWords: reply}
		must(t, records.PutRootAsk(ctx, tx, kept))
		older := kept
		older.PersonSeq, older.PersonWords = 11, &ActWords{Text: "An older reply.", By: "user:alice", At: at}
		must(t, records.PutRootAsk(ctx, tx, older))
		bare := RootAsk{Issue: "LEGION-1", Ask: "ask-r", Kind: AskQuestion, Open: true, Seq: 4, WaitingOn: "human", WaitingOnSeq: 4, ByTree: true}
		must(t, records.PutRootAsk(ctx, tx, bare))

		got, err := records.RootAsk(ctx, tx, "LEGION-1", "ask-q")
		must(t, err)
		if got == nil || !reflect.DeepEqual(*got, kept) {
			t.Fatalf("the ask reads back as %#v, want %#v: the words of each act whole, and no older write over them", got, kept)
		}
		if len(got.PersonWords.Text) != 5000 {
			t.Fatalf("the reply reads back with %d units, want all 5000 it was written with", len(got.PersonWords.Text))
		}
		all, err := records.RootAsks(ctx, tx, []string{"LEGION-1"})
		must(t, err)
		if want := map[string][]RootAsk{"LEGION-1": {kept, bare}}; !reflect.DeepEqual(all, want) {
			t.Fatalf("the asks read back as %#v, want %#v", all, want)
		}

		newer := kept
		newer.PersonSeq, newer.PersonWords = 14, &ActWords{Text: "Then ship it Monday.", By: "user:bob", At: at.Add(time.Hour)}
		must(t, records.PutRootAsk(ctx, tx, newer))
		got, err = records.RootAsk(ctx, tx, "LEGION-1", "ask-q")
		must(t, err)
		if got == nil || !reflect.DeepEqual(got.PersonWords, newer.PersonWords) || !reflect.DeepEqual(got.PersonAnswerWords, answer) {
			t.Fatalf("after a newer reply the ask holds %#v and %#v, want the newer reply's words and the answer's kept",
				got.PersonWords, got.PersonAnswerWords)
		}
	})
}
