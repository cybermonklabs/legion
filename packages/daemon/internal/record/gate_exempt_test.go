package record

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// A gate is open when a person's approval stands on its current version, or when its current
// version is the version that proposes nothing and so opened with no person (R23). It is exempt
// only under the rule of a change request, at its current version: an exemption of an older
// version, or one recorded under the rule of a person's approval, opens nothing.
func TestAGateOpensByApprovalOrByTheExemptionOfItsCurrentVersion(t *testing.T) {
	three, four := 3, 4
	for _, tc := range []struct {
		name         string
		gate         DesignGate
		open, exempt bool
	}{
		{"a gate nobody has reviewed", DesignGate{LatestVersion: 4}, false, false},
		{"a person's approval of the current version", DesignGate{LatestVersion: 4, ApprovedVersion: &four}, true, false},
		{"the exemption of the current version", DesignGate{LatestVersion: 4, Rule: GateRuleChangeRequest, ExemptVersion: &four}, true, true},
		{"the exemption of an older version", DesignGate{LatestVersion: 4, Rule: GateRuleChangeRequest, ExemptVersion: &three}, false, false},
		{"both at the current version", DesignGate{LatestVersion: 4, Rule: GateRuleChangeRequest, ApprovedVersion: &four, ExemptVersion: &four}, true, true},
		{"an exemption under the rule of a person's approval", DesignGate{LatestVersion: 4, ExemptVersion: &four}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GateOpen(tc.gate); got != tc.open {
				t.Errorf("GateOpen(%+v) = %t, want %t", tc.gate, got, tc.open)
			}
			if got := GateExempt(tc.gate); got != tc.exempt {
				t.Errorf("GateExempt(%+v) = %t, want %t", tc.gate, got, tc.exempt)
			}
			if got := (TreeGate{Steps: []DesignStep{DesignStepSpec}, Rows: map[DesignStep]DesignGate{DesignStepSpec: tc.gate}}).Open(); got != tc.open {
				t.Errorf("the tree's gate over %+v opens = %t, want %t", tc.gate, got, tc.open)
			}
		})
	}
}

// A rule is written as its word: a row written before the rule existed, and every gate built with
// no rule, is under the rule of a person's approval.
func TestAGateRuleIsWrittenAsItsWord(t *testing.T) {
	for rule, word := range map[GateRule]string{GateRuleApproval: "approval", GateRuleChangeRequest: "change_request"} {
		if got := rule.Word(); got != word {
			t.Errorf("rule %q is written %q, want %q", rule, got, word)
		}
	}
	if (DesignGate{}).Rule != GateRuleApproval {
		t.Fatalf("a gate built with no rule is under %q, want the rule of a person's approval", (DesignGate{}).Rule)
	}
}

// A gate keeps the rule it started under and the version that opened with no person, and a row
// written before either existed (the release before writes neither column) reads back under the
// rule of a person's approval with no exemption.
func TestGateRoundTripsItsRuleAndItsExemption(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	key := fmt.Sprintf("%s-%d", "ACME", 31)
	issue := Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: "ACME", Title: "a root", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "A"}
	five := 5
	exempt := DesignGate{Issue: key, Step: DesignStepSpec, ArtifactID: "the-spec", LatestVersion: 5, Rule: GateRuleChangeRequest,
		ExemptVersion: &five, EverOpened: true}
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		must(t, records.PutGate(ctx, tx, exempt))
		gates, err := records.GatesOf(ctx, tx, key)
		must(t, err)
		if got := SpecGate(gates); !reflect.DeepEqual(got, &exempt) {
			t.Fatalf("gate = %#v, want %#v", got, exempt)
		}
		if _, err := tx.Exec(ctx, `insert into design_gates (issue, step, artifact_id, latest_version) values ($1, 'criteria', 'the-criteria', 2)`, key); err != nil {
			t.Fatalf("write a gate as the release before does: %v", err)
		}
		gates, err = records.GatesOf(ctx, tx, key)
		must(t, err)
		if got, want := gates[DesignStepCriteria], (DesignGate{Issue: key, Step: DesignStepCriteria, ArtifactID: "the-criteria", LatestVersion: 2}); !reflect.DeepEqual(got, want) {
			t.Fatalf("a gate the release before wrote = %#v, want %#v", got, want)
		}
	})
}

// The daemon keeps one reading of the ending condition per issue: the head it was read at, the
// state Dispatch answered and each reason code the architect was told of at that head. A reading
// replaces the one before it whole, and an issue never read has none.
func TestSpecEndingKeepsTheLastReadingOfAnIssue(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	key := fmt.Sprintf("%s-%d", "ACME", 33)
	issue := Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: key, Project: "ACME", Title: "a root", Phase: phase.Merging, Generation: 1, Status: "in_progress", Rank: "A"}
	read := time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)
	inTx(t, st, func(tx pgx.Tx) {
		must(t, records.PutIssue(ctx, tx, issue))
		if got, err := records.SpecEnding(ctx, tx, key); err != nil || got != nil {
			t.Fatalf("the ending of an issue never read = %+v, %v; want none", got, err)
		}
		waiting := SpecEnding{Issue: key, Head: "first-head", State: SpecEndingWaiting, Told: []string{"no_conclusion"}, ReadAt: read}
		must(t, records.PutSpecEnding(ctx, tx, waiting))
		if got, err := records.SpecEnding(ctx, tx, key); err != nil || !reflect.DeepEqual(got, &waiting) {
			t.Fatalf("the ending read back = %+v, %v; want %+v", got, err, waiting)
		}
		if !waiting.Waits() {
			t.Fatalf("a waiting reading does not wait")
		}
		ready := SpecEnding{Issue: key, Head: "second-head", State: SpecEndingReady, Told: []string{}, ReadAt: read.Add(time.Minute)}
		must(t, records.PutSpecEnding(ctx, tx, ready))
		if got, err := records.SpecEnding(ctx, tx, key); err != nil || !reflect.DeepEqual(got, &ready) {
			t.Fatalf("the ending after a second reading = %+v, %v; want %+v", got, err, ready)
		}
		if ready.Waits() {
			t.Fatalf("a ready reading waits")
		}
		if err := records.PutSpecEnding(ctx, tx, SpecEnding{Issue: key, Head: "third-head", State: "maybe", ReadAt: read}); err == nil {
			t.Fatalf("a reading of a state Dispatch never answers was written")
		}
	})
}
