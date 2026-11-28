package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/specstore"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// describedConflict is a 409 code as a description names it: `409 CODE`.
var describedConflict = regexp.MustCompile(`\b409 ([A-Z][A-Z0-9_]*[A-Z0-9])\b`)

// describedConflicts is each 409 code description names.
func describedConflicts(description string) map[string]bool {
	codes := map[string]bool{}
	for _, match := range describedConflict.FindAllStringSubmatch(description, -1) {
		codes[match[1]] = true
	}
	return codes
}

// TestTheSpecImportRouteDescriptionNamesEachRefusal: the description of POST
// /api/v1/issues/{key}/spec-import-level, and that of the generated dispatch_spec_import_level
// tool that calls it, each name every 409 refusal the route answers and no 409 it does not. One
// case drives each refusal through the real route; the test collects the 409 codes the route
// answered and fails, naming the code, when the route's answers, the route's description and the
// tool's description do not name the same codes. It is a test of its own for the reason
// TestTheInboxRouteDescriptionStatesTheRule gives: nothing else fails when a description outlives
// the code it describes.
func TestTheSpecImportRouteDescriptionNamesEachRefusal(t *testing.T) {
	const header = "| Id | Kind | Name | Purpose | Parent or joins |\n| --- | --- | --- | --- | --- |\n"
	const billing = "| billing | component | Billing | It bills each order. | none |\n"
	note := func(id, supports string) string {
		return "### " + id + "\n\n- Kind: behavior\n- About: billing\n- Supports: " + supports + "\n\n"
	}
	// approved is a level rig whose level and notes the person approved.
	approved := func(t *testing.T, level, notes string) *levelRig {
		t.Helper()
		rig := newLevelRig(t, "A level", level)
		document := rig.upload(t, "notes.md", "# Notes\n\n"+notes)
		rig.approveLevel(t)
		rig.approveDocument(t, document.ID)
		return rig
	}
	answered := map[string][]string{}
	for _, c := range []struct {
		name   string
		refuse func(t *testing.T) *httptest.ResponseRecorder
	}{
		{name: "the repository is no spec source", refuse: func(t *testing.T) *httptest.ResponseRecorder {
			rig := newLevelRig(t, "Level one of the fixture", levelFixture)
			rig.upload(t, "notes.md", levelNotesFixture)
			return rig.importLevel(t, "notes.md")
		}},
		{name: "no person approved the level", refuse: func(t *testing.T) *httptest.ResponseRecorder {
			rig := newLevelRig(t, "Level one of the fixture", levelFixture)
			rig.upload(t, "notes.md", levelNotesFixture)
			rig.addSpecSource(t, specRepo)
			return rig.importLevel(t, "notes.md")
		}},
		{name: "a Components row that reads as no component", refuse: func(t *testing.T) *httptest.ResponseRecorder {
			return approved(t, "# Level\n\n## Components\n\n| Id | Kind | Name | Purpose |\n| --- | --- | --- | --- |\n| billing | component | Billing | It bills each order. |\n", "").
				importLevel(t, "notes.md")
		}},
		{name: "one id on two nodes of the level", refuse: func(t *testing.T) *httptest.ResponseRecorder {
			return approved(t, "# Level\n\n## Components\n\n"+header+billing+"| billing | component | Billing again | It bills each refund. | none |\n", "").
				importLevel(t, "notes.md")
		}},
		{name: "a node that breaks a rule of its kind", refuse: func(t *testing.T) *httptest.ResponseRecorder {
			return approved(t, "# Level\n\n## Components\n\n"+header+billing+"| billing-bank | interface | Billing to bank | Billing pays into the bank. | billing |\n", "").
				importLevel(t, "notes.md")
		}},
		{name: "a statement under a parent the approved spec holds as a component", refuse: func(t *testing.T) *httptest.ResponseRecorder {
			rig := approved(t, "# Level\n\n## Statements\n\n:::requirement{#a-bill-is-owned}\nA bill is owned.\n:::\n\n"+
				"### Under a-bill-is-owned\n\n:::requirement{#a-bill-names-its-owner}\nA bill names its owner.\n:::\n\n## Components\n\n"+header+billing,
				note("a-bill-is-owned", "none")+note("a-bill-names-its-owner", "none"))
			owned := rig.issue(t, "A component of the id a-bill-is-owned", specRepo, ":::component{#a-bill-is-owned name=\"Owned bills\"}\nIt keeps each owned bill.\n:::\n", "")
			rig.approve(t, rig.request(t, owned, false))
			return rig.importLevel(t, "notes.md")
		}},
		{name: "a loop of supports", refuse: func(t *testing.T) *httptest.ResponseRecorder {
			return approved(t, "# Level\n\n## Statements\n\n:::requirement{#a-bill-is-sent}\nA bill is sent.\n:::\n\n"+
				":::requirement{#a-bill-is-kept}\nA bill is kept.\n:::\n\n## Components\n\n"+header+billing,
				note("a-bill-is-sent", "a-bill-is-kept")+note("a-bill-is-kept", "a-bill-is-sent")).importLevel(t, "notes.md")
		}},
		{name: "other writers name a repair issue on its holds at each open", refuse: raceEachOpenOfTheRepairIssue},
	} {
		t.Run(c.name, func(t *testing.T) {
			response := c.refuse(t)
			code := decodeBody[levelRefusal](t, response).Code
			if response.Code != http.StatusConflict || code == "" {
				t.Fatalf("the route answers %d %.600s, want a 409 refusal", response.Code, response.Body.String())
			}
			answered[code] = append(answered[code], c.name)
		})
	}

	var route, tool string
	for _, row := range (&server{}).routes() {
		if row.Method+" "+row.Pattern == "POST /api/v1/issues/{key}/spec-import-level" {
			route = row.Description
		}
	}
	for _, spec := range contracts.DispatchTools {
		if spec.Name == "dispatch_spec_import_level" {
			tool = spec.Description
		}
	}
	if route == "" || tool == "" {
		t.Fatalf("the table describes the route as %q and the tools describe dispatch_spec_import_level as %q, want both", route, tool)
	}
	answers := map[string]bool{}
	for code := range answered {
		answers[code] = true
	}
	sets := []struct {
		name  string
		codes map[string]bool
	}{
		{"the route answers it", answers},
		{"the route's description names it", describedConflicts(route)},
		{"the tool's description names it", describedConflicts(tool)},
	}
	var union []string
	for _, set := range sets {
		for code := range set.codes {
			if !slices.Contains(union, code) {
				union = append(union, code)
			}
		}
	}
	slices.Sort(union)
	for _, code := range union {
		var holds, lacks []string
		for index, set := range sets {
			name := set.name
			if index == 0 && set.codes[code] {
				name += " (" + strings.Join(answered[code], ", ") + ")"
			}
			if set.codes[code] {
				holds = append(holds, name)
			} else {
				lacks = append(lacks, name)
			}
		}
		if len(lacks) > 0 {
			t.Errorf("409 %s: %s, and not: %s", code, strings.Join(holds, "; "), strings.Join(lacks, "; "))
		}
	}
}

// raceEachOpenOfTheRepairIssue imports level one of the fixture with notes no approval names, so
// it holds each of its statements, and has another writer name a repair issue on one more of those
// holds while each open of the repair issue waits, until the import has tried repairAttempts times,
// and answers the import's answer. A transaction of the test holds the project's owner lock until
// the import's create waits there, after the import's own transaction committed the holds; then
// one holds the lock of the approved spec, which the create's hook takes before it reads the holds
// again. Each time the hook waits on it, that transaction writes the repair issue on one hold,
// queues the next holder behind the hook, and commits, so the hook reads a hold that no longer
// waits for a repair issue and refuses the create, and the next open waits on the next holder.
func raceEachOpenOfTheRepairIssue(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	rig := newLevelRig(t, "Level one of the fixture", levelFixture)
	rig.upload(t, "notes.md", levelNotesFixture)
	rig.approveLevel(t)
	other := rig.issue(t, "The repair issue of another import", specRepo, "Another import opened this issue.\n", "")
	writer := model.Actor{Kind: "session", ID: sessionActor()["id"].(string)}
	request := rig.importRequest(t, "notes.md")
	begin := func(what string) pgx.Tx {
		t.Helper()
		tx, err := rig.database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin the transaction that holds %s: %v", what, err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		return tx
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	queued := func(what string, holder pgx.Tx, want int) {
		t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for storetest.WaitForLockWaiters(t, holder, 0) < want {
			select {
			case response := <-answered:
				t.Fatalf("the import answered %d %.600s before %s", response.Code, response.Body.String(), what)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: not within 30s", what)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}

	owner := begin("the project's owner lock")
	if err := store.LockOwner(ctx, owner, store.ProjectOwner("CORE")); err != nil {
		t.Fatalf("hold the owner lock of CORE: %v", err)
	}
	go func() {
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, request)
		answered <- response
	}()
	queued("the import's create queues behind the project's owner lock", owner, 1)
	spec := begin("the lock of the approved spec")
	if err := specstore.Lock(ctx, spec, "CORE", specRepo); err != nil {
		t.Fatalf("hold the lock of the approved spec: %v", err)
	}
	if err := owner.Rollback(ctx); err != nil {
		t.Fatalf("let go of the owner lock of CORE: %v", err)
	}
	for attempt := 1; attempt <= repairAttempts; attempt++ {
		queued(fmt.Sprintf("open %d of the repair issue queues behind the lock of the approved spec", attempt), spec, 1)
		facts := specstore.RepairIssueFacts("CORE", specRepo, []specstore.Hold{{ID: levelFixtureStatements[attempt-1]}}, other.Key, writer, time.Now().UTC())
		if err := store.RunWrite(ctx, spec, func(b *store.WriteBatch) error {
			graphstore.QueueWrite(b, nil, facts)
			return nil
		}); err != nil {
			t.Fatalf("name the repair issue of %s: %v", levelFixtureStatements[attempt-1], err)
		}
		var next pgx.Tx
		locked := make(chan error, 1)
		if attempt < repairAttempts {
			next = begin("the lock of the approved spec after open " + fmt.Sprint(attempt))
			go func() { locked <- specstore.Lock(ctx, next, "CORE", specRepo) }()
			queued(fmt.Sprintf("the holder after open %d queues behind it", attempt), spec, 2)
		}
		if err := spec.Commit(ctx); err != nil {
			t.Fatalf("commit the repair issue of %s: %v", levelFixtureStatements[attempt-1], err)
		}
		if next == nil {
			break
		}
		select {
		case err := <-locked:
			if err != nil {
				t.Fatalf("take the lock of the approved spec after open %d: %v", attempt, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("the lock of the approved spec after open %d: not within 30s", attempt)
		}
		spec = next
	}
	select {
	case response := <-answered:
		return response
	case <-time.After(30 * time.Second):
		t.Fatalf("the import answered nothing within 30s")
		return nil
	}
}
