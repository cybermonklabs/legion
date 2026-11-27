package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygws "github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

const (
	regionBlock    = ":::ask{#region urgency=\"med\" multiple=\"false\"}\nWhich region?\n:::\n"
	transportBlock = ":::ask{#transport urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- REST: Matches the platform.\n- gRPC: Streams.\n:::\n"
	// otherTransportBlock is a question of its own under transportBlock's id: other options.
	otherTransportBlock = ":::ask{#transport urgency=\"med\" multiple=\"false\"}\nWhich transport?\n\n- NATS: Already deployed.\n- Kafka: Durable.\n:::\n"
	freshBlock          = ":::ask{#fresh urgency=\"med\" multiple=\"false\"}\nShould the copy ask this?\n:::\n"
)

// While another build serves (serveTwoBuilds), whose pages read an ask's state from its block, the
// close of an issue writes each withdrawn ask's state into its own block and into every copy of it
// on another document of the issue, which keeps naming its source. A copy of an answered ask, a
// block asking something else under a withdrawn ask's id, and a block no settlement has indexed
// yet are left as they are; the last is written resolved only by a write that marks unindexed
// blocks, which names it. No version is written, and the closer is credited as no pending author:
// the authors already pending stay, and no one else is added.
func TestWritingIssueAskStatesReachesTheAsksBlocksAndTheirCopiesAlone(t *testing.T) {
	t.Parallel()
	service, original := newTestService(t)
	serveTwoBuilds(t, service.store)
	service.settle = time.Hour
	seedServiceText(t, service, original, "Context\n\n"+regionBlock+"\n"+transportBlock+"\n"+shipBlock)
	settleCurrentGeneration(t, service, original)
	region := blockAskOn(t, service, original, "region")
	transport := blockAskOn(t, service, original, "transport")
	ship := blockAskOn(t, service, original, "ship")
	answerAskOnItsNode(t, service, ship.ID, model.AskAnswer{User: "alice", Selected: []string{"Yes"}, At: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)})

	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, "Context\n\n"+regionBlock+"\n"+otherTransportBlock+"\n"+shipBlock)
	settleCurrentGeneration(t, service, copied)
	if count := asksOn(t, service, copied); count != 1 {
		t.Fatalf("the copy opened %d asks, want one, its own transport question", count)
	}
	// As an agent reads the copy, its asks member answers each block's state and a copy's source
	// (requireCopiedStates). While another build serves, settlement keeps the same in each block's
	// opening line of the live document (rollWindowAskAttributes), which the writes below change.
	requireCopiedStates(t, service, copied,
		`#region state="open" `+copiedFrom(t, service, region.ID, original)+"\n",
		`#ship state="answered" answered_by="alice" answered_at="2026-10-08T12:00:00Z" selected=["Yes"] `+copiedFrom(t, service, ship.ID, original)+"\n",
		`#transport state="open"`+"\n")
	regionCopy := `:::ask{#region urgency="med" multiple="false" state="%s" ` + copiedFrom(t, service, region.ID, original) + `}`
	live := documentText(t, service, copied)
	for _, opener := range []string{
		fmt.Sprintf(regionCopy, "open"),
		`:::ask{#ship urgency="med" multiple="false" state="answered" answered_by="alice" answered_at="2026-10-08T12:00:00Z" selected="[&#x22;Yes&#x22;]" ` + copiedFrom(t, service, ship.ID, original) + `}`,
		`:::ask{#transport urgency="med" multiple="false" state="open"}`,
	} {
		if !strings.Contains(live, opener) {
			t.Fatalf("the copy's live document lacks %q:\n%s", opener, live)
		}
	}
	asker := model.Actor{Kind: "session", ID: "fresh-asking-session"}
	writeThroughLedger(t, service, copied, asker, freshBlock, withoutVersion)

	originalBefore := documentText(t, service, original)
	copiedBefore := documentText(t, service, copied)
	versionsBefore := documentVersions(t, service, original, copied)
	closer := model.Actor{Kind: "user", ID: "carol"}
	withdrawn := []model.Ask{withdrawnAsk(region, closer), withdrawnAsk(transport, closer)}

	if marked := writeIssueAskStates(t, service, withdrawn, closer, false); len(marked) != 0 {
		t.Fatalf("a write that marks no unindexed block marked %+v", marked)
	}
	originalWant := strings.NewReplacer(
		`:::ask{#region urgency="med" multiple="false" state="open"}`, `:::ask{#region urgency="med" multiple="false" state="resolved"}`,
		`:::ask{#transport urgency="med" multiple="false" state="open"}`, `:::ask{#transport urgency="med" multiple="false" state="resolved"}`,
	).Replace(originalBefore)
	copiedWant := strings.Replace(copiedBefore, fmt.Sprintf(regionCopy, "open"), fmt.Sprintf(regionCopy, "resolved"), 1)
	if originalWant == originalBefore || copiedWant == copiedBefore {
		t.Fatalf("the documents lack the blocks this test withdraws:\n%s\n%s", originalBefore, copiedBefore)
	}
	requireText(t, service, original, originalWant)
	requireText(t, service, copied, copiedWant)

	marked := writeIssueAskStates(t, service, withdrawn, closer, true)
	if want := []BlockRef{{ArtifactID: copied, BlockID: "fresh"}}; !reflect.DeepEqual(marked, want) {
		t.Fatalf("the sweep marked %+v, want %+v", marked, want)
	}
	// An agent's ask block holds no state of its own (a server attribute has no written default),
	// so the block no settlement has indexed reads with none until the sweep writes it resolved.
	freshOpen := `:::ask{#fresh urgency="med" multiple="false"}`
	if !strings.Contains(copiedWant, freshOpen) {
		t.Fatalf("the copy lacks the unindexed block:\n%s", copiedWant)
	}
	copiedWant = strings.Replace(copiedWant, freshOpen, `:::ask{#fresh urgency="med" multiple="false" state="resolved"}`, 1)
	requireText(t, service, original, originalWant)
	requireText(t, service, copied, copiedWant)
	if marked := writeIssueAskStates(t, service, withdrawn, closer, true); len(marked) != 0 {
		t.Fatalf("a second sweep marked %+v, which the first already wrote", marked)
	}
	requireText(t, service, copied, copiedWant)

	if count := asksOn(t, service, copied); count != 1 {
		t.Fatalf("the copy holds %d asks after the writes, want its one", count)
	}
	if versions := documentVersions(t, service, original, copied); !reflect.DeepEqual(versions, versionsBefore) {
		t.Fatalf("the writes changed the stored versions:\n%v\nwant\n%v", versions, versionsBefore)
	}
	if pending := pendingAuthorRows(t, service.store, original); len(pending) != 0 {
		t.Fatalf("the original's pending authors = %+v, want none", pending)
	}
	if pending := pendingAuthorRows(t, service.store, copied); !reflect.DeepEqual(pending, []model.Actor{asker}) {
		t.Fatalf("the copy's pending authors = %+v, want %+v alone", pending, asker)
	}
}

// A block repeating an ask's id - a browser's paste copies the block with its id - is no ask, and
// a write addressed by that id lands on the ask's own block. While another build serves, a write
// that marks no unindexed block leaves the copy as it is; one that marks them writes it resolved
// by its place in the document and names it under the id it repeats, which it leaves alone for
// settlement to stamp.
func TestWritingIssueAskStatesMarksABlockRepeatingAnAsksIDByItsPlace(t *testing.T) {
	t.Parallel()
	service, original := newTestService(t)
	serveTwoBuilds(t, service.store)
	service.settle = time.Hour
	seedServiceText(t, service, original, "Context\n\n"+regionBlock)
	settleCurrentGeneration(t, service, original)
	region := blockAskOn(t, service, original, "region")
	appendLiveBlock(t, service, original, regionBlock)
	pastedBefore := documentText(t, service, original)
	if count := strings.Count(pastedBefore, ":::ask{#region "); count != 2 {
		t.Fatalf("the document holds %d blocks under region, want the ask's and its pasted copy:\n%s", count, pastedBefore)
	}
	closer := model.Actor{Kind: "user", ID: "carol"}
	withdrawn := []model.Ask{withdrawnAsk(region, closer)}
	resolved := `:::ask{#region urgency="med" multiple="false" state="resolved"}`

	if marked := writeIssueAskStates(t, service, withdrawn, closer, false); len(marked) != 0 {
		t.Fatalf("a write that marks no unindexed block marked %+v", marked)
	}
	want := strings.Replace(pastedBefore, `:::ask{#region urgency="med" multiple="false" state="open"}`, resolved, 1)
	if want == pastedBefore {
		t.Fatalf("the document lacks the ask's open block:\n%s", pastedBefore)
	}
	requireText(t, service, original, want)

	marked := writeIssueAskStates(t, service, withdrawn, closer, true)
	if want := []BlockRef{{ArtifactID: original, BlockID: "region"}}; !reflect.DeepEqual(marked, want) {
		t.Fatalf("the sweep marked %+v, want %+v", marked, want)
	}
	text := documentText(t, service, original)
	if strings.Count(text, ":::ask{") != 2 || strings.Count(text, resolved) != 2 {
		t.Fatalf("the document after the sweep:\n%s\nwant the ask's block and its pasted copy under region, both resolved", text)
	}
	if marked := writeIssueAskStates(t, service, withdrawn, closer, true); len(marked) != 0 {
		t.Fatalf("a second sweep marked %+v, which the first already wrote", marked)
	}
	requireText(t, service, original, text)
	if count := asksOn(t, service, original); count != 1 {
		t.Fatalf("the document holds %d asks after the writes, want its one", count)
	}
}

// appendLiveBlock appends the blocks markdown parses to, ids and all, to the live document, as a
// browser's paste does, past the id repair every server write runs.
func appendLiveBlock(t *testing.T, service *Service, artifactID, markdown string) {
	t.Helper()
	pasted, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse the pasted block: %v", err)
	}
	err = service.srv.Apply(context.Background(), artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, err := treeOf(doc)
		if err != nil {
			t.Errorf("read live tree: %v", err)
			return
		}
		tree.Children = append(tree.Children, pasted.Children...)
		transact(func(txn *crdt.Transaction) {
			if err := pmdoc.Update(txn, fragment, tree); err != nil {
				t.Errorf("paste the block: %v", err)
			}
		})
	})
	if err != nil {
		t.Fatalf("apply the paste: %v", err)
	}
}

// The close's write of its asks' states, made while another build serves, is no edit: it credits
// the closer as no pending author, and the document's latest edit source, which settlement names
// as the author of an ask block it indexes with no author of its own, stays the session that wrote
// the unindexed block, on the pending-settlement row and in the room. Settlement still names it
// when the session's edit wrote its own version, as POST /edits does, so that no one is pending.
// An edit the same transaction made before the close's write is still the latest edit source.
func TestWritingIssueAskStatesLeavesTheLatestEditSourceAlone(t *testing.T) {
	t.Parallel()
	session := model.Actor{Kind: "session", ID: "late-asking-session"}
	closer := model.Actor{Kind: "user", ID: "carol"}
	editor := model.Actor{Kind: "user", ID: "erin"}
	for _, test := range []struct {
		name    string
		version ledgerWriteVersion
		edit    bool
		want    model.Actor
		pending []model.Actor
	}{
		{name: "the close's write alone", version: withoutVersion, want: session, pending: []model.Actor{session}},
		{name: "after the session's edit wrote its own version", version: withSnapshotVersion, want: session},
		{name: "after an edit in the same transaction", version: withoutVersion, edit: true, want: editor, pending: []model.Actor{session, editor}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			serveTwoBuilds(t, service.store)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "Context\n\n"+regionBlock)
			settleCurrentGeneration(t, service, artifactID)
			region := blockAskOn(t, service, artifactID, "region")
			writeThroughLedger(t, service, artifactID, session, freshBlock, test.version)

			var marked []BlockRef
			err := joinedWrite(service, func(ctx context.Context) (err error) {
				tx, _ := txFromContext(ctx)
				if err := store.LockOwner(ctx, tx, store.IssueOwner("DOC-1")); err != nil {
					return fmt.Errorf("lock the issue: %w", err)
				}
				if test.edit {
					if _, err := service.ApplyOps(ctx, artifactID, []model.EditOp{{Op: "insert", After: "end", Markdown: "Erin's paragraph.\n"}}, editor, nil); err != nil {
						return fmt.Errorf("apply the edit: %w", err)
					}
				}
				marked, err = service.WriteIssueAskStates(ctx, "DOC-1", []model.Ask{withdrawnAsk(region, closer)}, closer, true)
				return err
			})
			if err != nil {
				t.Fatalf("write the close's ask states: %v", err)
			}
			if want := []BlockRef{{ArtifactID: artifactID, BlockID: "fresh"}}; !reflect.DeepEqual(marked, want) {
				t.Fatalf("the close's write marked %+v, want %+v", marked, want)
			}
			requireLatestEditSource(t, service, artifactID, test.want)
			pending := pendingAuthorRows(t, service.store, artifactID)
			if len(pending) != len(test.pending) || slices.Contains(pending, closer) {
				t.Fatalf("pending authors = %+v, want %+v", pending, test.pending)
			}
			for _, author := range test.pending {
				if !slices.Contains(pending, author) {
					t.Fatalf("pending authors = %+v, want %+v", pending, test.pending)
				}
			}
		})
	}
}

// The close moves a document's latest edit source from its room to its pending-settlement row, and
// a room that loads while the issue is closed restores none. The reopen closes that room, and the
// room the next load opens holds the source again, so the settlement the document owes names the
// session that edited it last, not the closer whose write of its asks' states came after that
// edit. That load takes the source from the row only while the record holds nothing newer than
// the row (rowIsCurrent): a record that still holds an update whose append has not landed - the
// reopen's own ask writes, published into the room just before the reopen closes it - keeps its
// own source through a load that starts before the close's flush takes that update, and that
// source is the one the reopen restored (restoreLastActor), not the none the close left.
func TestReopeningAnIssueRestoresTheLatestEditSourceOfARoomLoadedWhileClosed(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name string
		// holdAppend leaves the record holding an append that has not landed, from before the
		// reopen until after the next load.
		holdAppend bool
	}{
		{name: "the next load takes the row's source"},
		{name: "a load while the record still holds an append keeps the restored source", holdAppend: true},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			ctx := context.Background()
			seedServiceText(t, service, artifactID, "Context\n\n"+regionBlock)
			settleCurrentGeneration(t, service, artifactID)
			region := blockAskOn(t, service, artifactID, "region")
			session := model.Actor{Kind: "session", ID: "reopened-session"}
			writeThroughLedger(t, service, artifactID, session, freshBlock, withSnapshotVersion)
			closeTestIssue(t, service)
			closer := model.Actor{Kind: "user", ID: "carol"}
			writeIssueAskStates(t, service, []model.Ask{withdrawnAsk(region, closer)}, closer, true)
			load := func(what string) {
				t.Helper()
				if err := service.srv.Apply(withOwnerVerified(ctx), artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, ygws.ErrNoChanges) {
					t.Fatalf("load %s: %v", what, err)
				}
			}
			load("the closed issue's room")
			if service.srv.GetDoc(artifactID) == nil || !service.roomClosed(artifactID) {
				t.Fatal("the closed issue's room is not resident and flagged closed")
			}
			if row.holdAppend {
				state := service.room(artifactID)
				state.mu.Lock()
				state.durableAppends.Add(1)
				state.mu.Unlock()
				// The count is taken by hand, so it is released by hand: the room's worker ends
				// an append with the class it took (finishUpdateClass), and this holds none.
				defer func() {
					state.mu.Lock()
					state.durableAppends.Add(-1)
					service.unlockState(artifactID, state)
				}()
			}

			reopenTestIssue(t, service)
			waitForNoLiveDocument(t, service, artifactID)
			load("the reopened issue's room")
			if service.roomClosed(artifactID) {
				t.Fatal("the reopened issue's room is flagged closed")
			}
			requireLatestEditSource(t, service, artifactID, session)
		})
	}
}

// A close and a reopen are no edit of a document, so neither is ever its latest editor, and
// neither credits its actor as a pending author. That holds when the close releases the room and
// the reopen loads it again, which forgets every edit source the room held: the settlement the
// reopen leaves owed names the session that edited the document last on the ask it indexes,
// whether that session's edit already wrote its own version or still waits for this settlement's,
// and a version the settlement writes lists neither the closer nor the reopener and is the
// session's.
func TestASettlementAfterAReleasingCloseAndAReopenNamesTheLatestEditor(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		name    string
		version ledgerWriteVersion
	}{
		{name: "the session's edit still owed its version", version: withoutVersion},
		{name: "the session's edit already versioned", version: withSnapshotVersion},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			ctx := context.Background()
			seedServiceText(t, service, artifactID, "Context\n\n"+regionBlock)
			settleCurrentGeneration(t, service, artifactID)
			region := blockAskOn(t, service, artifactID, "region")
			session := model.Actor{Kind: "session", ID: "reopened-session"}
			writeThroughLedger(t, service, artifactID, session, freshBlock, row.version)
			if service.srv.GetDoc(artifactID) == nil {
				t.Fatal("the edited document's room is not resident")
			}

			closer := model.Actor{Kind: "user", ID: "carol"}
			writeIssueAskStates(t, service, []model.Ask{withdrawnAsk(region, closer)}, closer, true)
			closeTestIssue(t, service)
			waitFor(t, 30*time.Second, "the closed issue's room state to be released", func() bool {
				state := service.lockExistingState(artifactID)
				if state == nil {
					return true
				}
				service.unlockState(artifactID, state)
				return false
			})

			reopenTestIssue(t, service)
			reopener := model.Actor{Kind: "user", ID: "dave"}
			writeIssueAskStates(t, service, []model.Ask{region}, reopener, false)
			versionsBefore := len(documentVersions(t, service, artifactID))
			if err := service.SettleIssueDocuments(store.WithTransactionTracking(ctx), "DOC-1"); err != nil {
				t.Fatalf("settle the reopened issue's documents: %v", err)
			}

			if ask := blockAskOn(t, service, artifactID, "fresh"); ask.State != "open" || ask.Author != session {
				t.Fatalf("the settled block's ask is %s by %+v, want open by %+v", ask.State, ask.Author, session)
			}
			if row.version == withSnapshotVersion {
				// The reopen wrote the blocks back as the version holds them, so no version is owed.
				return
			}
			if versions := len(documentVersions(t, service, artifactID)); versions != versionsBefore+1 {
				t.Fatalf("the settlement left %d versions, want %d: one more than before it", versions, versionsBefore+1)
			}
			if authors := latestVersionAuthors(t, service, artifactID); slices.Contains(authors, closer) || slices.Contains(authors, reopener) {
				t.Fatalf("the settlement's version lists %+v, want neither %+v nor %+v", authors, closer, reopener)
			}
			var eventActor model.Actor
			if err := service.store.Pool.QueryRow(ctx, `
				select actor from events where issue_key = 'DOC-1' and type = 'artifact.version' order by seq desc limit 1
			`).Scan(&eventActor); err != nil {
				t.Fatalf("read the settlement's version event: %v", err)
			}
			if eventActor != session {
				t.Fatalf("the settlement's version event names %+v, want %+v", eventActor, session)
			}
		})
	}
}

// requireLatestEditSource fails unless want is artifactID's latest edit source: on its
// pending-settlement row, in its room, and as the actor settlement names (settlementAuthors).
func requireLatestEditSource(t *testing.T, service *Service, artifactID string, want model.Actor) {
	t.Helper()
	ctx := context.Background()
	owed, lastActor, err := readOwedSettlement(ctx, service.store.Pool, artifactID)
	if err != nil || !owed || lastActor == nil || *lastActor != want {
		t.Fatalf("the pending settlement (owed %t, %v) names %+v, want %+v", owed, err, lastActor, want)
	}
	owedAuthors, err := readPendingAuthors(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the pending authors: %v", err)
	}
	durableLatest, err := readLastPerson(ctx, service.store.Pool, artifactID)
	if err != nil {
		t.Fatalf("read the latest person sender: %v", err)
	}
	state := service.lockState(artifactID)
	latest := state.lastActor
	credit := settlementAuthors(state, owedAuthors, lastActor, false, durableLatest, everyCredit)
	service.unlockState(artifactID, state)
	if latest == nil || *latest != want {
		t.Fatalf("the room's latest edit source = %+v, want %+v", latest, want)
	}
	if credit.actor != want {
		t.Fatalf("settlement names %+v, want %+v", credit.actor, want)
	}
}

// A closed issue's documents refuse every ordinary write, and the close's own, made while another
// build serves, still reaches them, since its caller holds the issue's owner lock: a room flagged
// closed that a reader loaded again, and a document no room holds, whose write the store keeps.
func TestWritingIssueAskStatesReachesAClosedIssuesDocuments(t *testing.T) {
	t.Parallel()
	for _, resident := range []bool{true, false} {
		name := "no resident room"
		if resident {
			name = "a resident room flagged closed"
		}
		t.Run(name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			serveTwoBuilds(t, service.store)
			service.settle = time.Hour
			ctx := context.Background()
			seedServiceText(t, service, artifactID, "Context\n\n"+regionBlock)
			settleCurrentGeneration(t, service, artifactID)
			region := blockAskOn(t, service, artifactID, "region")
			before := documentText(t, service, artifactID)
			closeTestIssue(t, service)
			if resident {
				if err := service.srv.Apply(withOwnerVerified(ctx), artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, ygws.ErrNoChanges) {
					t.Fatalf("load the closed issue's room: %v", err)
				}
				if service.srv.GetDoc(artifactID) == nil || !service.roomClosed(artifactID) {
					t.Fatal("the closed issue's room is not resident and flagged closed")
				}
			} else if service.srv.GetDoc(artifactID) != nil {
				t.Fatal("the closed issue's room is still resident")
			}
			if _, err := joinedApplyOps(service, artifactID, []model.EditOp{{Op: "replace", Find: "Context", With: "Edited"}}, model.Actor{Kind: "user", ID: "alice"}, nil); !errors.Is(err, ErrIssueClosed) {
				t.Fatalf("an ordinary write to the closed issue's document = %v, want ErrIssueClosed", err)
			}

			closer := model.Actor{Kind: "user", ID: "carol"}
			writeIssueAskStates(t, service, []model.Ask{withdrawnAsk(region, closer)}, closer, false)
			want := strings.Replace(before,
				`:::ask{#region urgency="med" multiple="false" state="open"}`, `:::ask{#region urgency="med" multiple="false" state="resolved"}`, 1)
			if want == before {
				t.Fatalf("the document lacks the withdrawn block:\n%s", before)
			}
			requireText(t, service, artifactID, want)
			closeDocumentRoom(t, service, artifactID)
			requireText(t, service, artifactID, want)
		})
	}
}

// While another build serves, a reopen writes the withdrawn blocks back to open, so a document
// that had nothing else pending renders as its latest version again, and the settlement the close
// and the reopen left owed writes no version. That holds because no settlement ran while the
// issue was closed: the settle delay here is an hour, and nothing resumes the documents. A
// settlement on the closed issue would have saved the blocks resolved, and the reopen's
// settlement would then write a version with them open again.
func TestWritingAReopensAskStatesBackLeavesNoVersionToWrite(t *testing.T) {
	t.Parallel()
	service, original := newTestService(t)
	serveTwoBuilds(t, service.store)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, original, "Context\n\n"+regionBlock)
	settleCurrentGeneration(t, service, original)
	region := blockAskOn(t, service, original, "region")
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, "Copied context\n\n"+regionBlock)
	settleCurrentGeneration(t, service, copied)
	documents := []string{original, copied}
	for _, document := range documents {
		requireSettledAtLatestVersion(t, service, document)
	}
	versionsBefore := documentVersions(t, service, documents...)

	closer := model.Actor{Kind: "user", ID: "carol"}
	withdrawn := withdrawnAsk(region, closer)
	writeIssueAskStates(t, service, []model.Ask{withdrawn}, closer, false)
	resolution, _ := json.Marshal(withdrawn.Resolution)
	if _, err := service.store.Pool.Exec(ctx, `update asks set state = 'resolved', resolution = $2 where id = $1`, region.ID, resolution); err != nil {
		t.Fatalf("withdraw the region ask: %v", err)
	}
	closeTestIssue(t, service)
	for _, document := range documents {
		if text := documentText(t, service, document); !strings.Contains(text, `state="resolved"`) {
			t.Fatalf("the closed issue's document reads the withdrawn block open:\n%s", text)
		}
	}

	reopenTestIssue(t, service)
	if _, err := service.store.Pool.Exec(ctx, `update asks set state = 'open', answer = null, resolution = null where id = $1`, region.ID); err != nil {
		t.Fatalf("restore the region ask: %v", err)
	}
	reopener := model.Actor{Kind: "user", ID: "dave"}
	writeIssueAskStates(t, service, []model.Ask{blockAskOn(t, service, original, "region")}, reopener, false)
	for _, document := range documents {
		if text, latest := documentText(t, service, document), latestVersionMarkdown(t, service, document); text != latest {
			t.Fatalf("the reopened document renders\n%s\nwant its latest version\n%s", text, latest)
		}
		if pending := pendingAuthorRows(t, service.store, document); len(pending) != 0 {
			t.Fatalf("the reopened document's pending authors = %+v, want none: neither %+v nor %+v made an edit", pending, closer, reopener)
		}
		settleCurrentGeneration(t, service, document)
		requireSettledAtLatestVersion(t, service, document)
	}
	if versions := documentVersions(t, service, documents...); !reflect.DeepEqual(versions, versionsBefore) {
		t.Fatalf("the close and reopen left versions\n%v\nwant\n%v", versions, versionsBefore)
	}
}

// An ask block written while the settle delay holds its settlement off is an ask, credited to its
// writer, once SettleIssueDocuments returns: the settlement runs on the caller's goroutine.
func TestSettlingAnIssuesDocumentsIndexesABlockWrittenWithinTheSettleDelay(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	untouched := createIssueDocument(t, service.store, 1, "# Untouched")
	actor := model.Actor{Kind: "session", ID: "settled-asking-session"}
	writeThroughLedger(t, service, artifactID, actor, freshBlock, withoutVersion)
	if count := asksOn(t, service, artifactID); count != 0 {
		t.Fatalf("the block was indexed before the settlement: %d asks", count)
	}

	if err := service.SettleIssueDocuments(store.WithTransactionTracking(ctx), "DOC-1"); err != nil {
		t.Fatalf("settle the issue's documents: %v", err)
	}
	ask := blockAskOn(t, service, artifactID, "fresh")
	if ask.State != "open" || ask.Author != actor {
		t.Fatalf("the settled block's ask is %s by %+v, want open by %+v", ask.State, ask.Author, actor)
	}
	requireSettledAtLatestVersion(t, service, artifactID)
	if versions := documentVersions(t, service, untouched); len(versions) != 1 {
		t.Fatalf("the document that owed nothing has versions %v, want its first alone", versions)
	}
}

// SettleIssueDocuments opens a transaction of its own for each settlement, so it refuses to run
// inside one, and the block it would have indexed waits for its settlement.
func TestSettlingAnIssuesDocumentsRefusesToRunInsideATransaction(t *testing.T) {
	t.Parallel()
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	writeThroughLedger(t, service, artifactID, model.Actor{Kind: "session", ID: "waiting-session"}, freshBlock, withoutVersion)

	ctx := store.WithTransactionTracking(context.Background())
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the caller's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	if err := service.SettleIssueDocuments(ctx, "DOC-1"); !errors.Is(err, store.ErrNestedAcquire) {
		t.Fatalf("settling inside a tracked transaction = %v, want ErrNestedAcquire", err)
	}
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	if err := service.SettleIssueDocuments(joined, "DOC-1"); !errors.Is(err, errSettleJoined) {
		t.Fatalf("settling inside a joined transaction = %v, want errSettleJoined", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("end the caller's transaction: %v", err)
	}
	if count := asksOn(t, service, artifactID); count != 0 {
		t.Fatalf("a refused settlement indexed %d asks", count)
	}
	owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
	if err != nil || !owed {
		t.Fatalf("the document owes a settlement = %t (%v), want true", owed, err)
	}
}

// WriteIssueAskStates writes only inside a transaction a caller joined.
func TestWritingIssueAskStatesNeedsAJoinedTransaction(t *testing.T) {
	t.Parallel()
	service, _ := newTestService(t)
	if _, err := service.WriteIssueAskStates(context.Background(), "DOC-1", nil, model.Actor{Kind: "user", ID: "carol"}, true); !errors.Is(err, errUnjoined) {
		t.Fatalf("an unjoined write = %v, want errUnjoined", err)
	}
}

// withdrawnAsk is ask as the close of DOC-1 by closer leaves it.
func withdrawnAsk(ask model.Ask, closer model.Actor) model.Ask {
	ask.State = "resolved"
	ask.Answer = nil
	ask.Resolution = &model.AskResolution{Kind: "withdrawn", Actor: closer, Reason: "Withdrawn by the close of DOC-1.", At: time.Now().UTC()}
	return ask
}

// writeIssueAskStates runs WriteIssueAskStates for DOC-1 in a transaction of its own, as the issue
// route does, and commits it.
func writeIssueAskStates(t *testing.T, service *Service, asks []model.Ask, actor model.Actor, markUnindexed bool) []BlockRef {
	t.Helper()
	var marked []BlockRef
	err := joinedWrite(service, func(ctx context.Context) (err error) {
		tx, _ := txFromContext(ctx)
		// The issue route holds the issue's owner lock before it writes any document.
		if err := store.LockOwner(ctx, tx, store.IssueOwner("DOC-1")); err != nil {
			return fmt.Errorf("lock the issue: %w", err)
		}
		marked, err = service.WriteIssueAskStates(ctx, "DOC-1", asks, actor, markUnindexed)
		return err
	})
	if err != nil {
		t.Fatalf("write the issue's ask states: %v", err)
	}
	return marked
}

// documentText is the document's own markdown (Text), which the writes of an ask's state while
// another build serves change (WriteIssueAskStates) and which holds no ask's state once the fleet
// is one build: every caller compares it with stored bytes, a latest version or a document the
// same write changed, never with the text an agent reads, which holds no ask's state at all and
// answers each in its asks member (TextWithAsks).
func documentText(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	text, err := service.Text(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("read the document: %v", err)
	}
	return text
}

// documentVersions is every stored version of documents, as document, number and markdown.
func documentVersions(t *testing.T, service *Service, documents ...string) []string {
	t.Helper()
	rows, err := service.store.Pool.Query(context.Background(), `
		select artifact_id::text, number, markdown from artifact_versions
		where artifact_id::text = any($1) order by artifact_id, number
	`, documents)
	if err != nil {
		t.Fatalf("read the documents' versions: %v", err)
	}
	defer rows.Close()
	var versions []string
	for rows.Next() {
		var document, markdown string
		var number int
		if err := rows.Scan(&document, &number, &markdown); err != nil {
			t.Fatalf("scan a document version: %v", err)
		}
		versions = append(versions, fmt.Sprintf("%s #%d %q", document, number, markdown))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the documents' versions: %v", err)
	}
	return versions
}

func latestVersionMarkdown(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	var markdown string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read the latest version: %v", err)
	}
	return markdown
}

// requireSettledAtLatestVersion fails unless the document owes no settlement and renders as its
// latest version.
func requireSettledAtLatestVersion(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	owed, err := settlementPending(context.Background(), service.store.Pool, artifactID)
	if err != nil || owed {
		t.Fatalf("the document owes a settlement = %t (%v), want false", owed, err)
	}
	if text, latest := documentText(t, service, artifactID), latestVersionMarkdown(t, service, artifactID); text != latest {
		t.Fatalf("the settled document renders\n%s\nwant its latest version\n%s", text, latest)
	}
}

// serveTwoBuilds registers in server_processes, the registry the document service reads the fleet
// from (processes.Live), a live process of each of two builds for the rest of the test, as a roll
// serves them: one of the release before, with the schema numbers a process of it registers
// (baseSchemaVersion, baseSchemaCreatable), and one of this build, with its own; each at the owner
// lock protocol both builds register (store.OwnerLockProtocol). The fleet guard reads a process
// below this build's schema version as a build whose pages read an ask's state from its block
// (anotherBuildServes), so the document service then keeps the server-owned attributes of each
// ask block in step with its asks row (rollWindowAskAttributes) and writes an issue's close and
// reopen into its asks' blocks (WriteIssueAskStates). A test's editor presents no
// schema_creatable, so its creatable version is 1, at most the lowest version the fleet reads, and
// its document socket stays writable beside the release before (schemaFleet.admits).
func serveTwoBuilds(t *testing.T, database *store.Store) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into server_processes (instance, commit, schema_version, schema_creatable, heartbeat_at, owner_lock_protocol)
		values (gen_random_uuid(), 'the-release-before', $1, $2, now() + interval '1 hour', $5),
		       (gen_random_uuid(), 'this-release', $3, $4, now() + interval '1 hour', $5)
	`, baseSchemaVersion, baseSchemaCreatable, pmdoc.SchemaVersion(), pmdoc.SchemaCreatable(), store.OwnerLockProtocol); err != nil {
		t.Fatalf("register a live process of each of two builds: %v", err)
	}
}

// baseSchemaVersion and baseSchemaCreatable are the schema numbers a process of a release that
// still reads answers off block attributes registers: that release's pmdoc/schema/blocks.json is
// version 6, one below the version at which answers left the markdown (answersLeftTheMarkdown, 7),
// and creates nothing past version 5. So a fleet holding one drives the fleet guard at the real
// boundary, version 6 against 7; this build, at version 8, is past it as the release at 7 is.
const (
	baseSchemaVersion   = 6
	baseSchemaCreatable = 5
)
