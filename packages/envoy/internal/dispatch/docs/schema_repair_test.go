package docs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/graphstore"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testcleanup"
)

func newSchemaRepairService(database *store.Store) *Service {
	return New(Deps{
		Store:     database,
		Events:    events.NewBroker(),
		ServerURL: "https://dispatch.example",
		Settle:    20 * time.Millisecond,
	})
}

func writeSchemaInvalidElement(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if err := service.InjectSchemaInvalidForTest(context.Background(), artifactID); err != nil {
		t.Fatalf("write crafted element: %v", err)
	}
}

// A crafted client can persist a tree outside the Proof schema. After a restart, replacing that
// document from markdown repairs it instead of leaving every replacement permanently refused.
func TestStoredTreeOutsideSchemaIsRepairedByReplacementAfterRestart(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First\n")
	first := newSchemaRepairService(database)
	seedServiceText(t, first, artifactID, "before\n")
	writeSchemaInvalidElement(t, first, artifactID)
	if _, err := first.Text(context.Background(), artifactID); !errors.Is(err, ErrDocSchema) {
		t.Fatalf("read crafted tree: %v, want ErrDocSchema", err)
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown first service: %v", err)
	}

	second := newSchemaRepairService(database)
	testcleanup.Add(t, func() {
		if err := second.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown second service: %v", err)
		}
	})
	if _, err := second.Text(context.Background(), artifactID); !errors.Is(err, ErrDocOutsideSchema) {
		t.Fatalf("read crafted tree after restart: %v, want ErrDocOutsideSchema", err)
	}
	if _, err := joinedReplaceText(second, artifactID, "repaired\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replace unreadable document after restart: %v", err)
	}
	if text, err := second.Text(context.Background(), artifactID); err != nil || text != "repaired\n" {
		t.Fatalf("read repaired document: %q (%v), want repaired markdown", text, err)
	}
	if err := second.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown repaired service: %v", err)
	}

	third := newSchemaRepairService(database)
	testcleanup.Add(t, func() {
		if err := third.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown third service: %v", err)
		}
	})
	if text, err := third.Text(context.Background(), artifactID); err != nil || text != "repaired\n" {
		t.Fatalf("read repaired document after second restart: %q (%v), want repaired markdown", text, err)
	}
}

// A repair names no token: the live tree is outside the Proof schema, so it renders no markdown
// and has no token for any caller to name. One that names a token has named a token the
// document has never had, which is refused rather than taken with the guard dropped, and the
// unreadable document is left as it was; the repair that names none, the only way to fix such a
// document, is taken after it.
func TestSchemaRepairRefusesAReplacementThatNamesAToken(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First\n")
	first := newSchemaRepairService(database)
	seedServiceText(t, first, artifactID, "before\n")
	writeSchemaInvalidElement(t, first, artifactID)
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown first service: %v", err)
	}

	second := newSchemaRepairService(database)
	testcleanup.Add(t, func() {
		if err := second.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown second service: %v", err)
		}
	})
	alice := model.Actor{Kind: "user", ID: "alice"}
	named := &model.EditPrecondition{Document: "sha256:" + strings.Repeat("a", 64)}
	if _, err := joinedReplaceTextNaming(second, artifactID, "repaired\n", alice, named); !errors.Is(err, ErrPreconditionUnavailable) {
		t.Fatalf("a repair naming a token = %v, want ErrPreconditionUnavailable", err)
	}
	if _, err := second.Text(context.Background(), artifactID); !errors.Is(err, ErrDocOutsideSchema) {
		t.Fatalf("read after the refused repair: %v, want the document outside the schema, as it stood", err)
	}
	if _, err := joinedReplaceText(second, artifactID, "repaired\n", alice); err != nil {
		t.Fatalf("the repair naming no token: %v", err)
	}
	if text, err := second.Text(context.Background(), artifactID); err != nil || text != "repaired\n" {
		t.Fatalf("read the repaired document: %q (%v), want repaired markdown", text, err)
	}
}

// A repair has no readable previous tree to compare, so it preserves every open ask by its stored
// block ID and refuses a replacement that drops one.
func TestSchemaRepairKeepsOpenAskBlocks(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	first := newSchemaRepairService(database)
	const ask = ":::ask{#keep urgency=\"med\" multiple=\"false\" state=\"open\"}\nKeep this decision?\n:::\n"
	seedServiceText(t, first, artifactID, ask)
	first.settleRoom(artifactID, 0)
	var askID string
	if err := database.Pool.QueryRow(context.Background(), `
		select id::text from asks where block_artifact_id = $1 and block_id = 'keep' and state = 'open'
	`, artifactID).Scan(&askID); err != nil {
		t.Fatalf("read open ask: %v", err)
	}
	writeSchemaInvalidElement(t, first, artifactID)
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown first service: %v", err)
	}

	second := newSchemaRepairService(database)
	testcleanup.Add(t, func() {
		if err := second.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown second service: %v", err)
		}
	})
	_, err := joinedReplaceText(second, artifactID, "replacement without the ask\n", model.Actor{Kind: "user", ID: "alice"})
	var invalid *ErrInvalidAskBlock
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "keep") {
		t.Fatalf("replacement dropping the open ask: %v, want ErrInvalidAskBlock naming keep", err)
	}
	if _, err := joinedReplaceText(second, artifactID, ask+"\nRepaired.\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("replacement retaining the open ask: %v", err)
	}
	var state string
	if err := database.Pool.QueryRow(context.Background(), `select state from asks where id = $1`, askID).Scan(&state); err != nil {
		t.Fatalf("read retained ask: %v", err)
	}
	if state != "open" {
		t.Fatalf("retained ask state = %q, want open", state)
	}
}

// A repair is refused to an agent only for the answered asks whose block the document held before
// it, as its block index holds them: an answered ask whose block a person removed earlier is no
// block the repair drops, so it neither refuses the repair nor is named by its refusal. The spec
// holds two answered asks; alice, a person, removes q-gone; the document then falls outside the
// Proof schema, and an agent's session repairs it. A repair dropping q-kept too is refused for
// q-kept alone, and one keeping q-kept is taken.
func TestAnAgentsRepairIsRefusedOnlyForTheAnsweredAsksTheDocumentStillHeld(t *testing.T) {
	const head = "# Spec\n\nThe corpus is written once.\n\n"
	kept := answerableAskBlock("q-kept", "Which build writes the corpus?")
	issue := newSpecIssue(t, head+answerableAskBlock("q-gone", "Which store holds the corpus?")+kept)
	issue.answer("q-gone", "The database we run.")
	issue.answer("q-kept", "The base release.")
	issue.replaceAsPerson(head + kept)
	if removals := issue.removalsOf(blockAskOn(t, issue.service, issue.spec, "q-gone").ID); len(removals) != 1 || removals[0].ToID != "alice" {
		t.Fatalf("alice's removal of q-gone left the removals %+v, want one to alice", removals)
	}
	writeSchemaInvalidElement(t, issue.service, issue.spec)
	agent := model.Actor{Kind: "session", ID: "ses-repairing-agent"}

	_, err := joinedReplaceText(issue.service, issue.spec, head+"Repaired without either ask.\n", agent)
	var refused *ErrAnsweredAskKept
	if !errors.As(err, &refused) || len(refused.Asks) != 1 || !strings.HasSuffix(refused.Asks[0].Block, "#b-q-kept") {
		t.Fatalf("the agent's repair dropping q-kept answered %v, want ErrAnsweredAskKept naming q-kept alone", err)
	}
	if _, err := joinedReplaceText(issue.service, issue.spec, head+kept+"Repaired.\n", agent); err != nil {
		t.Fatalf("the agent's repair keeping q-kept answered %v, want it taken: q-gone left the spec in alice's earlier write", err)
	}
	if text, err := issue.service.Text(context.Background(), issue.spec); err != nil || !strings.Contains(text, "Repaired.") {
		t.Fatalf("the repaired spec reads %q (%v), want the agent's repair", text, err)
	}
}

// answerableAskBlock is an ask block of question under id, for a person to answer.
func answerableAskBlock(id, question string) string {
	return ":::ask{#" + id + " urgency=\"med\" multiple=\"false\"}\n" + question + "\n:::\n\n"
}

// specIssue is issue DOC-1 of a test database and its spec, written through the document service
// and settled, so each ask block of the spec holds its ask.
type specIssue struct {
	t        *testing.T
	database *store.Store
	service  *Service
	spec     string
}

// newSpecIssue creates project DOC, issue DOC-1 and its spec holding markdown as its version 1,
// written by alice and indexed as an upload of a new document is, and settles it.
func newSpecIssue(t *testing.T, markdown string) *specIssue {
	t.Helper()
	ctx := context.Background()
	database := storetest.Open(t)
	if _, err := database.Pool.Exec(ctx, `
		insert into projects (key, name, default_assignee) values ('DOC', 'Documents', 'alice');
		insert into issues (key, project_key, number, title, created_by, rank)
		values ('DOC-1', 'DOC', 1, 'Document', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("create the issue: %v", err)
	}
	service := newTestServiceOn(t, database)
	service.settle = time.Hour
	var spec string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ('DOC-1', 'DOC', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')
		returning id::text
	`).Scan(&spec); err != nil {
		t.Fatalf("create the spec: %v", err)
	}
	if err := joinedWrite(service, func(ctx context.Context) error {
		written, err := service.SeedText(ctx, spec, markdown, model.Actor{Kind: "user", ID: "alice"})
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		if _, err := tx.Exec(ctx, `
			insert into artifact_versions (artifact_id, number, markdown, authors)
			values ($1, 1, $2, '[{"kind":"user","id":"alice"}]')
		`, spec, written); err != nil {
			return err
		}
		_, write, err := service.IndexDocumentReferences(ctx, spec, 1, written)
		if err != nil {
			return err
		}
		return causeInWritePhase(ctx, tx, write, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("write the spec: %v", err)
	}
	settleCurrentGeneration(t, service, spec)
	return &specIssue{t: t, database: database, service: service, spec: spec}
}

// replaceAsPerson writes markdown over the spec as alice, a person, with its version, whose facts
// it appends with no event, and settles it. The upload names the token the spec stands at, read
// before the write as a caller reads it.
func (issue *specIssue) replaceAsPerson(markdown string) {
	t := issue.t
	t.Helper()
	alice := model.Actor{Kind: "user", ID: "alice"}
	base := liveDocumentToken(t, issue.service, issue.spec)
	if err := joinedWrite(issue.service, func(ctx context.Context) error {
		if _, err := issue.service.ReplaceText(ctx, issue.spec, markdown, alice, base); err != nil {
			return err
		}
		written, err := issue.service.SnapshotVersion(ctx, issue.spec, alice)
		if err != nil {
			return err
		}
		tx, _ := txFromContext(ctx)
		return causeInWritePhase(ctx, tx, written.Blocks, graphstore.NoEvent)
	}); err != nil {
		t.Fatalf("replace the spec: %v", err)
	}
	settleCurrentGeneration(t, issue.service, issue.spec)
}

// answer has alice answer the ask of the spec's block blockID in text.
func (issue *specIssue) answer(blockID, text string) {
	t := issue.t
	t.Helper()
	ask := blockAskOn(t, issue.service, issue.spec, blockID)
	alice := model.Actor{Kind: "user", ID: "alice"}
	at := time.Now().UTC()
	change := asks.Answered(model.AskAnswer{User: "alice", Selected: []string{}, Text: &text, At: at})
	change.Who, change.At = alice, at
	settleAskAs(t, issue.service, issue.spec, blockID, ask.ID, alice, change)
}

// removalsOf is every removed_by tuple of ask's node, in the order they were appended.
func (issue *specIssue) removalsOf(ask string) []graphstore.Tuple {
	issue.t.Helper()
	held, err := graphstore.History(context.Background(), issue.database.Pool, graphstore.KindAsk, ask, graphstore.TupleRemovedBy)
	if err != nil {
		issue.t.Fatalf("read the removals of ask %s: %v", ask, err)
	}
	return held
}

// A repair's markdown is a whole new document, so every ask block it holds is held to the ask
// content rule, as a replacement over a readable document holds each ask it writes: a question
// given only a list is refused, and the unreadable document stays as it was rather than taking
// an ask block the browser editor would drop.
func TestSchemaRepairRefusesAnAskBlockTheContentRuleRefuses(t *testing.T) {
	t.Parallel()
	const broken = ":::ask{#ask-broken}\n- only a list\n- no question\n:::\n"
	for _, test := range []struct {
		name       string
		unreadable bool
	}{
		{"over a readable document", false},
		{"repairing an unreadable document", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := storetest.Open(t)
			artifactID := createDocument(t, database, "")
			service := newSchemaRepairService(database)
			testcleanup.Add(t, func() {
				if err := service.Shutdown(context.Background()); err != nil {
					t.Errorf("shutdown document service: %v", err)
				}
			})
			seedServiceText(t, service, artifactID, "before\n")
			if test.unreadable {
				writeSchemaInvalidElement(t, service, artifactID)
			}
			_, err := joinedReplaceText(service, artifactID, broken, model.Actor{Kind: "user", ID: "alice"})
			var invalid *ErrInvalidAskBlock
			if !errors.As(err, &invalid) {
				t.Fatalf("replace with an ask block holding only a list: %v, want ErrInvalidAskBlock", err)
			}
			text, err := service.Text(context.Background(), artifactID)
			if test.unreadable {
				if !errors.Is(err, ErrDocOutsideSchema) {
					t.Fatalf("read after the refused repair: %q (%v), want ErrDocOutsideSchema", text, err)
				}
			} else if err != nil || text != "before\n" {
				t.Fatalf("read after the refused replacement: %q (%v), want \"before\\n\"", text, err)
			}
		})
	}
}

// A stored task item can pass the tree validator while the renderer rejects it: a checked task
// whose empty first paragraph precedes a heading has no markdown representation the browser reads
// back as a task. It remains repairable instead of failing room creation.
func TestStoredRenderOnlySchemaViolationLoadsForRepair(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	if err := AppendRenderOnlySchemaViolationForTest(context.Background(), database, artifactID); err != nil {
		t.Fatal(err)
	}
	service := newSchemaRepairService(database)
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown render-only service: %v", err)
		}
	})
	for _, read := range []struct {
		name string
		read func() error
	}{
		{"text", func() error {
			_, err := service.Text(context.Background(), artifactID)
			return err
		}},
		{"text with token", func() error {
			_, _, err := service.TextWithToken(context.Background(), artifactID)
			return err
		}},
		{"text with blocks", func() error {
			_, _, err := service.TextWithBlocks(context.Background(), artifactID)
			return err
		}},
		{"blocks", func() error {
			_, err := service.Blocks(context.Background(), artifactID)
			return err
		}},
	} {
		t.Run("cold/"+read.name, func(t *testing.T) {
			if err := read.read(); !errors.Is(err, ErrDocOutsideSchema) {
				t.Fatalf("cold read render-only violation: %v, want ErrDocOutsideSchema", err)
			}
			if cacheHas(service, artifactID) {
				t.Fatal("a document the renderer refuses was cached")
			}
			if service.srv.GetDoc(artifactID) != nil {
				t.Fatal("a cold schema read loaded the document room")
			}
		})
	}
	if _, err := service.BlockPath(context.Background(), artifactID, "absent"); !errors.Is(err, pmdoc.ErrTargetNotFound) {
		t.Fatalf("cold path for absent block: %v, want ErrTargetNotFound", err)
	}
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load render-only violation for repair: %v", err)
	}
	if _, _, err := service.TextWithBlocks(context.Background(), artifactID); !errors.Is(err, ErrDocOutsideSchema) {
		t.Fatalf("read render-only violation: %v, want ErrDocOutsideSchema", err)
	}
}
