package grapherase

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The sessions of the ask seedSessionAsk writes: the session that asks it, which follows its own
// ask as every asking session does (asks.FollowAuthor, which appends no follower event), and the
// session that follows it through the follow route, which appends an ask.follower_added.
const (
	sessionAsker    = "erase-asking-session"
	sessionFollower = "erase-following-session"
)

// An ask a session asked is the only row that names that session: the asks row's author and the
// follower row its own ask gave it. The erase of the ask deletes both rows with the node, and no
// erase removes a session (Erase refuses one), so the session's node stands and only the event log
// still names it: the ask.opened that recorded the ask, which the erase redacts nothing of. The
// census reads the asking session off that event as it reads a follower off its ask.follower_added,
// and reads zero differences after the erase as before it, both sessions still counted.
func TestTheCensusHoldsTheSessionsOfAnErasedAskToTheirNodes(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	document := seedPreStoreDocument(t, database)
	author := model.Actor{Kind: "session", ID: sessionAsker}
	authorJSON, err := json.Marshal(author)
	if err != nil {
		t.Fatalf("encode the asking session: %v", err)
	}
	var ask model.Ask
	issue := "CORE-1"
	ask.IssueKey, ask.Author, ask.Kind, ask.Question, ask.State = &issue, author, "question", "Ship it this week?", "open"
	if err := database.Pool.QueryRow(ctx, `
		insert into asks (issue_key, author, question, anchor)
		values ($1, $2::jsonb, $3, jsonb_build_object('artifact_id', $4::text))
		returning id::text, created_at
	`, issue, authorJSON, ask.Question, document.artifactID).Scan(&ask.ID, &ask.CreatedAt); err != nil {
		t.Fatalf("write the session's ask: %v", err)
	}
	ask.Anchor = &model.Anchor{ArtifactID: document.artifactID}
	for _, session := range []string{sessionAsker, sessionFollower} {
		if _, err := database.Pool.Exec(ctx, `insert into ask_followers (ask_id, session_id) values ($1::uuid, $2)`, ask.ID, session); err != nil {
			t.Fatalf("write the follower row of %s: %v", session, err)
		}
	}
	payload, err := events.AskEventPayload(ctx, database.Pool, ask, model.ReferenceChanges{})
	if err != nil {
		t.Fatalf("build the ask.opened payload: %v", err)
	}
	opened, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode the ask.opened payload: %v", err)
	}
	appendEvent(t, database, issue, "ask.opened", string(opened))
	appendEvent(t, database, issue, "ask.follower_added",
		`{"ask_id":"`+ask.ID+`","session_id":"`+sessionFollower+`","by":{"kind":"session","id":"`+sessionFollower+`"}}`)
	if _, err := graphcensus.Backfill(ctx, database, io.Discard); err != nil {
		t.Fatalf("backfill the store: %v", err)
	}
	var marked int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from asks where id = $1::uuid and node_kind is not null)
		     + (select count(*) from ask_followers where ask_id = $1::uuid and node_kind is not null)
	`, ask.ID).Scan(&marked); err != nil {
		t.Fatalf("read the ask's marks: %v", err)
	}
	if marked != 3 {
		t.Fatalf("the backfill marked %d of the ask's three rows, want all three, so this test would prove nothing", marked)
	}
	requireSessionsCounted(t, database, "before the erase")

	requireErased(t, database, graphstore.KindAsk, ask.ID)
	var rows int64
	if err := database.Pool.QueryRow(ctx, `
		select (select count(*) from asks where id = $1::uuid) + (select count(*) from ask_followers where ask_id = $1::uuid)
		     + (select count(*) from graph_nodes where kind = 'session' and id in ($2, $3) and state = 'live')
	`, ask.ID, sessionAsker, sessionFollower).Scan(&rows); err != nil {
		t.Fatalf("read what the erase left: %v", err)
	}
	if rows != 2 {
		t.Fatalf("after the erase %d rows of the ask and live session nodes stand, want the ask's rows gone and both sessions' nodes standing", rows)
	}
	requireSessionsCounted(t, database, "after the erase")
}

// requireSessionsCounted fails unless the census reads zero differences and counts both of the
// erased ask's sessions on its session kind's row side.
func requireSessionsCounted(t *testing.T, database *store.Store, when string) {
	t.Helper()
	requireCensusZero(t, database, when)
	report, err := graphcensus.StoreCensus(context.Background(), database.Pool.Config().ConnString(), false)
	if err != nil {
		t.Fatalf("the store census %s: %v", when, err)
	}
	for _, kind := range report.Kinds {
		if kind.Kind == graphstore.KindSession {
			t.Logf("%s: the census's session kind reads %d rows and %d live nodes", when, kind.Rows, kind.Nodes)
			if kind.Rows != 2 || kind.Nodes != 2 {
				t.Fatalf("%s: the census's session kind reads %d rows and %d live nodes, want the asking session and the follower", when, kind.Rows, kind.Nodes)
			}
			return
		}
	}
	t.Fatalf("%s: the census read no session kind", when)
}
