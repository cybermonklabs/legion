package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// TestAPersonPageListsWhatWasDeliveredToTheirOwnNode holds the person page to the deliveries this
// release writes to a person's own node: every write that reaches a person appends a delivered_to
// from its comment, ask or message to that person (inboxwrite.Record, through
// graphstore.ToPerson), with no session of theirs in between. carol is reached by a mention alone
// (dispatch://person/<login>, inboxwrite.Write.ReachesMentioned): she writes nothing and holds no
// session, so the received arm's read of the person's own node (personNodesTied) is the one way
// anything is on her page.
//
// In order:
//
//   - alice's comment mentioning carol is on carol's page, as she reads it and as bob does, on
//     one row whose relations are delivered_to alone;
//   - a second such comment, then reserved for alice as a later release's author-private write
//     marks one (the node moved to visibility author through graphstore.Write), is on alice's
//     read of carol's page and on no other, and is not counted either: and where it is all dave
//     was delivered, dave's page answers bob what a login nobody holds answers, byte for byte;
//   - the ask of an issue assigned to carol and an issue message mentioning her are on her page
//     too, each at the dashboard path it opens at, while a direct conversation - erin's message
//     to a session and that session's reply, delivered to erin as who started it - has no path
//     the page spells and is on no row. R15's rule (personPageShownQuery) counts every node of a
//     kind the registry holds a predicate for that the reader may see, which those messages are,
//     so bob reads erin's page as 200 with no rows, as erin does;
//   - the page reads the current tuple: moving the issue to bob retracts carol's delivery of the
//     ask (inboxwrite.QueueMoveWaiting), and the ask leaves her page for his.
func TestAPersonPageListsWhatWasDeliveredToTheirOwnNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	live := true
	sent := []map[string]any{}
	listener := sessionListener(t, &live, &sent)
	t.Cleanup(listener.Close)
	handler, database := newTargetedMessageHandler(t, listener.URL)
	const (
		carol       = "carol@example.com"
		dave        = "dave@example.com"
		erin        = "erin@example.com"
		reader      = "bob"
		nobodyLogin = "nobody-at-all@example.com"
	)
	seedPeople(t, database, carol, reader)
	issue := createInteractionIssue(t, handler, "RECV", "What reached a person", "Spec.\n")
	read := func(person, login string) *httptest.ResponseRecorder {
		t.Helper()
		return dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/"+url.PathEscape(person)+"/nodes", nil, login)
	}
	page := func(person, login string) personNodes {
		t.Helper()
		response := read(person, login)
		if response.Code != http.StatusOK {
			t.Fatalf("%s reads %s's page as %d %s, want 200", login, person, response.Code, response.Body.String())
		}
		return decodeBody[personNodes](t, response)
	}
	mention := func(person string) string { return "[" + person + "](dispatch://person/" + person + ")" }
	nobody := read(nobodyLogin, reader)
	if nobody.Code != http.StatusNotFound {
		t.Fatalf("a login nobody holds answers %d %s, want 404, so the comparisons below prove nothing", nobody.Code, nobody.Body.String())
	}
	// requireUnopened fails unless the reader's read of person's page, why naming what ties
	// person to anything, answers what a login nobody holds answers, with that login spelled as
	// person's wherever it appears.
	requireUnopened := func(person, why string) {
		t.Helper()
		answered := read(person, reader)
		want := strings.ReplaceAll(nobody.Body.String(), nobodyLogin, person)
		if answered.Code != nobody.Code || answered.Body.String() != want ||
			answered.Header().Get("Content-Type") != nobody.Header().Get("Content-Type") {
			t.Errorf("%s reads the page of %s, %s, as %d %q (%s), want what a login nobody holds answers, %d %q (%s)",
				reader, person, why, answered.Code, answered.Body.String(), answered.Header().Get("Content-Type"),
				nobody.Code, want, nobody.Header().Get("Content-Type"))
		}
	}

	// 1. A comment delivered to carol's own node: she neither wrote it nor holds a session.
	delivered := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Over to " + mention(carol) + " for the copy."}, "alice")
	requireDeliveredToPerson(t, database, graphstore.KindComment, delivered.ID, carol)
	if tied, err := graphstore.CurrentIn(ctx, database.Pool, graphstore.KindPerson, carol,
		[]string{graphstore.TupleAuthoredBy, graphstore.TupleAliasOf}); err != nil || len(tied) != 0 {
		t.Fatalf("carol's node holds the authored_by and alias_of tuples %+v (err %v), want none: she wrote nothing and holds no session", tied, err)
	}
	commentRow := personNode{Kind: graphstore.KindComment, ID: delivered.ID,
		Relations: []string{graphstore.TupleDeliveredTo}, Href: "/issues/" + issue.Key + "/comments/" + delivered.ID}
	for _, login := range []string{carol, reader} {
		answered := page(carol, login)
		t.Logf("%s reads %s's page once alice's comment %s is delivered to her: %+v", login, carol, delivered.ID, answered)
		requirePersonRows(t, fmt.Sprintf("%s's page as %s reads it", carol, login), answered, commentRow)
	}

	// 2. What the reader may not see: a second comment delivered to carol, reserved for alice once
	// it is, is on alice's read of carol's page alone; and all dave is delivered is such a comment.
	hidden := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Only for alice, though it names " + mention(carol) + "."}, "alice")
	onlyHidden := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Only for alice, though it names " + mention(dave) + "."}, "alice")
	for _, reached := range []struct{ comment, person string }{{hidden.ID, carol}, {onlyHidden.ID, dave}} {
		requireDeliveredToPerson(t, database, graphstore.KindComment, reached.comment, reached.person)
		if err := reserveForItsAuthor(ctx, database, reached.comment); err != nil {
			t.Fatalf("reserve comment %s for its author: %v", reached.comment, err)
		}
	}
	hiddenRow := personNode{Kind: graphstore.KindComment, ID: hidden.ID,
		Relations: []string{graphstore.TupleDeliveredTo}, Href: "/issues/" + issue.Key + "/comments/" + hidden.ID}
	for _, login := range []string{carol, reader} {
		requirePersonRows(t, fmt.Sprintf("%s's page as %s reads it, beside a comment reserved for alice", carol, login),
			page(carol, login), commentRow)
	}
	requirePersonRows(t, fmt.Sprintf("%s's page as alice, who may see the comment reserved for her, reads it", carol),
		page(carol, "alice"), commentRow, hiddenRow)
	requireUnopened(dave, "all of whose deliveries are a comment reserved for alice")
	requirePersonRows(t, fmt.Sprintf("%s's own page, all of whose deliveries are a comment reserved for alice", dave), page(dave, dave))
	requirePersonRows(t, fmt.Sprintf("%s's page as alice reads it", dave), page(dave, "alice"),
		personNode{Kind: graphstore.KindComment, ID: onlyHidden.ID,
			Relations: []string{graphstore.TupleDeliveredTo}, Href: "/issues/" + issue.Key + "/comments/" + onlyHidden.ID})

	// 3. The other kinds a write delivers to a person: an ask, by the issue it waits on, and an
	// issue message, by a mention. A direct conversation's messages are on no issue.
	assigned := createInteractionIssue(t, handler, "RECA", "An issue assigned to carol", "Spec.\n")
	assignIssue(t, handler, assigned.Key, carol)
	ask := createIssueAsk(t, handler, assigned.Key, "Is the copy final?")
	requireDeliveredToPerson(t, database, graphstore.KindAsk, ask, carol)
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Ask " + mention(carol) + " about the copy."}, "alice")
	requireDeliveredToPerson(t, database, graphstore.KindMessage, message.ID, carol)
	started := postAgentMessage(t, handler, "s1", map[string]any{"body": "Can you look at the deploy?", "delivery": "steer"}, erin)
	if started.Code != http.StatusCreated {
		t.Fatalf("send erin's direct message: status=%d body=%s", started.Code, started.Body.String())
	}
	replied := replyTo(t, handler, decodeBody[model.Message](t, started).ID, "Looking.")
	if replied.Code != http.StatusCreated {
		t.Fatalf("reply to erin's direct message: status=%d body=%s", replied.Code, replied.Body.String())
	}
	requireDeliveredToPerson(t, database, graphstore.KindMessage, decodeBody[model.Message](t, replied).ID, erin)
	askRow := personNode{Kind: graphstore.KindAsk, ID: ask,
		Relations: []string{graphstore.TupleDeliveredTo}, Href: "/issues/" + assigned.Key + "/asks/" + ask}
	messageRow := personNode{Kind: graphstore.KindMessage, ID: message.ID,
		Relations: []string{graphstore.TupleDeliveredTo}, Href: "/issues/" + issue.Key + "/messages/" + message.ID}
	for _, login := range []string{carol, reader} {
		requirePersonRows(t, fmt.Sprintf("%s's page as %s reads it once an ask and an issue message reach her", carol, login),
			page(carol, login), commentRow, askRow, messageRow)
	}
	for _, login := range []string{erin, reader} {
		answered := page(erin, login)
		if answered.Person != erin {
			t.Errorf("%s reads %s's page as the page of %q", login, erin, answered.Person)
		}
		requirePersonRows(t, fmt.Sprintf("%s's page as %s reads it, all of whose nodes are a direct conversation", erin, login), answered)
	}

	// 4. The current tuple: the issue moved to bob moves the ask's delivery with it.
	assignIssue(t, handler, assigned.Key, reader)
	requirePersonRows(t, fmt.Sprintf("%s's page once the ask's issue moved to %s", carol, reader),
		page(carol, reader), commentRow, messageRow)
	requirePersonRows(t, fmt.Sprintf("%s's own page once the ask's issue moved to him", reader), page(reader, reader), askRow)
}

// TestAPersonPageSkipsACommentOnAnAgentConversationsFile holds the list to the rows it can open
// (personNodeItems): a comment on a file an agent's conversation holds - an artifact no project
// and no issue owns - has no path the page spells, so it is skipped, and the page answers the
// rows it can open beside it rather than failing on the one it cannot.
//
// No route writes such a comment (ARTIFACT_AGENT_OWNED); a row one older binary could leave is
// written here by SQL, as TestArtifactReadsServeARowWithNoProject writes one, with what the store
// holds of it: its node, between the parties of the conversation (graphstore.SessionScope), who
// wrote it, as the comment write for a row naming no node states it (graphstore.QueueAuthored),
// and a delivery of it to carol's own node (graphstore.ToPerson). Beside it alice writes a
// comment on an issue that mentions carol. carol's page, which the delivery ties the file's
// comment to, and alice's, which its authored_by ties it to, each answer 200 with the issue's
// comment alone.
func TestAPersonPageSkipsACommentOnAnAgentConversationsFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database := newTestHandlerWithStore(t)
	const (
		carol   = "carol@example.com"
		reader  = "bob"
		session = "session-0123456789abcdef"
	)
	alice := model.Actor{Kind: "user", ID: "alice"}
	issue := createInteractionIssue(t, handler, "FILE", "A comment beside a conversation's file", "Spec.\n")
	onIssue := createThreadComment(t, handler, issue.Key,
		map[string]any{"body": "Over to [carol](dispatch://person/" + carol + ") for the copy."}, alice.ID)
	requireDeliveredToPerson(t, database, graphstore.KindComment, onIssue.ID, carol)

	var file, onFile string
	if err := pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			insert into artifacts (session_id, slug, name, kind, created_by)
			values ($1, 'photo-png', 'photo.png', 'image', jsonb_build_object('kind', 'session', 'id', $1::text))
			returning id::text
		`, session).Scan(&file); err != nil {
			return fmt.Errorf("store a conversation's file: %w", err)
		}
		if err := tx.QueryRow(ctx, `select gen_random_uuid()::text`).Scan(&onFile); err != nil {
			return err
		}
		at := time.Now().UTC()
		_, parties := graphstore.SessionScope("", session)
		if err := graphstore.Write(ctx, tx, []graphstore.Node{{
			Kind: graphstore.KindComment, ID: onFile, Supertag: graphstore.KindComment,
			Visibility: graphstore.VisibilityProject, State: graphstore.StateLive, Parties: parties,
			MadeAt: at, MadeBy: alice, SourceEvent: graphstore.NoEvent,
		}}, nil); err != nil {
			return fmt.Errorf("write the node of the comment on the file: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			insert into comments (id, artifact_id, author, body, node_kind, node_address, created_at)
			values ($1::uuid, $2::uuid, '{"kind":"user","id":"alice"}', 'A remark on the photo.', $3, $1, $4)
		`, onFile, file, graphstore.KindComment, at); err != nil {
			return fmt.Errorf("insert the comment on the file: %w", err)
		}
		if err := store.RunWrite(ctx, tx, func(b *store.WriteBatch) error {
			return graphstore.QueueAuthored(b, graphstore.KindComment, onFile, alice, at, graphstore.NoEvent)
		}); err != nil {
			return fmt.Errorf("record who wrote the comment on the file: %w", err)
		}
		return graphstore.ToPerson(ctx, tx, []graphstore.Tuple{{
			FromKind: graphstore.KindComment, FromID: onFile, Kind: graphstore.TupleDeliveredTo,
			ToKind: graphstore.KindPerson, ToID: carol,
			At: at, Who: alice, SourceEvent: graphstore.NoEvent, Asserted: true,
		}})
	}); err != nil {
		t.Fatalf("write a comment on a conversation's file, delivered to %s: %v", carol, err)
	}
	requireDeliveredToPerson(t, database, graphstore.KindComment, onFile, carol)

	for _, read := range []struct {
		person, login string
		want          personNode
	}{
		{carol, carol, personNode{Kind: graphstore.KindComment, ID: onIssue.ID,
			Relations: []string{graphstore.TupleDeliveredTo}, Href: "/issues/" + issue.Key + "/comments/" + onIssue.ID}},
		{carol, reader, personNode{Kind: graphstore.KindComment, ID: onIssue.ID,
			Relations: []string{graphstore.TupleDeliveredTo}, Href: "/issues/" + issue.Key + "/comments/" + onIssue.ID}},
		{alice.ID, alice.ID, personNode{Kind: graphstore.KindComment, ID: onIssue.ID,
			Relations: []string{graphstore.TupleAuthoredBy}, Href: "/issues/" + issue.Key + "/comments/" + onIssue.ID}},
	} {
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/people/"+url.PathEscape(read.person)+"/nodes", nil, read.login)
		t.Logf("%s reads %s's page beside the comment %s on the conversation's file %s: %d %s",
			read.login, read.person, onFile, file, response.Code, response.Body.String())
		if response.Code != http.StatusOK {
			t.Errorf("%s reads %s's page as %d %s, want 200", read.login, read.person, response.Code, response.Body.String())
			continue
		}
		requirePersonRows(t, fmt.Sprintf("%s's page as %s reads it", read.person, read.login),
			decodeBody[personNodes](t, response), read.want)
	}
}

// requireDeliveredToPerson fails unless the node kind/id holds a current delivered_to into the
// person login's own node: the tuple the page is read for, so a page without it proves nothing.
func requireDeliveredToPerson(t *testing.T, database *store.Store, kind, id, login string) {
	t.Helper()
	if reached := meInboxDeliveredTo(t, database, kind, id); !slices.Contains(reached, graphstore.KindPerson+":"+login) {
		t.Fatalf("%s %s is delivered to %v, want %s's own node among them", kind, id, reached, login)
	}
}

// reserveForItsAuthor moves the comment's node to visibility author, as a later release's
// author-private write marks one: the store's own writer writes the node as it stands with that
// visibility (graphstore.Write), and states the attribute beside it, in one transaction.
func reserveForItsAuthor(ctx context.Context, database *store.Store, comment string) error {
	return pgx.BeginFunc(ctx, database.Pool, func(tx pgx.Tx) error {
		node, found, err := graphstore.ReadNode(ctx, tx, graphstore.KindComment, comment)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("the store holds no node of the comment")
		}
		node.Visibility = graphstore.VisibilityAuthor
		return graphstore.Write(ctx, tx, []graphstore.Node{node}, nil)
	})
}

// requirePersonRows fails unless read, one answer of the person page, is want and nothing else, in
// any order: each row's kind, id, relations and path, and a total that counts them.
func requirePersonRows(t *testing.T, what string, read personNodes, want ...personNode) {
	t.Helper()
	key := func(row personNode) string {
		return row.Kind + ":" + row.ID + " " + strings.Join(row.Relations, ",") + " " + row.Href
	}
	got := make([]string, 0, len(read.Rows))
	for _, row := range read.Rows {
		got = append(got, key(row))
	}
	wanted := make([]string, 0, len(want))
	for _, row := range want {
		wanted = append(wanted, key(row))
	}
	slices.Sort(got)
	slices.Sort(wanted)
	if !slices.Equal(got, wanted) || read.Total != len(want) {
		t.Errorf("%s answers %d row(s), counting %d: %v; want %d: %v", what, len(read.Rows), read.Total, got, len(want), wanted)
	}
}
