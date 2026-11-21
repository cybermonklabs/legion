package docs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/asks"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The fleet guard reads another build by the schema version each live process registered, never
// by the commits among themselves: a process below the version at which answers left the markdown
// (the `decision` type's since) is one whose pages read an ask's state and answer from its block.
// So a fleet of base processes alone, as the registry stands before this process registers or
// after its heartbeat lapses, reads two builds; a fleet of two commits that both read this build's
// version, as a later release deployed over this one serves, reads one; and a process that
// registers no schema number, a build older than the fleet columns, reads two builds wherever it
// stands. A registry holding no process, or this build alone, reads one. And the bound is fixed,
// not this build's own version: a later build at the next schema version, which still holds this
// guard, reads a process of this release beside it as one build, and a base process as two.
func TestTheFleetGuardReadsAnotherBuildByTheSchemaVersionTheProcessesRegister(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	// version nil is a row that registers no schema number, which holds the column's default.
	type process struct {
		commit  string
		version *int
	}
	current, base := pmdoc.SchemaVersion(), baseSchemaVersion
	next := current + 1
	for _, test := range []struct {
		name      string
		fleet     []process
		twoBuilds bool
		// later runs the guard as a build of the next schema version, one that declares an
		// element this build's manifest does not (pmdoc.UseStagedBlockForTest).
		later bool
	}{
		{name: "no process registered"},
		{name: "this build alone", fleet: []process{{"this-release", &current}}},
		{name: "base processes alone", fleet: []process{{"the-release-before", &base}, {"the-release-before", &base}}, twoBuilds: true},
		{name: "this build beside a base process", fleet: []process{{"this-release", &current}, {"the-release-before", &base}}, twoBuilds: true},
		{name: "two commits both at this build's schema version", fleet: []process{{"this-release", &current}, {"the-release-after", &current}}},
		{name: "a process that registers no schema number alone", fleet: []process{{"the-release-before", nil}}, twoBuilds: true},
		{name: "a process that registers no schema number beside this build", fleet: []process{{"this-release", &current}, {"an-older-release", nil}}, twoBuilds: true},
		{name: "a later build at the next schema version beside this build", fleet: []process{{"the-release-after", &next}, {"this-release", &current}}, later: true},
		{name: "a later build at the next schema version beside a base process", fleet: []process{{"the-release-after", &next}, {"the-release-before", &base}}, twoBuilds: true, later: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.later {
				restore, err := pmdoc.UseStagedBlockForTest("future", true)
				if err != nil {
					t.Fatalf("act as a build of the next schema version: %v", err)
				}
				defer restore()
				if pmdoc.SchemaVersion() != next {
					t.Fatalf("the staged schema is at version %d, want %d", pmdoc.SchemaVersion(), next)
				}
			}
			if _, err := database.Pool.Exec(ctx, `delete from server_processes`); err != nil {
				t.Fatalf("empty the registry: %v", err)
			}
			for _, process := range test.fleet {
				var err error
				if process.version == nil {
					_, err = database.Pool.Exec(ctx, `
						insert into server_processes (instance, commit, heartbeat_at)
						values (gen_random_uuid(), $1, now() + interval '1 hour')`, process.commit)
				} else {
					_, err = database.Pool.Exec(ctx, `
						insert into server_processes (instance, commit, schema_version, schema_creatable, heartbeat_at)
						values (gen_random_uuid(), $1, $2, $2, now() + interval '1 hour')`, process.commit, *process.version)
				}
				if err != nil {
					t.Fatalf("register a process of %s: %v", process.commit, err)
				}
			}
			twoBuilds, err := anotherBuildServes(ctx, database.Pool)
			if err != nil {
				t.Fatalf("read the fleet: %v", err)
			}
			if twoBuilds != test.twoBuilds {
				t.Fatalf("anotherBuildServes = %t, want %t", twoBuilds, test.twoBuilds)
			}
		})
	}
}

// While another build serves, settlement weighs against what one upload may hold only the answer
// words its own repairs write into their blocks (repairAskInStep, withholdAnswersWithoutRoom), as
// the release before weighed only a repair that wrote an answer. The document, as settlement reads
// it, is some hundreds of bytes short of the cap. Its block late already holds its answer's 2,000
// characters of words, as the roll window wrote them, and is flagged invalid in the live tree,
// which its repair clears; its block early, before it, shows none of its ask's state, and its
// repair writes the 1,000 characters of words that ask was answered in since. The document has no
// room for early's words, which are withheld: its block says who answered and when. Late keeps the
// words it held, in the tree settlement versions and in the repairs it makes on the live document,
// which stays within the cap. Weighed beside early's, late's words gave the room to early's, and
// the document lost words it already held.
func TestSettlementWithholdsOnlyTheAnswerWordsItsRepairsWrite(t *testing.T) {
	const blocks = ":::ask{#early urgency=\"med\" multiple=\"false\"}\nShip it?\n:::\n\n" +
		":::ask{#late urgency=\"med\" multiple=\"false\"}\nShip what?\n:::\n"
	answered := func(id, login, words string) model.Ask {
		return model.Ask{ID: id, State: asks.StateAnswered, Answer: &model.AskAnswer{
			User: login, Selected: []string{}, Text: &words, At: time.Date(2026, 11, 17, 6, 0, 0, 123456000, time.UTC),
		}}
	}
	early := answered("early-ask", "bob", strings.Repeat("e", 1000))
	late := answered("late-ask", "alice", strings.Repeat("l", 2000))
	// withLateWords parses markdown and writes into its block late the words late held already,
	// as the roll window wrote them while the document had room for them.
	withLateWords := func(markdown string) *pmdoc.Node {
		t.Helper()
		tree, err := pmdoc.ParseRendering(markdown)
		if err != nil {
			t.Fatalf("parse the document: %v", err)
		}
		nodes, _ := askNodes(tree)
		if !askReadableWrite(true, late, "")(nodes["late"]) {
			t.Fatal("the roll window wrote nothing into late's block")
		}
		return tree
	}
	held, err := renderTree(withLateWords(blocks))
	if err != nil {
		t.Fatalf("render the blocks: %v", err)
	}
	prose := strings.TrimSpace(strings.Repeat("word ", (pmdoc.MaxDocumentBytes-500-len(held))/len("word ")))
	tree := withLateWords(prose + "\n\n" + blocks)
	before, err := renderTree(tree)
	if err != nil {
		t.Fatalf("render the document settlement reads: %v", err)
	}
	if room := pmdoc.MaxDocumentBytes - len(before); room < 100 || room > 1000 {
		t.Fatalf("the document settlement reads is %d bytes, %d short of the cap, want 100 to 1,000: room for early's state and none for its words", len(before), room)
	}
	live, err := pmdoc.ParseRendering(before)
	if err != nil {
		t.Fatalf("parse the live document: %v", err)
	}
	read, _ := askNodes(tree)
	standing, _ := askNodes(live)
	for _, flagged := range []*pmdoc.Node{read["late"], standing["late"]} {
		flagged.Attrs["invalid"] = `ask block "late" has an empty question`
	}

	var reconciliation settlementReconciliation
	reconciliation.repairAskInStep(read["early"], "early", true, early, "", nil)
	reconciliation.repairAskInStep(read["late"], "late", true, late, "", nil)
	reconciliation.withholdAnswersWithoutRoom("the document", tree, before)
	if !reconciliation.repairLive(live) {
		t.Fatal("settlement's repairs changed nothing in the live document")
	}
	for name, document := range map[string]*pmdoc.Node{"the tree settlement versions": tree, "the live document": live} {
		shown, _ := askNodes(document)
		if words, _ := shown["late"].Attrs["answer"].(string); words != *late.Answer.Text {
			t.Errorf("%s: late's block holds the words %.40q (%d characters), want the %d it held before settlement", name, words, len(words), len(*late.Answer.Text))
		}
		if reason, flagged := shown["late"].Attrs["invalid"]; flagged {
			t.Errorf("%s: late's block is still flagged %v, want the flag its repair clears", name, reason)
		}
		if words, held := shown["early"].Attrs["answer"].(string); held || shown["early"].Attrs["answered_by"] != "bob" {
			t.Errorf("%s: early's block holds the answerer %v and %d characters of words, want who answered and when, without the words the document has no room for",
				name, shown["early"].Attrs["answered_by"], len(words))
		}
		rendered, err := renderTree(document)
		if err != nil {
			t.Fatalf("%s: render: %v", name, err)
		}
		if len(rendered) > pmdoc.MaxDocumentBytes {
			t.Errorf("%s is %d bytes, past the %d one upload may hold", name, len(rendered), pmdoc.MaxDocumentBytes)
		}
	}
}
