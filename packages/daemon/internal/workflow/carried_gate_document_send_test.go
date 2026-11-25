package workflow

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// rowRecordingNoDocument writes a design_gates row with the columns a daemon before
// design_gates.document_issue names, so the row records no document issue, as every row such a
// daemon writes (a carry it makes during a rollback among them).
func rowRecordingNoDocument(t *testing.T, pool *pgxpool.Pool, issue string, step record.DesignStep, artifact string, latest, approved int) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `insert into design_gates (issue, step, artifact_id, latest_version, approved_version, ever_opened)
		values ($1, $2, $3, $4, $5, $6)`, issue, string(step), artifact, latest, approved, approved == latest); err != nil {
		t.Fatalf("write the gate row of %s %s: %v", issue, step, err)
	}
}

// seedRootCarriedWithItsGate records key, a root of its own a hand-over created at generation 2
// with its old root's gate (handed_over_roots gate_carried), at admitted in the line holding the
// admission slot of index slot, whose handed_over_from names nothing: no reader of a gate's
// document reads it, and the promotion that opens the root's tree clears it.
func seedRootCarriedWithItsGate(t *testing.T, pool *pgxpool.Pool, key, parent string, slot int) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: key, Project: "LEGION", Title: "a carried root", Parent: new(parent), Phase: phase.Admitted, Generation: 2,
		Status: "in_progress", Rank: "V", HandedOver: true, LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: key, Index: slot, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	seedRecord(t, pool, func(tx pgx.Tx) error {
		return record.NewStore().PutHandedOverRoot(t.Context(), tx, record.HandedOverRoot{Issue: key, Generation: 2, GateCarried: true})
	})
}

// dispatchRead is a stand-in for the daemon's read of Dispatch at a send
// (dispatchDocumentIssue): the issue holding each artifact it answers for, and every artifact it
// was asked about. A nil holds answers nothing, as a Dispatch that does not answer.
type dispatchRead struct {
	holds map[string]string
	asked []string
}

func (d *dispatchRead) issueOf(artifact string) (string, bool) {
	d.asked = append(d.asked, artifact)
	issue, ok := d.holds[artifact]
	return issue, ok
}

// startDocuments is the document of each gate step a sent architect-start task gives, by step,
// read from the task's gates rows.
func startDocuments(t *testing.T, task string) map[string]string {
	t.Helper()
	documents := map[string]string{}
	lines := strings.Split(task, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "gates[") || !strings.HasSuffix(line, "]{step,document,version,state}:") {
			continue
		}
		for _, row := range lines[i+1:] {
			cells, indented := strings.CutPrefix(row, "  ")
			if !indented {
				break
			}
			fields := strings.Split(cells, ",")
			if len(fields) != 4 {
				t.Fatalf("the start task's gates row %q is not four cells", row)
			}
			document := fields[1]
			if unquoted, err := strconv.Unquote(document); err == nil {
				document = unquoted
			}
			documents[fields[0]] = document
		}
	}
	return documents
}

// sentDocuments is the document each gate step names, by step, in key's architect-start task and in
// its catch-up, each as the daemon builds it and sends it: the start task as ArchitectStartOf reads
// it and ArchitectStartTask writes it, named at its start (the outbox's NameStartDocuments), and
// the catch-up its architect's ready queues (QueueCatchUp), named at its publish (the outbox's
// NameCatchUpDocuments), both through read. built is the start task's documents as the record
// alone names them, before the send.
func sentDocuments(t *testing.T, pool *pgxpool.Pool, key string, read *dispatchRead) (built, start, catchUp map[string]string) {
	t.Helper()
	var task string
	built = map[string]string{}
	if err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		root, err := record.NewStore().Issue(t.Context(), tx, key)
		if err != nil {
			return err
		}
		derived, err := ArchitectStartOf(t.Context(), tx, record.NewStore(), twoSteps(), *root)
		if err != nil {
			return err
		}
		facts, served := ArchitectStartFacts(derived)
		if !served || facts.Shape != texts.ShapeGateStanding {
			t.Fatalf("the start task of %s = %+v, served %t; want a gate-standing task", key, facts, served)
		}
		for _, gate := range facts.Gates {
			built[gate.Step] = gate.Document
		}
		task = ArchitectStartTask(derived)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	start = startDocuments(t, NameStartDocuments(task, read.issueOf))

	claimReady(t, pool, stepsEngine(twoSteps(), nil), key, claim.RoleArchitect, 1)
	queued := lastCatchUp(t, pool)
	NameCatchUpDocuments(&queued, read.issueOf)
	catchUp = map[string]string{}
	for _, gate := range queued.Gates {
		catchUp[string(gate.Step)] = gate.Document
	}
	return built, start, catchUp
}

// A carried gate's row that records no document issue — one a daemon before the column wrote, a
// carry it made during a rollback among them — is named from the gate's artifact, never from the
// carried root's handed_over_from (which the promotion clears), alike in the architect-start task
// and the catch-up: from the store's rows on that artifact where the old root's own registration
// still stands there, and otherwise from Dispatch where the text is sent, with the bare artifact
// reference where Dispatch does not answer (INFRA-3686 R8 and R2, the architect's ruling on test
// handoff v13's failures 1 and 2). The second and third cases are the re-admission of the old root,
// which clears its rows (record.Store.ClearTreeGeneration); the fourth is a copy of a copy, one
// step on the first old root's document and one on a document the middle root registered itself
// while it was carried in, which the store cannot tell from a copy.
func TestACarriedGatesDocumentIsNamedAlikeInTheStartTaskAndTheCatchUpWhateverWroteItsRow(t *testing.T) {
	criteria, spec := string(record.DesignStepCriteria), string(record.DesignStepSpec)
	const ownCriteria = "artifact-criteria-209"
	for _, tc := range []struct {
		name string
		// chain seeds LEGION-210, carried from LEGION-209, in place of LEGION-209 carried from the
		// old root.
		chain   bool
		cleared bool
		holds   map[string]string
		// built is what the record alone names, sent what both sent texts name, and asked what the
		// send asks Dispatch of, in each of the two texts.
		built, sent map[string]string
		asked       []string
	}{
		{
			name:  "the old root's registration rows stand",
			built: map[string]string{criteria: stepRef(gatedRoot, record.DesignStepCriteria), spec: stepRef(gatedRoot, record.DesignStepSpec)},
			sent:  map[string]string{criteria: stepRef(gatedRoot, record.DesignStepCriteria), spec: stepRef(gatedRoot, record.DesignStepSpec)},
		},
		{
			name: "a re-admission cleared the old root's rows and Dispatch answers", cleared: true,
			holds: map[string]string{criteriaDoc: gatedRoot, gateDoc: gatedRoot},
			built: map[string]string{criteria: texts.ArtifactReference(criteriaDoc), spec: texts.ArtifactReference(gateDoc)},
			sent:  map[string]string{criteria: stepRef(gatedRoot, record.DesignStepCriteria), spec: stepRef(gatedRoot, record.DesignStepSpec)},
			asked: []string{criteriaDoc, gateDoc},
		},
		{
			name: "a re-admission cleared the old root's rows and Dispatch does not answer", cleared: true,
			built: map[string]string{criteria: texts.ArtifactReference(criteriaDoc), spec: texts.ArtifactReference(gateDoc)},
			sent:  map[string]string{criteria: texts.ArtifactReference(criteriaDoc), spec: texts.ArtifactReference(gateDoc)},
			asked: []string{criteriaDoc, gateDoc},
		},
		{
			name: "a copy of a copy, Dispatch answering", chain: true,
			holds: map[string]string{ownCriteria: "LEGION-209"},
			built: map[string]string{criteria: texts.ArtifactReference(ownCriteria), spec: stepRef(gatedRoot, record.DesignStepSpec)},
			sent:  map[string]string{criteria: stepRef("LEGION-209", record.DesignStepCriteria), spec: stepRef(gatedRoot, record.DesignStepSpec)},
			asked: []string{ownCriteria},
		},
		{
			name: "a copy of a copy, Dispatch not answering", chain: true,
			built: map[string]string{criteria: texts.ArtifactReference(ownCriteria), spec: stepRef(gatedRoot, record.DesignStepSpec)},
			sent:  map[string]string{criteria: texts.ArtifactReference(ownCriteria), spec: stepRef(gatedRoot, record.DesignStepSpec)},
			asked: []string{ownCriteria},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
				Tree: gatedRoot, Project: "LEGION", Title: "the old root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U"})
			carried := "LEGION-209"
			if tc.chain {
				// The old root registered the spec alone. LEGION-209, carried from it, registered its
				// own criteria document while it stood carried in, and LEGION-210 was carried from
				// LEGION-209 with a copy of each of its rows.
				rowRecordingNoDocument(t, pool, gatedRoot, record.DesignStepSpec, gateDoc, 4, 4)
				seedRootCarriedWithItsGate(t, pool, "LEGION-209", gatedRoot, 1)
				rowRecordingNoDocument(t, pool, "LEGION-209", record.DesignStepSpec, gateDoc, 4, 4)
				rowRecordingNoDocument(t, pool, "LEGION-209", record.DesignStepCriteria, ownCriteria, 2, 1)
				carried = "LEGION-210"
				seedRootCarriedWithItsGate(t, pool, carried, "LEGION-209", 0)
				rowRecordingNoDocument(t, pool, carried, record.DesignStepSpec, gateDoc, 4, 4)
				rowRecordingNoDocument(t, pool, carried, record.DesignStepCriteria, ownCriteria, 2, 1)
			} else {
				seedRootCarriedWithItsGate(t, pool, carried, gatedRoot, 0)
				for _, issue := range []string{gatedRoot, carried} {
					rowRecordingNoDocument(t, pool, issue, record.DesignStepCriteria, criteriaDoc, 2, 1)
					rowRecordingNoDocument(t, pool, issue, record.DesignStepSpec, gateDoc, 4, 4)
				}
			}
			if tc.cleared {
				seedRecord(t, pool, func(tx pgx.Tx) error {
					return record.NewStore().ClearTreeGeneration(t.Context(), tx, gatedRoot, time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC))
				})
			}

			read := &dispatchRead{holds: tc.holds}
			built, start, catchUp := sentDocuments(t, pool, carried, read)
			for _, step := range []string{criteria, spec} {
				if built[step] != tc.built[step] {
					t.Errorf("the record names the %s step's document of %s %q before the send, want %q", step, carried, built[step], tc.built[step])
				}
				if start[step] != tc.sent[step] || catchUp[step] != tc.sent[step] {
					t.Errorf("the sent start task of %s names the %s step's document %q and its catch-up %q, want both %q",
						carried, step, start[step], catchUp[step], tc.sent[step])
				}
			}
			if want := append(append([]string{}, tc.asked...), tc.asked...); strings.Join(read.asked, " ") != strings.Join(want, " ") {
				t.Errorf("the sends asked Dispatch of %v, want %v: once in each text for each document the record does not name, and of nothing else",
					read.asked, want)
			}
		})
	}
}
