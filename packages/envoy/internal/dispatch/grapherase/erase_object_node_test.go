package grapherase

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// An issue, a document and an ask each hold their text in their own row: the issue's title, the
// document's name and versions, the ask's question. The erase reads text only from the comments,
// the messages and the document blocks that derive from a node, so a run over one of these nodes
// would delete the node and its tuples, record an erasure and answer applied, and leave the row
// and its text where they were; the project census would then read the row as missing a node, and
// the backfill would write the node again. The erase refuses each of the three kinds while its
// row stands: run as an operator runs it, without --confirm and then with a digest, each run
// answers the refusal, and the node, its tuples and its row with its text are all still there,
// with no erasure recorded.
//
// A message is erased with its row: that row names the message's node, so it is one of its
// projection rows, and the erase deletes it and marks the message's body in every event that
// carried it - its own message.created, and the quote each reply's event carries of it
// (reply_body, and reply_block's text for a reply that names one of its blocks, with or without
// in_reply_to). A standing reply refuses the run (it points at the message through in_reply_to,
// or states replies_to into its block), so the replies here are their events alone, as each
// reply's own erase leaves them. A message has no edit, so no event of one. The listing names the
// message's row among the rows the run deletes and none of it as left; after the erase no row of
// the message, its node or its tuples stands, every one of those events holds the marker and no
// byte of the quoted body, each reply's own words stand, and so does another message's event that
// says the same words, which is that message's text and the listing lists as left. A message row
// a build before this one wrote names no node until the backfill marks it, so the erase finds it
// by no address and refuses that message, leaving it whole.
//
// Its controls are the refusal taken out of Erasure.read, under which the three kinds' dry run
// lists the node, the confirmed run over that listing answers applied, and the row's text is still
// in place; and the erase reading no reply's event (readEvents), under which a reply's quote of
// the erased message stands.
func TestTheEraseRefusesAnIssueADocumentAndAnAskWhileTheirRowStandsAndTakesAMessagesTextEverywhere(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedObjectNodes(t, database)
	url := database.Pool.Config().ConnString()

	const refusal = "its row stands and holds the node's text, which the erase does not reach"
	for _, node := range []struct{ kind, id, table, column, key string }{
		{graphstore.KindIssue, seeded.issue, "issues", "title", "key"},
		{graphstore.KindArtifact, seeded.documentRef, "artifacts", "name", "ref_key"},
		{graphstore.KindAsk, seeded.ask, "asks", "question", "id::text"},
	} {
		t.Run(node.kind, func(t *testing.T) {
			requireRefusedWhileTheRowStands(t, database, node.kind, node.id, node.table, node.column, node.key, refusal)
		})
	}

	t.Run(graphstore.KindMessage, func(t *testing.T) {
		const body = "A message somebody wrote"
		head := body[:len(body)-6]
		block := "message/" + seeded.message + "#b1"
		own := appendEvent(t, database, seeded.issue, "message.created",
			`{"id":"`+seeded.message+`","issue_key":"ERASE-1","body":"`+body+`","in_reply_to":null,"reply_block":null}`)
		quoting := []int64{
			appendEvent(t, database, seeded.issue, "message.created",
				`{"id":"22222222-2222-4222-8222-000000000011","issue_key":"ERASE-1","body":"Seen it.",`+
					`"in_reply_to":"`+seeded.message+`","reply_block":null,"reply_body":"`+head+`"}`),
			appendEvent(t, database, seeded.issue, "message.created",
				`{"id":"22222222-2222-4222-8222-000000000012","issue_key":"ERASE-1","body":"On that line.",`+
					`"in_reply_to":"`+seeded.message+`","reply_block":{"node":"`+block+`","block_id":"b1","text":"`+body+`","removed":false},`+
					`"reply_body":"`+body+`"}`),
			appendEvent(t, database, seeded.issue, "message.answered",
				`{"id":"22222222-2222-4222-8222-000000000013","issue_key":"ERASE-1","body":"Looking now.",`+
					`"in_reply_to":null,"reply_block":{"node":"`+block+`","block_id":"b1","text":"`+head+`","removed":false},`+
					`"reply_body":"`+head+`"}`),
		}
		other := appendEvent(t, database, seeded.issue, "message.created",
			`{"id":"22222222-2222-4222-8222-000000000014","issue_key":"ERASE-1","body":"`+body+`","in_reply_to":null}`)
		before := readTypeNode(t, database, graphstore.KindMessage, seeded.message)
		if !before.row || before.out == 0 {
			t.Fatalf("the seed holds message:%s as %+v, want a node with tuples, so this test would prove nothing", seeded.message, before)
		}

		listing, err := Erase(ctx, url, eraseServerURL, graphstore.KindMessage, seeded.message, "", 0)
		if err != nil {
			t.Fatalf("erase --node message:%s answered %v, want its listing", seeded.message, err)
		}
		// The listing names the message's row among the rows the run deletes, the four events among
		// the ones it redacts, and no messages row as one it leaves; another message's event, which
		// stands, is listed as left.
		if !listed(listing, "delete messages (id)=", seeded.message) {
			t.Errorf("the listing of message:%s reads\n\t%s\nwith no delete of its messages row", seeded.message, joinedListing(listing))
		}
		if listed(listing, "leave public.messages") {
			t.Errorf("the listing of message:%s reads\n\t%s\nleaving a messages row the run deletes", seeded.message, joinedListing(listing))
		}
		for _, event := range append([]int64{own}, quoting...) {
			if !listed(listing, fmt.Sprintf("events %d ", event)) {
				t.Errorf("the listing of message:%s reads\n\t%s\nwith no redaction of event %d", seeded.message, joinedListing(listing), event)
			}
		}
		if !listed(listing, "leave public.events", fmt.Sprintf("(id)=(%d)", other)) {
			t.Errorf("the listing of message:%s reads\n\t%s\nwith no line leaving another message's event %d", seeded.message, joinedListing(listing), other)
		}
		erased, err := Erase(ctx, url, eraseServerURL, graphstore.KindMessage, seeded.message, listing.Digest, 0)
		if err != nil || !erased.Applied {
			t.Fatalf("the confirmed erase of message:%s answered %+v, %v; want applied", seeded.message, erased, err)
		}
		if held := readTypeNode(t, database, graphstore.KindMessage, seeded.message); held.row || held.out != 0 || held.in != 0 || held.erased != 1 {
			t.Errorf("after the erase the store holds %+v of the message's node, want no row, no tuple and one erasure", held)
		}
		var rows int64
		if err := database.Pool.QueryRow(ctx, `select count(*) from messages where id = $1::uuid or body = $2`,
			seeded.message, body).Scan(&rows); err != nil {
			t.Fatalf("count the message's rows: %v", err)
		}
		if rows != 0 {
			t.Errorf("after the erase %d messages row(s) are the message's or hold its body, want none", rows)
		}
		marker := "[erased by envoy-dispatch erase message:" + seeded.message + ", recorded in graph_erasures]"
		for _, event := range append([]int64{own}, quoting...) {
			payload, raw := eventPayload(t, database, event)
			if strings.Contains(raw, head) || !strings.Contains(raw, marker) {
				t.Errorf("after the erase event %d reads %s, want every byte of the message's body replaced by the marker", event, raw)
			}
			if event == own {
				continue
			}
			if payload["reply_body"] != marker {
				t.Errorf("after the erase event %d's reply_body reads %v, want the marker whole", event, payload["reply_body"])
			}
			if quoted, ok := payload["reply_block"].(map[string]any); ok && quoted["text"] != marker {
				t.Errorf("after the erase event %d's reply_block text reads %v, want the marker whole", event, quoted["text"])
			}
		}
		for index, words := range []string{"Seen it.", "On that line.", "Looking now."} {
			if payload, _ := eventPayload(t, database, quoting[index]); payload["body"] != words {
				t.Errorf("after the erase reply event %d's own body reads %v, want %q as its writer wrote it", quoting[index], payload["body"], words)
			}
		}
		if _, raw := eventPayload(t, database, other); !strings.Contains(raw, body) {
			t.Errorf("after the erase another message's event reads %s, want its own words left standing", raw)
		}

		// A message row an older build wrote, beside the node this build wrote for it, names no node.
		const older = "22222222-2222-4222-8222-000000000015"
		writeOlderMessage(t, database, older, "A message an older build kept")
		requireRefusedWhileTheRowStands(t, database, graphstore.KindMessage, older, "messages", "body", "id::text",
			"its row stands and names no node yet")
	})
}

// requireRefusedWhileTheRowStands runs the erase of kind:id as an operator does, without --confirm
// and then with a digest, and holds each run to the refusal, with the node, its tuples and the row
// that holds its text as they were and no erasure recorded. A dry run that lists the node instead
// is followed by the confirmed run an operator runs next, so the failure says what that erase does
// to the row.
func requireRefusedWhileTheRowStands(t *testing.T, database *store.Store, kind, id, table, column, key, refusal string) {
	t.Helper()
	ctx := context.Background()
	url := database.Pool.Config().ConnString()
	before := readTypeNode(t, database, kind, id)
	if !before.row || before.out == 0 {
		t.Fatalf("the seed holds %s:%s as %+v, want a node with tuples, so this test would prove nothing", kind, id, before)
	}
	text := rowText(t, database, table, column, key, id)
	listing, err := Erase(ctx, url, eraseServerURL, kind, id, "", 0)
	if err == nil && listing != nil {
		confirmed, confirmErr := Erase(ctx, url, eraseServerURL, kind, id, listing.Digest, 0)
		t.Fatalf("erase --node %s:%s listed it and its confirmed run answered applied %t (err %v); the %s row still reads %q",
			kind, id, confirmed != nil && confirmed.Applied, confirmErr, table, rowText(t, database, table, column, key, id))
	}
	for _, confirm := range []string{"", "any digest"} {
		erasure, err := Erase(ctx, url, eraseServerURL, kind, id, confirm, 0)
		if err == nil || !strings.Contains(err.Error(), "refuses "+kind+":"+id+": ") || !strings.Contains(err.Error(), refusal) {
			t.Errorf("erase --node %s:%s (confirm %q) answered %v (applied %t), want the refusal %q",
				kind, id, confirm, err, erasure != nil && erasure.Applied, refusal)
		}
	}
	if after := readTypeNode(t, database, kind, id); after != before {
		t.Errorf("erase --node %s:%s left the store holding %+v of it, want %+v as before the runs", kind, id, after, before)
	}
	if after := rowText(t, database, table, column, key, id); after != text {
		t.Errorf("erase --node %s:%s left its %s row reading %q, want %q as before the runs", kind, id, table, after, text)
	}
}

// writeOlderMessage writes message id on ERASE-1 as a build before this one writes it during a
// roll: the row names no node, beside the node this build's create wrote for the address.
func writeOlderMessage(t *testing.T, database *store.Store, id, body string) {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the older message: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{
		graphstore.MessageNode(id, "ERASE", nil, model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC()),
	}, nil); err != nil {
		t.Fatalf("write the older message's node: %v", err)
	}
	if _, err := tx.Exec(ctx, `insert into messages (id, issue_key, author, body)
		values ($1::uuid, 'ERASE-1', '{"kind":"user","id":"alice"}', $2)`, id, body); err != nil {
		t.Fatalf("write the older message's row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the older message: %v", err)
	}
}

// A build before this one writes no node, so a document it removes during a roll or after a
// rollback leaves the live node this build wrote for it, with no row behind it: the project
// census reads it as orphaned, and no backfill reads a node no row stands behind. The erase is
// that node's remedy: with no row standing, it lists the node and its tuples, removes them on the
// confirmed run, and records the erasure, and the census then reads no orphaned node.
//
// Its control is the refusal made regardless of the row, as Erase made it before a connection
// once: the orphaned node then has no remedy, and the census keeps reading it.
func TestTheEraseRemovesAnOrphanedDocumentsNode(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedObjectNodes(t, database)
	url := database.Pool.Config().ConnString()
	// The removal a build before this one makes: the row goes, and nothing states its node gone.
	if _, err := database.Pool.Exec(ctx, `delete from artifacts where ref_key = $1`, seeded.documentRef); err != nil {
		t.Fatalf("remove the document's row: %v", err)
	}
	requireOrphaned(t, database, 1)

	listing, err := Erase(ctx, url, eraseServerURL, graphstore.KindArtifact, seeded.documentRef, "", 0)
	if err != nil {
		t.Fatalf("erase --node artifact:%s with no row behind it answered %v, want its listing", seeded.documentRef, err)
	}
	if want := "delete graph_nodes artifact:" + seeded.documentRef; !strings.Contains(strings.Join(listing.Listing, "\n"), want) {
		t.Errorf("the listing reads %q, want it to name %q", listing.Listing, want)
	}
	erased, err := Erase(ctx, url, eraseServerURL, graphstore.KindArtifact, seeded.documentRef, listing.Digest, 0)
	if err != nil || !erased.Applied {
		t.Fatalf("the confirmed erase answered %+v, %v; want applied", erased, err)
	}
	if held := readTypeNode(t, database, graphstore.KindArtifact, seeded.documentRef); held.row || held.out != 0 || held.in != 0 || held.erased != 1 {
		t.Errorf("after the erase the store holds %+v of the document's node, want no row, no tuple and one erasure", held)
	}
	requireOrphaned(t, database, 0)
	// The issue's node, whose row stands, is untouched.
	if held := readTypeNode(t, database, graphstore.KindIssue, seeded.issue); !held.row || held.erased != 0 {
		t.Errorf("after the document's erase the store holds %+v of the issue's node, want it as seeded", held)
	}
}

// A create of this build writes a document's node before its row, in one transaction, so it holds
// the node's row from its node write until it commits the row. The erase reads whether the row
// stands under its own lock of the node (lockOwnersThenNode, then read), so an erase that queues
// behind such a create reads the row the create committed, and refuses. Here a build before this
// one has removed a document, leaving its node live behind no row, and a create of this build
// takes the address again: it has written the node and the row and not committed when the
// confirmed erase starts, the erase queues behind it on the node, and the create commits. The
// erase answers the refusal, and the node, its tuples and the row all stand, with no erasure
// recorded and nothing for the project census to name.
//
// Its control is the row's check moved out of read to just before the node's `for update` in
// lockOwnersThenNode: the erase then reads no row, queues, and erases the node the created row
// stands behind, which the census reads as a row with no node.
func TestTheErasesRowCheckUnderTheNodeLockRefusesTheCreateItWaitedFor(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seeded := seedObjectNodes(t, database)
	if _, err := database.Pool.Exec(ctx, `delete from artifacts where ref_key = $1`, seeded.documentRef); err != nil {
		t.Fatalf("remove the document's row as a build before this one does: %v", err)
	}
	before := readTypeNode(t, database, graphstore.KindArtifact, seeded.documentRef)
	create := &lockstep.Hold{Name: "a create of " + seeded.documentRef + " that wrote its node and its row"}
	lockstep.Begin(t, database, create)
	if _, err := graphstore.WriteUncaused(ctx, create.Tx, []graphstore.Node{graphstore.ArtifactNode(
		seeded.documentRef, "ERASE", "", model.Actor{Kind: "user", ID: "alice"}, time.Now().UTC())}, nil); err != nil {
		t.Fatalf("write the created document's node: %v", err)
	}
	if _, err := create.Tx.Exec(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('ERASE-1', 'ERASE', 'notes-md', 'Notes made again', 'doc', '{"kind":"user","id":"alice"}')
	`); err != nil {
		t.Fatalf("write the created document's row: %v", err)
	}
	pid, erased := startErase(t, database, graphstore.KindArtifact, seeded.documentRef)
	if early, done := storetest.WaitForQueuedBehind(t, create.Tx, pid, erased, lockstep.Wait); done {
		t.Fatalf("the erase answered (%+v, %v) without waiting on the node the create holds, so this test proves nothing", early.erasure, early.err)
	}
	t.Logf("the erase waits behind %s at %q", create.Name, lockstep.WaitingStatement(t, database, pid))
	if err := create.Tx.Commit(ctx); err != nil {
		t.Fatalf("commit the create: %v", err)
	}
	create.Release()
	var answer eraseAnswer
	select {
	case answer = <-erased:
	case <-time.After(lockstep.Wait):
		t.Fatalf("the erase did not answer within %s of the create's commit", lockstep.Wait)
	}
	if answer.err == nil || !strings.Contains(answer.err.Error(), "refuses artifact:"+seeded.documentRef+": its row stands") {
		t.Errorf("the erase queued behind the create answered %+v, %v; want the refusal while the row stands",
			answer.erasure, answer.err)
	}
	if after := readTypeNode(t, database, graphstore.KindArtifact, seeded.documentRef); after != before {
		t.Errorf("after the create and the erase the store holds %+v of the document's node, want %+v as the create left it", after, before)
	}
	if name := rowText(t, database, "artifacts", "name", "ref_key", seeded.documentRef); name != "Notes made again" {
		t.Errorf("the created document's row reads %q, want the create's name", name)
	}
	incomplete, err := graphcensus.ProjectComplete(ctx, database.Pool)
	if err != nil {
		t.Fatalf("read the incomplete rows: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("after the create and the erase the project census names %q, want nothing", incomplete)
	}
}

// requireOrphaned fails unless the project census reads want orphaned nodes, all of them
// documents, and no other finding.
func requireOrphaned(t *testing.T, database *store.Store, want int64) {
	t.Helper()
	census, err := graphcensus.ProjectCensus(context.Background(), database.Pool)
	if err != nil {
		t.Fatalf("take the project census: %v", err)
	}
	for _, kind := range census {
		orphaned := int64(0)
		if kind.Kind == graphstore.KindArtifact {
			orphaned = want
		}
		if kind.Orphaned != orphaned || kind.Findings() != orphaned {
			t.Errorf("the project census reads %+v for %s, want orphaned=%d and no other finding", kind, kind.Kind, orphaned)
		}
	}
}

// objectNodes is what seedObjectNodes wrote: an issue, its document, an ask on the issue and a
// message on it, each a row and its node in the project.
type objectNodes struct {
	issue, documentRef, ask, message string
}

// seedObjectNodes writes project ERASE, issue ERASE-1 with a document, an ask and a message, each
// row with its node as its create writes it (graphstore.IssueNode and its siblings) and the base
// attributes that node write states; the message's row names its node, as this build's create
// writes it.
func seedObjectNodes(t *testing.T, database *store.Store) objectNodes {
	t.Helper()
	ctx := context.Background()
	seeded := objectNodes{
		issue: "ERASE-1", documentRef: "ERASE-1/notes-md",
		ask: "22222222-2222-4222-8222-000000000001", message: "22222222-2222-4222-8222-000000000002",
	}
	now := time.Now().UTC()
	alice := model.Actor{Kind: "user", ID: "alice"}
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the seed: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := graphstore.Write(ctx, tx, []graphstore.Node{
		graphstore.IssueNode(seeded.issue, "ERASE", alice, now),
		graphstore.ArtifactNode(seeded.documentRef, "ERASE", "", alice, now),
		graphstore.AskNode(seeded.ask, "ERASE", nil, alice, now),
		graphstore.MessageNode(seeded.message, "ERASE", nil, alice, now),
	}, nil); err != nil {
		t.Fatalf("write the nodes: %v", err)
	}
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('ERASE', 'Erase', 'alice')`,
		`insert into issues (key, project_key, number, title, created_by, rank)
		 values ('ERASE-1', 'ERASE', 1, 'A title somebody wrote', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		 values ('ERASE-1', 'ERASE', 'notes-md', 'Notes somebody named', 'doc', '{"kind":"user","id":"alice"}')`,
		`insert into asks (id, issue_key, author, question, options, multiple, urgency, kind)
		 values ('` + seeded.ask + `', 'ERASE-1', '{"kind":"user","id":"alice"}', 'A question somebody asked', '[]', false, 'med', 'question')`,
		`insert into messages (id, issue_key, author, body, node_kind, node_address)
		 values ('` + seeded.message + `', 'ERASE-1', '{"kind":"user","id":"alice"}', 'A message somebody wrote', 'message', '` + seeded.message + `')`,
	} {
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatalf("seed the rows: %v\n%s", err, statement)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the seed: %v", err)
	}
	return seeded
}

// rowText reads column of the table's row whose key reads id.
func rowText(t *testing.T, database *store.Store, table, column, key, id string) string {
	t.Helper()
	var text string
	if err := database.Pool.QueryRow(context.Background(),
		`select `+column+` from `+table+` where `+key+` = $1`, id).Scan(&text); err != nil {
		t.Fatalf("read %s.%s of %s: %v", table, column, id, err)
	}
	return text
}
