package refs

import (
	"context"
	"errors"
	"github.com/sjawhar/envoy/internal/dispatch/text"
	"maps"
	"reflect"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore/graphstoretest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The reads below answer nodes and each binds the registry's predicate of every kind it answers
// (visibility.go). Each test reserves one node of its fixture for its author and reads it twice:
// as another person, who is answered nothing of it, and as its author, who is answered it. The
// census holds every such read to the registry by its SQL
// (TestEveryNodeKindAnswersAVisibilityPredicate); these hold each one to what it answers.

// privateAuthor writes the nodes these tests reserve, and privateReader is anybody else. Both read
// with a viewer that sees every project (model.ServerViewer), so the author rule alone decides.
var (
	privateAuthor = model.Actor{Kind: "user", ID: "alice", Viewer: model.ServerViewer}
	privateReader = model.Actor{Kind: "user", ID: "bob", Viewer: model.ServerViewer}
)

// reserveForAuthor writes the store's node of kind and id reserved for privateAuthor, through the
// store's own writer and the visibility a write that asked for `author` would get
// (graphstoretest.VisibilityFor), handed the one rule that answers whether a credential may
// reserve a node (graphstore.ProvesAuthor).
func reserveForAuthor(t *testing.T, tx pgx.Tx, kind, id string) {
	t.Helper()
	visibility, err := graphstoretest.VisibilityFor(graphstore.VisibilityAuthor, graphstore.ProvesAuthor(privateAuthor))
	if err != nil {
		t.Fatalf("reserve %s %s for its author: %v", kind, id, err)
	}
	if err := graphstore.Write(context.Background(), tx, []graphstore.Node{{
		Kind: kind, ID: id, Supertag: kind, Visibility: visibility, State: graphstore.StateLive,
		MadeAt: time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC), MadeBy: privateAuthor, SourceEvent: graphstore.NoEvent,
	}}, nil); err != nil {
		t.Fatalf("write the private %s %s: %v", kind, id, err)
	}
}

// closureOf is the members of CORE-1's references read as who reads it.
func closureOf(t *testing.T, tx pgx.Tx, who model.Actor) []Member {
	t.Helper()
	references, err := Closure(context.Background(), tx, who, "CORE-1", text.NewOrigins(""))
	if err != nil {
		t.Fatalf("read the closure as %s: %v", who.ID, err)
	}
	return references.Members
}

// linkedFromOf is the nodes that link to CORE-1 or to its own content, as who reads them, each as
// the tests compare a row of linked_from (relatedSummary): its node, its name, the kinds of its
// links and its via, sorted.
func linkedFromOf(t *testing.T, tx pgx.Tx, who model.Actor) []string {
	t.Helper()
	references, err := Closure(context.Background(), tx, who, "CORE-1", text.NewOrigins(""))
	if err != nil {
		t.Fatalf("read the references as %s: %v", who.ID, err)
	}
	rows := make([]string, 0, len(references.LinkedFrom))
	for _, row := range references.LinkedFrom {
		rows = append(rows, relatedSummary(row.Node.Kind, row.Node.ID, row.Name, row.Kinds, row.Via))
	}
	slices.Sort(rows)
	return rows
}

// A private comment's links are no part of the closure another person reads, so it is neither
// a member's via nor reaches a member, and a private document is no member at all. Its author
// reads both.
func TestTheClosureAnswersANodeReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	_, publicDocument := createReferenceArtifact(t, tx, "", "CORE", "public-md")
	_, privateDocument := createReferenceArtifact(t, tx, "", "CORE", "private-md")
	publicComment := createReferenceComment(t, tx, "CORE-1")
	privateComment := createReferenceComment(t, tx, "CORE-1")
	insertReference(t, tx, "comment", privateComment, "artifact", publicDocument)
	insertReference(t, tx, "comment", publicComment, "artifact", privateDocument)
	reserveForAuthor(t, tx, "comment", privateComment)
	reserveForAuthor(t, tx, "artifact", privateDocument)

	if got := closureOf(t, tx, privateReader); len(got) != 0 {
		t.Errorf("another person's closure = %#v; want no member, since one link is the private comment's and the other reaches the private document", got)
	}
	want := []Member{
		{RefKey: privateDocument, Depth: 1, Via: model.ReferenceVia{Kind: "comment", ID: publicComment}},
		{RefKey: publicDocument, Depth: 1, Via: model.ReferenceVia{Kind: "comment", ID: privateComment}},
	}
	sort.Slice(want, func(left, right int) bool { return want[left].RefKey < want[right].RefKey })
	if got := closureOf(t, tx, privateAuthor); !reflect.DeepEqual(got, want) {
		t.Errorf("the author's closure = %#v; want %#v", got, want)
	}
}

// The references read reads one hop, under the visibility of the reader. The issue's comment links
// a public document A and, directly, a document B reserved for its author; A links a third
// document C reserved for its author. Another person's read is A alone, at depth 1 through the
// comment: nothing of B, which is reserved for its author, and nothing of C. The author's read is
// A and B, each at depth 1 through the comment. C is in neither: it is two hops from the issue's
// own content, and the read of one hop never reaches it, whoever reads.
func TestTheReferencesReadAnswersADocumentReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	publicDocument, publicDocumentKey := createReferenceArtifact(t, tx, "", "CORE", "public-md")
	_, privateDocumentKey := createReferenceArtifact(t, tx, "", "CORE", "private-md")
	_, farDocumentKey := createReferenceArtifact(t, tx, "", "CORE", "far-private-md")
	comment := createReferenceComment(t, tx, "CORE-1")
	insertReference(t, tx, "comment", comment, "artifact", publicDocumentKey)
	insertReference(t, tx, "comment", comment, "artifact", privateDocumentKey)
	insertReference(t, tx, "artifact", publicDocument, "artifact", farDocumentKey)
	reserveForAuthor(t, tx, "artifact", privateDocumentKey)
	reserveForAuthor(t, tx, "artifact", farDocumentKey)

	via := model.ReferenceVia{Kind: "comment", ID: comment}
	public := Member{RefKey: publicDocumentKey, Depth: 1, Via: via}
	if got, want := closureOf(t, tx, privateReader), []Member{public}; !reflect.DeepEqual(got, want) {
		t.Errorf("another person's read = %#v; want %#v, without the private document %s and the far one %s",
			got, want, privateDocumentKey, farDocumentKey)
	}
	want := []Member{public, {RefKey: privateDocumentKey, Depth: 1, Via: via}}
	sort.Slice(want, func(left, right int) bool { return want[left].RefKey < want[right].RefKey })
	if got := closureOf(t, tx, privateAuthor); !reflect.DeepEqual(got, want) {
		t.Errorf("the author's read = %#v; want %#v, without the far document %s, which is two hops away",
			got, want, farDocumentKey)
	}
}

// The references read reads the links into the issue under the visibility of the reader too. A
// public document A and a document B reserved for its author each link the issue CORE-1 itself.
// Another person's read lists A alone among the nodes that link in, through the issue: nothing of
// B, which is reserved for its author. The author's read lists A and B, each through the issue. A
// link in carries no depth: its row is the node that links, the kinds of its links and its via,
// the node of the issue's own at the near end (Closure's LinkedFrom, the in statement's rows).
func TestTheReferencesReadAnswersALinkFromADocumentReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	publicDocument, _ := createReferenceArtifact(t, tx, "", "CORE", "public-md")
	privateDocument, privateDocumentKey := createReferenceArtifact(t, tx, "", "CORE", "private-md")
	insertReference(t, tx, "artifact", publicDocument, "issue", "CORE-1")
	insertReference(t, tx, "artifact", privateDocument, "issue", "CORE-1")
	reserveForAuthor(t, tx, "artifact", privateDocumentKey)

	via := model.ReferenceVia{Kind: "issue", ID: "CORE-1"}
	public := relatedSummary("artifact", publicDocument, "public-md", []string{"mentions"}, via)
	if got, want := linkedFromOf(t, tx, privateReader), []string{public}; !slices.Equal(got, want) {
		t.Errorf("another person's linked_from = %q; want %q, without the private document %s",
			got, want, privateDocumentKey)
	}
	want := []string{public, relatedSummary("artifact", privateDocument, "private-md", []string{"mentions"}, via)}
	slices.Sort(want)
	if got := linkedFromOf(t, tx, privateAuthor); !slices.Equal(got, want) {
		t.Errorf("the author's linked_from = %q; want %q", got, want)
	}
}

// insertBlockLink writes a mention that the block blockID of the document artifactID holds, to
// the node toKind/toID, as IndexBlocks writes one into block_refs.
func insertBlockLink(t *testing.T, tx pgx.Tx, artifactID, blockID, fromNode, toKind, toID string) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `
		insert into block_refs (artifact_id, block_id, from_node, to_kind, to_id, kind, created_version)
		values ($1::uuid, $2, $3, $4, $5, 'mentions', 1)
	`, artifactID, blockID, fromNode, toKind, toID); err != nil {
		t.Fatalf("insert the block link %s -> %s/%s: %v", fromNode, toKind, toID, err)
	}
}

// The references read reads the issue's own documents only where the reader may see them. CORE-1's
// document D, reserved for its author, links two public documents: A through the block b1 that
// holds the mention (block_refs), and B through a refs row of its own that no block holds. Another
// person reads neither, since the only link to each is D's; the author reads both, each at depth 1
// through D. A is held by the out statement's arm of the documents' block links, and B by its arm
// of their own refs rows, so the test fails when either arm stops holding D to the reader.
func TestTheReferencesReadAnswersTheLinksOfTheIssuesDocumentReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	tx := prefixFixture(t)
	privateDocument, privateDocumentKey := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	_, blockTargetKey := createReferenceArtifact(t, tx, "", "CORE", "block-target-md")
	_, rowTargetKey := createReferenceArtifact(t, tx, "", "CORE", "row-target-md")
	insertDocumentBlock(t, tx, privateDocument, "b1", privateDocumentKey+"#b1")
	insertBlockLink(t, tx, privateDocument, "b1", privateDocumentKey+"#b1", "artifact", blockTargetKey)
	insertReference(t, tx, "artifact", privateDocument, "artifact", rowTargetKey)
	reserveForAuthor(t, tx, "artifact", privateDocumentKey)

	if got := closureOf(t, tx, privateReader); len(got) != 0 {
		t.Errorf("another person's read = %#v; want no member, since every link is the private document %s's", got, privateDocumentKey)
	}
	via := model.ReferenceVia{Kind: "artifact", ID: privateDocument}
	want := []Member{{RefKey: blockTargetKey, Depth: 1, Via: via}, {RefKey: rowTargetKey, Depth: 1, Via: via}}
	sort.Slice(want, func(left, right int) bool { return want[left].RefKey < want[right].RefKey })
	if got := closureOf(t, tx, privateAuthor); !reflect.DeepEqual(got, want) {
		t.Errorf("the author's read = %#v; want %#v", got, want)
	}
}

// The references read reads the links into the issue's own items only where the reader may see the
// item. A public document links CORE-1's comment C, which is reserved for its author. Another
// person's read lists no node linking in: the link's target, and so its via, is C. The author's
// read lists the document, through C.
func TestTheReferencesReadAnswersALinkToTheIssuesCommentReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	tx := prefixFixture(t)
	publicDocument, _ := createReferenceArtifact(t, tx, "", "CORE", "public-md")
	privateComment := createReferenceComment(t, tx, "CORE-1")
	insertReference(t, tx, "artifact", publicDocument, "comment", privateComment)
	reserveForAuthor(t, tx, "comment", privateComment)

	if got := linkedFromOf(t, tx, privateReader); len(got) != 0 {
		t.Errorf("another person's linked_from = %q; want none, since the one link in ends at the private comment %s", got, privateComment)
	}
	want := []string{relatedSummary("artifact", publicDocument, "public-md", []string{"mentions"}, model.ReferenceVia{Kind: "comment", ID: privateComment})}
	if got := linkedFromOf(t, tx, privateAuthor); !slices.Equal(got, want) {
		t.Errorf("the author's linked_from = %q; want %q", got, want)
	}
}

// The in statement's own target filter is the one clause that holds a link read from an item's
// block: the arm of an ask's, a comment's or a message's block links binds its source block and
// its item, and no target clause, where the refs and block_refs arms bind the target again. An ask
// of a public document has a part, an option, that links CORE-1's comment C, reserved for its
// author: an option's link is in ask_block_refs and in no refs row of the ask. Another person's
// read lists no node linking in; the author's lists the ask, through C.
func TestTheReferencesReadAnswersALinkFromAnAskPartToTheIssuesCommentReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	tx := prefixFixture(t)
	ctx := context.Background()
	publicDocument, _ := createReferenceArtifact(t, tx, "", "CORE", "public-md")
	privateComment := createReferenceComment(t, tx, "CORE-1")
	const question = "Which option holds?"
	var ask string
	if err := tx.QueryRow(ctx, `
		insert into asks (artifact_id, author, question)
		values ($1::uuid, '{"kind":"user","id":"alice"}', $2)
		returning id::text
	`, publicDocument, question).Scan(&ask); err != nil {
		t.Fatalf("create the document's ask: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into ask_blocks (ask_id, block_id, node_id, position, type, text_sha256, excerpt,
		                        made_at, changed_revision, changed_at)
		values ($1::uuid, 'b2', 'ask/' || $1 || '#b2', 1, 'paragraph', 'sha', 'An option.', now(), 1, now())
	`, ask); err != nil {
		t.Fatalf("index the ask's option part: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		insert into ask_block_refs (ask_id, block_id, from_node, to_kind, to_id, kind)
		values ($1::uuid, 'b2', 'ask/' || $1 || '#b2', 'comment', $2, 'mentions')
	`, ask, privateComment); err != nil {
		t.Fatalf("link the private comment from the ask's option: %v", err)
	}
	reserveForAuthor(t, tx, "comment", privateComment)

	if got := linkedFromOf(t, tx, privateReader); len(got) != 0 {
		t.Errorf("another person's linked_from = %q; want none, since the one link in ends at the private comment %s", got, privateComment)
	}
	want := []string{relatedSummary("ask", ask, question, []string{"mentions"}, model.ReferenceVia{Kind: "comment", ID: privateComment})}
	if got := linkedFromOf(t, tx, privateAuthor); !slices.Equal(got, want) {
		t.Errorf("the author's linked_from = %q; want %q", got, want)
	}
}

// The references read reads a block of the issue's own document only where the reader may see the
// block, whichever end of a link it is. CORE-1's document D is public and its block b1 is reserved
// for its author. As a source, b1 holds D's mention of a public document A (block_refs, and D's own
// refs row, which a block holds and so is read as the block's link alone). As a target, a public
// document B links b1. Another person reads neither A among the links out nor B among the links
// in; the author reads A, through D, and B, through D, the document b1 counts as.
func TestTheReferencesReadAnswersALinkOfTheIssuesBlockReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	tx := prefixFixture(t)
	document, documentKey := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	_, targetKey := createReferenceArtifact(t, tx, "", "CORE", "target-md")
	source, _ := createReferenceArtifact(t, tx, "", "CORE", "source-md")
	privateBlock := documentKey + "#b1"
	insertDocumentBlock(t, tx, document, "b1", privateBlock)
	insertBlockLink(t, tx, document, "b1", privateBlock, "artifact", targetKey)
	insertReference(t, tx, "artifact", document, "artifact", targetKey)
	insertReference(t, tx, "artifact", source, "block", privateBlock)
	reserveForAuthor(t, tx, "block", privateBlock)

	if got := closureOf(t, tx, privateReader); len(got) != 0 {
		t.Errorf("another person's read = %#v; want no member, since the one link out is the private block %s's", got, privateBlock)
	}
	if got := linkedFromOf(t, tx, privateReader); len(got) != 0 {
		t.Errorf("another person's linked_from = %q; want none, since the one link in ends at the private block %s", got, privateBlock)
	}
	if got, want := closureOf(t, tx, privateAuthor), []Member{{RefKey: targetKey, Depth: 1, Via: model.ReferenceVia{Kind: "artifact", ID: document}}}; !reflect.DeepEqual(got, want) {
		t.Errorf("the author's read = %#v; want %#v", got, want)
	}
	want := []string{relatedSummary("artifact", source, "source-md", []string{"mentions"}, model.ReferenceVia{Kind: "artifact", ID: documentKey})}
	if got := linkedFromOf(t, tx, privateAuthor); !slices.Equal(got, want) {
		t.Errorf("the author's linked_from = %q; want %q", got, want)
	}
}

// A private target is no mention another person reads among a document's outgoing ones, and a
// private document lists no mention to them at all. Its author reads both.
func TestOutgoingAnswersANodeReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	ctx := context.Background()
	source, _ := createReferenceArtifact(t, tx, "", "CORE", "source-md")
	privateSource, privateSourceKey := createReferenceArtifact(t, tx, "", "CORE", "secret-md")
	_, publicTarget := createReferenceArtifact(t, tx, "", "CORE", "public-md")
	_, privateTarget := createReferenceArtifact(t, tx, "", "CORE", "private-md")
	insertReference(t, tx, "artifact", source, "artifact", publicTarget)
	insertReference(t, tx, "artifact", source, "artifact", privateTarget)
	insertReference(t, tx, "artifact", privateSource, "artifact", publicTarget)
	reserveForAuthor(t, tx, "artifact", privateTarget)
	reserveForAuthor(t, tx, "artifact", privateSourceKey)

	targets := func(who model.Actor, document string) []string {
		t.Helper()
		outgoing, err := Outgoing(ctx, tx, who, document)
		if err != nil {
			t.Fatalf("read the mentions of %s as %s: %v", document, who.ID, err)
		}
		names := []string{}
		for _, reference := range outgoing {
			names = append(names, reference.ToID)
		}
		return names
	}
	both := []string{privateTarget, publicTarget}
	slices.Sort(both)
	for _, read := range []struct {
		who      model.Actor
		document string
		want     []string
	}{
		{privateReader, source, []string{publicTarget}},
		{privateReader, privateSource, []string{}},
		{privateAuthor, source, both},
		{privateAuthor, privateSource, []string{publicTarget}},
	} {
		if got := targets(read.who, read.document); !reflect.DeepEqual(got, read.want) {
			t.Errorf("the mentions of %s read as %s = %v; want %v", read.document, read.who.ID, got, read.want)
		}
	}
}

// A private comment is no candidate another person's prefix names: a prefix it alone carries
// names nothing, and one it shares names the public comment alone, so neither its id nor its
// first line is answered. A private block is no candidate either. Its author reads both, the
// shared prefix as the two comments it names.
func TestResolvePrefixAnswersANodeReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	ctx := context.Background()
	const (
		shared         = "5a5a5a5a"
		publicID       = shared + "-1111-4000-8000-000000000001"
		privateID      = shared + "-2222-4000-8000-000000000002"
		privateOnly    = shared + "-2222"
		privateBody    = "The private body."
		privateBlock   = "bbbb2222aa"
		privateExcerpt = "A private excerpt."
	)
	insertPrefixComment(t, tx, publicID, "CORE-1", "The public body.")
	insertPrefixComment(t, tx, privateID, "CORE-1", privateBody)
	document, documentKey := createReferenceArtifact(t, tx, "", "CORE", "blocks-md")
	insertPrefixBlock(t, tx, document, documentKey, privateBlock, privateExcerpt, 1)
	reserveForAuthor(t, tx, "comment", privateID)
	reserveForAuthor(t, tx, "block", documentKey+"#"+privateBlock)

	resolve := func(who model.Actor, kind string, scope Scope, prefix string) (Candidate, error) {
		return ResolvePrefix(ctx, tx, who, kind, scope, prefix, Viewer{Human: true})
	}
	if got, err := resolve(privateReader, "comment", Scope{}, shared); err != nil || got.ID != publicID {
		t.Errorf("another person's comment prefix %q = %#v, %v; want the public comment alone", shared, got, err)
	}
	if got, err := resolve(privateReader, "comment", Scope{}, privateOnly); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("another person's comment prefix %q = %#v, %v; want no match", privateOnly, got, err)
	}
	if got, err := resolve(privateReader, "block", Scope{Document: documentKey}, privateBlock[:8]); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("another person's block prefix %q = %#v, %v; want no match", privateBlock[:8], got, err)
	}

	var ambiguous *ErrPrefixAmbiguous
	if _, err := resolve(privateAuthor, "comment", Scope{}, shared); !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 2 {
		t.Errorf("the author's comment prefix %q = %v; want both comments listed", shared, err)
	}
	if got, err := resolve(privateAuthor, "comment", Scope{}, privateOnly); err != nil || got.ID != privateID || got.FirstLine != privateBody {
		t.Errorf("the author's comment prefix %q = %#v, %v; want the private comment with its first line", privateOnly, got, err)
	}
	if got, err := resolve(privateAuthor, "block", Scope{Document: documentKey}, privateBlock[:8]); err != nil || got.ID != privateBlock || got.FirstLine != privateExcerpt {
		t.Errorf("the author's block prefix %q = %#v, %v; want the private block with its excerpt", privateBlock[:8], got, err)
	}
}

// ResolvePrefixes answers each reader what ResolvePrefix answers the same reader: a private
// comment is no candidate another person's batch names, so a prefix it alone carries names
// nothing, and one it shares names the public comment alone and is not ambiguous. A private
// block is no candidate either, of the latest indexed version or of a version a reference pins.
// Its author reads every one of them, the shared prefix as ambiguous. Each reader asks every id
// in one call, so the statement of the comments and the two statements of the blocks each bind
// that reader's credential within one batch.
func TestResolvePrefixesAnswersANodeReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	ctx := context.Background()
	const (
		shared       = "6b6b6b6b"
		publicID     = shared + "-1111-4000-8000-000000000001"
		privateID    = shared + "-2222-4000-8000-000000000002"
		privateOnly  = shared + "-2222"
		privateBody  = "The private body."
		privateBlock = "cccc2222aa"
		privateText  = "A private passage."
	)
	insertPrefixComment(t, tx, publicID, "CORE-1", "The public body.")
	insertPrefixComment(t, tx, privateID, "CORE-1", privateBody)
	document, documentKey := createReferenceArtifact(t, tx, "", "CORE", "batch-md")
	insertPrefixBlock(t, tx, document, documentKey, privateBlock, privateText, 1)
	recorded := privateText
	insertPrefixBlockText(t, tx, document, privateBlock, 1, &recorded)
	reserveForAuthor(t, tx, "comment", privateID)
	reserveForAuthor(t, tx, "block", documentKey+"#"+privateBlock)

	ids := []ShortID{
		{Kind: "comment", Prefix: shared},
		{Kind: "comment", Prefix: privateOnly},
		{Kind: "block", Scope: Scope{Document: documentKey}, Prefix: privateBlock[:8]},
		{Kind: "block", Scope: Scope{Document: documentKey, Version: 1}, Prefix: privateBlock[:8]},
	}
	for _, read := range []struct {
		who model.Actor
		// named is the item each id names, by its position in ids, and ambiguous the ids two
		// items carry; an id in neither names nothing.
		named     map[int]string
		ambiguous []int
	}{
		{privateReader, map[int]string{0: publicID}, nil},
		{privateAuthor, map[int]string{1: privateID, 2: privateBlock, 3: privateBlock}, []int{0}},
	} {
		who := read.who
		whole, ambiguous, err := ResolvePrefixes(ctx, tx, &who, ids, "")
		if err != nil {
			t.Fatalf("ResolvePrefixes as %s: %v", who.ID, err)
		}
		for index, id := range ids {
			got, found := whole[id]
			if want, named := read.named[index]; named != found || got.ID != want {
				t.Errorf("ResolvePrefixes as %s names %#v for the %s prefix %q in %+v; want %q", who.ID, got, id.Kind, id.Prefix, id.Scope, want)
			}
			if want := slices.Contains(read.ambiguous, index); ambiguous[id] != want {
				t.Errorf("ResolvePrefixes as %s marks the %s prefix %q in %+v ambiguous %t; want %t", who.ID, id.Kind, id.Prefix, id.Scope, ambiguous[id], want)
			}
			// The same id through ResolvePrefix, as the same reader, names the same item.
			single, err := ResolvePrefix(ctx, tx, who, id.Kind, id.Scope, id.Prefix, Viewer{Human: true})
			var many *ErrPrefixAmbiguous
			switch {
			case err == nil:
				if !found || !reflect.DeepEqual(got, single) {
					t.Errorf("as %s, ResolvePrefix names %#v for the %s prefix %q in %+v and ResolvePrefixes %#v", who.ID, single, id.Kind, id.Prefix, id.Scope, got)
				}
			case errors.As(err, &many):
				if !ambiguous[id] {
					t.Errorf("as %s, ResolvePrefix refuses the %s prefix %q in %+v as ambiguous and ResolvePrefixes does not mark it", who.ID, id.Kind, id.Prefix, id.Scope)
				}
			case errors.Is(err, pgx.ErrNoRows):
				if found || ambiguous[id] {
					t.Errorf("as %s, ResolvePrefix names nothing for the %s prefix %q in %+v and ResolvePrefixes %#v, ambiguous %t", who.ID, id.Kind, id.Prefix, id.Scope, got, ambiguous[id])
				}
			default:
				t.Fatalf("ResolvePrefix as %s of the %s prefix %q in %+v: %v", who.ID, id.Kind, id.Prefix, id.Scope, err)
			}
		}
	}
}

// A private comment belongs to no document another person reads, and nor does a public comment
// whose document is private. Its author reads both scopes.
func TestScopeOfAnswersANodeReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	ctx := context.Background()
	spec, specKey := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	publicComment := createReferenceComment(t, tx, "CORE-1")
	privateComment := createReferenceComment(t, tx, "CORE-1")
	reserveForAuthor(t, tx, "comment", privateComment)

	scope := func(who model.Actor, comment string) string {
		t.Helper()
		document, err := ScopeOf(ctx, tx, who, "comment", comment)
		if err != nil {
			t.Fatalf("read the scope of %s as %s: %v", comment, who.ID, err)
		}
		return document
	}
	if got := scope(privateReader, publicComment); got != spec {
		t.Fatalf("another person's scope of the public comment = %q; want the spec %s, or the fixture decides nothing", got, spec)
	}
	if got := scope(privateReader, privateComment); got != "" {
		t.Errorf("another person's scope of the private comment = %q; want none", got)
	}
	if got := scope(privateAuthor, privateComment); got != spec {
		t.Errorf("the author's scope of the private comment = %q; want the spec %s", got, spec)
	}
	reserveForAuthor(t, tx, "artifact", specKey)
	if got := scope(privateReader, publicComment); got != "" {
		t.Errorf("another person's scope of a comment on the private spec = %q; want none", got)
	}
	if got := scope(privateAuthor, publicComment); got != spec {
		t.Errorf("the author's scope of a comment on the private spec = %q; want the spec %s", got, spec)
	}
}

// A private ask is no question another person reads in its document's question map, and a
// private document has no question map to them at all. Its author reads both.
func TestQuestionMapAnswersANodeReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	ctx := context.Background()
	spec, specKey := createReferenceArtifact(t, tx, "CORE-1", "CORE", "spec")
	insertScopeBlockAsk(t, tx, "CORE-1", spec, "q1")
	privateAsk := insertScopeBlockAsk(t, tx, "CORE-1", spec, "q2")
	insertScopeVersion(t, tx, spec, 1, ":::ask{#q1}\nWhich?\n:::\n\n:::ask{#q2}\nWhich?\n:::\n")
	reserveForAuthor(t, tx, "ask", privateAsk)

	questions := func(who model.Actor) []string {
		t.Helper()
		answered, err := QuestionMap(ctx, tx, who, spec)
		if err != nil {
			t.Fatalf("read the question map as %s: %v", who.ID, err)
		}
		return slices.Sorted(maps.Keys(answered))
	}
	if got := questions(privateReader); !reflect.DeepEqual(got, []string{"q1"}) {
		t.Errorf("another person's question map = %v; want q1 alone", got)
	}
	if got := questions(privateAuthor); !reflect.DeepEqual(got, []string{"q1", "q2"}) {
		t.Errorf("the author's question map = %v; want q1 and q2", got)
	}
	reserveForAuthor(t, tx, "artifact", specKey)
	if got := questions(privateReader); len(got) != 0 {
		t.Errorf("another person's question map of the private spec = %v; want none", got)
	}
	if got := questions(privateAuthor); !reflect.DeepEqual(got, []string{"q1", "q2"}) {
		t.Errorf("the author's question map of the private spec = %v; want q1 and q2", got)
	}
}

// A private document's stored excerpt, and an excerpt that is a private block, are no excerpt
// another person reads. Their author reads both.
func TestStoredExcerptsAnswerANodeReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	ctx := context.Background()
	privateDocument, privateDocumentKey := createReferenceArtifact(t, tx, "", "CORE", "private-md")
	publicDocument, publicDocumentKey := createReferenceArtifact(t, tx, "", "CORE", "public-md")
	for _, citing := range []string{privateDocument, publicDocument} {
		if _, err := tx.Exec(ctx, `
			insert into refs (from_kind, from_id, to_kind, to_id, excerpt_block_id, excerpt_text, excerpt_version)
			values ('artifact', $1, 'issue', 'CORE-1', 'b1', 'An excerpt naming CORE-1.', 1)
		`, citing); err != nil {
			t.Fatalf("record the excerpt of %s: %v", citing, err)
		}
	}
	reserveForAuthor(t, tx, "artifact", privateDocumentKey)
	reserveForAuthor(t, tx, "block", publicDocumentKey+"#b1")

	excerpts := func(who model.Actor) []string {
		t.Helper()
		stored, err := StoredExcerpts(ctx, tx, who, "issue", "CORE-1")
		if err != nil {
			t.Fatalf("read the stored excerpts as %s: %v", who.ID, err)
		}
		return slices.Sorted(maps.Keys(stored))
	}
	if got := excerpts(privateReader); len(got) != 0 {
		t.Errorf("another person reads the excerpts of %v; want none", got)
	}
	both := []string{privateDocument, publicDocument}
	slices.Sort(both)
	if got := excerpts(privateAuthor); !reflect.DeepEqual(got, both) {
		t.Errorf("the author reads the excerpts of %v; want %v", got, both)
	}
}

// A block of a comment reserved for its author is no block another person reads a reply as
// answering, and no block of that comment they may name: a block's text is its comment's text.
// Its author reads it either way. The node reserved is the comment's own, the one the store
// holds (graphstore.KindComment): nothing writes a node of kind comment_block, so a read that
// tested one would answer the text to everybody.
func TestReplyBlocksAnswerANodeReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	ctx := context.Background()
	const privateText = "A private block of the parent."
	parent := createReferenceComment(t, tx, "CORE-1")
	node := "comment/" + parent + "#b1"
	if _, err := tx.Exec(ctx, `
		insert into comment_blocks (comment_id, block_id, node_id, position, type, text_sha256, excerpt,
		                            made_at, changed_revision, changed_at)
		values ($1::uuid, 'b1', $2, 0, 'paragraph', 'sha', $3, now(), 1, now())
	`, parent, node, privateText); err != nil {
		t.Fatalf("index the parent's block: %v", err)
	}
	var reply string
	if err := tx.QueryRow(ctx, `
		insert into comments (issue_key, author, body, reply_to, replies_to_block)
		values ('CORE-1', '{"kind":"user","id":"bob"}', 'A reply.', $1::uuid, $2)
		returning id::text
	`, parent, node).Scan(&reply); err != nil {
		t.Fatalf("write the reply: %v", err)
	}
	reserveForAuthor(t, tx, graphstore.KindComment, parent)

	for _, read := range []struct {
		who  model.Actor
		held bool
	}{{privateReader, false}, {privateAuthor, true}} {
		blocks, err := ReplyBlocks(ctx, tx, read.who, ItemComment, []string{reply})
		if err != nil {
			t.Fatalf("read the reply's block as %s: %v", read.who.ID, err)
		}
		if block, answered := blocks[reply]; answered != read.held || answered && block.Text != privateText {
			t.Errorf("the reply's block read as %s = %#v; want answered %t", read.who.ID, block, read.held)
		}
		named, excerpt, found, err := CurrentItemBlock(ctx, tx, read.who, ItemComment, parent, "b1")
		if err != nil {
			t.Fatalf("read the parent's block as %s: %v", read.who.ID, err)
		}
		if found != read.held || found && (named != node || excerpt != privateText) {
			t.Errorf("the parent's block read as %s = %q, %q, %t; want found %t", read.who.ID, named, excerpt, found, read.held)
		}
	}
}

// A private issue is no descendant another person reads at any depth, and neither is an issue
// under it; its author reads them all. CORE-2, a child of CORE-1, holds the walk's first step to
// that, and CORE-5, under the public CORE-4, holds every step below the first.
func TestIssueDescendantsAnswerANodeReservedForItsAuthorToThatAuthorAlone(t *testing.T) {
	t.Parallel()
	tx := prefixFixture(t)
	ctx := context.Background()
	for _, issue := range []struct {
		key, parent string
		number      int
	}{
		{"CORE-2", "CORE-1", 2}, {"CORE-3", "CORE-2", 3},
		{"CORE-4", "CORE-1", 4}, {"CORE-5", "CORE-4", 5}, {"CORE-6", "CORE-5", 6},
	} {
		if _, err := tx.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, parent_key)
			values ($1, 'CORE', $2, 'A child', '{"kind":"user","id":"alice"}', 'U', $3)
		`, issue.key, issue.number, issue.parent); err != nil {
			t.Fatalf("create issue %s: %v", issue.key, err)
		}
	}
	reserveForAuthor(t, tx, "issue", "CORE-2")
	reserveForAuthor(t, tx, "issue", "CORE-5")

	for _, read := range []struct {
		who  model.Actor
		want []string
	}{
		{privateReader, []string{"CORE-4"}},
		{privateAuthor, []string{"CORE-2", "CORE-3", "CORE-4", "CORE-5", "CORE-6"}},
	} {
		got, err := IssueDescendants(ctx, tx, read.who, "CORE-1")
		if err != nil {
			t.Fatalf("read the descendants of CORE-1 as %s: %v", read.who.ID, err)
		}
		if !reflect.DeepEqual(got, read.want) {
			t.Errorf("the descendants of CORE-1 read as %s = %v; want %v", read.who.ID, got, read.want)
		}
	}
}
