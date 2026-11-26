package docs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
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

// A stored tree the validator refuses for an attribute's value - an ask answered at a time that
// is a person's address, as a crafted client can store it - is refused to its reader with the
// whole refusal, the value quoted, and the room that loads it logs the refusal by its class alone:
// a log line is read by whoever reads the server's logs, which no person's address in a document
// reaches.
func TestAStoredRefusalOfAnAttributesValueIsLoggedWithoutTheValue(t *testing.T) {
	const address = "person-7@example.com"
	logs := &logtest.Recorder{}
	logtest.SetDefault(t, logs)
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "")
	doc := crdt.New()
	fragment := doc.GetXmlFragment(fragmentName)
	doc.Transact(func(transaction *crdt.Transaction) {
		ask := crdt.NewYXmlElement("ask")
		fragment.InsertElement(transaction, 0, ask)
		ask.SetAttributeValue(transaction, "answered_at", address)
		paragraph := crdt.NewYXmlElement("paragraph")
		ask.InsertElement(transaction, 0, paragraph)
		question := crdt.NewYXmlText()
		paragraph.InsertText(transaction, 0, question)
		question.Insert(transaction, 0, "Which one?", nil)
	})
	if _, err := NewPgVersioned(database).AppendUpdate(context.Background(), artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		t.Fatalf("store the ask answered at an address: %v", err)
	}
	service := newSchemaRepairService(database)
	testcleanup.Add(t, func() {
		if err := service.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	if _, err := service.Text(context.Background(), artifactID); !errors.Is(err, ErrDocOutsideSchema) || !strings.Contains(err.Error(), address) {
		t.Fatalf("read the ask answered at an address: %v, want ErrDocOutsideSchema quoting the value", err)
	}
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the ask answered at an address: %v", err)
	}
	const loaded = "dispatch: loaded document outside Proof schema; a replacement from markdown repairs it"
	var lines []string
	for _, record := range logs.Records() {
		line := record.Message
		record.Attrs(func(attribute slog.Attr) bool {
			line += fmt.Sprintf(" %s=%q", attribute.Key, attribute.Value.String())
			return true
		})
		lines = append(lines, line)
		if strings.Contains(line, address) {
			t.Errorf("a log line carries the document's attribute value: %s", line)
		}
	}
	if want := fmt.Sprintf("%s room=%q error=%q", loaded, artifactID, ErrDocSchema.Error()); !strings.Contains(strings.Join(lines, "\n"), want) {
		t.Errorf("the log holds no line %s:\n%s", want, strings.Join(lines, "\n"))
	}
}

// A refusal of an ask or of a check block quotes what it refuses - an ask's question, a check's
// ci_check - and either can be a person's address. None of these refusals is a sentinel errors.Is
// can name: each is an error type, made here by the code a write refuses with, which LoggedError
// names by its type's class (refusalClasses), bare or wrapped with %w as a caller's context wraps
// it, so the line names the class and carries none of the refusal's text. A refusal a caller
// formats with %v is in no chain errors.As reads, and no class can name it (LoggedFault).
func TestARefusalOfAnAskOrACheckIsLoggedByItsClassWithoutItsText(t *testing.T) {
	const address = "person-7@example.com"
	question := "Should " + address + " review it?"
	broken := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
		Type:  "ask",
		Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "a1"},
		Children: []*pmdoc.Node{
			{Type: "paragraph", Children: []*pmdoc.Node{{Type: "text", Text: question}}},
			{Type: "code_block", Children: []*pmdoc.Node{{Type: "text", Text: "x"}}},
		},
	}}}
	reason := validateStoredAskBlocks(broken)
	if reason == nil {
		t.Fatal("an ask holding a code block after its question breaks no rule, want its content rule's refusal")
	}
	// A check proved by CI that carries a run breaks a rule read from its attributes alone
	// (refuseCICheckText), so the refusal reads no database and needs no transaction.
	check := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
		Type:  "check",
		Attrs: pmdoc.Attrs{pmdoc.BlockIDAttr: "c1", "ci_check": address, "run": "go test ./..."},
	}}}
	refusals := []struct {
		name  string
		err   error
		class string
	}{
		{"an ask block breaking its content rule", &ErrInvalidAskBlock{Reason: reason}, "the write would leave an ask block unreadable"},
		{"an ask block carrying the question otherwise", compareAskBlockText(askBlock{question: question}, AskBlockText{Question: "Should we review it?"}), "the ask block cannot carry the text unchanged"},
		{"a check block whose ci_check breaks a rule", refuseCICheckText(introducedCIChecks(check, nil)), "the write leaves a check block whose ci_check breaks a rule"},
	}
	logs := &logtest.Recorder{}
	logtest.SetDefault(t, logs)
	want := map[string]string{}
	for _, refusal := range refusals {
		if refusal.err == nil || !strings.Contains(refusal.err.Error(), address) {
			t.Fatalf("%s: %v, want a refusal quoting the address", refusal.name, refusal.err)
		}
		for _, wrapped := range []struct {
			how string
			err error
		}{
			{"bare", refusal.err},
			{"wrapped with %w", fmt.Errorf("sweep the ask blocks of a closed issue: %w", refusal.err)},
		} {
			line := refusal.name + ", " + wrapped.how
			if got := RefusalClass(wrapped.err); got != refusal.class {
				t.Errorf("%s: RefusalClass = %q, want %q", line, got, refusal.class)
			}
			want[line] = refusal.class
			slog.Warn(line, "error", LoggedError(wrapped.err))
		}
	}
	got := map[string]string{}
	for _, record := range logs.Records() {
		record.Attrs(func(attribute slog.Attr) bool {
			if strings.Contains(attribute.Value.String(), address) {
				t.Errorf("the line %q carries the refusal's text in %s: %q", record.Message, attribute.Key, attribute.Value.String())
			}
			if attribute.Key == "error" {
				got[record.Message] = attribute.Value.String()
			}
			return true
		})
	}
	if !maps.Equal(got, want) {
		t.Errorf("the lines name their refusals as %v, want %v", got, want)
	}
}
