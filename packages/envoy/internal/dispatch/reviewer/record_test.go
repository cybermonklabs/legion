package reviewer

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The authors the seeded rows carry.
const (
	writerActor = `{"kind":"session","id":"s-writer"}`
	otherActor  = `{"kind":"session","id":"s-other"}`
	samiActor   = `{"kind":"user","id":"sami"}`
)

// recordAt is the time of the text the seeded records are read for.
var recordAt = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// seeded are the ids of the rows seedRecords writes.
type seeded struct {
	ask, blockAsk, notes, handbook, threadReply, directReply string
}

// seedRecords writes an issue with its primary document at two versions, its decisions document,
// a document with a decision block, a project document, an ask with replies, the writer's and
// another session's comments and messages, a comment thread anchored in the document, a direct
// message thread, and the reader's word records before and after recordAt.
func seedRecords(t *testing.T, database *store.Store) seeded {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := database.Pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", strings.Fields(sql)[2], err)
		}
		return id
	}
	at := func(d time.Duration) time.Time { return recordAt.Add(d) }
	var s seeded
	exec(`insert into projects (key, name, default_assignee) values ('PRJ', 'Project One', 'sami') returning key`)
	exec(`insert into issues (key, project_key, number, title, created_by, rank) values ('PRJ-1', 'PRJ', 1, 'Ship the reviewer', $1, 'a0') returning key`, samiActor)
	document := func(issue any, slug string, primary bool, role any) string {
		return exec(`insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, role, created_by)
			values ($1, 'PRJ', $2, $2, 'doc', $3, $4, $5) returning id`, issue, slug, primary, role, samiActor)
	}
	version := func(id string, number int, markdown string, written time.Time) {
		exec(`insert into artifact_versions (artifact_id, number, markdown, created_at) values ($1, $2, $3, $4) returning id`, id, number, markdown, written)
	}
	spec := document("PRJ-1", "spec", true, nil)
	version(spec, 1, "# Spec\n\nThe first version.\n", at(-48*time.Hour))
	version(spec, 2, "# Spec\n\nA version written after the text.\n", at(time.Hour))
	decisions := document("PRJ-1", "decisions-md", false, "decisions")
	version(decisions, 1, "# Decisions\n\n1. We keep one store.\n", at(-24*time.Hour))
	s.notes = document("PRJ-1", "notes", false, nil)
	version(s.notes, 1, "# Notes\n\nThe east region is the quiet one.\n\n:::ask{#blk urgency=\"med\" multiple=\"false\"}\nWhich store?\n\n"+
		"- A: one store\n- B: see [the memo](https://memo.test)\n:::\n", at(-24*time.Hour))
	s.handbook = document(nil, "handbook", false, nil)
	version(s.handbook, 1, "# Handbook\n\nHow we work.\n", at(-24*time.Hour))

	// An ask's row holds its options with no id, as every write of one stores them
	// (api.validateAskOptions): an option's id is its part's, which a read attaches.
	s.ask = exec(`insert into asks (issue_key, author, question, options, created_at) values ('PRJ-1', $1, 'Which store do we keep?',
		'[{"label":"One"},{"label":"Two","description":"Keep both"}]', $2) returning id`, writerActor, at(-3*time.Hour))
	s.blockAsk = exec(`insert into asks (issue_key, author, question, options, block_id, block_artifact_id, created_at) values ('PRJ-1', $1, 'Which store?',
		'[{"label":"A","description":"one store"},{"label":"B","description":"see the memo"}]', 'blk', $2, $3) returning id`, writerActor, s.notes, at(-24*time.Hour))
	comment := func(author string, body string, askID, replyTo, anchor any, written time.Time) string {
		return exec(`insert into comments (issue_key, author, body, ask_id, reply_to, anchor, created_at) values ('PRJ-1', $1, $2, $3, $4, $5, $6) returning id`,
			author, body, askID, replyTo, anchor, written)
	}
	comment(samiActor, "What is a store?", s.ask, nil, nil, at(-2*time.Hour))
	comment(writerActor, "The store is the database.", s.ask, nil, nil, at(-time.Hour))
	comment(samiActor, "A reply written after the text.", s.ask, nil, nil, at(time.Hour))
	comment(writerActor, "I moved the store.", nil, nil, nil, at(-5*time.Hour))
	comment(otherActor, "Another agent's comment.", nil, nil, nil, at(-4*time.Hour))
	root := comment(samiActor, "Why this region?", nil, nil, `{"artifact_id":"`+s.notes+`","quote":"the quiet one"}`, at(-6*time.Hour))
	s.threadReply = comment(writerActor, "It has the least load.", nil, root, nil, at(-5*time.Hour-30*time.Minute))
	message := func(author, body string, issue, target, inReplyTo any, written time.Time) string {
		return exec(`insert into messages (issue_key, target, author, body, in_reply_to, created_at) values ($1, $2, $3, $4, $5, $6) returning id`,
			issue, target, author, body, inReplyTo, written)
	}
	message(writerActor, "Status: the tests pass.", "PRJ-1", nil, nil, at(-4*time.Hour-30*time.Minute))
	message(writerActor, "A message written after the text.", "PRJ-1", nil, nil, at(2*time.Hour))
	first := message(samiActor, "Can you explain the store?", nil, "session:s-writer", nil, at(-2*time.Hour))
	s.directReply = message(writerActor, "The store is the database.", nil, "user:sami", first, at(-time.Hour))

	for i, use := range []store.DefineUse{
		{Person: "sami", Words: []string{"Store"}, At: at(-48 * time.Hour)},
		{Person: "sami", Words: []string{"store"}, At: at(-24 * time.Hour)},
		{Person: "sami", Words: []string{"tombstone"}, At: at(time.Hour)},
		{Person: "ada", Words: []string{"shard"}, At: at(-24 * time.Hour)},
	} {
		use.DefinitionID = "d" + string(rune('0'+i))
		if err := database.RecordDefineUse(ctx, use); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

// entriesOf is each entry as "kind, author: text", in order.
func entriesOf(entries []Entry) []string {
	var out []string
	for _, entry := range entries {
		out = append(out, string(entry.Kind)+", "+entry.Author+": "+entry.Text)
	}
	return out
}

// The record of a held reply on an ask, at its time: the ask with its replies before it, the
// issue's primary document at the version current then, its title and decisions document, the
// writer's comments and messages on the issue, and the reader's words recorded before it.
func TestBuildRecordReadsTheRecordBeforeTheText(t *testing.T) {
	database := storetest.Open(t)
	s := seedRecords(t, database)
	held := Text{Kind: KindComment, Body: "The store keeps one copy."}
	record, err := BuildRecord(t.Context(), database, Subject{
		Text: held, IssueKey: "PRJ-1", AskID: s.ask, Writer: "s-writer", Reader: "Sami", At: recordAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record.Text, held) || record.Reader != "Sami" || record.IssueKey != "PRJ-1" || record.IssueTitle != "Ship the reviewer" || record.Project != "" {
		t.Errorf("record = %+v", record)
	}
	if want := []string{
		"ask, the writer agent: Which store do we keep?\n- One\n- Two: Keep both",
		"comment, person sami: What is a store?",
		"comment, the writer agent: The store is the database.",
	}; !reflect.DeepEqual(entriesOf(record.Thread), want) {
		t.Errorf("thread = %q, want %q", entriesOf(record.Thread), want)
	}
	if want := (&Document{Name: "spec", Markdown: "# Spec\n\nThe first version.\n"}); !reflect.DeepEqual(record.Document, want) {
		t.Errorf("document = %+v, want the primary document's version current at the text's time", record.Document)
	}
	if record.Decisions != "# Decisions\n\n1. We keep one store.\n" {
		t.Errorf("decisions = %q", record.Decisions)
	}
	if want := []string{
		"comment, the writer agent: It has the least load.",
		"comment, the writer agent: I moved the store.",
		"message, the writer agent: Status: the tests pass.",
		"comment, the writer agent: The store is the database.",
	}; !reflect.DeepEqual(entriesOf(record.WriterTexts), want) {
		t.Errorf("writer's texts = %q, want %q", entriesOf(record.WriterTexts), want)
	}
	// Decision 19: the words recorded before the text, never one recorded after it.
	if want := []string{"store"}; !reflect.DeepEqual(record.ReaderWords, want) {
		t.Errorf("reader words = %q, want %q", record.ReaderWords, want)
	}
	// A record read at no time reads the word recorded later too.
	later, err := BuildRecord(t.Context(), database, Subject{Text: held, IssueKey: "PRJ-1", Reader: "sami"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tombstone", "store"}; !reflect.DeepEqual(later.ReaderWords, want) {
		t.Errorf("reader words read at no time = %q, want %q", later.ReaderWords, want)
	}
}

// A person's Rewrite of a stored ask reads the ask's own wording and leaves the ask out of its
// thread, and a rewrite of it that keeps its labels, their number and order and its choice keeps
// what a rewrite keeps; a decision block's record holds its document, by which a rewrite of an
// option line the document holds more of is not applied.
func TestAskSubjectReadsAStoredAsk(t *testing.T) {
	database := storetest.Open(t)
	s := seedRecords(t, database)
	subject, err := AskSubject(t.Context(), database, s.ask)
	if err != nil {
		t.Fatal(err)
	}
	want := Subject{
		Text: Text{Kind: KindAsk, Body: "Which store do we keep?", Options: []model.AskOption{{Label: "One"}, {Label: "Two", Description: "Keep both"}}},
		ID:   s.ask, AskID: s.ask, IssueKey: "PRJ-1", Writer: "s-writer",
	}
	if !reflect.DeepEqual(subject, want) {
		t.Errorf("subject = %+v, want %+v", subject, want)
	}
	record, err := BuildRecord(t.Context(), database, subject)
	if err != nil {
		t.Fatal(err)
	}
	if got := entriesOf(record.Thread); len(got) != 3 || strings.HasPrefix(got[0], "ask") {
		t.Errorf("a stored ask's thread = %q, want its three replies and not the ask", got)
	}
	kept := record.Text
	kept.Body = "Which store do we keep: one, or both?"
	kept.Options = []model.AskOption{{Label: "One", Description: "Keep one store"}, {Label: "Two", Description: "Keep both stores"}}
	if got := Preserve(record, "", kept); got != nil {
		t.Errorf("a rewrite of the stored ask that keeps its labels and their order: %v", got)
	}

	subject, err = AskSubject(t.Context(), database, s.blockAsk)
	if err != nil {
		t.Fatal(err)
	}
	if subject.Text.Kind != KindDecisionBlock || subject.DocumentID != s.notes || subject.BlockID != "blk" {
		t.Fatalf("a decision block's subject = %+v", subject)
	}
	record, err = BuildRecord(t.Context(), database, subject)
	if err != nil {
		t.Fatal(err)
	}
	if record.Document == nil || record.Document.Name != "notes" || !record.Document.Holds || record.Document.BlockID != "blk" {
		t.Fatalf("a decision block's document = %+v", record.Document)
	}
	if record.IssueKey != "PRJ-1" || record.Decisions == "" {
		t.Errorf("a decision block's issue = %q, decisions %q", record.IssueKey, record.Decisions)
	}
	rewrite := record.Text
	rewrite.Options = []model.AskOption{{Label: "A", Description: "one store"}, {Label: "B", Description: "wait for the memo"}}
	if got := Preserve(record, "", rewrite); got == nil || got.Rule != KeepOptionLines {
		t.Errorf("Preserve = %+v, want the option line the document holds more of", got)
	}
}

// A reply in a comment thread reads the thread from its first comment, and the document that
// comment quotes; a reply to a direct message reads its message thread; a project document's
// record names its project.
func TestBuildRecordReadsEachThread(t *testing.T) {
	database := storetest.Open(t)
	s := seedRecords(t, database)

	record, err := BuildRecord(t.Context(), database, Subject{Text: Text{Kind: KindComment, Body: "Thanks."}, ReplyTo: s.threadReply, Writer: "s-writer", At: recordAt})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"comment, person sami: Why this region?", "comment, the writer agent: It has the least load."}; !reflect.DeepEqual(entriesOf(record.Thread), want) {
		t.Errorf("comment thread = %q, want %q", entriesOf(record.Thread), want)
	}
	if record.Document == nil || record.Document.Name != "notes" || !record.Document.Holds || record.Document.Quote != "the quiet one" || record.IssueKey != "PRJ-1" {
		t.Errorf("a reply's document = %+v on issue %q, want the notes its thread quotes", record.Document, record.IssueKey)
	}
	if !strings.Contains(record.input(extra{}), "The east region is the quiet one.") {
		t.Error("a reply's input does not show the quoted place")
	}

	record, err = BuildRecord(t.Context(), database, Subject{Text: Text{Kind: KindMessage, Body: "It keeps rows."}, InReplyTo: s.directReply, Writer: "s-writer", At: recordAt})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"message, person sami: Can you explain the store?", "message, the writer agent: The store is the database."}; !reflect.DeepEqual(entriesOf(record.Thread), want) {
		t.Errorf("message thread = %q, want %q", entriesOf(record.Thread), want)
	}
	if record.IssueKey != "" || record.Document != nil || len(record.WriterTexts) != 0 {
		t.Errorf("a direct message's record = %+v, want no issue", record)
	}

	record, err = BuildRecord(t.Context(), database, Subject{Text: Text{Kind: KindDocument, Body: "# Handbook\n\nHow we work.\n"}, ID: s.handbook, DocumentID: s.handbook, Writer: "s-writer"})
	if err != nil {
		t.Fatal(err)
	}
	if record.Project != "Project One" || record.IssueKey != "" || record.Document == nil || record.Document.Name != "handbook" {
		t.Errorf("a project document's record = %+v, want its project's name", record)
	}
}

func TestAuthor(t *testing.T) {
	for _, test := range []struct {
		actor model.Actor
		want  string
	}{
		{model.Actor{Kind: "session", ID: "s-writer"}, "the writer agent"},
		{model.Actor{Kind: "user", ID: "sami", Act: &model.ActorAct{Session: "s-writer"}}, "the writer agent"},
		{model.Actor{Kind: "user", ID: "sami"}, "person sami"},
		{model.Actor{Kind: "user", ID: "sami", Act: &model.ActorAct{Session: "s-other"}}, "another agent"},
		{model.Actor{Kind: "session", ID: "s-other"}, "another agent"},
		{model.Actor{Kind: "system", ID: "dispatch"}, "Dispatch"},
	} {
		if got := Author(test.actor, "s-writer"); got != test.want {
			t.Errorf("Author(%+v) = %q, want %q", test.actor, got, test.want)
		}
	}
}

// A decision block's record holds its question as the document holds it, picture and link with
// it, which the ask's row leaves out; every other kind's text, and a block the version does not
// hold yet, keep the text they were given.
func TestBlockQuestionReadsTheDecisionBlocksMarkdown(t *testing.T) {
	const markdown = "# Rollout\n\n:::ask{#flip urgency=\"med\" multiple=\"false\" state=\"open\"}\n" +
		"Run the audit first? See [the run](https://ci.test/runs/7) ![Flip rate](https://ci.test/flip.png)\n\n" +
		"- Ship: Release it\n- Hold: Wait\n:::\n"
	for name, test := range map[string]struct {
		kind    Kind
		blockID string
		want    string
	}{
		"decision block": {KindDecisionBlock, "flip", "Run the audit first? See [the run](https://ci.test/runs/7) ![Flip rate](https://ci.test/flip.png)"},
		"ask":            {KindAsk, "flip", "the row's question"},
		"block not held": {KindDecisionBlock, "elsewhere", "the row's question"},
	} {
		text := Text{Kind: test.kind, Body: "the row's question"}
		if err := blockQuestion(&text, markdown, test.blockID); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if text.Body != test.want {
			t.Errorf("%s: the record's question is %q, want %q", name, text.Body, test.want)
		}
	}
}
