//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go. This file's test reads the
// base release at $TMPDIR/base-dispatch (baseRelease), built from BASELINE_SHA, and is run as
//
//	go test -tags basebuild -timeout 30m ./cmd/dispatch -run '^TestTheBaseReleasesAgentDismissalsKeepTheRootsColumn$' -count=1 -v

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// The Agent thread's root carries its author's dismissal (messages.agent_dismissed_at, migration
// 20261125052902), which this head's Inbox reads its live roots by, and which the trigger of
// 20261125053125 keeps from user_agent_thread_dismissed whichever build writes a mark. The release
// before this head knows nothing of the column, and serves beside it while it rolls.
//
// The base release serves a database of the run's own alone, as the fleet stands before a roll,
// and dismisses one of two Agent threads of alice's through its own route, before this head has
// migrated anything: a mark no trigger mirrored. This head then boots beside it and migrates, and
// its catch-up gives that root the mark's moment. Then, while both serve, the base release
// dismisses the second thread and restores the first through its own route, and a session's reply
// is written into the second as that release writes one - a row that names no node, which the
// clear trigger of 20261105093507 takes the root's marks off. After each write the root's column
// holds what alice's mark holds, and this head's Inbox lists the threads the marks leave in it.
func TestTheBaseReleasesAgentDismissalsKeepTheRootsColumn(t *testing.T) {
	ctx := context.Background()
	databaseURL := censusDatabase(t)
	base := baseRelease(t)
	head := cmdtest.BuildStamped(t, "envoy-dispatch", headBuildStamp)

	previous := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: base, SigningKey: previousBuildSigningKey})
	previous.waitHealthy(t)
	database, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the run's database: %v", err)
	}
	t.Cleanup(func() { database.Close(ctx) })

	// Two one-off messages of alice's to a session, each with the session's reply, written as the
	// base release writes them.
	thread := func(body string) (root, reply string) {
		t.Helper()
		if err := database.QueryRow(ctx, `
			insert into messages (author, body, target, created_at)
			values ('{"kind":"user","id":"alice"}', $1, 'session:s1', now() - interval '10 minutes')
			returning id::text
		`, body).Scan(&root); err != nil {
			t.Fatalf("write the root %q: %v", body, err)
		}
		return root, sessionReplyAsTheBaseWritesIt(t, ctx, database, root)
	}
	first, firstReply := thread("Can you look at the deploy?")
	second, secondReply := thread("And the runbook?")

	dismissAsTheBase(t, previous, first, firstReply)

	current := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: head, SigningKey: previousBuildSigningKey})
	current.waitHealthy(t)
	requireRootColumnIsAlicesMark(t, ctx, database, first, "the catch-up of the mark the base release wrote before this head migrated")
	requireRootColumnIsAlicesMark(t, ctx, database, second, "a root no mark names")
	requireInbox(t, current, []string{second}, []string{first})

	dismissAsTheBase(t, previous, second, secondReply)
	requireRootColumnIsAlicesMark(t, ctx, database, second, "the base release's dismissal while both serve")
	requireInbox(t, current, nil, []string{first, second})

	restored := previous.send(t, http.MethodPost, "/api/v1/me/inbox/dismiss", encodeJSON(t, map[string]any{
		"threads": []string{"agent:" + first}, "restore": true,
	}), asUser("alice"))
	if restored.status != http.StatusOK || !dismissedOne(t, restored) {
		t.Fatalf("the base release's restore of %s: %s, want one thread restored", first, restored)
	}
	requireRootColumnIsAlicesMark(t, ctx, database, first, "the base release's restore while both serve")
	requireInbox(t, current, []string{first}, []string{second})

	sessionReplyAsTheBaseWritesIt(t, ctx, database, secondReply)
	requireRootColumnIsAlicesMark(t, ctx, database, second, "a session's reply the base release writes, two deep")
	requireInbox(t, current, []string{first, second}, nil)
}

// sessionReplyAsTheBaseWritesIt writes s1's reply to parent as the release before writes one: a
// row that names no node and holds no threads_recorded, which the clear trigger of 20261105093507
// takes its root's marks off. It answers the reply's id.
func sessionReplyAsTheBaseWritesIt(t *testing.T, ctx context.Context, database *pgx.Conn, parent string) string {
	t.Helper()
	var reply string
	if err := database.QueryRow(ctx, `
		insert into messages (author, body, in_reply_to, target)
		select '{"kind":"session","id":"s1"}', 'Looking.', parent.id, parent.target
		from messages parent where parent.id = $1
		returning id::text
	`, parent).Scan(&reply); err != nil {
		t.Fatalf("write s1's reply to %s: %v", parent, err)
	}
	return reply
}

// dismissAsTheBase dismisses alice's Agent thread under root through server's own route, as the
// base release's Inbox sends it, with reply as the thread's newest reply and the one it drew
// unread, and fails unless the answer says it changed that one thread.
func dismissAsTheBase(t *testing.T, server *dispatchProcess, root, reply string) {
	t.Helper()
	answer := server.send(t, http.MethodPost, "/api/v1/me/inbox/dismiss", encodeJSON(t, map[string]any{
		"threads": []string{"agent:" + root},
		"seen_replies": map[string]any{"agent:" + root: map[string]any{
			"newest_reply": reply, "unread_replies": []string{reply},
		}},
	}), asUser("alice"))
	if answer.status != http.StatusOK || !dismissedOne(t, answer) {
		t.Fatalf("the base release's dismissal of %s: %s, want one thread dismissed", root, answer)
	}
}

// dismissedOne is whether a dismissal's answer says it changed one thread.
func dismissedOne(t *testing.T, answer httpAnswer) bool {
	t.Helper()
	var changed struct {
		Dismissed int `json:"dismissed"`
	}
	if err := json.Unmarshal(answer.body, &changed); err != nil {
		t.Fatalf("read the dismissal's answer %s: %v", answer.body, err)
	}
	return changed.Dismissed == 1
}

// requireRootColumnIsAlicesMark fails unless the root's agent_dismissed_at holds alice's mark of
// it: that mark's moment, or nothing where she holds none.
func requireRootColumnIsAlicesMark(t *testing.T, ctx context.Context, database *pgx.Conn, root, what string) {
	t.Helper()
	var column, mark *time.Time
	if err := database.QueryRow(ctx, `
		select m.agent_dismissed_at,
		       (select d.dismissed_at from user_agent_thread_dismissed d where d.login = 'alice' and d.message_id = m.id)
		from messages m where m.id = $1
	`, root).Scan(&column, &mark); err != nil {
		t.Fatalf("%s: read the root %s and alice's mark of it: %v", what, root, err)
	}
	if (column == nil) != (mark == nil) || (column != nil && !column.Equal(*mark)) {
		t.Fatalf("%s: the root %s holds agent_dismissed_at %v, and alice's mark is %v", what, root, column, mark)
	}
	t.Logf("%s: the root %s holds agent_dismissed_at %v, alice's mark %v", what, root, column, mark)
}

// requireInbox fails unless this head's Inbox (GET /api/v1/me/inbox?state=inbox) lists for alice
// every root of in and none of out.
func requireInbox(t *testing.T, current *dispatchProcess, in, out []string) {
	t.Helper()
	answer := current.send(t, http.MethodGet, "/api/v1/me/inbox?state=inbox", "", asUser("alice"))
	if answer.status != http.StatusOK {
		t.Fatalf("this head's Inbox: %s", answer)
	}
	var read struct {
		Entries []struct {
			Node struct {
				ID string `json:"id"`
			} `json:"node"`
			Agent json.RawMessage `json:"agent"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(answer.body, &read); err != nil {
		t.Fatalf("read this head's Inbox: %v", err)
	}
	listed := []string{}
	for _, entry := range read.Entries {
		if entry.Agent != nil {
			listed = append(listed, entry.Node.ID)
		}
	}
	for _, root := range in {
		if !slices.Contains(listed, root) {
			t.Errorf("this head's Inbox lists the Agent rows %v, want %s among them", listed, root)
		}
	}
	for _, root := range out {
		if slices.Contains(listed, root) {
			t.Errorf("this head's Inbox lists the Agent rows %v, want %s dismissed out of it", listed, root)
		}
	}
}
