//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go. This file's test reads the
// base release the Verification document's preamble builds once per head (baseRelease), and is
// run as
//
//	go test -tags basebuild -timeout 30m ./cmd/dispatch -run '^TestAnAnswerThePreviousBuildWroteReachesTheNode$' -count=1 -v

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
)

// baseRelease is the envoy-dispatch binary of the base release, which the Verification
// document's preamble builds once for each head from BASELINE_SHA, stamped with that commit, at
// $TMPDIR/base-dispatch. It builds nothing: a test of this tag compares this head with that one
// build, and a binary this helper built would be another. It fails the test where the binary is
// not there, never skips it, since a skipped comparison with the previous release reads as one
// that held.
func baseRelease(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("TMPDIR")
	if dir == "" {
		t.Fatal("TMPDIR is unset, so the base release at $TMPDIR/base-dispatch cannot be found; build it as the Verification document's preamble does")
	}
	binary := filepath.Join(dir, "base-dispatch")
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("the base release %s cannot be read: %v; build it from BASELINE_SHA as the Verification document's preamble does", binary, err)
	}
	if info.IsDir() || info.Mode()&0o111 == 0 {
		t.Fatalf("the base release %s is no executable file", binary)
	}
	return binary
}

// previousBuildSigningKey is the one DISPATCH_SIGNING_KEY both builds carry: a process whose key
// a live peer does not share refuses to start (processes.Register).
const previousBuildSigningKey = "previous-build-answer-signing-key"

// headBuildStamp is the commit this head is stamped with for the test, which no base release
// carries, so the two processes serve two builds as a roll has them (processes.Process.Commit).
const headBuildStamp = "1150115011501150115011501150115011501150"

// An answer a process of the release before writes reaches the ask node (dispatch://INFRA-1150,
// the catch-up of ruling E1). The base release serves a database of the run's own alone, as the
// fleet stands before a roll, until it has announced its build; this head, stamped with a commit
// of its own, then boots beside it and creates an ask, which its writer marks and states on the
// ask's node. The base release answers that ask through its own answer route, which writes the
// asks row and an ask.answered event and states nothing on the node. While both builds serve, the
// head's decisions step catches up every event of the window no tuple states on each tick, so
// within a tick or two the ask's node holds that answer - its answer_state, its words, its
// selection, its time and its person - each under the base release's ask.answered, and the row
// reads the same answer.
func TestAnAnswerThePreviousBuildWroteReachesTheNode(t *testing.T) {
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
	defer database.Close(ctx)
	var baseCommit string
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := database.QueryRow(ctx, `select coalesce((select commit from server_build_announcements order by seq desc limit 1), '')`).Scan(&baseCommit); err != nil {
			t.Fatalf("read the base release's announcement: %v", err)
		}
		if baseCommit != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if baseCommit == "" || baseCommit == headBuildStamp {
		t.Fatalf("the base release announced the build %q, want its own stamp, which is not this head's %q", baseCommit, headBuildStamp)
	}
	t.Logf("the base release serves the build %s alone", baseCommit)

	current := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: head, SigningKey: previousBuildSigningKey})
	current.waitHealthy(t)

	current.call(t, http.MethodPost, "/api/v1/projects", `{"key":"ROLL","name":"A roll"}`, http.StatusCreated)
	issue, _ := createCensusIssue(t, current, "ROLL", "An ask this head opens", "# Spec\n\nThe spec.\n")
	opened := current.call(t, http.MethodPost, "/api/v1/issues/"+issue+"/asks",
		`{"question":"Ship the roll?","options":[{"label":"Ship"},{"label":"Hold"}]}`, http.StatusCreated)
	var ask struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(opened, &ask); err != nil || ask.ID == "" {
		t.Fatalf("read the ask this head opened: %v: %s", err, opened)
	}
	var marked bool
	if err := database.QueryRow(ctx, `select node_kind is not null from asks where id = $1::uuid`, ask.ID).Scan(&marked); err != nil || !marked {
		t.Fatalf("the ask this head opened names its node %t (%v), want its writer to have marked it", marked, err)
	}

	answer := previous.send(t, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer",
		`{"selected":["Ship"],"text":"Ship it once the roll is done.","expected_edited_at":null}`, asUser(dispatchTestLogin))
	if answer.status != http.StatusOK {
		t.Fatalf("the base release answers the ask: %s", answer)
	}
	var event int64
	if err := database.QueryRow(ctx, `
		select coalesce(max(id), 0) from events where type = 'ask.answered' and payload->>'id' = $1
	`, ask.ID).Scan(&event); err != nil || event == 0 {
		t.Fatalf("read the base release's ask.answered: %d, %v", event, err)
	}
	var stateRow string
	var answerRow []byte
	if err := database.QueryRow(ctx, `select state, answer from asks where id = $1::uuid`, ask.ID).Scan(&stateRow, &answerRow); err != nil {
		t.Fatalf("read the row the base release answered: %v", err)
	}
	t.Logf("the base release answered ask %s under event %d: state %s, answer %s", ask.ID, event, stateRow, answerRow)
	var stated struct {
		User     string    `json:"user"`
		Selected []string  `json:"selected"`
		Text     *string   `json:"text"`
		At       time.Time `json:"at"`
	}
	if err := json.Unmarshal(answerRow, &stated); err != nil {
		t.Fatalf("decode the answer the base release wrote: %v", err)
	}

	// The head's catch-up runs on each tick while the base release lives, every
	// docs.BlockIndexReconcileInterval (30 s); three of them is room for the one that follows the
	// answer.
	held := map[string]graphstore.Tuple{}
	deadline = time.Now().Add(100 * time.Second)
	for time.Now().Before(deadline) {
		held = map[string]graphstore.Tuple{}
		rows, err := database.Query(ctx, `
			select kind, to_kind, to_id, source_event from (
				select distinct on (kind, case when to_kind = 'value' then '' else to_id end)
				       kind, to_kind, to_id, source_event, asserted
				from graph_tuples where from_kind = 'ask' and from_id = $1 and kind = any($2)
				order by kind, case when to_kind = 'value' then '' else to_id end, seq desc) newest
			where asserted
		`, ask.ID, asks.AnswerKinds())
		if err != nil {
			t.Fatalf("read the ask's node: %v", err)
		}
		for rows.Next() {
			var tuple graphstore.Tuple
			if err := rows.Scan(&tuple.Kind, &tuple.ToKind, &tuple.ToID, &tuple.SourceEvent); err != nil {
				t.Fatalf("read a tuple of the ask's node: %v", err)
			}
			held[tuple.Kind] = tuple
		}
		rows.Close()
		if held[graphstore.TupleAnswerState].SourceEvent == event {
			break
		}
		time.Sleep(time.Second)
	}
	t.Logf("the ask's node holds %v", held)
	want := map[string]string{
		graphstore.TupleAnswerState: asks.StateAnswered,
		graphstore.TupleAnswer:      "Ship it once the roll is done.",
		graphstore.TupleSelected:    asks.SelectedValue(stated.Selected),
		graphstore.TupleAnsweredAt:  asks.AnsweredAtValue(stated.At),
		graphstore.TupleAnsweredBy:  strings.ToLower(stated.User),
		graphstore.TupleDecidedBy:   strings.ToLower(stated.User),
	}
	for kind, value := range want {
		got, ok := held[kind]
		if !ok || got.ToID != value || got.SourceEvent != event {
			t.Errorf("the ask's node holds %s %q under event %d (held %t), want %q under the base release's ask.answered %d",
				kind, got.ToID, got.SourceEvent, ok, value, event)
		}
	}
	if stateRow != asks.StateAnswered || stated.Text == nil || *stated.Text != want[graphstore.TupleAnswer] {
		t.Errorf("the row reads state %q and answer %s, want the answer the node holds", stateRow, answerRow)
	}
}
