//go:build basebuild

// The basebuild tag, and how a test of it is run: store_census_test.go. This file's test reads the
// base release the Verification document's preamble builds once per head (baseRelease), and is
// run as
//
//	go test -tags basebuild -timeout 30m ./cmd/dispatch -run '^TestTheRestoreCarriesOrSupersedesTheApprovalOfBothDocuments$' -count=1 -v

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/graphcensus"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// restoreSession is the agent's session whose writes move the answered asks out of the specs.
const restoreSession = "ses_restore_approvals_agent"

// copyBlock is the block id of the answered ask of issue C's questions.md and of its copy in spec
// C, and copyQuestion the question both ask.
const (
	copyBlock    = "q-c-shelf"
	copyQuestion = "Which shelf keeps the census?"
)

// restoreAsk is an ask block of the fixture: id, question and two options.
func restoreAsk(id, question string) string {
	return ":::ask{#" + id + " urgency=\"med\" multiple=\"false\"}\n" + question + "\n\n- First: The first way.\n- Second: The second way.\n:::\n"
}

// asAgentSession sends body, a write the agent's session makes through the shared agent bearer,
// naming that session as its actor, to p, and fails unless p answers status.
func asAgentSession(t *testing.T, p *dispatchProcess, method, path string, body map[string]any, status int) httpAnswer {
	t.Helper()
	body["actor"] = map[string]string{"kind": "session", "id": restoreSession}
	answer := p.send(t, method, path, encodeJSON(t, body), agentBearer())
	if answer.status != status {
		t.Fatalf("the agent's %s %s: %s, want %d", method, path, answer, status)
	}
	return answer
}

// agentEdits applies ops to document as the agent's session.
func agentEdits(t *testing.T, p *dispatchProcess, document string, ops ...map[string]string) {
	t.Helper()
	asAgentSession(t, p, http.MethodPost, "/api/v1/artifacts/"+document+"/edits", map[string]any{"ops": ops}, http.StatusOK)
}

// agentUpload uploads content as the document name of issue, as the agent's session, and answers
// its id.
func agentUpload(t *testing.T, p *dispatchProcess, issue, name, content string) string {
	t.Helper()
	answer := asAgentSession(t, p, http.MethodPost, "/api/v1/issues/"+issue+"/artifacts", map[string]any{"name": name, "content": content}, http.StatusCreated)
	return uploadedID(t, answer)
}

// askOfBlock is the ask whose row names block of document, read off the asks table once a
// settlement of the release that wrote it has opened it.
func askOfBlock(t *testing.T, ctx context.Context, database *pgx.Conn, document, block string) string {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		var id string
		err := database.QueryRow(ctx, `select id::text from asks where block_artifact_id = $1::uuid and block_id = $2 and state = 'open'`, document, block).Scan(&id)
		if err == nil {
			return id
		}
		if time.Now().After(deadline) {
			t.Fatalf("no open ask names block %s of %s after 30s: %v", block, document, err)
		}
	}
}

// documentState is what the test reads of a document to tell a restore that wrote from one rolled
// back: how many versions and reviews it holds, its newest version, its approval state - its
// newest review in docs.ReviewOrder, the order every approval read takes, with that review's state
// and the version it names - and the rows of the asks named. A rollback that leaves the counts as
// they were and moves an approval still reads apart.
type documentState struct {
	versions, latest, reviews int
	approval, asks            string
}

// readDocumentState reads document's state, with the block and document each of asks' rows names.
func readDocumentState(t *testing.T, ctx context.Context, database *pgx.Conn, document string, asks ...string) documentState {
	t.Helper()
	var state documentState
	if err := database.QueryRow(ctx, `
		select (select count(*) from artifact_versions where artifact_id = $1::uuid),
		       (select coalesce(max(number), 0) from artifact_versions where artifact_id = $1::uuid),
		       (select count(*) from artifact_reviews where artifact_id = $1::uuid),
		       coalesce((select artifact_reviews.id::text || ' ' || artifact_reviews.state || ' of version ' || artifact_reviews.version
		                 from artifact_reviews where artifact_id = $1::uuid order by `+docs.ReviewOrder+` limit 1), 'no review'),
		       coalesce((select string_agg(k.id || '@' || k.block_artifact_id || '#' || k.block_id ||
		                   '/tuples=' || (select count(*) from graph_tuples t where t.from_kind = 'ask' and t.from_id = k.id::text), ',' order by k.id)
		                 from asks k where k.id::text = any($2::text[])), '')
	`, document, asks).Scan(&state.versions, &state.latest, &state.reviews, &state.approval, &state.asks); err != nil {
		t.Fatalf("read the state of %s: %v", document, err)
	}
	return state
}

// requireApprovedNow fails unless the document's current version reads approved through p
// (approvalOf, GET /api/v1/artifacts/{id}).
func requireApprovedNow(t *testing.T, p *dispatchProcess, document, what string) {
	t.Helper()
	if approval := approvalOf(t, p, document); !approval.approvedLatest() {
		t.Fatalf("%s: GET /api/v1/artifacts/%s answers %+v, want the current version approved", what, document, approval)
	}
}

// restoreLine is the line the restore printed for issue.
func restoreLine(t *testing.T, out, issue string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, " issue="+issue+" ") {
			return line
		}
	}
	t.Fatalf("the restore printed no line for %s:\n%s", issue, out)
	return ""
}

// runRestore runs `envoy-dispatch restore-answered-asks --project RST`, and --dry-run where dry,
// in this process, over databaseURL, fails unless it exits 0, and answers what it printed.
func runRestore(t *testing.T, ctx context.Context, databaseURL string, dry bool) string {
	t.Helper()
	code, out := runRestoreExit(t, ctx, databaseURL, dry)
	if code != 0 {
		t.Fatalf("restore-answered-asks (dry run %t) exited %d:\n%s", dry, code, out)
	}
	return out
}

// runRestoreExit is runRestore's run, answering its exit code and what it printed on either
// stream, whatever the code.
func runRestoreExit(t *testing.T, ctx context.Context, databaseURL string, dry bool) (int, string) {
	t.Helper()
	args := []string{"--project", "RST"}
	if dry {
		args = append(args, "--dry-run")
	}
	var out, errOut bytes.Buffer
	env := readSettings(func(name string) (string, bool) {
		if name == "DATABASE_URL" {
			return databaseURL, true
		}
		return "", false
	})
	code := runSubcommand(ctx, append([]string{"restore-answered-asks"}, args...), env, &out, &errOut)
	t.Logf("restore-answered-asks %v exited %d and printed:\n%s%s", args, code, out.String(), errOut.String())
	return code, out.String() + errOut.String()
}

// databaseWrites is what the test reads to tell a dry run that wrote from one that wrote nothing,
// beside each document's state: the migrations the database records, the asks no node names yet,
// and the events and the tuples it holds.
type databaseWrites struct {
	migrations, unmarkedAsks, events, tuples int
}

// readDatabaseWrites reads the database's databaseWrites. A database this build's migrations have
// not reached yet may hold no graph_tuples table and no asks.node_kind column: it holds no tuple,
// and each of its asks names no node.
func readDatabaseWrites(t *testing.T, ctx context.Context, database *pgx.Conn) databaseWrites {
	t.Helper()
	var writes databaseWrites
	var tuples bool
	if err := database.QueryRow(ctx, `
		select (select count(*) from schema_migrations),
		       (select count(*) from asks k where to_jsonb(k) ->> 'node_kind' is null),
		       (select count(*) from events),
		       to_regclass('graph_tuples') is not null
	`).Scan(&writes.migrations, &writes.unmarkedAsks, &writes.events, &tuples); err != nil {
		t.Fatalf("read what the database holds: %v", err)
	}
	if tuples {
		if err := database.QueryRow(ctx, `select count(*) from graph_tuples`).Scan(&writes.tuples); err != nil {
			t.Fatalf("count the tuples: %v", err)
		}
	}
	return writes
}

// restoredDocument is a document of the fixture the restore can write a version of: what the test
// calls it, its id, and whether the restore must write one (the decisions.md of the main shape
// keeps every version it held).
type restoredDocument struct {
	name, id string
	written  bool
}

// keptBlockOf is the ask whose node the kept attributes of the ask block blockID of document are
// written from, read as the drivers' kept_form reads it (answered-decisions.sh): the ask whose row
// names the block of document under its id; or, for a block no row names whose block node holds a
// standing copied_from (settlement's claimCopies), the ask it copies, with the address of the
// document that ask's row names (refs.ArtifactRef). It answers false for a block that has no ask
// row and is no copy of an ask.
func keptBlockOf(t *testing.T, ctx context.Context, database *pgx.Conn, document, blockID string) (keptBlock, bool) {
	t.Helper()
	var own, source, copied string
	if err := database.QueryRow(ctx, `
		with block as (
			select (select a.id::text from asks a where a.block_artifact_id = $1::uuid and a.block_id = $2) as own,
			       (select c.to_id from document_blocks d
			          cross join lateral (
			            select distinct on (t.to_kind, t.to_id) t.to_id, t.asserted from graph_tuples t
			            where t.from_kind = d.node_kind and t.from_id = d.node_address and t.kind = 'copied_from' and t.to_kind = 'ask'
			            order by t.to_kind, t.to_id, t.seq desc) c
			          where d.artifact_id = $1::uuid and d.block_id = $2 and d.node_address is not null and c.asserted
			          limit 1) as copied
		)
		select coalesce(own, ''), coalesce(case when own is null then copied end, ''),
		       coalesce((select case when sa.session_id is not null then 'dispatch://agent/' || sa.session_id || '/artifact/' || sa.slug
		                             when sa.issue_key is null then 'dispatch://' || sa.project_key || '/artifact/' || sa.slug
		                             when sa.is_primary and sa.kind = 'doc' then 'dispatch://' || sa.issue_key || '/spec'
		                             else 'dispatch://' || sa.issue_key || '/artifact/' || sa.slug end
		                 from asks s join artifacts sa on sa.id = s.block_artifact_id
		                 where own is null and s.id::text = copied), '')
		from block
	`, document, blockID).Scan(&own, &source, &copied); err != nil {
		t.Fatalf("read the ask of the ask block %s of %s: %v", blockID, document, err)
	}
	switch {
	case own != "":
		return keptBlock{block: blockID, ask: own}, true
	case source != "":
		return keptBlock{block: blockID, ask: source, copied: copied}, true
	}
	return keptBlock{block: blockID}, false
}

// requireEveryAskBlockKept fails t unless every ask block of markdown, a version of document (what
// says which), holds the form R10 keeps, each value equal to its ask's node (requireKeptForm): the
// node of its own ask, or of the ask it is a copy of (keptBlockOf). A block that has no ask row and
// is no copy of an ask holds none of the eight server-owned attributes. A block that differs fails
// the test, which prints the block. It answers the answer_state each block's ask's node holds,
// "" for a block with no ask, by block id, and each block it read as a copy of an ask, with the
// ask and the address of that ask's document, by block id.
func requireEveryAskBlockKept(t *testing.T, ctx context.Context, database *pgx.Conn, what, document, markdown string) (map[string]string, map[string]keptBlock) {
	t.Helper()
	tree, err := pmdoc.ParseRendering(markdown)
	if err != nil {
		t.Fatalf("%s: read the version: %v\n%s", what, err, markdown)
	}
	var blocks []string
	pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
		if node.Type != "ask" {
			return true
		}
		if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id != "" {
			blocks = append(blocks, id)
		} else {
			t.Errorf("%s: an ask block holds no id, so no ask's node can be read for it:\n%s", what, markdown)
		}
		return true
	})
	states, copies := map[string]string{}, map[string]keptBlock{}
	for _, block := range blocks {
		if kept, asked := keptBlockOf(t, ctx, database, document, block); asked {
			states[block] = requireKeptForm(t, ctx, database, what, markdown, kept)
			if kept.copied != "" {
				copies[block] = kept
			}
			continue
		}
		states[block] = ""
		var held []string
		for name, value := range storedAskBlock(t, markdown, block).Attrs {
			if slices.Contains(serverOwnedAskAttributes, name) {
				held = append(held, name+"="+mustJSON(t, value))
			}
		}
		if len(held) > 0 {
			slices.Sort(held)
			t.Errorf("%s: the ask block %s has no ask row and is no copy of an ask, and holds %s, want none of the eight server-owned attributes:\n%s",
				what, block, strings.Join(held, " "), askBlockText(markdown, block))
		}
	}
	return states, copies
}

// The restore says what it does to the approval of each document it changes, and carries or
// supersedes each by one rule (c-restore-approvals), over rows the base
// release wrote:
//
//   - issue A: a person approved the spec while it held two answered asks and three it keeps, one
//     answered, one open and one declined; an agent then removed the first ask's block and
//     restated the ask in a decisions.md section (the main shape), and moved the second ask's
//     block into the decisions.md (the second shape), whose own approval a person gave. The
//     restore is the spec's first accepted write on a fleet of this one build, so its one version
//     restores both blocks and cuts the base release's server-owned attributes of every ask block
//     to the ones a version keeps (R10: the state, who answered and when, never the answer's
//     words), and the approval carries across that combined version, naming the version the
//     person approved; the blocks stand in the `## Decisions` part, in place of the pointer line.
//     The decisions.md's approval is superseded and reported. The two versions are one
//     transaction's, one authors value and one xmin. Every ask block of every version the restore
//     wrote, of every issue and both documents, holds the form R10 keeps, each value its ask's
//     node's (requireEveryAskBlockKept).
//   - issue B: a spec with no `## Decisions` part; the restore adds the heading, writes the spec
//     alone, and that version still reads approved.
//   - issue C: a person approved the spec after its ask left, so the approved bytes never held the
//     ask; the restore carries. The approved spec holds a copy of the answered ask of the issue's
//     questions.md (copyBlock), which no row names, so every version the restore wrote of the spec
//     holds that copy, and its block keeps copied_from naming that ask and copied_from_document
//     naming questions.md beside the form R10 keeps from that ask's node. Once this build's
//     settlement of the spec has claimed the copy (its copied_from tuple), requireEveryAskBlockKept
//     reads the block as that copy, and the versions the restore wrote hold a copy of an answered
//     ask, so the copy's arm of the form is read too.
//   - issue D: the colliding ask of each shape, whose block id a callout of the spec holds; each
//     takes a new id, the approval carries, and no superseded approval is reported.
//   - issue E: an ask of the second shape whose question an agent edited in the decisions.md after
//     the move; the restore returns the edited question, supersedes the approval and reports it
//     with the ask's id and its node.
//   - issue F: a person approved the spec while its ask was open, an agent reworded the ask's
//     question in the spec, which its row took, and another person answered the new words before the
//     agent removed the block. The restore returns the ask under the block id the approved version
//     held it under, so the one difference no case carries is the words: the restore supersedes
//     the approval for them alone (sameAskBytes, restoredAskBlocks) and reports it.
//
// The dry run writes nothing (`--dry-run`). Over the database the base release left, which has
// this build's migrations still to apply, it refuses and applies none; once they are applied, the
// step's first phase still owes every ask row the base release wrote, which it would commit, so
// it refuses again and states none. Once that phase has run, it restores both asks of A and rolls
// every issue back, leaving both documents, both approvals, the ask nodes, the events and the
// tuples as they were.
func TestTheRestoreCarriesOrSupersedesTheApprovalOfBothDocuments(t *testing.T) {
	ctx := context.Background()
	databaseURL := censusDatabase(t)
	base := baseRelease(t)
	head := cmdtest.BuildStamped(t, "envoy-dispatch", headBuildStamp)
	database, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect to the run's database: %v", err)
	}
	defer database.Close(ctx)

	previous := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: base, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay})
	previous.waitHealthy(t)
	baseCommit := announcedBaseCommit(t, ctx, database)
	previous.call(t, http.MethodPost, "/api/v1/projects", `{"key":"RST","name":"The restore"}`, http.StatusCreated)
	pointer := func(issue string) map[string]string {
		return map[string]string{"op": "insert", "after": "end",
			"markdown": "## Decisions\n\nThe decisions this spec rests on are in dispatch://" + issue + "/artifact/decisions-md."}
	}
	remove := func(block string) map[string]string { return map[string]string{"op": "delete", "block": block} }

	// Issue A: approved while it held the two answered asks it loses.
	issueA, specA := createCensusIssue(t, previous, "RST", "Both shapes", "# Both shapes\n\n:::requirement{#r-one}\nThe restore returns what was moved.\n:::\n\n"+
		restoreAsk("q-main", "Which store holds the corpus?")+"\n"+restoreAsk("q-moved", "Which reader reads the corpus first?")+"\n"+
		restoreAsk("q-kept", "Which colour does the census print in?")+"\n"+restoreAsk("q-open", "Which day does the census run on?")+"\n"+
		restoreAsk("q-declined", "Which team reads the census first?"))
	mainAsk, movedAsk, keptAsk := blockAskOf(t, previous, issueA, "q-main"), blockAskOf(t, previous, issueA, "q-moved"), blockAskOf(t, previous, issueA, "q-kept")
	answerAsk(t, previous, mainAsk, "First", "The database we run.")
	answerAsk(t, previous, movedAsk, "Second", "The view reads it first.")
	answerAsk(t, previous, keptAsk, "First", "Plain text.")
	// The asks the spec keeps open and declined, whose blocks the restore's version keeps too.
	blockAskOf(t, previous, issueA, "q-open")
	previous.call(t, http.MethodPost, "/api/v1/asks/"+blockAskOf(t, previous, issueA, "q-declined")+"/decline", `{"note":"Not this census."}`, http.StatusOK)
	approvedA := approveAndCapture(t, ctx, previous, database, issueA, specA, "spec A")
	for _, name := range []string{"state", "answered_by", "answered_at", "selected", "answer"} {
		if !strings.Contains(approvedA.markdown, " "+name+`="`) {
			t.Fatalf("the base release's approved spec A holds no %s on its ask blocks:\n%s", name, approvedA.markdown)
		}
	}
	agentEdits(t, previous, specA, remove("q-main"), remove("q-moved"))
	agentEdits(t, previous, specA, pointer(issueA))
	decisionsA := agentUpload(t, previous, issueA, "decisions.md", "# Decisions\n\n## Which store holds the corpus\n\n"+
		"The person chose the database, and we add that it is backed up nightly. This shapes dispatch://"+issueA+"/spec#b-r-one. "+
		"Decided in dispatch://"+issueA+"/ask/"+mainAsk+".\n\n## Which reader reads the corpus first\n\n"+restoreAsk("q-moved", "Which reader reads the corpus first?"))
	settledDocument(t, ctx, previous, database, decisionsA)
	approvedDecisions := approveAndCapture(t, ctx, previous, database, issueA, decisionsA, "decisions.md of A")

	// Issue B: no Decisions part, approved while it held its answered ask.
	issueB, specB := createCensusIssue(t, previous, "RST", "No Decisions part", "# No Decisions part\n\n:::requirement{#r-b}\nThe corpus is written once.\n:::\n\n"+
		restoreAsk("q-b", "Which build writes the corpus?"))
	answerAsk(t, previous, blockAskOf(t, previous, issueB, "q-b"), "First", "The base release.")
	approvedB := approveAndCapture(t, ctx, previous, database, issueB, specB, "spec B")
	agentEdits(t, previous, specB, remove("q-b"))
	decisionsB := agentUpload(t, previous, issueB, "decisions.md", "# Decisions\n\n## The base release writes the corpus\n\n"+
		"The older build writes it. This shapes dispatch://"+issueB+"/spec#b-r-b. Decided in dispatch://"+issueB+"/ask/"+blockAskOf(t, previous, issueB, "q-b")+".\n")
	settledDocument(t, ctx, previous, database, decisionsB)

	// Issue C: approved after its ask left. Before the approval an agent puts into spec C a copy of
	// the answered ask of questions.md - the same id, question, options and urgency - which the base
	// release settles as a copy of that ask, as seed-decisions.sh's copies.md is settled: no row
	// names it, and its block holds copied_from naming that ask and copied_from_document naming
	// questions.md. The restore writes a version of spec C, so that version holds the copy.
	issueC, specC := createCensusIssue(t, previous, "RST", "Approved after the move", "# Approved after the move\n\n"+restoreAsk("q-c", "Which census runs first?"))
	askC := blockAskOf(t, previous, issueC, "q-c")
	answerAsk(t, previous, askC, "First", "The store census.")
	questionsC := agentUpload(t, previous, issueC, "questions.md", "# Questions\n\n"+restoreAsk(copyBlock, copyQuestion))
	copiedAsk := askOfBlock(t, ctx, database, questionsC, copyBlock)
	answerAsk(t, previous, copiedAsk, "Second", "The second shelf.")
	settledDocument(t, ctx, previous, database, questionsC)
	agentEdits(t, previous, specC, remove("q-c"))
	agentEdits(t, previous, specC, map[string]string{"op": "insert", "after": "end", "markdown": strings.TrimSuffix(restoreAsk(copyBlock, copyQuestion), "\n")})
	agentEdits(t, previous, specC, pointer(issueC))
	approvedC := approveAndCapture(t, ctx, previous, database, issueC, specC, "spec C")
	var copyRows int
	if err := database.QueryRow(ctx, `select count(*) from asks where block_artifact_id = $1::uuid and block_id = $2`, specC, copyBlock).Scan(&copyRows); err != nil {
		t.Fatalf("read the ask rows of the block %s of spec C: %v", copyBlock, err)
	}
	if copied := askBlockText(approvedC.markdown, copyBlock); copyRows != 0 || !strings.Contains(copied, ` copied_from="`+copiedAsk+`"`) ||
		!strings.Contains(copied, ` copied_from_document="`) || !strings.Contains(copied, ` state="answered"`) {
		t.Fatalf("the base release settled the block %s of spec C under %d ask rows and wrote it as:\n%s\nwant a copy no row names, answered, naming the ask %s of questions.md and its document",
			copyBlock, copyRows, copied, copiedAsk)
	}
	// The version the person approved never held the ask: the restore is what returns it, and the
	// version its carry reaches is the one that holds it again.
	if blockOpener(approvedC.markdown, "q-c") != "" {
		t.Fatalf("the approved version %d of spec C holds the block q-c, want the version the person approved after the ask left it:\n%s", approvedC.version, approvedC.markdown)
	}
	var questionsSlug string
	if err := database.QueryRow(ctx, `select slug from artifacts where id = $1::uuid`, questionsC).Scan(&questionsSlug); err != nil {
		t.Fatalf("read the slug of questions.md of %s: %v", issueC, err)
	}
	copySource := keptBlock{block: copyBlock, ask: copiedAsk, copied: "dispatch://" + issueC + "/artifact/" + questionsSlug}

	// Issue D: the colliding ask of each shape.
	issueD, specD := createCensusIssue(t, previous, "RST", "Collisions", "# Collisions\n\n"+restoreAsk("q-dmain", "Which line names a move?"))
	answerAsk(t, previous, blockAskOf(t, previous, issueD, "q-dmain"), "First", "One line a move.")
	agentEdits(t, previous, specD, remove("q-dmain"))
	agentEdits(t, previous, specD, map[string]string{"op": "insert", "after": "end", "markdown": ":::callout{#q-dmain kind=\"note\" title=\"\"}\nA callout took the id.\n:::\n\n" +
		":::callout{#q-dmove kind=\"note\" title=\"\"}\nAnother callout took the second id.\n:::"})
	agentEdits(t, previous, specD, pointer(issueD))
	decisionsD := agentUpload(t, previous, issueD, "decisions.md", "# Decisions\n\n## Which way the census reads\n\n"+restoreAsk("q-dmove", "Which way does the census read?"))
	answerAsk(t, previous, askOfBlock(t, ctx, database, decisionsD, "q-dmove"), "Second", "Backwards.")
	approvedD := approveAndCapture(t, ctx, previous, database, issueD, specD, "spec D")

	// Issue E: approved while its ask was open; the person declines the ask, an agent moves its
	// block into the decisions.md and edits its question there, which opens the ask of that
	// document's block on the new words, and the person answers it. Every write of an open ask's
	// block but a person's own editor is refused its removal (refuseDroppedAskBlocks), so the move
	// is of an ask its person settled first, and the edit after the move is what the person then
	// answers: the ask the restore returns asks other words than the spec the person approved.
	issueE, specE := createCensusIssue(t, previous, "RST", "Edited after the move", "# Edited after the move\n\n:::requirement{#r-e}\nThe question is asked once.\n:::\n\n"+
		restoreAsk("q-e", "Which census reads the moves?"))
	declined := blockAskOf(t, previous, issueE, "q-e")
	approvedE := approveAndCapture(t, ctx, previous, database, issueE, specE, "spec E")
	previous.call(t, http.MethodPost, "/api/v1/asks/"+declined+"/decline", `{"note":"Ask it again once it names the census."}`, http.StatusOK)
	settledDocument(t, ctx, previous, database, specE)
	agentEdits(t, previous, specE, remove("q-e"))
	decisionsE := agentUpload(t, previous, issueE, "decisions.md", "# Decisions\n\n## Which census reads the moves\n\n"+restoreAsk("q-e", "Which census reads the moves?"))
	settledDocument(t, ctx, previous, database, decisionsE)
	agentEdits(t, previous, decisionsE, map[string]string{"op": "replace", "find": "Which census reads the moves?", "with": "Which census reads every move, and when?"})
	settledDocument(t, ctx, previous, database, decisionsE)
	edited := askOfBlock(t, ctx, database, decisionsE, "q-e")
	answerAsk(t, previous, edited, "First", "The store census, at each deploy.")
	settledDocument(t, ctx, previous, database, decisionsE)
	var question string
	if err := database.QueryRow(ctx, `select question from asks where id = $1::uuid`, edited).Scan(&question); err != nil || question != "Which census reads every move, and when?" {
		t.Fatalf("the edited ask reads %q (%v), want the question the agent edited in the decisions.md", question, err)
	}

	// Issue F: approved while its ask was open; an agent rewords the open ask's question in the
	// spec, which its row takes, another person answers the new words - so the base release's own
	// carry, which follows the approver's own answer, leaves the approval at the words it approved
	// - and the agent then removes the answered ask's block.
	issueF, specF := createCensusIssue(t, previous, "RST", "Reworded before the answer", "# Reworded before the answer\n\n:::requirement{#r-f}\nThe census counts each move.\n:::\n\n"+
		restoreAsk("q-f", "Which census counts the moves?"))
	reworded := blockAskOf(t, previous, issueF, "q-f")
	approvedF := approveAndCapture(t, ctx, previous, database, issueF, specF, "spec F")
	agentEdits(t, previous, specF, map[string]string{"op": "replace", "find": "Which census counts the moves?", "with": "Which census counts every move?"})
	settledDocument(t, ctx, previous, database, specF)
	if err := database.QueryRow(ctx, `select question from asks where id = $1::uuid`, reworded).Scan(&question); err != nil || question != "Which census counts every move?" {
		t.Fatalf("the reworded ask reads %q (%v), want the question the agent reworded in the spec", question, err)
	}
	// The other person answers the words as they read them now: the answer names the ask's edit
	// revision, which the rewording moved.
	var revision struct {
		Ask struct {
			EditedAt json.RawMessage `json:"edited_at"`
		} `json:"ask"`
	}
	if err := json.Unmarshal(previous.call(t, http.MethodGet, "/api/v1/asks/"+reworded, "", http.StatusOK), &revision); err != nil {
		t.Fatalf("read the reworded ask: %v", err)
	}
	if answered := previous.send(t, http.MethodPost, "/api/v1/asks/"+reworded+"/answer", encodeJSON(t, map[string]any{
		"selected": []string{"First"}, "text": "The store census.", "expected_edited_at": revision.Ask.EditedAt,
	}), asUser("restore-approvals-other")); answered.status != http.StatusOK {
		t.Fatalf("another person's answer of the reworded ask: %s, want 200", answered)
	}
	settledDocument(t, ctx, previous, database, specF)
	agentEdits(t, previous, specF, remove("q-f"))
	t.Logf("the base release %s wrote the fixture: spec A approved at version %d, its decisions.md at version %d, B at %d, C at %d, D at %d, E at %d, F at %d",
		baseCommit, approvedA.version, approvedDecisions.version, approvedB.version, approvedC.version, approvedD.version, approvedE.version, approvedF.version)
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

	// The dry run writes nothing. Over the database the base release left, this build's migrations
	// are still to apply: it refuses and applies none.
	asksOfA := []string{mainAsk, movedAsk, keptAsk}
	beforeWrites := readDatabaseWrites(t, ctx, database)
	beforeSpec := readDocumentState(t, ctx, database, specA, asksOfA...)
	if code, printed := runRestoreExit(t, ctx, databaseURL, true); code != 1 || !strings.Contains(printed, "--dry-run writes nothing") || !strings.Contains(printed, "still to apply") {
		t.Fatalf("the dry run over a database with migrations to apply exited %d and printed:\n%s\nwant exit 1 refusing it", code, printed)
	}
	if after := readDatabaseWrites(t, ctx, database); after != beforeWrites {
		t.Fatalf("after the refused dry run the database reads %+v, want it as it was: %+v", after, beforeWrites)
	}
	if after := readDocumentState(t, ctx, database, specA, asksOfA...); after != beforeSpec {
		t.Fatalf("after the refused dry run spec A reads %+v, want it as it was: %+v", after, beforeSpec)
	}
	// Once the migrations are applied, the step's first phase still owes every ask row the base
	// release wrote, which it would commit: it refuses again and states none.
	migrated, err := store.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open the run's database: %v", err)
	}
	defer migrated.Pool.Close()
	if err := migrated.Migrate(ctx); err != nil {
		t.Fatalf("apply this build's migrations: %v", err)
	}
	beforeWrites = readDatabaseWrites(t, ctx, database)
	if beforeWrites.unmarkedAsks == 0 {
		t.Fatal("no ask row the base release wrote waits for the step's first phase")
	}
	if code, printed := runRestoreExit(t, ctx, databaseURL, true); code != 1 || !strings.Contains(printed, docs.ErrDryRunOwesBackfill.Error()) {
		t.Fatalf("the dry run over a database whose first phase is owed exited %d and printed:\n%s\nwant exit 1 refusing it", code, printed)
	}
	if after := readDatabaseWrites(t, ctx, database); after != beforeWrites {
		t.Fatalf("after the dry run the first phase owes, the database reads %+v, want it as it was: %+v", after, beforeWrites)
	}
	// Once the first phase has run, the dry run restores both asks of A and rolls every issue back.
	if _, err := graphcensus.BackfillAsks(ctx, migrated); err != nil {
		t.Fatalf("run the step's first phase: %v", err)
	}
	beforeWrites = readDatabaseWrites(t, ctx, database)
	beforeSpec = readDocumentState(t, ctx, database, specA, asksOfA...)
	beforeDecisions := readDocumentState(t, ctx, database, decisionsA)
	dry := runRestore(t, ctx, databaseURL, true)
	if line := restoreLine(t, dry, issueA); !strings.Contains(line, "restored=2") || !strings.Contains(line, "dry_run=true") {
		t.Fatalf("the dry run printed %q for %s, want both asks restored, rolled back", line, issueA)
	}
	if after := readDatabaseWrites(t, ctx, database); after != beforeWrites {
		t.Fatalf("after the dry run the database reads %+v, want it as it was: %+v", after, beforeWrites)
	}
	if after := readDocumentState(t, ctx, database, specA, asksOfA...); after != beforeSpec {
		t.Fatalf("after the dry run spec A reads %+v, want it as it was: %+v", after, beforeSpec)
	}
	if after := readDocumentState(t, ctx, database, decisionsA); after != beforeDecisions {
		t.Fatalf("after the dry run the decisions.md of A reads %+v, want it as it was: %+v", after, beforeDecisions)
	}
	t.Logf("the dry run rolled back: spec A %+v, its decisions.md %+v, the database %+v", beforeSpec, beforeDecisions, beforeWrites)

	// Every document the restore can write a version of, with the newest version each holds before
	// it runs: the versions above that, up to the newest each holds once it has run and before this
	// build's server boots, are the ones the restore wrote.
	restored := []restoredDocument{
		{"spec A", specA, true}, {"the decisions.md of A", decisionsA, true}, {"spec B", specB, true}, {"the decisions.md of B", decisionsB, false},
		{"spec C", specC, true}, {"spec D", specD, true}, {"the decisions.md of D", decisionsD, true},
		{"spec E", specE, true}, {"the decisions.md of E", decisionsE, true}, {"spec F", specF, true},
	}
	beforeRestore, afterRestore := map[string]int{}, map[string]int{}
	for _, document := range restored {
		beforeRestore[document.id] = readDocumentState(t, ctx, database, document.id).latest
	}
	specBVersions := readDocumentState(t, ctx, database, specB).versions
	decisionsBVersions := readDocumentState(t, ctx, database, decisionsB).versions
	out := runRestore(t, ctx, databaseURL, false)
	for _, document := range restored {
		afterRestore[document.id] = readDocumentState(t, ctx, database, document.id).latest
	}

	current := startDispatchProcessWith(t, databaseURL, dispatchOptions{Binary: head, SigningKey: previousBuildSigningKey, SettleDelay: oneAnswerSettleDelay,
		Env: map[string]string{"DISPATCH_TEST_HOOKS": "1", "DISPATCH_TEST_HOLD_DECISIONS_STEP": "1"}})
	current.waitHealthy(t)

	// The copy in spec C, read from the ask the fixture made it of: every version the restore wrote
	// of spec C holds the block copyBlock with copied_from naming that ask, copied_from_document
	// naming questions.md, and the rest of the form R10 keeps as that ask's node gives it.
	versionMarkdown := func(document string, number int) string {
		t.Helper()
		var version struct {
			Markdown string `json:"markdown"`
		}
		readJSON(t, current, fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", document, number), &version)
		return version.Markdown
	}
	for number := beforeRestore[specC] + 1; number <= afterRestore[specC]; number++ {
		requireKeptForm(t, ctx, database, fmt.Sprintf("the copy %s in version %d of spec C, which the restore wrote", copyBlock, number),
			versionMarkdown(specC, number), copySource)
	}
	// This build's settlement of spec C claims the copy: it states the copied_from tuple of the
	// block's node, which keptBlockOf reads a copy by. The restore's write left spec C owing a
	// settlement, which the load of its room runs.
	settledOnTheNextLoad(t, ctx, current, database, specC, copyQuestion)
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(200 * time.Millisecond) {
		var claimed bool
		if err := database.QueryRow(ctx, `
			select exists (select 1 from document_blocks d
				join graph_tuples t on t.from_kind = d.node_kind and t.from_id = d.node_address
				where d.artifact_id = $1::uuid and d.block_id = $2 and t.kind = 'copied_from' and t.to_kind = 'ask' and t.to_id = $3 and t.asserted)
		`, specC, copyBlock, copiedAsk).Scan(&claimed); err != nil {
			t.Fatalf("read the copied_from of the block %s of spec C: %v", copyBlock, err)
		}
		if claimed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no settlement of this build stated the copied_from of the block %s of spec C to the ask %s within 30s of the load of its room", copyBlock, copiedAsk)
		}
	}

	// Every ask block of every version the restore wrote, of both documents and in both shapes,
	// holds the form R10 keeps, each value equal to its ask's node; and the versions hold an ask of
	// each answer state and a copy of an answered ask, so each arm of the form is read.
	keptStates := map[string]bool{}
	answeredCopies := 0
	for _, document := range restored {
		if document.written && afterRestore[document.id] == beforeRestore[document.id] {
			t.Errorf("the restore wrote no version of %s, which still stands at version %d", document.name, beforeRestore[document.id])
		}
		for number := beforeRestore[document.id] + 1; number <= afterRestore[document.id]; number++ {
			markdown := versionMarkdown(document.id, number)
			what := fmt.Sprintf("version %d of %s, which the restore wrote", number, document.name)
			states, copies := requireEveryAskBlockKept(t, ctx, database, what, document.id, markdown)
			for _, state := range states {
				keptStates[state] = true
			}
			for block := range copies {
				if states[block] == asks.StateAnswered {
					answeredCopies++
				}
			}
			if read, copied := copies[copyBlock]; document.id == specC && (!copied || read != copySource) {
				t.Errorf("%s: its block %s reads as the copy %+v (a copy: %t), want the copy %+v of the ask of questions.md:\n%s",
					what, copyBlock, read, copied, copySource, askBlockText(markdown, copyBlock))
			}
			t.Logf("%s: the answer state of the ask of each of its ask blocks: %v, and the copies among them: %v", what, states, copies)
		}
	}
	if answeredCopies == 0 {
		t.Errorf("no ask block of a version the restore wrote reads as a copy of an answered ask, so the form R10 keeps for a copy is never read")
	}
	for _, state := range []string{asks.StateAnswered, asks.StateResolved, asks.StateOpen} {
		if !keptStates[state] {
			t.Errorf("no ask block of a version the restore wrote stands for an ask whose node is %s, so the form R10 keeps for it is never read", state)
		}
	}

	// A: one combined version of the spec, carried; the decisions.md's approval superseded.
	lineA := restoreLine(t, out, issueA)
	if !strings.Contains(lineA, "restored=2 moved=1") || !strings.Contains(lineA, "spec_approval=carried") ||
		!strings.Contains(lineA, "superseded=dispatch://"+issueA+"/artifact/decisions-md/review=") {
		t.Fatalf("the restore printed %q for %s, want two asks restored, one moved out of the decisions.md, the spec's approval carried and the decisions.md's superseded", lineA, issueA)
	}
	numberA, markdownA := latestMarkdown(t, current, specA)
	requireApprovedNow(t, current, specA, "spec A")
	for _, name := range []string{"selected", "answer"} {
		if strings.Contains(markdownA, " "+name+`="`) {
			t.Fatalf("the restored spec A still holds the base release's %s on an ask block:\n%s", name, markdownA)
		}
	}
	for _, block := range []string{"q-main", "q-moved", "q-kept"} {
		if !strings.Contains(markdownA, "#"+block+` urgency="med" multiple="false" state="answered" answered_by="`) {
			t.Fatalf("the restored spec A's block %s keeps no answered state and answerer (R10's kept form):\n%s", block, markdownA)
		}
	}
	decisionsPart := markdownA[strings.Index(markdownA, "## Decisions"):]
	if !strings.Contains(decisionsPart, "#q-main ") || !strings.Contains(decisionsPart, "#q-moved ") || strings.Contains(markdownA, "artifact/decisions-md") {
		t.Fatalf("the restored spec A holds no restored blocks in its Decisions part in place of the pointer line:\n%s", markdownA)
	}
	newest := newestReviewOf(t, current, specA)
	carriedFrom := fmt.Sprintf("Carried forward from the approval of version %d: ", approvedA.version)
	if newest.Version != numberA || newest.State != "approved" || newest.Reason == nil || !strings.HasPrefix(*newest.Reason, carriedFrom) ||
		!strings.Contains(*newest.Reason, "the server-owned attributes of its ask blocks") || !strings.Contains(*newest.Reason, "the ask blocks its Decisions part holds again") {
		t.Fatalf("the newest review of spec A is %+v, want the carry to version %d of the approval of version %d across the restore and the attribute removal", newest, numberA, approvedA.version)
	}
	t.Logf("spec A: version %d reads approved: %s", numberA, *newest.Reason)
	if approval := approvalOf(t, current, decisionsA); approval.approvedLatest() {
		t.Fatalf("the decisions.md of A reads %+v, want its approval superseded by the restore", approval)
	}
	var xmins, authors []string
	rows, err := database.Query(ctx, `
		select v.xmin::text, v.authors::text from artifact_versions v
		where (v.artifact_id = $1::uuid and v.number = $2) or (v.artifact_id = $3::uuid and v.number = (select max(number) from artifact_versions where artifact_id = $3::uuid))
	`, specA, numberA, decisionsA)
	if err != nil {
		t.Fatalf("read the restore's versions: %v", err)
	}
	for rows.Next() {
		var xmin, author string
		if err := rows.Scan(&xmin, &author); err != nil {
			t.Fatal(err)
		}
		xmins, authors = append(xmins, xmin), append(authors, author)
	}
	rows.Close()
	if len(xmins) != 2 || xmins[0] != xmins[1] || authors[0] != authors[1] {
		t.Fatalf("the restore's two versions read xmin %v and authors %v, want one transaction's and one writer's", xmins, authors)
	}
	t.Logf("spec A and its decisions.md: versions written in one transaction (xmin %s) by %s", xmins[0], authors[0])

	// B: the heading added, the spec written alone, still approved.
	if line := restoreLine(t, out, issueB); !strings.Contains(line, "restored=1") || !strings.Contains(line, "spec_approval=carried") || !strings.Contains(line, "decisions_version=0") {
		t.Fatalf("the restore printed %q for %s, want one ask restored into the spec alone, its approval carried", line, issueB)
	}
	if got := readDocumentState(t, ctx, database, specB).versions; got != specBVersions+1 {
		t.Fatalf("spec B holds %d versions, want %d: the main shape writes one version of the spec", got, specBVersions+1)
	}
	if got := readDocumentState(t, ctx, database, decisionsB).versions; got != decisionsBVersions {
		t.Fatalf("the decisions.md of B holds %d versions, want the %d it held: the main shape writes the spec alone", got, decisionsBVersions)
	}
	_, markdownB := latestMarkdown(t, current, specB)
	if heading := strings.Index(markdownB, "## Decisions\n"); heading < 0 || !strings.Contains(markdownB[heading:], "#q-b ") {
		t.Fatalf("the restored spec B holds no Decisions part with the ask under it:\n%s", markdownB)
	}
	requireApprovedNow(t, current, specB, "spec B")

	// C: the approved bytes never held the ask, and the restore puts that same ask back. The copy
	// copyBlock alone also takes a write of the kept form, so the approval at the latest version
	// proves nothing of q-c by itself: the restore names one restored ask under its own block id,
	// and the newest version it wrote of spec C holds the block q-c with its question.
	if line := restoreLine(t, out, issueC); !strings.Contains(line, "restored=1 moved=0 new_ids=0") || !strings.Contains(line, "spec_approval=carried") {
		t.Fatalf("the restore printed %q for %s, want one ask restored under its own block id and the approval carried", line, issueC)
	}
	restoredC := versionMarkdown(specC, afterRestore[specC])
	if blockOpener(restoredC, "q-c") == "" || !strings.Contains(askBlockText(restoredC, "q-c"), "Which census runs first?") {
		t.Fatalf("version %d of spec C, which the restore wrote, holds no block q-c asking %q:\n%s", afterRestore[specC], "Which census runs first?", restoredC)
	}
	requireApprovedNow(t, current, specC, "spec C")

	// D: a new block id in each shape, carried, nothing superseded.
	if line := restoreLine(t, out, issueD); !strings.Contains(line, "restored=2 moved=1 new_ids=2") || !strings.Contains(line, "spec_approval=carried") || !strings.Contains(line, "superseded= ") {
		t.Fatalf("the restore printed %q for %s, want both colliding asks given new ids, the approval carried and nothing superseded", line, issueD)
	}
	requireApprovedNow(t, current, specD, "spec D")

	// E: the edited question returned, the approval superseded and reported.
	lineE := restoreLine(t, out, issueE)
	if !strings.Contains(lineE, "spec_approval=superseded") || !strings.Contains(lineE, "superseded=dispatch://"+issueE+"/spec/review=") ||
		!strings.Contains(lineE, "asks=dispatch://"+issueE+"/ask/"+edited) || !strings.Contains(lineE, "/nodes=dispatch://"+issueE+"/spec#b-") ||
		!strings.Contains(lineE, "approved_version="+strconv.Itoa(approvedE.version)) {
		t.Fatalf("the restore printed %q for %s, want the spec's approval of version %d superseded and reported with the ask %s and its node", lineE, issueE, approvedE.version, edited)
	}
	_, markdownE := latestMarkdown(t, current, specE)
	if !strings.Contains(markdownE, "Which census reads every move, and when?") {
		t.Fatalf("the restored spec E holds no edited question:\n%s", markdownE)
	}
	if approval := approvalOf(t, current, specE); approval.approvedLatest() {
		t.Fatalf("spec E reads %+v, want the restore that changed the question's words to supersede its approval", approval)
	}
	t.Logf("spec E: %s", lineE)

	// F: the ask returned under the block id the approved version held it under, with the words
	// the person answered; the approval superseded for those words alone, and reported.
	lineF := restoreLine(t, out, issueF)
	if !strings.Contains(lineF, "restored=1 moved=0 new_ids=0") || !strings.Contains(lineF, "spec_approval=superseded") ||
		!strings.Contains(lineF, "superseded=dispatch://"+issueF+"/spec/review=") || !strings.Contains(lineF, "asks=dispatch://"+issueF+"/ask/"+reworded) ||
		!strings.Contains(lineF, "/nodes=dispatch://"+issueF+"/spec#b-q-f") || !strings.Contains(lineF, "approved_version="+strconv.Itoa(approvedF.version)) {
		t.Fatalf("the restore printed %q for %s, want the ask returned under q-f and the spec's approval of version %d superseded for its words, reported with the ask %s", lineF, issueF, approvedF.version, reworded)
	}
	_, markdownF := latestMarkdown(t, current, specF)
	if !strings.Contains(markdownF, "#q-f ") || !strings.Contains(markdownF, "Which census counts every move?") {
		t.Fatalf("the restored spec F holds no ask q-f asking the reworded question:\n%s", markdownF)
	}
	if approval := approvalOf(t, current, specF); approval.approvedLatest() {
		t.Fatalf("spec F reads %+v, want the restore that returned other words under the approved block id to supersede its approval", approval)
	}
	t.Logf("spec F: %s", lineF)
}
