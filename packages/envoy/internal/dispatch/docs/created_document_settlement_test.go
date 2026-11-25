package docs

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// createdDocuments is a document service on a database of its own, holding project DOC and its
// issue DOC-1, whose settlements wait an hour, so only a test's own calls run one. It counts each
// room a settlement takes (afterSettleWarm).
type createdDocuments struct {
	database *store.Store
	service  *Service
	mu       sync.Mutex
	taken    []string
}

func newCreatedDocuments(t *testing.T) *createdDocuments {
	t.Helper()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into projects (key, name, default_assignee) values ('DOC', 'Documents', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank) values
			('DOC-1', 'DOC', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("create the test project and its issue: %v", err)
	}
	f := &createdDocuments{database: database, service: newShutdownTestService(t, database, nil)}
	f.service.afterSettleWarm = func(room string) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.taken = append(f.taken, room)
	}
	return f
}

// create creates document slug holding markdown through CreateDocument, as alice, in a
// transaction of its own: a document of issue, or of project DOC itself where issue is "".
func (f *createdDocuments) create(t *testing.T, issue, slug, markdown string) (model.Artifact, WrittenText) {
	t.Helper()
	project := "DOC"
	row := NewArtifact{Project: &project, RefPrefix: project, Slug: slug, Name: slug + ".md", CreatedBy: model.Actor{Kind: "user", ID: "alice"}}
	if issue != "" {
		row.IssueKey, row.RefPrefix = &issue, issue
	}
	var artifact model.Artifact
	var written WrittenText
	if err := joinedWrite(f.service, func(ctx context.Context) error {
		var err error
		artifact, written, err = CreateDocument(ctx, f.service, row, markdown)
		return err
	}); err != nil {
		t.Fatalf("create %s: %v", slug, err)
	}
	return artifact, written
}

// takes is how many times a settlement took room's room.
func (f *createdDocuments) takes(room string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, took := range f.taken {
		if took == room {
			count++
		}
	}
	return count
}

// owed is whether room's document has a pending-settlement row.
func (f *createdDocuments) owed(t *testing.T, room string) bool {
	t.Helper()
	owes, err := settlementPending(context.Background(), f.database.Pool, room)
	if err != nil {
		t.Fatalf("read whether %s owes a settlement: %v", room, err)
	}
	return owes
}

// record is whether the service holds a record of room's room, and whether that record holds an
// edit source no settlement has read.
func (f *createdDocuments) record(room string) (held, unsettled bool) {
	state := f.service.lockExistingState(room)
	if state == nil {
		return false, false
	}
	unsettled = state.unsettled
	f.service.unlockState(room, state)
	return true, unsettled
}

// armed is whether a settlement timer is armed for room.
func (f *createdDocuments) armed(room string) bool {
	state := f.service.lockExistingState(room)
	if state == nil {
		return false
	}
	armed := f.service.isSettleTimerArmed(state.settle)
	f.service.unlockState(room, state)
	return armed
}

// A document CreateDocument creates whose tree a settlement has nothing to write into - every
// block given its id by the parse, no ask block, no check block carrying `ci_check`, as a
// project's `evals-md` is on every project create - owes no settlement (seedOwesSettlement). Its
// commit leaves no pending-settlement row and no record of its room, and the version
// CreateDocument wrote stands holding the seeded markdown. Nothing settles it afterwards, so no
// settlement takes its room: not settleOwedNow, not a load of the room, not Shutdown. The same
// create of a document holding an ask block still owes its settlement, which runs as before: the
// row and the room's record stand after the commit, and settleOwedNow takes the room, opens the
// ask and deletes the row. Red where the seed owes a settlement whatever its tree holds: the plain
// document's create leaves the row, which settleOwedNow then settles, warming its room.
func TestACreatedDocumentOwesASettlementOnlyWhereItsTreeNeedsOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCreatedDocuments(t)
	database, service := f.database, f.service
	alice := model.Actor{Kind: "user", ID: "alice"}

	plain, plainWritten := f.create(t, "", "plain-md", "# Evals of Documents\n\nOne section for each eval.\n\n"+
		"## review-latency\n\n:::callout{#method-1}\nCounts the hours to a first review.\n\n- Only merged pull requests.\n:::\n")
	asking, askingWritten := f.create(t, "", "asking-md", "# Asks\n\n:::ask{#ship urgency=\"med\"}\nShip it?\n:::\n")

	if plainWritten.OwesSettlement {
		t.Errorf("CreateDocument answered that the plain document owes a settlement")
	}
	if f.owed(t, plain.ID) {
		t.Errorf("the plain document's create left a pending-settlement row")
	}
	if held, unsettled := f.record(plain.ID); held {
		t.Errorf("the plain document's create left a record of its room (unsettled %t)", unsettled)
	}
	if service.roomHeld(plain.ID) {
		t.Errorf("the plain document's create loaded its room")
	}
	if number, markdown, authors := latestDocumentVersion(t, database, plain.ID); number != 1 || markdown != plainWritten.Markdown ||
		len(authors) != 1 || authors[0] != alice {
		t.Errorf("the plain document's latest version is %d holding %q by %v, want version 1 holding the seeded %q by alice",
			number, markdown, authors, plainWritten.Markdown)
	}
	// What the store holds is what the decision read: a settlement would stamp no id into it and
	// index no ask.
	if stored, err := service.storedTree(ctx, plain.ID); err != nil {
		t.Errorf("read the plain document's stored tree: %v", err)
	} else if repairs := pmdoc.BlockIDRepairCount(stored); repairs != 0 || pmdoc.HoldsAskBlock(stored) {
		t.Errorf("the plain document's stored tree lacks %d block ids or holds an ask block (%t), so a settlement owed it something",
			repairs, pmdoc.HoldsAskBlock(stored))
	}
	if !askingWritten.OwesSettlement {
		t.Errorf("CreateDocument answered that the document holding an ask block owes no settlement")
	}
	if !f.owed(t, asking.ID) {
		t.Errorf("the create of the document holding an ask block left no pending-settlement row")
	}
	if held, unsettled := f.record(asking.ID); !held || !unsettled {
		t.Errorf("the create of the document holding an ask block left its room's record held %t, unsettled %t; want an unsettled record", held, unsettled)
	}

	deadline := time.Now().Add(roomWait(t))
	if err := service.settleOwedNow(ctx, plain.ID, deadline); err != nil {
		t.Errorf("settle the plain document now: %v", err)
	}
	if f.takes(plain.ID) != 0 || service.roomHeld(plain.ID) {
		t.Errorf("settleOwedNow took the plain document's room %d times (held %t), want none", f.takes(plain.ID), service.roomHeld(plain.ID))
	}
	if err := service.settleOwedNow(ctx, asking.ID, deadline); err != nil {
		t.Errorf("settle the document holding an ask block now: %v", err)
	}
	var opened int
	if err := database.Pool.QueryRow(ctx, `select count(*) from asks where block_artifact_id = $1 and state = 'open'`, asking.ID).Scan(&opened); err != nil {
		t.Fatalf("count the opened asks: %v", err)
	}
	if f.takes(asking.ID) != 1 || opened != 1 || f.owed(t, asking.ID) {
		t.Errorf("settleOwedNow took the room of the document holding an ask block %d times, opened %d asks and left it owing %t; want once, one and nothing owed",
			f.takes(asking.ID), opened, f.owed(t, asking.ID))
	}

	// A load of the plain document's room arms no settlement: no row says one is owed.
	liveTree(t, service, plain.ID)
	state := service.lockExistingState(plain.ID)
	if state == nil {
		t.Fatal("the load of the plain document's room left no record of it")
	}
	armed, unsettled := service.isSettleTimerArmed(state.settle), state.unsettled
	service.unlockState(plain.ID, state)
	if armed || unsettled {
		t.Errorf("the load of the plain document's room armed its settlement (%t) or marked its record unsettled (%t)", armed, unsettled)
	}

	shutdownCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := service.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shut the service down: %v", err)
	}
	if f.takes(plain.ID) != 0 || f.owed(t, plain.ID) {
		t.Errorf("Shutdown took the plain document's room %d times and left it owing %t, want neither", f.takes(plain.ID), f.owed(t, plain.ID))
	}
	if number, markdown, _ := latestDocumentVersion(t, database, plain.ID); number != 1 || markdown != plainWritten.Markdown {
		t.Errorf("after Shutdown the plain document's latest version is %d holding %q, want version 1 holding %q", number, markdown, plainWritten.Markdown)
	}
}

// The decision that a created document owes no settlement covers its seed alone: the docs code
// serves every document, and a later write that gives the tree what a settlement writes into owes
// that settlement as any write does. Each case creates a document that owes none, then makes one
// such write through a write path that changes an existing document on the server, as bob:
//   - an ops edit inserting an ask block into a project document, as the eval registry's write
//     of a method into `evals-md` and the edit route make it (ApplyOps, then SnapshotVersion);
//   - an upload replacing an issue document's whole text (WriteDocumentText), which gives a
//     check block that the seed wrote without `ci_check` a `ci_check`.
//
// The write's commit leaves the pending-settlement row and an unsettled record of the room, and
// arms the room's settlement timer, as the commit of a write to a document no room holds does
// (publishLiveWrite). Its answer says a settlement is owed (the batch applied; the upload owes
// one), so the test schedules the settlement again, as each caller does. settleOwedNow then runs
// that settlement in the timer's place: it takes the room once, deletes the row, and does what a
// settlement of that block does. For the ask block it opens the ask, with the edit's writer as
// its author. For the check it holds the check to the four rules: a result recorded against the
// check after the upload read none, as a tester's result that commits beside the upload can be,
// breaks the fourth rule, so the settlement deletes the check's `ci_check` in the live document
// and in the version it writes, and appends one block.invalid that names the check. Red where
// the create's decision outlives the seed: the write leaves no row, and settleOwedNow settles
// nothing, so no ask opens and the check keeps its `ci_check`. Red too where the upload answers
// that it owes no settlement, so its caller schedules none.
func TestACreatedDocumentOwesTheSettlementOfALaterEditThatNeedsOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newCreatedDocuments(t)
	database, service := f.database, f.service
	bob := model.Actor{Kind: "user", ID: "bob"}
	const heldCheck = `negative_control="Break the build and watch the run fail."`
	for _, test := range []struct {
		name, issue, slug, seed string
		// edit makes the later write of document as bob, in a transaction of its own, and answers
		// whether the write says a settlement is owed, which its caller reads to schedule one.
		edit func(t *testing.T, document string) bool
		// meanwhile, when set, runs after the write has committed and before its settlement.
		meanwhile func(t *testing.T, document string)
		// settled checks what the settlement did with the block the write added.
		settled func(t *testing.T, document string)
	}{
		{
			name: "an ops edit inserts an ask block",
			slug: "methods-md",
			seed: "# Evals of Documents\n\nOne section for each eval.\n\n" +
				"## review-latency\n\n:::callout{#method-1}\nCounts the hours to a first review.\n:::\n",
			edit: func(t *testing.T, document string) bool {
				t.Helper()
				var edit EditOutcome
				if err := joinedWrite(service, func(ctx context.Context) error {
					var err error
					if edit, err = service.ApplyOps(ctx, document, []model.EditOp{{
						Op: "insert", After: "end", Markdown: ":::ask{#ship urgency=\"med\" multiple=\"false\"}\nShip it?\n:::\n",
					}}, bob, nil); err != nil {
						return err
					}
					written, err := service.SnapshotVersion(ctx, document, bob)
					if err == nil && !written.Wrote {
						err = errors.New("the edit wrote no version")
					}
					return err
				}); err != nil {
					t.Fatalf("insert the ask block: %v", err)
				}
				if len(edit.AddedAskBlocks) != 1 || edit.AddedAskBlocks[0].ID != "ship" {
					t.Errorf("the ops edit answered that it added the ask blocks %+v, want ship alone", edit.AddedAskBlocks)
				}
				// The edit route and the eval registry schedule a settlement for every batch that applied.
				return edit.Applied > 0
			},
			settled: func(t *testing.T, document string) {
				t.Helper()
				var kind, id string
				if err := database.Pool.QueryRow(ctx, `
					select author->>'kind', author->>'id' from asks
					where block_artifact_id = $1 and block_id = 'ship' and state = 'open'
				`, document).Scan(&kind, &id); err != nil {
					t.Errorf("read the open ask that the settlement opens for the edit's ask block: %v", err)
				} else if kind != bob.Kind || id != bob.ID {
					t.Errorf("the settlement opened the edit's ask with author %s %s, want the edit's writer bob", kind, id)
				}
			},
		},
		{
			name:  "an upload gives a check block ci_check",
			issue: "DOC-1",
			slug:  "checks-md",
			seed:  "# Checks\n\n" + ciCheckOf("c1", heldCheck),
			edit: func(t *testing.T, document string) bool {
				t.Helper()
				var written WrittenText
				if err := joinedWrite(service, func(ctx context.Context) error {
					var err error
					written, err = WriteDocumentText(ctx, service, document, "# Checks\n\n"+ciCheckOf("c1", ciCheckKept), bob, nil, false, nil)
					return err
				}); err != nil {
					t.Fatalf("upload the check carrying ci_check: %v", err)
				}
				return written.OwesSettlement
			},
			meanwhile: func(t *testing.T, document string) {
				t.Helper()
				if _, err := database.Pool.Exec(ctx, `
					insert into check_results (issue_key, check_issue_key, check_artifact_id, check_block_id, check_version, check_sha256, role, commit_sha, verdict, actor)
					values ('DOC-1', 'DOC-1', $1, 'c1', 2, 'digest', 'tester', repeat('a', 40), 'pass', '{"kind":"user","id":"alice"}')
				`, document); err != nil {
					t.Fatalf("record a result against c1: %v", err)
				}
			},
			settled: func(t *testing.T, document string) {
				t.Helper()
				var invalid int
				if err := database.Pool.QueryRow(ctx, `
					select count(*) from events
					where type = 'block.invalid' and issue_key = 'DOC-1' and payload->>'block_id' = 'c1'
						and payload->>'block_type' = 'check' and payload->>'reason' = $1
				`, ciCheckRuleRecorded).Scan(&invalid); err != nil {
					t.Fatalf("count the block.invalid events naming c1: %v", err)
				}
				live := liveCIChecks(t, service, document)["c1"]
				number, markdown, _ := latestDocumentVersion(t, database, document)
				if invalid != 1 || live != "" || number <= 2 || strings.Contains(markdown, "ci_check=") {
					t.Errorf("the settlement appended %d block.invalid naming c1's recorded result, left c1 carrying ci_check %q in the live document, "+
						"and left version %d holding %q as the latest; want one, none, and a version after the upload's 2 holding no ci_check",
						invalid, live, number, markdown)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			document, created := f.create(t, test.issue, test.slug, test.seed)
			if held, _ := f.record(document.ID); created.OwesSettlement || f.owed(t, document.ID) || held {
				t.Fatalf("the create left the document owing a settlement (answer %t, row %t, room record %t): this test is for a document that owes none",
					created.OwesSettlement, f.owed(t, document.ID), held)
			}

			owes := test.edit(t, document.ID)
			if !owes {
				t.Errorf("the write answered that the document owes no settlement, so its caller schedules none")
			}
			if !f.owed(t, document.ID) {
				t.Errorf("the write's commit left no pending-settlement row")
			}
			if held, unsettled := f.record(document.ID); !held || !unsettled {
				t.Errorf("the write's commit left its room's record held %t, unsettled %t; want an unsettled record", held, unsettled)
			}
			// The commit of a write to a document no room holds arms its settlement
			// (publishLiveWrite); the caller schedules it again once the transaction has
			// committed, where the answer says one is owed.
			if !f.armed(document.ID) {
				t.Errorf("the write's commit armed no settlement")
			}
			if owes {
				service.ScheduleSettlement(document.ID)
			}
			if test.meanwhile != nil {
				test.meanwhile(t, document.ID)
			}

			if err := service.settleOwedNow(ctx, document.ID, time.Now().Add(roomWait(t))); err != nil {
				t.Errorf("settle the document now: %v", err)
			}
			if f.takes(document.ID) != 1 || f.owed(t, document.ID) {
				t.Errorf("settleOwedNow took the document's room %d times and left it owing %t; want once and nothing owed",
					f.takes(document.ID), f.owed(t, document.ID))
			}
			test.settled(t, document.ID)
		})
	}
}
