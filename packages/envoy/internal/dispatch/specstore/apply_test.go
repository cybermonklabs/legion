package specstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specchange"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

var person = model.Actor{Kind: "user", ID: "alice"}

// issue is the name of the nth issue whose change request a test approves. The store keeps an
// issue's name as the provenance of what its approval wrote and reads nothing else of it.
func issue(n int) string { return fmt.Sprintf("<issue %d>", n) }

// request is the change request of issue holding blocks, each `<id>|<block>`.
func request(issue string, blocks ...string) specchange.Request {
	var read []specchange.Block
	for _, block := range blocks {
		id, text, _ := strings.Cut(block, "|")
		read = append(read, specchange.Block{Address: issue + "/spec#" + id, Text: text})
	}
	return specchange.FromBlocks(issue, read)
}

func statement(id, text, attributes string) string {
	return id + "|:::requirement{#" + id + " kind=\"behavior\"" + attributes + "}\n" + text + "\n:::\n"
}

func check(id, serves, claim, attributes string) string {
	return id + "|:::check{#" + id + " statement=\"" + serves + "\" subject=\"code\" procedure=\"agent\"" + attributes + "}\n" +
		"Claim: " + claim + "\n\nProcedure: An agent reads it.\n\nFailing example: A build that breaks it.\n:::\n"
}

// apply runs Apply for issue in a transaction of its own and commits it. It answers every failure,
// so a goroutine of a test may call it.
func apply(store *store.Store, issue string, blocks ...string) (Applied, error) {
	ctx := context.Background()
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		return Applied{}, err
	}
	defer tx.Rollback(ctx)
	applied, err := Apply(ctx, tx, Input{Project: "CORE", Repo: "acme/app", Issue: issue, Request: request(issue, blocks...), ChecksIncluded: true,
		Provenance: Provenance{Act: ActApproval, Issue: issue, Person: person}, Who: person}, RunNow(ctx, tx))
	if err != nil {
		return Applied{}, err
	}
	return applied, tx.Commit(ctx)
}

func mustApply(t *testing.T, store *store.Store, issue string, blocks ...string) Applied {
	t.Helper()
	applied, err := apply(store, issue, blocks...)
	if err != nil {
		t.Fatalf("approve %s: %v", issue, err)
	}
	return applied
}

// current is the current text of each node of the approved spec, by id, removed nodes left out.
func current(t *testing.T, store *store.Store) map[string]string {
	t.Helper()
	state, err := Read(context.Background(), store.Pool, "CORE", "acme/app")
	if err != nil {
		t.Fatalf("read the approved spec: %v", err)
	}
	texts := map[string]string{}
	for id, history := range state.Nodes {
		if version := history.Current(); version != nil {
			block, err := version.Block()
			if err != nil {
				t.Fatalf("print %s: %v", id, err)
			}
			texts[id] = block
		}
	}
	return texts
}

// refusedAsRetired is whether err refuses an approval for writing an id the approved spec removed.
func refusedAsRetired(err error) bool {
	var refusal *Refusal
	return errors.As(err, &refusal) && refusal.Code == RefusalNodeInvalid && strings.Contains(refusal.Detail, "retired-id")
}

// The approved spec keeps each version and each removal of a node in the order they were written,
// numbers each version after the one before, and prints each from its fields; specgraph.Approved
// reads the same nodes, a removed one as removed.
func TestTheApprovedSpecKeepsEachVersionOfANode(t *testing.T) {
	database := storetest.Open(t)
	mustApply(t, database, issue(1), statement("a-bill-is-sent", "A bill is sent.", ""), statement("a-bill-is-kept", "A bill is kept.", ""))
	mustApply(t, database, issue(2), statement("a-bill-is-sent", "A bill is sent once.", ` base="1"`))
	mustApply(t, database, issue(3), statement("a-bill-is-kept", "A bill is kept.", ` base="1" removes="true"`))
	history, err := ReadNode(context.Background(), database.Pool, "CORE", "acme/app", "a-bill-is-sent")
	if err != nil || history == nil {
		t.Fatalf("read a-bill-is-sent: %v, %v", history, err)
	}
	if len(history.Versions) != 2 || history.Versions[0].Version != 1 || history.Versions[1].Version != 2 ||
		history.Versions[0].Provenance.Issue != issue(1) || history.Versions[1].Provenance.Issue != issue(2) {
		t.Errorf("a-bill-is-sent holds %+v, want version 1 of %s and version 2 of %s", history.Versions, issue(1), issue(2))
	}
	approved, err := Approved(context.Background(), database.Pool, "CORE", "acme/app")
	if err != nil {
		t.Fatalf("build the approved spec: %v", err)
	}
	kept := approved.Nodes["a-bill-is-kept"]
	if kept.Removed == nil || !*kept.Removed || len(kept.Versions) != 1 {
		t.Errorf("a-bill-is-kept reads %+v, want removed with its one version", kept)
	}
	sent := approved.Nodes["a-bill-is-sent"]
	if sent.Removed == nil || *sent.Removed || !strings.Contains(sent.Versions[1].Block, "A bill is sent once.") {
		t.Errorf("a-bill-is-sent reads %+v, want version 2 current", sent)
	}
	if _, err := apply(database, issue(4), statement("a-bill-is-kept", "A bill is kept again.", "")); !refusedAsRetired(err) {
		t.Errorf("writing the removed id again answers %v, want it refused as a retired id", err)
	}
}

// A request that brings back an earlier text of a statement brings back the checks of that text
// (R25): each check that served it while that text stood, at the text it held then, and the
// removal of each check that serves it now and did not then.
func TestAnEarlierTextBringsBackItsChecks(t *testing.T) {
	database := storetest.Open(t)
	mustApply(t, database, issue(1),
		statement("a-bill-is-sent", "A bill is sent.", ""),
		check("a-sent-bill-reads-sent", "a-bill-is-sent", "A sent bill reads sent.", ""))
	mustApply(t, database, issue(2),
		statement("a-bill-is-sent", "A bill is sent by mail.", ` base="1"`),
		check("a-sent-bill-reads-sent", "a-bill-is-sent", "A mailed bill reads sent.", ` base="1"`),
		check("a-mailed-bill-has-a-stamp", "a-bill-is-sent", "A mailed bill has a stamp.", ""))
	applied := mustApply(t, database, issue(3), statement("a-bill-is-sent", "A bill is sent.", ` base="2"`))
	texts := current(t, database)
	if !strings.Contains(texts["a-bill-is-sent"], "A bill is sent.\n") {
		t.Errorf("a-bill-is-sent reads %q, want its first text back", texts["a-bill-is-sent"])
	}
	if !strings.Contains(texts["a-sent-bill-reads-sent"], "Claim: A sent bill reads sent.") {
		t.Errorf("a-sent-bill-reads-sent reads %q, want the claim it held with the first text", texts["a-sent-bill-reads-sent"])
	}
	if text, held := texts["a-mailed-bill-has-a-stamp"]; held {
		t.Errorf("a-mailed-bill-has-a-stamp reads %q, want it removed: it did not serve the first text", text)
	}
	acts := map[string]string{}
	for _, written := range applied.Written {
		acts[written.ID] = written.Act
	}
	if acts["a-bill-is-sent"] != ActApproval || acts["a-sent-bill-reads-sent"] != ActBroughtBack || acts["a-mailed-bill-has-a-stamp"] != ActBroughtBack {
		t.Errorf("the act wrote %+v, want the statement by the approval and both checks brought back with it", applied.Written)
	}
}

// A check a later approval removed comes back when a request restores the text of its statement it
// served (R25), as a version written after its removal, and goes again when a request restores the
// text that removed it. Its id stays retired for any other write: a request that writes it as a new
// node, and not as a check of a restored text, is refused, before the restore and after it.
func TestRestoringAnEarlierTextBringsBackARemovedCheck(t *testing.T) {
	database := storetest.Open(t)
	mustApply(t, database, issue(1),
		statement("a-bill-is-sent", "A bill is sent.", ""),
		check("a-sent-bill-reads-sent", "a-bill-is-sent", "A sent bill reads sent.", ""))
	mustApply(t, database, issue(2),
		statement("a-bill-is-sent", "A bill is sent by mail.", ` base="1"`),
		check("a-sent-bill-reads-sent", "a-bill-is-sent", "A sent bill reads sent.", ` base="1" removes="true"`))
	if _, err := apply(database, issue(3), check("a-sent-bill-reads-sent", "a-bill-is-sent", "A sent bill reads sent again.", "")); !refusedAsRetired(err) {
		t.Fatalf("writing the removed check as a new node answers %v, want it refused as a retired id", err)
	}

	restored, err := apply(database, issue(4), statement("a-bill-is-sent", "A bill is sent.", ` base="2"`))
	if err != nil {
		t.Fatalf("restoring the first text of a-bill-is-sent answers %v, want it approved with the check that text held", err)
	}
	if text := current(t, database)["a-sent-bill-reads-sent"]; !strings.Contains(text, "Claim: A sent bill reads sent.") {
		t.Errorf("a-sent-bill-reads-sent reads %q, want the claim it held with the first text", text)
	}
	history, err := ReadNode(context.Background(), database.Pool, "CORE", "acme/app", "a-sent-bill-reads-sent")
	if err != nil || history == nil {
		t.Fatalf("read a-sent-bill-reads-sent: %v, %v", history, err)
	}
	if len(history.Versions) != 2 || len(history.Removals) != 1 || history.Versions[1].Version != 2 ||
		history.Versions[1].Provenance.Act != ActBroughtBack || history.Versions[1].Provenance.BroughtBackWith != "a-bill-is-sent" {
		t.Errorf("a-sent-bill-reads-sent holds versions %+v and removals %+v, want version 1, its removal, and version 2 brought back with a-bill-is-sent",
			history.Versions, history.Removals)
	}
	if !writes(restored, "a-sent-bill-reads-sent", ActBroughtBack, false) {
		t.Errorf("the restore wrote %+v, want a-sent-bill-reads-sent brought back", restored.Written)
	}

	reversed := mustApply(t, database, issue(5), statement("a-bill-is-sent", "A bill is sent by mail.", ` base="3"`))
	if text, held := current(t, database)["a-sent-bill-reads-sent"]; held {
		t.Errorf("a-sent-bill-reads-sent reads %q, want it removed again: it did not serve the text restored now", text)
	}
	if !writes(reversed, "a-sent-bill-reads-sent", ActBroughtBack, true) {
		t.Errorf("the second restore wrote %+v, want the removal of a-sent-bill-reads-sent brought back with it", reversed.Written)
	}
	if _, err := apply(database, issue(6), check("a-sent-bill-reads-sent", "a-bill-is-sent", "A sent bill reads sent again.", "")); !refusedAsRetired(err) {
		t.Errorf("writing the removed check as a new node after the restores answers %v, want it refused as a retired id", err)
	}
}

// A request that restores an earlier text of a statement brings back the checks of that text (R25)
// whether or not it shows the statement's current checks unchanged beside it: a check the request
// holds as context, at the current version its base names, is no write of the request's, so it
// neither keeps its current text nor keeps a check the earlier text did not have. Only a check the
// request itself changes is left as the request has it.
func TestARestoredTextBringsBackItsChecksBesideTheirCurrentContext(t *testing.T) {
	for _, arm := range []struct {
		name   string
		checks []string
		// claim is the claim a-sent-bill-reads-sent must hold after the restore, and act the act
		// that must have written it.
		claim, act string
	}{
		{name: "no check block in the request", claim: "A sent bill reads sent.", act: ActBroughtBack},
		{name: "the current checks as context", claim: "A sent bill reads sent.", act: ActBroughtBack, checks: []string{
			check("a-sent-bill-reads-sent", "a-bill-is-sent", "A mailed bill reads sent.", ` base="2"`),
			check("a-mailed-bill-has-a-stamp", "a-bill-is-sent", "A mailed bill has a stamp.", ` base="1"`),
		}},
		{name: "a check the request changes", claim: "A sent bill reads sent at once.", act: ActApproval, checks: []string{
			check("a-sent-bill-reads-sent", "a-bill-is-sent", "A sent bill reads sent at once.", ` base="2"`),
		}},
	} {
		t.Run(arm.name, func(t *testing.T) {
			database := storetest.Open(t)
			mustApply(t, database, issue(1),
				statement("a-bill-is-sent", "A bill is sent.", ""),
				check("a-sent-bill-reads-sent", "a-bill-is-sent", "A sent bill reads sent.", ""))
			mustApply(t, database, issue(2),
				statement("a-bill-is-sent", "A bill is sent by mail.", ` base="1"`),
				check("a-sent-bill-reads-sent", "a-bill-is-sent", "A mailed bill reads sent.", ` base="1"`),
				check("a-mailed-bill-has-a-stamp", "a-bill-is-sent", "A mailed bill has a stamp.", ""))
			restored := mustApply(t, database, issue(3), append([]string{statement("a-bill-is-sent", "A bill is sent.", ` base="2"`)}, arm.checks...)...)
			texts := current(t, database)
			if got := texts["a-sent-bill-reads-sent"]; !strings.Contains(got, "Claim: "+arm.claim) {
				t.Errorf("a-sent-bill-reads-sent reads %q after the restore, want the claim %q", got, arm.claim)
			}
			if !writes(restored, "a-sent-bill-reads-sent", arm.act, false) {
				t.Errorf("the restore wrote %+v, want a-sent-bill-reads-sent written by the act %s", restored.Written, arm.act)
			}
			if text, held := texts["a-mailed-bill-has-a-stamp"]; held {
				t.Errorf("a-mailed-bill-has-a-stamp reads %q after the restore, want it removed: it did not serve the first text", text)
			}
			if !writes(restored, "a-mailed-bill-has-a-stamp", ActBroughtBack, true) {
				t.Errorf("the restore wrote %+v, want the removal of a-mailed-bill-has-a-stamp brought back with the statement", restored.Written)
			}
		})
	}
}

// writes is whether the act applied wrote id by act, as a removal where removed.
func writes(applied Applied, id, act string, removed bool) bool {
	for _, written := range applied.Written {
		if written.ID == id {
			return written.Act == act && written.Removed == removed
		}
	}
	return false
}

// Two approvals at one base are judged one after the other: the second waits on the repository's
// lock while the first's transaction is open, and once the first commits it reads what the first
// wrote and is refused as stale.
func TestTheSecondApprovalAtOneBaseWaitsAndIsStale(t *testing.T) {
	database := storetest.Open(t)
	mustApply(t, database, issue(1), statement("a-bill-is-sent", "A bill is sent.", ""))
	ctx := context.Background()
	first, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer first.Rollback(ctx)
	if _, err := Apply(ctx, first, Input{Project: "CORE", Repo: "acme/app", Issue: issue(2),
		Request:    request(issue(2), statement("a-bill-is-sent", "A bill is sent today.", ` base="1"`)),
		Provenance: Provenance{Act: ActApproval, Issue: issue(2), Person: person}, Who: person}, RunNow(ctx, first)); err != nil {
		t.Fatalf("approve %s: %v", issue(2), err)
	}
	second := make(chan error, 1)
	go func() {
		_, err := apply(database, issue(3), statement("a-bill-is-sent", "A bill is sent tomorrow.", ` base="1"`))
		second <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := database.Pool.QueryRow(ctx, `select count(*) from pg_locks where locktype = 'advisory' and not granted`).Scan(&waiting); err != nil {
			t.Fatalf("read the waiting locks: %v", err)
		}
		if waiting == 1 {
			break
		}
		select {
		case err := <-second:
			t.Fatalf("the second approval at one base ended while the first was open, with %v: it took no lock", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("the second approval at one base never waited on the repository's lock")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", issue(2), err)
	}
	var refusal *Refusal
	if err := <-second; !errors.As(err, &refusal) || refusal.Code != RefusalStale || len(refusal.Stale) != 1 || refusal.Stale[0].Current != 2 {
		t.Fatalf("the second approval answers %v, want SPEC_REQUEST_STALE naming version 2", err)
	}
	if text := current(t, database)["a-bill-is-sent"]; !strings.Contains(text, "A bill is sent today.") {
		t.Errorf("a-bill-is-sent reads %q, want the first approval's text", text)
	}
}
