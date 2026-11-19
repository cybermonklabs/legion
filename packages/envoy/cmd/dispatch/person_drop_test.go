//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go. This file's test reads the
// base release the Verification document's preamble builds once per head (baseRelease), and is
// run as
//
//	BASELINE_SHA=<commit> go test -tags basebuild -timeout 30m ./cmd/dispatch -run '^TestADropByAPersonScopedBrokerTokenUnderTheBaseStays$' -count=1 -v

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/brokertoken"
	"github.com/sjawhar/envoy/internal/brokertoken/brokertokentest"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/testbaseline"
)

// personDropOrigin is the server URL the base release is given (DISPATCH_SERVER_URL), which its
// broker-token check reads as the origin each DPoP proof's htu names: the caller signs every
// proof for it, whatever loopback port the process serves on.
const personDropOrigin = "https://dispatch-under-test.example"

// personDropSession is the session the secrets broker says the person-scoped token acts for.
const personDropSession = "ses_person_drop_agent"

// A drop of an answered ask's block by a broker-issued token the secrets broker scoped to a person
// stays, read under the base release (R6). The base release trusts a stub broker of the test's
// own (brokertokentest), and an agent of the test's person, alice, drops the answered ask q-scoped
// from its spec with a token the broker scoped to her, presenting a fresh DPoP proof: the base
// release refuses no such drop, and records the version's author as the session the token proved,
// owned by alice, with an act whose scopes hold `person`. Beside it, an agent's session drops the
// answered ask q-agent through the shared token, a version recorded as a session with no act.
//
// This head's step (`envoy-dispatch restore-answered-asks`) then restores q-agent and declines
// q-scoped: it writes no block of it, reports it declined to alice at that version, and appends one
// asserted removed_by from its node to alice under the event of that version. A second run touches
// neither ask.
func TestADropByAPersonScopedBrokerTokenUnderTheBaseStays(t *testing.T) {
	ctx := context.Background()
	databaseURL := censusDatabase(t)
	base := baseRelease(t)
	baseline := testbaseline.Commit(t)
	broker := brokertokentest.NewStubBroker(t)
	caller := brokertokentest.NewCaller(t)
	accountToken := filepath.Join(t.TempDir(), "account-token")
	if err := os.WriteFile(accountToken, []byte("the dispatch account's projected token\n"), 0o600); err != nil {
		t.Fatalf("write the account token: %v", err)
	}
	database, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the run's database: %v", err)
	}
	defer database.Close(ctx)

	previous := startDispatchProcessWith(t, databaseURL, dispatchOptions{
		Binary: base, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay,
		Env: map[string]string{
			"DISPATCH_AGENT_SECRETS_URL":                broker.Server.URL,
			"DISPATCH_AGENT_SECRETS_ACCOUNT_TOKEN_FILE": accountToken,
			"DISPATCH_SERVER_URL":                       personDropOrigin,
		},
	})
	previous.waitHealthy(t)
	if announced := announcedBaseCommit(t, ctx, database); announced != baseline {
		t.Fatalf("the base release announced the build %s, want BASELINE_SHA's %s", announced, baseline)
	}
	previous.call(t, http.MethodPost, "/api/v1/projects", `{"key":"PSD","name":"A person's drop"}`, http.StatusCreated)
	issueA, specA := createCensusIssue(t, previous, "PSD", "Dropped by a person's agent", "# Dropped by a person's agent\n\n"+
		restoreAsk("q-scoped", "Which store holds the corpus?"))
	issueB, specB := createCensusIssue(t, previous, "PSD", "Dropped by a session", "# Dropped by a session\n\n"+
		restoreAsk("q-agent", "Which reader reads the corpus first?"))
	scopedAsk, agentAsk := blockAskOf(t, previous, issueA, "q-scoped"), blockAskOf(t, previous, issueB, "q-agent")
	answerAsk(t, previous, scopedAsk, "First", "The database we run.")
	answerAsk(t, previous, agentAsk, "Second", "The view reads it first.")
	settledDocument(t, ctx, previous, database, specA)
	settledDocument(t, ctx, previous, database, specB)

	// The person-scoped token's drop, with a proof of the caller's key signed for the origin the
	// base release checks.
	const token = "a-broker-token-scoped-to-alice"
	broker.Says(token, brokertoken.Introspection{
		Active: true, Session: personDropSession, Person: dispatchTestLogin, ProjectScope: brokertoken.ProjectScopePerson,
		Scope: "dispatch.write", ExpiresAt: time.Now().Add(time.Hour).Unix(), ClientID: brokertoken.ExchangeClientID,
		Thumbprint: caller.JKT(),
	})
	path := "/api/v1/artifacts/" + specA + "/edits"
	signed := caller.Signed(t, http.MethodPost, personDropOrigin+path, token)
	if dropped := previous.send(t, http.MethodPost, path, `{"ops":[{"op":"delete","block":"q-scoped"}]}`, http.Header{
		"Authorization": {signed.Header.Get("Authorization")}, "Dpop": {signed.Header.Get("DPoP")},
	}); dropped.status != http.StatusOK {
		t.Fatalf("the person-scoped token's drop of q-scoped under the base release: %s, want 200", dropped)
	}
	var dropVersion int
	var recorded []byte
	if err := database.QueryRow(ctx, `select number, authors from artifact_versions where artifact_id = $1::uuid order by number desc limit 1`,
		specA).Scan(&dropVersion, &recorded); err != nil {
		t.Fatalf("read the version of the drop: %v", err)
	}
	var authors []model.Actor
	if err := json.Unmarshal(recorded, &authors); err != nil {
		t.Fatalf("decode the authors of version %d: %v", dropVersion, err)
	}
	if len(authors) != 1 || authors[0].Kind != "session" || authors[0].ID != personDropSession || authors[0].Owner == nil ||
		*authors[0].Owner != dispatchTestLogin || authors[0].Act == nil || !slices.Contains(authors[0].Act.Scopes, model.ScopePerson) {
		t.Fatalf("the base release recorded version %d of spec A as written by %s, want the session %s owned by %s with an act holding %s",
			dropVersion, recorded, personDropSession, dispatchTestLogin, model.ScopePerson)
	}
	var dropEvent int64
	if err := database.QueryRow(ctx, `
		select id from events where type = 'artifact.version' and payload->>'artifact_id' = $1 and payload->'version'->>'number' = $2
	`, specA, strconv.Itoa(dropVersion)).Scan(&dropEvent); err != nil {
		t.Fatalf("read the event of version %d of spec A: %v", dropVersion, err)
	}
	// The drop by a session with no act, through the shared token.
	agentEdits(t, previous, specB, map[string]string{"op": "delete", "block": "q-agent"})
	settledDocument(t, ctx, previous, database, specA)
	settledDocument(t, ctx, previous, database, specB)
	stopProcess(t, previous)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(500 * time.Millisecond) {
		var live int
		if err := database.QueryRow(ctx, `select count(*) from server_processes where heartbeat_at > now() - interval '15 seconds'`).Scan(&live); err != nil {
			t.Fatalf("read the live processes: %v", err)
		}
		if live == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d processes still read live after the base release stopped", live)
		}
	}

	// This head's step, through its command.
	out := runPersonDropRestore(t, ctx, databaseURL)
	declined := scopedAsk + "@" + dispatchTestLogin + "@v" + strconv.Itoa(dropVersion)
	if line := restoreLine(t, out, issueA); !strings.Contains(line, " restored=0 ") || !strings.Contains(line, " declined=1 declined_asks="+declined+" ") {
		t.Fatalf("the step printed %q for %s, want q-scoped declined as %s and nothing restored", line, issueA, declined)
	}
	if line := restoreLine(t, out, issueB); !strings.Contains(line, " restored=1 ") || !strings.Contains(line, " declined=0 ") {
		t.Fatalf("the step printed %q for %s, want q-agent restored and nothing declined", line, issueB)
	}
	type removal struct {
		person   string
		asserted bool
		event    int64
	}
	removalsOf := func(ask string) []removal {
		t.Helper()
		rows, err := database.Query(ctx, `
			select to_id, asserted, source_event from graph_tuples
			where from_kind = $1 and from_id = $2 and kind = $3 and to_kind = $4 order by seq
		`, graphstore.KindAsk, ask, graphstore.TupleRemovedBy, graphstore.KindPerson)
		if err != nil {
			t.Fatalf("read the removals of ask %s: %v", ask, err)
		}
		held, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (removal, error) {
			var one removal
			err := row.Scan(&one.person, &one.asserted, &one.event)
			return one, err
		})
		if err != nil {
			t.Fatalf("read the removals of ask %s: %v", ask, err)
		}
		return held
	}
	if held := removalsOf(scopedAsk); len(held) != 1 || held[0] != (removal{dispatchTestLogin, true, dropEvent}) {
		t.Fatalf("q-scoped's node holds the removals %+v, want one asserted to %s under the event %d of version %d",
			held, dispatchTestLogin, dropEvent, dropVersion)
	}
	if held := removalsOf(agentAsk); len(held) != 0 {
		t.Fatalf("q-agent's node holds the removals %+v, want none: a session with no act dropped it", held)
	}
	latest := func(document string) string {
		t.Helper()
		var markdown string
		if err := database.QueryRow(ctx, `select markdown from artifact_versions where artifact_id = $1::uuid order by number desc limit 1`,
			document).Scan(&markdown); err != nil {
			t.Fatalf("read the newest version of %s: %v", document, err)
		}
		return markdown
	}
	if spec := latest(specA); strings.Contains(spec, "#q-scoped ") {
		t.Fatalf("spec A holds q-scoped again, which a person's agent removed:\n%s", spec)
	}
	if spec := latest(specB); !strings.Contains(spec, "#q-agent ") {
		t.Fatalf("spec B holds no block of q-agent, which the restore returns:\n%s", spec)
	}

	// A second run touches neither ask.
	again := runPersonDropRestore(t, ctx, databaseURL)
	for _, line := range strings.Split(again, "\n") {
		if strings.Contains(line, " issue="+issueA+" ") && !strings.Contains(line, " restored=0 declined=0 ") {
			t.Fatalf("the second run printed %q for %s, want nothing restored and nothing declined", line, issueA)
		}
	}
	if held := removalsOf(scopedAsk); len(held) != 1 {
		t.Fatalf("after the second run q-scoped's node holds the removals %+v, want the one the first run appended", held)
	}
}

// runPersonDropRestore runs `envoy-dispatch restore-answered-asks --project PSD` in this process,
// over databaseURL, fails unless it exits 0, and answers what it printed.
func runPersonDropRestore(t *testing.T, ctx context.Context, databaseURL string) string {
	t.Helper()
	var out, errOut bytes.Buffer
	env := readSettings(func(name string) (string, bool) {
		if name == "DATABASE_URL" {
			return databaseURL, true
		}
		return "", false
	})
	code := runSubcommand(ctx, []string{"restore-answered-asks", "--project", "PSD"}, env, &out, &errOut)
	t.Logf("restore-answered-asks --project PSD exited %d and printed:\n%s%s", code, out.String(), errOut.String())
	if code != 0 {
		t.Fatalf("restore-answered-asks --project PSD exited %d:\n%s%s", code, out.String(), errOut.String())
	}
	return out.String() + errOut.String()
}
