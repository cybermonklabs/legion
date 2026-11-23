package reviewer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// Kind is the kind of a text the component reads.
type Kind string

// The kinds of text (R5): the four a review may hold, a document version, which is reviewed after
// the save, and a decision block in a document.
const (
	KindAsk           Kind = "ask"
	KindIssue         Kind = "issue"
	KindComment       Kind = "comment"
	KindMessage       Kind = "message"
	KindDocument      Kind = "document"
	KindDecisionBlock Kind = "decision_block"
)

// Kinds is every kind of text, in the order above.
var Kinds = []Kind{KindAsk, KindIssue, KindComment, KindMessage, KindDocument, KindDecisionBlock}

// asks reports whether a text of the kind is a question with options: an ask or a decision block.
func (k Kind) asks() bool { return k == KindAsk || k == KindDecisionBlock }

// Text is one text's wording, as its kind holds it.
type Text struct {
	Kind Kind `json:"kind"`
	// Title is an issue's title; no other kind has one.
	Title string `json:"title,omitempty"`
	// Body is an ask's or a decision block's question, a comment's or a message's body, an
	// issue's spec, and a document's Markdown.
	Body string `json:"body"`
	// Options are an ask's or a decision block's options, and Multiple is whether its reader may
	// choose more than one.
	Options  []model.AskOption `json:"options,omitempty"`
	Multiple bool              `json:"multiple,omitempty"`
}

// Entry is one text of a record besides the text itself: its kind, who wrote it (Author), when,
// and its words.
type Entry struct {
	Kind   Kind
	Author string
	At     time.Time
	Text   string
}

// Document is the document a record reads around its text.
type Document struct {
	// Name is the document's name.
	Name string
	// Markdown is the whole document, at the version the record reads.
	Markdown string
	// Holds is whether the text stands in the document: the document itself, a decision block
	// of it, or an ask or a comment anchored in it. Where it does not, the document is the
	// issue's primary document, which a record reads for a text that stands in no document.
	Holds bool
	// BlockID is the id of the decision block the text is, and Quote the words of the document
	// the text is anchored to. The record shows the document around the first of them it
	// finds, and from its start where it finds neither.
	BlockID, Quote string
}

// Record is Dispatch's record of one text (INFRA-3552's R2, INFRA-3231's R5): every input of a
// model call about the text besides the rules. It holds no session text. The server builds it
// from the store (BuildRecord); the harness fills its fields from a dataset case, with the
// records of the reader's words written before the case's text (Decision 19), so both give the
// model the same record (Record.input).
type Record struct {
	// Text is the text the record is for.
	Text Text
	// Thread is the texts before the text in its conversation, oldest first: an ask's replies,
	// or the comments or the messages of a reply's thread.
	Thread []Entry
	// Document is the document the text stands in, else the issue's primary document; nil for
	// none.
	Document *Document
	// IssueKey and IssueTitle name the issue the text is on; both are empty for a text on no
	// issue.
	IssueKey, IssueTitle string
	// Decisions is the issue's decisions document, "" where it has none.
	Decisions string
	// WriterTexts are the comments and messages the writer session wrote on the issue, oldest
	// first.
	WriterTexts []Entry
	// Project is the project's name, for a text of a project document, which has no issue.
	Project string
	// Reader is the login of the person who reads the text, and ReaderWords the words that
	// person asked about before the text was written (R8's records, the newest first). A call's
	// user message shows each of them but a function word (readerStopWord).
	Reader      string
	ReaderWords []string
}

// readerStopWords are the function words a call does not show among the reader's words: the
// articles, the pronouns, the forms of "be", "do" and "have", and the most common conjunctions
// and prepositions. A word record holds one where the records job took it from a reader's
// question ("what is it?"). It is no term the reader did not know, and the reader_words rule set
// on it made a rewrite put other words in place of each "it" or "you" of a text (INFRA-3231's
// round 2).
var readerStopWords = func() map[string]bool {
	words := map[string]bool{}
	for _, word := range strings.Fields(`
		a an the
		i me my you your we us our he him his she her it its they them their
		this that these those there here what which who
		am is are was were be been do does did have has had
		and or but if so not no of to in on at for with by from as about`) {
		words[word] = true
	}
	return words
}()

// ShownReaderWords is the reader's words a call shows: ReaderWords less the function words
// (readerStopWords), matched in any case and with the space around them, in their order.
func (r Record) ShownReaderWords() []string {
	var shown []string
	for _, word := range r.ReaderWords {
		if !readerStopWords[strings.ToLower(strings.TrimSpace(word))] {
			shown = append(shown, word)
		}
	}
	return shown
}

// Subject names one text and where it stands, for BuildRecord to read its record.
type Subject struct {
	// Text is the text as its writer wrote it. A held text is in no table, so the record takes
	// it from here.
	Text Text
	// ID is the text's own id where it is stored already (a document version reviewed after
	// the save, an ask a person asked to rewrite), so the record leaves it out of the thread
	// and of the writer's texts. A held text has none.
	ID string
	// IssueKey is the issue the text is on: for a new issue, its parent, "" for none. A text in
	// a document of an issue may leave it empty.
	IssueKey string
	// DocumentID is the document the text stands in, and BlockID or Quote where in it (Document).
	// A reply in a comment thread anchored in a document may leave them empty.
	DocumentID     string
	BlockID, Quote string
	// AskID is the ask the text is, or the ask a comment replies on. ReplyTo is the comment a
	// comment replies to, and InReplyTo the message a message replies to. The record reads
	// the thread from the first of them that is set.
	AskID, ReplyTo, InReplyTo string
	// Writer is the session that wrote the text.
	Writer string
	// Reader is the login of the person who reads the text.
	Reader string
	// At is the text's time. The record reads what was written before it, and the document
	// version current at it. The zero time reads everything.
	At time.Time
}

// The limits of what the record reads from the store. What the model reads of each part is cut
// further where the input is built (Record.input).
const (
	// threadLimit is the most texts a thread holds: its newest.
	threadLimit = 60
	// writerTextsLimit is the most texts of the writer the record holds: its newest.
	writerTextsLimit = 30
	// threadDepth bounds how far up and down a reply thread the record reads.
	threadDepth = 100
)

// The authors a record names besides a person (Author).
const (
	authorWriter   = "the writer agent"
	authorAgent    = "another agent"
	authorDispatch = "Dispatch"
)

// Author names the writer of a text as a record shows it: "the writer agent" for writer, the
// session that wrote the record's text, or a person's credential acting for it; "person <login>"
// for a person's own write; "another agent" for any other session, or a person's credential
// acting for one; and "Dispatch" for the system.
func Author(actor model.Actor, writer string) string {
	switch {
	case writer != "" && ((actor.Kind == "session" && actor.ID == writer) || (actor.Act != nil && actor.Act.Session == writer)):
		return authorWriter
	case actor.Kind == "user" && actor.Act == nil:
		return "person " + actor.ID
	case actor.Kind == "user", actor.Kind == "session":
		return authorAgent
	case actor.Kind == "system":
		return authorDispatch
	}
	return actor.Kind
}

// queryer is what BuildRecord reads through: the store's pool, or a transaction.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// BuildRecord reads the record of subject's text from database: the text's thread, the document
// it stands in (or the issue's primary document), the issue's title, its decisions document, the
// comments and messages the writer session wrote on the issue, the project's name for a project
// document, and the words the reader asked about before subject.At (store.ReaderWords).
func BuildRecord(ctx context.Context, database *store.Store, subject Subject) (Record, error) {
	q := database.Pool
	record := Record{Text: subject.Text, Reader: subject.Reader}
	at := timeBound(subject.At)

	documentID, quote := subject.DocumentID, subject.Quote
	var err error
	switch {
	case subject.AskID != "":
		record.Thread, err = askThread(ctx, q, subject, at)
	case subject.ReplyTo != "":
		var anchor commentAnchor
		record.Thread, anchor, err = commentThread(ctx, q, subject, at)
		if documentID == "" && anchor.documentID != "" {
			documentID, quote = anchor.documentID, anchor.quote
		}
	case subject.InReplyTo != "":
		record.Thread, err = messageThread(ctx, q, subject, at)
	}
	if err != nil {
		return Record{}, err
	}

	issueKey, projectKey := subject.IssueKey, ""
	if documentID != "" {
		document, owner, err := readDocument(ctx, q, documentID, at)
		if err != nil {
			return Record{}, err
		}
		document.Holds, document.BlockID, document.Quote = true, subject.BlockID, quote
		record.Document = &document
		if issueKey == "" {
			issueKey, projectKey = owner.issueKey, owner.projectKey
		}
		if err := blockQuestion(&record.Text, document.Markdown, subject.BlockID); err != nil {
			return Record{}, err
		}
	}
	if issueKey != "" {
		if err := readIssue(ctx, q, issueKey, at, &record); err != nil {
			return Record{}, err
		}
		if record.WriterTexts, err = writerTexts(ctx, q, issueKey, subject, at); err != nil {
			return Record{}, err
		}
	} else if projectKey != "" {
		if err := q.QueryRow(ctx, `select name from projects where key = $1`, projectKey).Scan(&record.Project); err != nil {
			return Record{}, fmt.Errorf("reviewer: read the name of project %s: %w", projectKey, err)
		}
	}

	if subject.Reader != "" {
		words, err := database.ReaderWords(ctx, model.CanonicalLogin(subject.Reader), subject.At)
		if err != nil {
			return Record{}, fmt.Errorf("reviewer: %w", err)
		}
		for _, word := range words {
			record.ReaderWords = append(record.ReaderWords, word.Word)
		}
	}
	return record, nil
}

// AskSubject is the subject of the stored ask id, for a person's Rewrite or Define of it: its
// wording, its issue, or its document and the block or the quote it stands at, and the session
// that asked it. It reads up to now (a zero At). The caller sets Reader, the person the ask waits
// on.
func AskSubject(ctx context.Context, database *store.Store, id string) (Subject, error) {
	var (
		subject                 = Subject{ID: id, AskID: id}
		issueKey, blockID       string
		blockDocument, document string
		ownerDocument           string
		options                 []byte
		author                  []byte
	)
	err := database.Pool.QueryRow(ctx, `
		select coalesce(issue_key, ''), question, options, multiple, author,
			coalesce(block_id, ''), coalesce(block_artifact_id::text, ''),
			coalesce(anchor->>'artifact_id', ''), coalesce(anchor->>'quote', ''),
			coalesce(artifact_id::text, '')
		from asks where id = $1::uuid`, id).Scan(&issueKey, &subject.Text.Body, &options, &subject.Text.Multiple, &author,
		&blockID, &blockDocument, &document, &subject.Quote, &ownerDocument)
	if err != nil {
		return Subject{}, fmt.Errorf("reviewer: read ask %s: %w", id, err)
	}
	if err := json.Unmarshal(options, &subject.Text.Options); err != nil {
		return Subject{}, fmt.Errorf("reviewer: read the options of ask %s: %w", id, err)
	}
	var actor model.Actor
	if err := json.Unmarshal(author, &actor); err != nil {
		return Subject{}, fmt.Errorf("reviewer: read the author of ask %s: %w", id, err)
	}
	subject.Writer = writerOf(actor)
	subject.IssueKey = issueKey
	subject.Text.Kind = KindAsk
	switch {
	case blockID != "" && blockDocument != "":
		subject.Text.Kind, subject.DocumentID, subject.BlockID, subject.Quote = KindDecisionBlock, blockDocument, blockID, ""
	case document != "":
		subject.DocumentID = document
	default:
		subject.DocumentID, subject.Quote = ownerDocument, ""
	}
	return subject, nil
}

// writerOf is the session that wrote as actor: the session itself, or the one a person's
// credential acted for; "" for a person's own write and for the system.
func writerOf(actor model.Actor) string {
	if actor.Act != nil && actor.Act.Session != "" {
		return actor.Act.Session
	}
	if actor.Kind == "session" {
		return actor.ID
	}
	return ""
}

// timeBound is at as a query's bound: nil, which bounds nothing, for the zero time.
func timeBound(at time.Time) *time.Time {
	if at.IsZero() {
		return nil
	}
	return &at
}

// askThread is the ask subject.AskID with the comments on it, oldest first, before at: the ask
// itself where the text is not that ask, and never the text's own row.
func askThread(ctx context.Context, q queryer, subject Subject, at *time.Time) ([]Entry, error) {
	rows, err := q.Query(ctx, `
		select * from (
			select 'ask' as kind, author, question as text, options, created_at from asks
			where $5::boolean and id = $1::uuid and ($3::timestamptz is null or created_at < $3)
			union all
			select 'comment', author, body, '[]'::jsonb, created_at from comments
			where ask_id = $1::uuid and id::text <> $2 and ($3::timestamptz is null or created_at < $3)
		) thread order by created_at desc limit $4`, subject.AskID, subject.ID, at, threadLimit, subject.ID != subject.AskID)
	if err != nil {
		return nil, fmt.Errorf("reviewer: read the thread of ask %s: %w", subject.AskID, err)
	}
	return threadEntries(rows, subject.Writer, func(row pgx.Rows) (Entry, []byte, error) {
		var (
			entry           Entry
			author, options []byte
		)
		if err := row.Scan(&entry.Kind, &author, &entry.Text, &options, &entry.At); err != nil {
			return Entry{}, nil, err
		}
		if entry.Kind == KindAsk {
			var opts []model.AskOption
			if err := json.Unmarshal(options, &opts); err != nil {
				return Entry{}, nil, err
			}
			entry.Text = askText(entry.Text, opts)
		}
		return entry, author, nil
	})
}

// commentAnchor is where the first comment of a thread stands in a document: the document and
// the words it quotes.
type commentAnchor struct{ documentID, quote string }

// commentThread is the comment thread subject.ReplyTo is in, oldest first, before at: every
// comment from the thread's first down through the replies, and where that first comment stands
// in a document.
func commentThread(ctx context.Context, q queryer, subject Subject, at *time.Time) ([]Entry, commentAnchor, error) {
	var anchor commentAnchor
	rows, err := q.Query(ctx, `
		with recursive up as (
			select id, reply_to, 0 as depth from comments where id = $1::uuid
			union all
			select c.id, c.reply_to, up.depth + 1 from comments c join up on c.id = up.reply_to
			where up.depth < $5
		), root as (
			select id from up order by depth desc limit 1
		), down as (
			select id, 0 as depth from root
			union all
			select c.id, down.depth + 1 from comments c join down on c.reply_to = down.id
			where down.depth < $5
		)
		select c.author, c.body, c.created_at, c.id in (select id from root),
			coalesce(c.anchor->>'artifact_id', c.artifact_id::text, ''), coalesce(c.anchor->>'quote', '')
		from comments c
		where c.id in (select id from down) and c.id::text <> $2
			and ($3::timestamptz is null or c.created_at < $3)
		order by c.created_at desc limit $4`, subject.ReplyTo, subject.ID, at, threadLimit, threadDepth)
	if err != nil {
		return nil, anchor, fmt.Errorf("reviewer: read the thread of comment %s: %w", subject.ReplyTo, err)
	}
	entries, err := threadEntries(rows, subject.Writer, func(row pgx.Rows) (Entry, []byte, error) {
		var (
			entry         Entry
			author        []byte
			root          bool
			document, quo string
		)
		if err := row.Scan(&author, &entry.Text, &entry.At, &root, &document, &quo); err != nil {
			return Entry{}, nil, err
		}
		if root {
			anchor = commentAnchor{documentID: document, quote: quo}
		}
		entry.Kind = KindComment
		return entry, author, nil
	})
	return entries, anchor, err
}

// messageThread is the message thread subject.InReplyTo is in, oldest first, before at.
func messageThread(ctx context.Context, q queryer, subject Subject, at *time.Time) ([]Entry, error) {
	rows, err := q.Query(ctx, `
		with recursive up as (
			select id, in_reply_to, 0 as depth from messages where id = $1::uuid
			union all
			select m.id, m.in_reply_to, up.depth + 1 from messages m join up on m.id = up.in_reply_to
			where up.depth < $5
		), root as (
			select id from up order by depth desc limit 1
		), down as (
			select id, 0 as depth from root
			union all
			select m.id, down.depth + 1 from messages m join down on m.in_reply_to = down.id
			where down.depth < $5
		)
		select m.author, m.body, m.created_at from messages m
		where m.id in (select id from down) and m.id::text <> $2
			and ($3::timestamptz is null or m.created_at < $3)
		order by m.created_at desc limit $4`, subject.InReplyTo, subject.ID, at, threadLimit, threadDepth)
	if err != nil {
		return nil, fmt.Errorf("reviewer: read the thread of message %s: %w", subject.InReplyTo, err)
	}
	return threadEntries(rows, subject.Writer, func(row pgx.Rows) (Entry, []byte, error) {
		var (
			entry  Entry
			author []byte
		)
		if err := row.Scan(&author, &entry.Text, &entry.At); err != nil {
			return Entry{}, nil, err
		}
		entry.Kind = KindMessage
		return entry, author, nil
	})
}

// threadEntries reads rows, the newest first, into entries the oldest first, each with its
// author named for writer (Author).
func threadEntries(rows pgx.Rows, writer string, scan func(pgx.Rows) (Entry, []byte, error)) ([]Entry, error) {
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		entry, author, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("reviewer: read a thread: %w", err)
		}
		var actor model.Actor
		if err := json.Unmarshal(author, &actor); err != nil {
			return nil, fmt.Errorf("reviewer: read the author of a thread's text: %w", err)
		}
		entry.Author = Author(actor, writer)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reviewer: read a thread: %w", err)
	}
	reverse(entries)
	return entries, nil
}

func reverse(entries []Entry) {
	for i, j := 0, len(entries)-1; i < j; i, j = i+1, j-1 {
		entries[i], entries[j] = entries[j], entries[i]
	}
}

// documentOwner is the issue or the project a document belongs to.
type documentOwner struct{ issueKey, projectKey string }

// versionAt is the Markdown of the newest version of the document a.id written at or before $2
// (no bound where $2 is null), "" where it has none.
const versionAt = `coalesce((select v.markdown from artifact_versions v
	where v.artifact_id = a.id and ($2::timestamptz is null or v.created_at <= $2)
	order by v.number desc limit 1), '')`

// blockQuestion gives text, a decision block's, the block's question as the document's markdown
// holds it (docs.AskBlockQuestionMarkdown): with the pictures, the links and the marks the ask's
// row leaves out, so every rule of what a rewrite keeps weighs them (Preserve) and the rewrite
// writes them back (the server's rewriteDecisionBlock). It leaves every other kind's text as it
// is, and a block the version read does not hold yet, whose row is all there is to read.
func blockQuestion(text *Text, markdown, blockID string) error {
	if text.Kind != KindDecisionBlock || blockID == "" {
		return nil
	}
	question, err := docs.AskBlockQuestionMarkdown(markdown, blockID)
	switch {
	case errors.Is(err, pmdoc.ErrTargetNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("reviewer: read the question of decision block %s: %w", blockID, err)
	}
	text.Body = question
	return nil
}

// readDocument is the document id at at, and its owner.
func readDocument(ctx context.Context, q queryer, id string, at *time.Time) (Document, documentOwner, error) {
	var (
		document Document
		owner    documentOwner
	)
	err := q.QueryRow(ctx, `select a.name, coalesce(a.issue_key, ''), coalesce(a.project_key, ''), `+versionAt+`
		from artifacts a where a.id = $1::uuid`, id, at).Scan(&document.Name, &owner.issueKey, &owner.projectKey, &document.Markdown)
	if err != nil {
		return Document{}, documentOwner{}, fmt.Errorf("reviewer: read document %s: %w", id, err)
	}
	return document, owner, nil
}

// readIssue reads into record the issue's title, its primary document where the record has no
// document, and its decisions document.
func readIssue(ctx context.Context, q queryer, key string, at *time.Time, record *Record) error {
	if err := q.QueryRow(ctx, `select title from issues where key = $1`, key).Scan(&record.IssueTitle); err != nil {
		return fmt.Errorf("reviewer: read issue %s: %w", key, err)
	}
	record.IssueKey = key
	rows, err := q.Query(ctx, `select a.is_primary, a.name, `+versionAt+`
		from artifacts a where a.issue_key = $1 and (a.is_primary or a.role = 'decisions')
		order by a.is_primary desc, a.created_at`, key, at)
	if err != nil {
		return fmt.Errorf("reviewer: read the documents of issue %s: %w", key, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			primary        bool
			name, markdown string
		)
		if err := rows.Scan(&primary, &name, &markdown); err != nil {
			return fmt.Errorf("reviewer: read the documents of issue %s: %w", key, err)
		}
		switch {
		case primary && record.Document == nil:
			record.Document = &Document{Name: name, Markdown: markdown}
		case !primary && record.Decisions == "":
			record.Decisions = markdown
		}
	}
	return rows.Err()
}

// writerTexts are the comments and messages subject.Writer wrote on the issue key, and on its
// documents, before at, oldest first: its newest writerTextsLimit.
func writerTexts(ctx context.Context, q queryer, key string, subject Subject, at *time.Time) ([]Entry, error) {
	if subject.Writer == "" {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		select kind, body, created_at from (
			select 'comment' as kind, c.id, c.author, c.body, c.created_at from comments c
			where c.issue_key = $1 or c.artifact_id in (select id from artifacts where issue_key = $1)
			union all
			select 'message', m.id, m.author, m.body, m.created_at from messages m where m.issue_key = $1
		) texts
		where ((author->>'kind' = 'session' and author->>'id' = $2) or author->'act'->>'session' = $2)
			and id::text <> $3 and ($4::timestamptz is null or created_at < $4)
		order by created_at desc limit $5`, key, subject.Writer, subject.ID, at, writerTextsLimit)
	if err != nil {
		return nil, fmt.Errorf("reviewer: read what the writer wrote on issue %s: %w", key, err)
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		entry := Entry{Author: authorWriter}
		if err := rows.Scan(&entry.Kind, &entry.Text, &entry.At); err != nil {
			return nil, fmt.Errorf("reviewer: read what the writer wrote on issue %s: %w", key, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reviewer: read what the writer wrote on issue %s: %w", key, err)
	}
	reverse(entries)
	return entries, nil
}

// askText is an ask's question with its options, one line each, as a thread shows it.
func askText(question string, options []model.AskOption) string {
	var text strings.Builder
	text.WriteString(question)
	for _, option := range options {
		text.WriteString("\n- " + optionLine(option))
	}
	return text.String()
}

// optionLine is an option as an ask block writes it: `Label: Description`, or the bare label.
func optionLine(option model.AskOption) string {
	if option.Description == "" {
		return option.Label
	}
	return option.Label + ": " + option.Description
}
