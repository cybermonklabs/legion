package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
	"github.com/sjawhar/envoy/internal/dispatch/specstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The import of an approved level (D9) writes each component of the level's table, and each
// statement whose fields a version a person approved gives, as an approved node with that approval
// as provenance; it keeps each other statement as a hold, which one child issue of the level, assigned
// to the level's assignee, repairs. These tests drive POST /api/v1/issues/{key}/spec-import-level
// and read the approved spec through the node route and specstore.

// levelFixture is a level document in the form of level one: statements with ids and no
// attributes, under headings, one under a `### Under <id>` heading, and its components table.
const levelFixture = "# Level one of the fixture\n\n" +
	"## Statements\n\n### Billing\n\n" +
	":::requirement{#orders-are-billed-once}\nEach order is billed once.\n:::\n\n" +
	":::requirement{#a-bill-follows-each-rule}\nEach bill follows the rules of its project.\n:::\n\n" +
	":::requirement{#the-bank-keeps-each-payment}\nThe bank keeps each payment.\n:::\n\n" +
	":::requirement{#a-refund-reads-its-order}\nA refund reads its order.\n:::\n\n" +
	":::requirement{#a-total-is-exact}\nEach total is exact to the cent.\n:::\n\n" +
	":::requirement{#a-bill-has-no-owner}\nA bill has no owner of its own.\n:::\n\n" +
	"### Under a-bill-has-no-owner\n\n" +
	":::requirement{#an-unowned-bill-is-flagged}\nAn unowned bill is flagged.\n:::\n\n" +
	"## Components\n\n" +
	"| Id | Kind | Name | Purpose | Parent or joins |\n| --- | --- | --- | --- | --- |\n" +
	"| billing | component | Billing | It bills each order. | none |\n" +
	"| ledger | component | Ledger | It records each bill. | billing |\n" +
	"| bank | component | Bank | It holds the money. | none |\n" +
	"| billing-bank | interface | Billing to bank | Billing pays into the bank. | billing, bank |\n"

// levelNotesFixture is the notes document of levelFixture: the kind, about and supports of each
// statement, in the four kinds level one writes, and a statement with no kind.
const levelNotesFixture = "# Notes of the fixture\n\n" +
	"### orders-are-billed-once\n\n- Kind: behavior\n- About: billing\n- Supports: none\n\n" +
	"### a-bill-follows-each-rule\n\n- Kind: rule of structure\n- About: billing, ledger\n- Supports: none\n\n" +
	"### the-bank-keeps-each-payment\n\n- Kind: description of structure\n- About: bank, billing-bank\n- Supports: orders-are-billed-once\n\n" +
	"### a-refund-reads-its-order\n\n- Kind: behavior\n- About: billing\n- Supports: none\n\n" +
	"### a-total-is-exact\n\n- Kind: property\n- About: ledger\n- Supports: none\n\n" +
	"### a-bill-has-no-owner\n\n- About: billing\n- Supports: none\n\n" +
	"### an-unowned-bill-is-flagged\n\n- Kind: behavior\n- About: billing\n- Supports: none\n"

// levelFixtureStatements and levelFixtureComponents are the ids levelFixture holds, sorted.
var (
	levelFixtureStatements = []string{"a-bill-follows-each-rule", "a-bill-has-no-owner", "a-refund-reads-its-order", "a-total-is-exact",
		"an-unowned-bill-is-flagged", "orders-are-billed-once", "the-bank-keeps-each-payment"}
	levelFixtureComponents = []string{"bank", "billing", "billing-bank", "ledger"}
)

// levelImport is POST /api/v1/issues/{key}/spec-import-level's answer.
type levelImport struct {
	Issue   string   `json:"issue"`
	Repo    string   `json:"repo"`
	Written []string `json:"written"`
	Held    []struct {
		ID         string         `json:"id"`
		Text       string         `json:"text"`
		Lacks      []string       `json:"lacks"`
		Provenance specProvenance `json:"provenance"`
	} `json:"held"`
	RepairIssue *string `json:"repair_issue"`
}

// lacks is each hold of the import by id, with the fields it lacks.
func (i levelImport) lacks() map[string][]string {
	lacks := map[string][]string{}
	for _, hold := range i.Held {
		lacks[hold.ID] = hold.Lacks
	}
	return lacks
}

// levelRig is a server whose project CORE holds one level issue of specRepo, assigned to bob.
type levelRig struct {
	*specRig
	level model.Issue
}

// newLevelRig creates the level issue whose spec is text, assigned to bob. specRepo is no spec
// source of CORE yet: level one was approved before its repository held The Spec.
func newLevelRig(t *testing.T, title, text string) *levelRig {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	rig := &levelRig{specRig: &specRig{handler: handler, database: database}}
	rig.level = rig.issue(t, title, specRepo, text, "")
	// bob signs in, so the level can be his.
	dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+rig.level.Key, nil, "bob")
	if response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+rig.level.Key, map[string]any{"assignee": "bob"}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("assign the level to bob: status=%d body=%s", response.Code, response.Body.String())
	}
	return rig
}

// upload writes the document name of the level issue, as alice, and answers its artifact.
func (r *levelRig) upload(t *testing.T, name, text string) model.Artifact {
	t.Helper()
	return uploadDocument(t, r.handler, "/api/v1/issues/"+r.level.Key, name, text).Artifact
}

// approveLevel has the person approve the level's spec through a request that names the spec
// alone, as level one's request did, then names specRepo a spec source of CORE, and answers the
// request's id.
func (r *levelRig) approveLevel(t *testing.T) string {
	t.Helper()
	ask := r.request(t, r.level, false)
	r.approve(t, ask)
	r.addSpecSource(t, specRepo)
	return ask
}

// approveDocument has the person approve the document artifactID through an approval request of
// that document alone, and answers the request's id.
func (r *levelRig) approveDocument(t *testing.T, artifactID string) string {
	t.Helper()
	response := sessionRequest(t, r.handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/approval-requests", map[string]any{"actor": sessionActor()})
	if response.Code != http.StatusCreated {
		t.Fatalf("request approval of the document %s: status=%d body=%s", artifactID, response.Code, response.Body.String())
	}
	ask := decodeBody[struct {
		Ask struct {
			ID string `json:"id"`
		} `json:"ask"`
	}](t, response).Ask.ID
	r.approve(t, ask)
	return ask
}

// importRequest is the agent's request to import the level, reading the notes document notes. A
// test that serves several at once builds each here, before it starts them.
func (r *levelRig) importRequest(t *testing.T, notes string) *http.Request {
	t.Helper()
	data, err := json.Marshal(map[string]any{"actor": sessionActor(), "notes": notes})
	if err != nil {
		t.Fatalf("encode the import: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+r.level.Key+"/spec-import-level", bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer agent-token")
	return request
}

// importLevel imports the level with the notes document notes and answers the route's answer.
func (r *levelRig) importLevel(t *testing.T, notes string) *httptest.ResponseRecorder {
	t.Helper()
	response := httptest.NewRecorder()
	r.handler.ServeHTTP(response, r.importRequest(t, notes))
	return response
}

// imported imports the level and fails unless the import answers 200.
func (r *levelRig) imported(t *testing.T, notes string) levelImport {
	t.Helper()
	response := r.importLevel(t, notes)
	if response.Code != http.StatusOK {
		t.Fatalf("import the level of %s: status=%d body=%s", r.level.Key, response.Code, response.Body.String())
	}
	return decodeBody[levelImport](t, response)
}

// children is the key of each child issue of the level, sorted.
func (r *levelRig) children(t *testing.T) []string {
	t.Helper()
	rows, err := r.database.Pool.Query(context.Background(), `select key from issues where parent_key = $1 order by key`, r.level.Key)
	if err != nil {
		t.Fatalf("read the children of %s: %v", r.level.Key, err)
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatalf("read a child of %s: %v", r.level.Key, err)
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the children of %s: %v", r.level.Key, err)
	}
	return keys
}

// requireOneRepairChild fails unless the level has exactly one child issue, assigned to bob, whose
// spec lists each of ids, and the open holds of specRepo are exactly ids, each naming that child as
// its repair; it answers the child's key.
func (r *levelRig) requireOneRepairChild(t *testing.T, ids []string) string {
	t.Helper()
	children := r.children(t)
	if len(children) != 1 {
		t.Fatalf("%s has the child issues %v, want one that repairs its holds", r.level.Key, children)
	}
	read := dispatchRequest(t, r.handler, http.MethodGet, "/api/v1/issues/"+children[0], nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the child issue %s: status=%d body=%s", children[0], read.Code, read.Body.String())
	}
	child := decodeBody[model.Issue](t, read)
	if child.Assignee == nil || *child.Assignee != "bob" {
		t.Errorf("the child issue %s is assigned to %v, want bob, the level's assignee", child.Key, child.Assignee)
	}
	spec := artifactTextOf(t, r.handler, child.PrimaryArtifactID)
	for _, id := range ids {
		if !strings.Contains(spec, "`"+id+"`") {
			t.Errorf("the spec of the child issue %s does not list the hold %s:\n%s", child.Key, id, spec)
		}
	}
	holds, err := specstore.Holds(context.Background(), r.database.Pool, "CORE", specRepo)
	if err != nil {
		t.Fatalf("read the holds of %s: %v", specRepo, err)
	}
	var held []string
	for _, hold := range holds {
		held = append(held, hold.ID)
		if hold.RepairIssue != child.Key {
			t.Errorf("the hold of %s names the repair issue %q, want %s", hold.ID, hold.RepairIssue, child.Key)
		}
	}
	// Holds lists them in the database's collation order; compare them in Go's.
	if slices.Sort(held); !slices.Equal(held, slices.Sorted(slices.Values(ids))) {
		t.Errorf("the open holds of %s are %v, want %v", specRepo, held, slices.Sorted(slices.Values(ids)))
	}
	return child.Key
}

// onRepairIssueFact installs body as a trigger that runs before each insert of a fact naming a
// hold's repair issue.
func onRepairIssueFact(t *testing.T, database *store.Store, setup, body string) {
	t.Helper()
	statements := []string{
		`create function test_repair_issue_fact() returns trigger language plpgsql as $$ ` + body + ` $$`,
		`create trigger test_repair_issue_fact before insert on graph_tuples for each row
			when (new.kind = 'repair_issue') execute function test_repair_issue_fact()`,
	}
	if setup != "" {
		statements = append([]string{setup}, statements...)
	}
	for _, statement := range statements {
		if _, err := database.Pool.Exec(context.Background(), statement); err != nil {
			t.Fatalf("install the trigger on the repair issue facts: %v\n%s", err, statement)
		}
	}
}

// TestImportLevel: the approved level is imported with that approval as provenance, and the notes
// with the person's own approval of them; kind maps from the notes (behavior, both kinds of
// structure, property); a statement with no approved kind, and one under it, are kept as holds with
// their text and provenance, and one child issue of the level's issue, assigned to its assignee,
// lists them; the approval of the change request that supplies the kind writes the node and clears
// its hold, and the import run again writes nothing and opens no second issue (D9).
func TestImportLevel(t *testing.T) {
	rig := newLevelRig(t, "Level one of the fixture", levelFixture)
	notes := rig.upload(t, "notes.md", levelNotesFixture)
	ask := rig.approveLevel(t)
	notesAsk := rig.approveDocument(t, notes.ID)

	imported := rig.imported(t, "notes.md")
	wantWritten := []string{"a-bill-follows-each-rule", "a-refund-reads-its-order", "a-total-is-exact", "bank", "billing", "billing-bank", "ledger", "orders-are-billed-once", "the-bank-keeps-each-payment"}
	if written := slices.Sorted(slices.Values(imported.Written)); !slices.Equal(written, wantWritten) {
		t.Errorf("the import writes %v, want %v", written, wantWritten)
	}
	for id, kind := range map[string]string{
		"orders-are-billed-once": `kind="behavior"`, "a-bill-follows-each-rule": `kind="structure"`,
		"the-bank-keeps-each-payment": `kind="structure"`, "a-total-is-exact": `kind="property"`,
		"billing-bank": `kind="interface"`,
	} {
		node := rig.requireVersions(t, specRepo, id, kind)
		provenance := node.Versions[0].Provenance
		if provenance.Act != "import" || provenance.Issue != rig.level.Key || provenance.Ask == nil || *provenance.Ask != ask || provenance.Review == nil {
			t.Errorf("%s is written with the provenance %+v, want the import of %s under the approval answering %s", id, provenance, rig.level.Key, ask)
		}
		if read := provenance.Notes; read == nil || read.ArtifactID != notes.ID || read.Version != 1 || read.Digest == "" ||
			read.Ask == nil || *read.Ask != notesAsk || read.Review == nil {
			t.Errorf("%s names the notes %+v, want version 1 of %s under the person's approval answering %s", id, read, notes.ID, notesAsk)
		}
	}
	rig.requireVersions(t, specRepo, "the-bank-keeps-each-payment", `serves="orders-are-billed-once"`)
	rig.requireVersions(t, specRepo, "ledger", `parent="billing"`)
	for _, hold := range imported.Held {
		if hold.Text == "" || hold.Provenance.Issue != rig.level.Key || hold.Provenance.Ask == nil || *hold.Provenance.Ask != ask {
			t.Errorf("the hold of %s keeps %q and %+v, want its approved text and provenance", hold.ID, hold.Text, hold.Provenance)
		}
	}
	if held := imported.lacks(); !reflect.DeepEqual(held, map[string][]string{"a-bill-has-no-owner": {"kind"}, "an-unowned-bill-is-flagged": {"parent"}}) {
		t.Errorf("the import holds %v, want a-bill-has-no-owner lacking its kind and an-unowned-bill-is-flagged its parent", held)
	}
	rig.requireAbsent(t, specRepo, "a-bill-has-no-owner")
	rig.requireAbsent(t, specRepo, "an-unowned-bill-is-flagged")
	if imported.RepairIssue == nil {
		t.Fatalf("the import names no child issue for its holds: %+v", imported)
	}
	if child := rig.requireOneRepairChild(t, []string{"a-bill-has-no-owner", "an-unowned-bill-is-flagged"}); child != *imported.RepairIssue {
		t.Errorf("the import names the child issue %s, and the holds name %s", *imported.RepairIssue, child)
	}

	// The owner supplies the kind in a change request, and the person approves it.
	supplied := rig.issue(t, "The kind of a held statement", specRepo,
		":::requirement{#a-bill-has-no-owner kind=\"behavior\" about=\"billing\"}\nA bill has no owner of its own.\n:::\n\n"+
			":::requirement{#an-unowned-bill-is-flagged kind=\"behavior\" about=\"billing\" under=\"a-bill-has-no-owner\"}\nAn unowned bill is flagged.\n:::\n", "")
	rig.approve(t, rig.request(t, supplied, false))
	rig.requireVersions(t, specRepo, "a-bill-has-no-owner", "A bill has no owner of its own.")
	rig.requireVersions(t, specRepo, "an-unowned-bill-is-flagged", `parent="a-bill-has-no-owner"`)

	again := rig.imported(t, "notes.md")
	if len(again.Written) != 0 || len(again.Held) != 0 {
		t.Errorf("the import run again writes %v and holds %+v, want nothing: the approval cleared both holds", again.Written, again.Held)
	}
	if children := rig.children(t); len(children) != 1 {
		t.Errorf("%s has the child issues %v, want the one the first import opened", rig.level.Key, children)
	}
}

// TestImportLevelTakesNoFieldNoApprovalNamed: the notes give a statement's fields only at a version
// a person approved - the one the level's approval request names, or the one the person's own
// approval of the notes names - so notes that no approval names, and another notes document written
// before the level's approval, install no field: each statement is held for the kind it lacks, and
// none is written (D9). An import run again with the notes the person approved writes what they
// resolve and clears those holds.
func TestImportLevelTakesNoFieldNoApprovalNamed(t *testing.T) {
	requireEveryStatementHeld := func(t *testing.T, rig *levelRig, imported levelImport) string {
		t.Helper()
		if written := slices.Sorted(slices.Values(imported.Written)); !slices.Equal(written, levelFixtureComponents) {
			t.Errorf("the import writes %v, want the components %v alone: no approved version gives a statement its kind", written, levelFixtureComponents)
		}
		want := map[string][]string{}
		for _, id := range levelFixtureStatements {
			want[id] = []string{"kind"}
			rig.requireAbsent(t, specRepo, id)
		}
		if held := imported.lacks(); !reflect.DeepEqual(held, want) {
			t.Errorf("the import holds %v, want each statement lacking its kind", held)
		}
		if imported.RepairIssue == nil {
			t.Fatalf("the import names no child issue for its holds: %+v", imported)
		}
		child := rig.requireOneRepairChild(t, levelFixtureStatements)
		if child != *imported.RepairIssue {
			t.Errorf("the import names the child issue %s, and the holds name %s", *imported.RepairIssue, child)
		}
		return child
	}

	t.Run("notes that no approval names", func(t *testing.T) {
		rig := newLevelRig(t, "Level one of the fixture", levelFixture)
		rig.upload(t, "notes.md", levelNotesFixture)
		rig.approveLevel(t)
		requireEveryStatementHeld(t, rig, rig.imported(t, "notes.md"))
	})

	t.Run("another notes document written before the approval", func(t *testing.T) {
		rig := newLevelRig(t, "Level one of the fixture", levelFixture)
		notes := rig.upload(t, "notes.md", levelNotesFixture)
		notesAsk := rig.approveDocument(t, notes.ID)
		rig.upload(t, "other-notes.md", strings.Replace(levelNotesFixture, "# Notes of the fixture", "# Other notes of the fixture", 1))
		rig.approveLevel(t)
		child := requireEveryStatementHeld(t, rig, rig.imported(t, "other-notes.md"))

		// The notes the person approved give five statements their fields: the import run with them
		// writes those five and clears their holds, and the two others stay held with their child.
		approved := rig.imported(t, "notes.md")
		resolved := []string{"a-bill-follows-each-rule", "a-refund-reads-its-order", "a-total-is-exact", "orders-are-billed-once", "the-bank-keeps-each-payment"}
		if written := slices.Sorted(slices.Values(approved.Written)); !slices.Equal(written, resolved) {
			t.Errorf("the import with the approved notes writes %v, want %v", written, resolved)
		}
		if held := approved.lacks(); !reflect.DeepEqual(held, map[string][]string{"a-bill-has-no-owner": {"kind"}, "an-unowned-bill-is-flagged": {"kind"}}) {
			t.Errorf("the import with the approved notes holds %v, want the two statements it could not resolve, as the first import held them", held)
		}
		if approved.RepairIssue == nil || *approved.RepairIssue != child {
			t.Errorf("the import with the approved notes names the child issue %v, want %s", approved.RepairIssue, child)
		}
		rig.requireOneRepairChild(t, []string{"a-bill-has-no-owner", "an-unowned-bill-is-flagged"})
		node := rig.requireVersions(t, specRepo, "orders-are-billed-once", `kind="behavior"`)
		if read := node.Versions[0].Provenance.Notes; read == nil || read.ArtifactID != notes.ID || read.Ask == nil || *read.Ask != notesAsk {
			t.Errorf("orders-are-billed-once names the notes %+v, want %s under the person's approval answering %s", read, notes.ID, notesAsk)
		}
	})
}

// TestImportLevelOpensOneRepairChild: the repair child and the facts that name it on each hold are
// written in one transaction, so two imports at once, and an import run again after a failure
// between the child's create and those facts, leave exactly one child holding every hold (D9).
func TestImportLevelOpensOneRepairChild(t *testing.T) {
	t.Run("two imports at once", func(t *testing.T) {
		rig := newLevelRig(t, "Level one of the fixture", levelFixture)
		rig.upload(t, "notes.md", levelNotesFixture)
		rig.approveLevel(t)
		// The first fact that names a repair issue in a transaction waits two seconds, so each import
		// reads the holds before either has named a repair issue on them.
		onRepairIssueFact(t, rig.database, "", `begin
			if current_setting('test.repair_issue_waited', true) is distinct from 'yes' then
				perform set_config('test.repair_issue_waited', 'yes', true);
				perform pg_sleep(2);
			end if;
			return new;
		end`)
		requests := []*http.Request{rig.importRequest(t, "notes.md"), rig.importRequest(t, "notes.md")}
		responses := make([]*httptest.ResponseRecorder, len(requests))
		var wait sync.WaitGroup
		for index, request := range requests {
			wait.Add(1)
			go func() {
				defer wait.Done()
				responses[index] = httptest.NewRecorder()
				rig.handler.ServeHTTP(responses[index], request)
			}()
		}
		wait.Wait()
		child := rig.requireOneRepairChild(t, levelFixtureStatements)
		for index, response := range responses {
			if response.Code != http.StatusOK {
				t.Errorf("import %d: status=%d body=%s, want 200", index+1, response.Code, response.Body.String())
				continue
			}
			if named := decodeBody[levelImport](t, response).RepairIssue; named == nil || *named != child {
				t.Errorf("import %d names the child issue %v, want %s", index+1, named, child)
			}
		}
	})

	t.Run("a failure between the create and the link", func(t *testing.T) {
		rig := newLevelRig(t, "Level one of the fixture", levelFixture)
		rig.upload(t, "notes.md", levelNotesFixture)
		rig.approveLevel(t)
		// The first fact that names a repair issue fails; every later one is written.
		onRepairIssueFact(t, rig.database, `create sequence test_repair_issue_facts`, `begin
			if nextval('test_repair_issue_facts') = 1 then
				raise exception 'the test fails the first fact that names a repair issue';
			end if;
			return new;
		end`)
		if failed := rig.importLevel(t, "notes.md"); failed.Code == http.StatusOK {
			t.Fatalf("the import whose repair issue could not be named answers 200: %s", failed.Body.String())
		}
		again := rig.imported(t, "notes.md")
		child := rig.requireOneRepairChild(t, levelFixtureStatements)
		if again.RepairIssue == nil || *again.RepairIssue != child {
			t.Errorf("the import run again names the child issue %v, want %s", again.RepairIssue, child)
		}
	})
}

// holdTheClearOfAHold is a trigger that holds each statement stating a hold of the approved spec
// gone - the node write's update of the hold's row, which an approval of a change request runs in
// its write phase under the lock of the approved spec (specstore.Apply) - on the advisory key
// clearOfAHoldKey, which a transaction of the test takes first.
const holdTheClearOfAHold = `
	create function test_hold_the_clear_of_a_hold() returns trigger language plpgsql as $$
	begin
		perform pg_advisory_xact_lock(hashtext('test: the clear of a hold'));
		return new;
	end $$;
	create trigger test_hold_the_clear_of_a_hold before update on graph_nodes for each row
		when (new.kind = 'spec_import_hold' and new.state = 'gone')
		execute function test_hold_the_clear_of_a_hold();`

// clearOfAHoldKey is the statement that takes the key holdTheClearOfAHold's trigger waits on.
const clearOfAHoldKey = `select pg_advisory_xact_lock(hashtext('test: the clear of a hold'))`

// TestImportLevelRepairChildListsTheHoldsOpenWhenItIsCreated: an approval of a change request that
// clears a hold while the import opens the repair child of its holds leaves exactly one child,
// listing exactly the holds still open when it is created (D9). The create that opens the child
// reads the holds again under the lock of the approved spec, which the approval holds until it
// commits, so it waits for the approval and finds that hold cleared.
//
// A transaction of the test holds the project's owner lock, so the import's create waits there,
// after the import's own transaction committed the holds a-bill-has-no-owner and
// an-unowned-bill-is-flagged. The approval of the change request that supplies a-bill-has-no-owner's
// kind then takes the lock of the approved spec, and a second transaction of the test holds it
// inside its write phase, at the statement that states that hold gone (holdTheClearOfAHold). The
// first transaction lets the create go on; once the import has answered, or two backends wait
// behind the second transaction, the second lets the approval go on and commit.
func TestImportLevelRepairChildListsTheHoldsOpenWhenItIsCreated(t *testing.T) {
	ctx := context.Background()
	rig := newLevelRig(t, "Level one of the fixture", levelFixture)
	notes := rig.upload(t, "notes.md", levelNotesFixture)
	rig.approveLevel(t)
	rig.approveDocument(t, notes.ID)
	// The change request and its approval request each take the project's owner lock, so both are
	// written before the race.
	supplied := rig.issue(t, "The kind of a held statement", specRepo,
		":::requirement{#a-bill-has-no-owner kind=\"behavior\" about=\"billing\"}\nA bill has no owner of its own.\n:::\n", "")
	rig.request(t, supplied, false)
	if _, err := rig.database.Pool.Exec(ctx, holdTheClearOfAHold); err != nil {
		t.Fatalf("install the trigger that holds the clear of a hold: %v", err)
	}
	importRequest := rig.importRequest(t, "notes.md")
	approvalRequest := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+supplied.PrimaryArtifactID+"/reviews",
		strings.NewReader(`{"state":"approved"}`))
	approvalRequest.Header.Set("Content-Type", "application/json")
	approvalRequest.Header.Set("X-Dispatch-User", "alice")

	// The two transactions the race is held by, each rolled back at the end where the test has not
	// let go of it already.
	owner, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the project's owner lock: %v", err)
	}
	t.Cleanup(func() { _ = owner.Rollback(context.Background()) })
	clearing, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that holds the clear of a hold: %v", err)
	}
	t.Cleanup(func() { _ = clearing.Rollback(context.Background()) })
	// The backends waiting behind each transaction, directly or behind an earlier waiter.
	behindOwner := func() int { return storetest.WaitForLockWaiters(t, owner, 0, 0) }
	behindClearing := func() int { return storetest.WaitForLockWaiters(t, clearing, 0, 0) }
	poll := func(what string, until func() bool) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for !until() {
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within 30s", what)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	serve := func(request *http.Request) <-chan *httptest.ResponseRecorder {
		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			response := httptest.NewRecorder()
			rig.handler.ServeHTTP(response, request)
			answered <- response
		}()
		return answered
	}
	answer := func(what string, answered <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
		t.Helper()
		select {
		case response := <-answered:
			return response
		case <-time.After(30 * time.Second):
			t.Fatalf("%s answered nothing within 30s", what)
			return nil
		}
	}

	if err := store.LockOwner(ctx, owner, store.ProjectOwner("CORE")); err != nil {
		t.Fatalf("hold the owner lock of CORE: %v", err)
	}
	imports := serve(importRequest)
	var imported *httptest.ResponseRecorder
	importAnswered := func() bool {
		if imported == nil {
			select {
			case imported = <-imports:
			default:
			}
		}
		return imported != nil
	}
	poll("the import's create queues behind the project's owner lock", func() bool {
		if importAnswered() {
			t.Fatalf("the import answered status=%d body=%s before its create queued behind the project's owner lock", imported.Code, imported.Body.String())
		}
		return behindOwner() >= 1
	})

	if _, err := clearing.Exec(ctx, clearOfAHoldKey); err != nil {
		t.Fatalf("take the key the clear of a hold waits on: %v", err)
	}
	approvals := serve(approvalRequest)
	poll("the approval queues behind the clear of its hold", func() bool {
		select {
		case response := <-approvals:
			t.Fatalf("the approval answered status=%d body=%s before it queued behind the clear of its hold", response.Code, response.Body.String())
		default:
		}
		return behindClearing() >= 1
	})
	if err := owner.Rollback(ctx); err != nil {
		t.Fatalf("let go of the owner lock of CORE: %v", err)
	}
	poll("the import answers, or its create queues behind the approval", func() bool {
		return importAnswered() || behindClearing() >= 2
	})
	if err := clearing.Commit(ctx); err != nil {
		t.Fatalf("let go of the clear of a hold: %v", err)
	}
	if approved := answer("the approval", approvals); approved.Code != http.StatusCreated {
		t.Fatalf("the approval of %s: status=%d body=%s, want 201", supplied.Key, approved.Code, approved.Body.String())
	}
	if imported == nil {
		imported = answer("the import", imports)
	}
	if imported.Code != http.StatusOK {
		t.Fatalf("the import of %s: status=%d body=%s, want 200", rig.level.Key, imported.Code, imported.Body.String())
	}

	rig.requireVersions(t, specRepo, "a-bill-has-no-owner", "A bill has no owner of its own.")
	child := rig.requireOneRepairChild(t, []string{"an-unowned-bill-is-flagged"})
	if named := decodeBody[levelImport](t, imported).RepairIssue; named == nil || *named != child {
		t.Errorf("the import names the child issue %v, want %s", named, child)
	}
	read := dispatchRequest(t, rig.handler, http.MethodGet, "/api/v1/issues/"+child, nil, "alice")
	if read.Code != http.StatusOK {
		t.Fatalf("read the child issue %s: status=%d body=%s", child, read.Code, read.Body.String())
	}
	if spec := artifactTextOf(t, rig.handler, decodeBody[model.Issue](t, read).PrimaryArtifactID); strings.Contains(spec, "`a-bill-has-no-owner`") {
		t.Errorf("the spec of the child issue %s lists the hold of a-bill-has-no-owner, which the approval cleared before the child was created:\n%s", child, spec)
	}
}

// The pinned fixture of level one (testdata/level-one): level.md is version 10 of the level
// document, the version the person approved, byte for byte; notes.md holds, of each statement of
// its notes document, the heading and the Kind, About and Supports lines.
const (
	levelOneSHA256      = "705243dabb6ed9b5e8ed95a94bff32167cb10ceb524a18e29553e6bdd082c6e7"
	levelOneNotesSHA256 = "ceefd712d832cf19a42fd39009c7f3c22b9dd2a3b46600a5d02a5874c7d1b1a6"
)

// levelOneKinds is D9's map from a kind the notes write to a statement's kind.
var levelOneKinds = map[string]specgraph.Kind{
	"behavior":                 specgraph.KindBehavior,
	"rule of structure":        specgraph.KindStructure,
	"description of structure": specgraph.KindStructure,
	"property":                 specgraph.KindProperty,
}

// levelOneStatement is one requirement block of the level document: its id and its text.
var levelOneStatement = regexp.MustCompile(`(?m)^:::requirement\{#([-0-9a-z]+)\}\n((?s:.*?))\n:::$`)

// levelOneNote is what the notes give one statement.
type levelOneNote struct {
	kind            string
	about, supports []string
}

// levelOneComponent is one row of the level's components table.
type levelOneComponent struct {
	kind, name, purpose string
	related             []string
}

// levelOne is the pinned fixture as this test reads it, apart from the import's own reader: each
// statement's id in order and its text, each note, and each component.
type levelOne struct {
	level, notes string
	statements   []string
	texts        map[string]string
	noted        map[string]levelOneNote
	components   map[string]levelOneComponent
}

// fixtureIDs is the ids a cell or a line names, separated by commas; "none" names none.
func fixtureIDs(list string) []string {
	var ids []string
	for _, id := range strings.Split(list, ",") {
		if id = strings.TrimSpace(id); id != "" && id != "none" {
			ids = append(ids, id)
		}
	}
	return ids
}

// readLevelOne reads the pinned fixture and fails unless its files are the pinned bytes and hold
// the 59 statements, the four kinds and the 43 components of level one.
func readLevelOne(t *testing.T) levelOne {
	t.Helper()
	read := func(name, digest string) string {
		t.Helper()
		data, err := os.ReadFile("testdata/level-one/" + name)
		if err != nil {
			t.Fatalf("read the fixture %s: %v", name, err)
		}
		if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != digest {
			t.Fatalf("the fixture %s holds sha256 %x, want the pinned %s", name, sum, digest)
		}
		return string(data)
	}
	fixture := levelOne{level: read("level.md", levelOneSHA256), notes: read("notes.md", levelOneNotesSHA256),
		texts: map[string]string{}, noted: map[string]levelOneNote{}, components: map[string]levelOneComponent{}}
	for _, match := range levelOneStatement.FindAllStringSubmatch(fixture.level, -1) {
		fixture.statements = append(fixture.statements, match[1])
		fixture.texts[match[1]] = strings.TrimSpace(match[2])
	}
	current := ""
	for _, line := range strings.Split(fixture.notes, "\n") {
		note := fixture.noted[current]
		switch {
		case strings.HasPrefix(line, "### "):
			current = strings.TrimPrefix(line, "### ")
			fixture.noted[current] = levelOneNote{}
			continue
		case strings.HasPrefix(line, "- Kind: "):
			note.kind = strings.TrimPrefix(line, "- Kind: ")
		case strings.HasPrefix(line, "- About: "):
			note.about = fixtureIDs(strings.TrimPrefix(line, "- About: "))
		case strings.HasPrefix(line, "- Supports: "):
			note.supports = fixtureIDs(strings.TrimPrefix(line, "- Supports: "))
		default:
			continue
		}
		fixture.noted[current] = note
	}
	_, table, found := strings.Cut(fixture.level, "\n## Components\n")
	if !found {
		t.Fatal("the level fixture holds no components table")
	}
	interfaces := 0
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for index := range cells {
			cells[index] = strings.TrimSpace(cells[index])
		}
		if len(cells) != 5 || cells[0] == "Id" || strings.HasPrefix(cells[0], "---") {
			continue
		}
		fixture.components[cells[0]] = levelOneComponent{kind: cells[1], name: cells[2], purpose: cells[3], related: fixtureIDs(cells[4])}
		if cells[1] == "interface" {
			interfaces++
		}
	}

	if len(fixture.statements) != 59 || len(fixture.texts) != 59 {
		t.Fatalf("the level fixture holds %d statements, %d of them distinct, want the 59 of level one", len(fixture.statements), len(fixture.texts))
	}
	if len(fixture.components) != 43 || interfaces != 12 {
		t.Fatalf("the level fixture holds %d components, %d of them interfaces, want 43 and 12", len(fixture.components), interfaces)
	}
	kinds := map[string]int{}
	for _, id := range fixture.statements {
		note, noted := fixture.noted[id]
		if !noted {
			t.Fatalf("the notes fixture holds no entry for %s", id)
		}
		kinds[note.kind]++
		for _, about := range note.about {
			if _, held := fixture.components[about]; !held {
				t.Errorf("the notes of %s name %s as about, and the table holds no such component", id, about)
			}
		}
		for _, supports := range note.supports {
			if _, held := fixture.texts[supports]; !held {
				t.Errorf("the notes of %s name %s as supported, and the level holds no such statement", id, supports)
			}
		}
	}
	if len(fixture.noted) != 59 {
		t.Errorf("the notes fixture holds %d entries, want one for each of the 59 statements", len(fixture.noted))
	}
	if want := map[string]int{"behavior": 49, "rule of structure": 8, "description of structure": 1, "property": 1}; !reflect.DeepEqual(kinds, want) {
		t.Fatalf("the notes fixture holds the kinds %v, want %v", kinds, want)
	}
	return fixture
}

// TestImportLevelOne: the import of the pinned level one writes all 43 components and interfaces
// of its table, each with its parent or the components it joins; with the notes the person approved
// it writes each of the 59 statements with its approved text, its kind by D9's map (49 behavior, 9
// structure, 1 property) and its about and supports; with notes no approval names it holds each of
// the 59 for its kind, in one child issue. Either way, the ids it read are the ids it wrote and the
// ids it held, and no other (D9).
func TestImportLevelOne(t *testing.T) {
	fixture := readLevelOne(t)
	components := slices.Sorted(func(yield func(string) bool) {
		for id := range fixture.components {
			if !yield(id) {
				return
			}
		}
	})
	statements := slices.Sorted(slices.Values(fixture.statements))
	input := slices.Sorted(slices.Values(slices.Concat(statements, components)))
	requireInputIsWrittenAndHeld := func(t *testing.T, imported levelImport) {
		t.Helper()
		out := slices.Clone(imported.Written)
		for _, hold := range imported.Held {
			out = append(out, hold.ID)
		}
		if slices.Sort(out); !slices.Equal(out, input) {
			t.Errorf("the import wrote %d ids and held %d, %v; want the %d ids of the level, each once", len(imported.Written), len(imported.Held), out, len(input))
		}
	}
	requireComponents := func(t *testing.T, state specstore.State) {
		t.Helper()
		for _, id := range components {
			want := fixture.components[id]
			history, held := state.Nodes[id]
			if !held || len(history.Versions) != 1 {
				t.Errorf("the approved spec holds %s at %+v, want one version", id, history)
				continue
			}
			version := history.Versions[0]
			node, err := specstore.DecodeNode(version.Kind, version.Node)
			if err != nil {
				t.Fatalf("decode %s: %v", id, err)
			}
			component, ok := node.(specgraph.Component)
			if !ok || version.Kind != want.kind || component.Name != want.name || component.Purpose != want.purpose {
				t.Errorf("the approved spec holds %s as %s %+v, want the %s %q: %q", id, version.Kind, node, want.kind, want.name, want.purpose)
				continue
			}
			var parent string
			var joins []string
			if want.kind == "interface" {
				joins = slices.Sorted(slices.Values(want.related))
			} else if len(want.related) > 0 {
				parent = want.related[0]
			}
			if component.Parent != parent || !slices.Equal(component.Joins, joins) {
				t.Errorf("%s stands under %q and joins %v, want %q and %v", id, component.Parent, component.Joins, parent, joins)
			}
		}
	}
	readState := func(t *testing.T, rig *levelRig) specstore.State {
		t.Helper()
		state, err := specstore.Read(context.Background(), rig.database.Pool, "CORE", specRepo)
		if err != nil {
			t.Fatalf("read the approved spec of %s: %v", specRepo, err)
		}
		return state
	}

	t.Run("notes the person approved", func(t *testing.T) {
		rig := newLevelRig(t, "Level one", fixture.level)
		notes := rig.upload(t, "notes.md", fixture.notes)
		ask := rig.approveLevel(t)
		notesAsk := rig.approveDocument(t, notes.ID)
		imported := rig.imported(t, "notes.md")
		requireInputIsWrittenAndHeld(t, imported)
		if len(imported.Held) != 0 || imported.RepairIssue != nil {
			t.Errorf("the import holds %v and names the child issue %v, want no hold", imported.lacks(), imported.RepairIssue)
		}
		state := readState(t, rig)
		if len(state.Nodes) != len(input) {
			t.Errorf("the approved spec holds %d nodes, want the %d of the level", len(state.Nodes), len(input))
		}
		requireComponents(t, state)
		kinds := map[specgraph.Kind]int{}
		for _, id := range statements {
			note := fixture.noted[id]
			history, held := state.Nodes[id]
			if !held || len(history.Versions) != 1 {
				t.Errorf("the approved spec holds %s at %+v, want one version", id, history)
				continue
			}
			version := history.Versions[0]
			node, err := specstore.DecodeNode(version.Kind, version.Node)
			if err != nil {
				t.Fatalf("decode %s: %v", id, err)
			}
			statement, ok := node.(specgraph.Statement)
			if !ok {
				t.Errorf("the approved spec holds %s as %s %+v, want a statement", id, version.Kind, node)
				continue
			}
			kinds[statement.Kind]++
			switch {
			case statement.Kind != levelOneKinds[note.kind]:
				t.Errorf("%s holds the kind %q, want %q for the notes' %q", id, statement.Kind, levelOneKinds[note.kind], note.kind)
			case statement.Text != fixture.texts[id]:
				t.Errorf("%s holds the text %q, want the level's %q", id, statement.Text, fixture.texts[id])
			case !slices.Equal(statement.About, slices.Sorted(slices.Values(note.about))):
				t.Errorf("%s is about %v, want the notes' %v", id, statement.About, note.about)
			case !slices.Equal(statement.Serves, slices.Sorted(slices.Values(note.supports))):
				t.Errorf("%s supports %v, want the notes' %v", id, statement.Serves, note.supports)
			case statement.Parent != "":
				t.Errorf("%s stands under %q, want the top: level one holds no heading `### Under`", id, statement.Parent)
			}
			// The provenance as its stored value spells it, read into the route's form.
			var provenance specProvenance
			if data, err := json.Marshal(version.Provenance); err != nil || json.Unmarshal(data, &provenance) != nil {
				t.Fatalf("read the provenance of %s: %v", id, err)
			}
			if provenance.Act != specstore.ActImport || provenance.Issue != rig.level.Key || provenance.Ask == nil || *provenance.Ask != ask ||
				provenance.Notes == nil || provenance.Notes.ArtifactID != notes.ID || provenance.Notes.Ask == nil || *provenance.Notes.Ask != notesAsk ||
				provenance.Notes.Review == nil {
				t.Errorf("%s is written with the provenance %+v, want the import of %s under the approvals answering %s and %s", id, provenance, rig.level.Key, ask, notesAsk)
			}
		}
		if want := map[specgraph.Kind]int{specgraph.KindBehavior: 49, specgraph.KindStructure: 9, specgraph.KindProperty: 1}; !reflect.DeepEqual(kinds, want) {
			t.Errorf("the approved spec holds the kinds %v, want %v", kinds, want)
		}
		if holds, err := specstore.Holds(context.Background(), rig.database.Pool, "CORE", specRepo); err != nil || len(holds) != 0 {
			t.Errorf("the approved spec of %s holds the holds %+v (%v), want none", specRepo, holds, err)
		}
		if children := rig.children(t); len(children) != 0 {
			t.Errorf("%s has the child issues %v, want none: nothing is held", rig.level.Key, children)
		}
	})

	t.Run("notes that no approval names", func(t *testing.T) {
		rig := newLevelRig(t, "Level one", fixture.level)
		rig.upload(t, "notes.md", fixture.notes)
		ask := rig.approveLevel(t)
		imported := rig.imported(t, "notes.md")
		requireInputIsWrittenAndHeld(t, imported)
		if written := slices.Sorted(slices.Values(imported.Written)); !slices.Equal(written, components) {
			t.Errorf("the import writes %v, want the 43 components alone", written)
		}
		state := readState(t, rig)
		if len(state.Nodes) != len(components) {
			t.Errorf("the approved spec holds %d nodes, want the %d components", len(state.Nodes), len(components))
		}
		requireComponents(t, state)
		for _, hold := range imported.Held {
			switch {
			case !slices.Equal(hold.Lacks, []string{"kind"}):
				t.Errorf("the hold of %s lacks %v, want its kind alone", hold.ID, hold.Lacks)
			case hold.Text != fixture.texts[hold.ID]:
				t.Errorf("the hold of %s keeps the text %q, want the level's %q", hold.ID, hold.Text, fixture.texts[hold.ID])
			case hold.Provenance.Act != specstore.ActImport || hold.Provenance.Issue != rig.level.Key || hold.Provenance.Ask == nil || *hold.Provenance.Ask != ask || hold.Provenance.Notes != nil:
				t.Errorf("the hold of %s keeps the provenance %+v, want the import of %s under the approval answering %s, and no notes", hold.ID, hold.Provenance, rig.level.Key, ask)
			}
		}
		if imported.RepairIssue == nil {
			t.Fatalf("the import names no child issue for its %d holds", len(imported.Held))
		}
		if child := rig.requireOneRepairChild(t, statements); child != *imported.RepairIssue {
			t.Errorf("the import names the child issue %s, and the holds name %s", *imported.RepairIssue, child)
		}
	})
}
