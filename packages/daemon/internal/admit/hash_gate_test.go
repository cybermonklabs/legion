package admit

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The re-admission reads the gate by Dispatch's verdict (dispatch://INFRA-6065, R5).
//
// keepStaleApproval keeps the person's approval a re-admitted root's gate holds for its next
// architect start only where that approval does not stand, by the gate's own rule
// (record.GateOpen), and not where its version is merely below the latest: an approval of 7 that
// Dispatch reports current for 8 keeps nothing, while one Dispatch reports not current for 8, or
// one whose version 8 still owes its digest (a fill window, where the gate reads closed), is kept
// and named with both versions. An approval of the latest version that does not stand keeps
// nothing either, so no pair ever names a latest version at or below the approved one, which
// readmitted_approvals' check refuses. A step open on an ancestor's approval is read on the
// person's own approval it keeps apart, with the root's own verdict (dispatch://INFRA-3320, R2),
// which is the root's own from the step's entry into hash mode on, an entry on a version of the
// root's own document included (classify.ApplyGateFact).
//
// carriedGate gives the copy it writes onto a handed-over child the old root's verdict, the digest
// of the approval and the standing last told, and marks it a copy (record.DesignGate.Carried).
func TestHashGateReadmissionKeepsTheVerdict(t *testing.T) {
	m10 := record.EventMark{Seq: 10, At: fixedNow.Add(-2 * time.Hour)}
	m20 := record.EventMark{Seq: 20, At: fixedNow.Add(-time.Hour)}
	verdict := func(version int, current bool) *record.GateVerdict {
		return &record.GateVerdict{Version: version, Current: current, Digest: "sha256:content", Mark: m20}
	}

	t.Run("keepStaleApproval", func(t *testing.T) {
		hash := func(latest, approved int, at *record.GateVerdict, told bool) record.DesignGate {
			return record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: latest, ApprovedVersion: new(approved),
				ApprovedMark: m10, Verdict: at, ToldOpen: new(told)}
		}
		basis := record.GateBasis{Issue: "LEGION-UP", ArtifactID: "artifact-up", Version: 3, Approval: record.EventMark{Seq: 40, At: fixedNow},
			Approver: "user:alice", RegisteredAt: fixedNow}
		// The person approved version 2 of LEGION-ROOT's primary document; the basis opened version 4.
		onBasis := func(current bool) record.DesignGate {
			gate := record.OpenOnBasis(record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4,
				ApprovedVersion: new(2), ApprovedMark: m10}, basis)
			gate.Verdict, gate.ToldOpen = verdict(4, current), new(true)
			return gate
		}
		// enteredOnVersion is the step open on the basis in version mode as it takes a replayed
		// artifact.version of 4 with digest_owed false, which enters hash mode
		// (classify.ApplyGateFact) and raises nothing: its entry verdict is the root's own, false,
		// since the person approved 2 and only the basis opened 4.
		enteredOnVersion := classify.ApplyGateFact(record.OpenOnBasis(record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root",
			LatestVersion: 4, ApprovedVersion: new(2), ApprovedMark: m10}, basis),
			classify.GateFact{Kind: classify.DesignGateVersion, Version: 4, DigestOwed: new(false)}).Gate
		for _, tc := range []struct {
			name string
			gate record.DesignGate
			// approved and latest are the pair the re-admission keeps, zeros for none.
			approved, latest int
		}{
			{"an approval of 7 that Dispatch reports current for 8 keeps nothing", hash(8, 7, verdict(8, true), true), 0, 0},
			{"an approval of 7 that Dispatch reports not current for 8 is kept", hash(8, 7, verdict(8, false), false), 7, 8},
			{"an approval of 7 whose version 8 owes its digest is kept", hash(8, 7, verdict(7, true), true), 7, 8},
			{"an approval of the latest version that does not stand keeps nothing", hash(8, 8, verdict(8, false), false), 0, 0},
			{"version mode: an approval of 7 at 8 is kept", record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root",
				LatestVersion: 8, ApprovedVersion: new(7), ApprovedMark: m10}, 7, 8},
			{"a step open on a basis whose root's verdict reports the person's approval of 2 current for 4 keeps nothing", onBasis(true), 0, 0},
			{"a step open on a basis whose root's verdict reports the person's approval of 2 not current for 4 keeps it", onBasis(false), 2, 4},
			{"a step open on a basis that entered hash mode on a version of its own keeps the person's approval of 2, which does not stand for 4",
				enteredOnVersion, 2, 4},
		} {
			t.Run(tc.name, func(t *testing.T) {
				ctx := context.Background()
				pool := migratedPool(t)
				engine := workflow.New(record.NewStore(), workflow.Config{Project: testProject, Repositories: testRepositories,
					Clock: func() time.Time { return fixedNow }, Steps: config.DesignGateRootIssues.Steps()}, nil)
				admission := New(record.NewStore(), engine, 1, testProject, nil)
				admission.now = func() time.Time { return fixedNow }
				parkedRoot(t, pool)
				seedGate(t, pool, tc.gate)
				if got := storedGate(t, pool, "LEGION-ROOT"); !reflect.DeepEqual(got.Verdict, tc.gate.Verdict) {
					t.Fatalf("the seeded gate reads the verdict %#v, want %#v as written", got.Verdict, tc.gate.Verdict)
				}

				readmitRoot(t, pool, admission)

				want := ""
				if tc.approved != 0 {
					want = keptVersions(tc.approved, tc.latest)
				}
				starts := architectStartRows(t, pool)
				if task := architectStart(t, pool, admission, "LEGION-ROOT", want); len(starts) != 1 || starts[0].Task != task {
					t.Fatalf("architect starts %#v, want one whose task is %q", starts, task)
				}
				if kept := starts[0].Kept; tc.approved != 0 && (kept == nil || kept.Approved != tc.approved || kept.Latest != tc.latest) ||
					tc.approved == 0 && kept != nil && kept.Approved != 0 {
					t.Fatalf("the start's kept record %#v, want the pair %d of %d", kept, tc.approved, tc.latest)
				}
				inTx(t, pool, func(tx pgx.Tx) {
					approved, latest, err := record.NewStore().ReadmittedApproval(ctx, tx, "LEGION-ROOT")
					if err != nil || approved != tc.approved || latest != tc.latest {
						t.Fatalf("kept approval after the re-admission = %d, %d, %v; want %d, %d", approved, latest, err, tc.approved, tc.latest)
					}
					if approved != 0 && latest <= approved {
						t.Fatalf("kept approval %d of %d names a latest version at or below the approved one", approved, latest)
					}
				})
			})
		}
	})

	t.Run("carriedGate", func(t *testing.T) {
		hash := func(latest int, at *record.GateVerdict, told *bool) record.DesignGate {
			return record.DesignGate{Issue: carryRoot, ArtifactID: carryRootDoc, LatestVersion: latest, ApprovedVersion: new(7),
				ApprovedMark: m10, ApprovedDigest: "sha256:approved", Verdict: at, ToldOpen: told}
		}
		for _, tc := range []struct {
			name string
			old  record.DesignGate
			open bool
		}{
			{"a step whose approval of 7 Dispatch reports current for 8", hash(8, verdict(8, true), new(true)), true},
			{"a step whose approval of 7 Dispatch reports not current for 8", hash(8, verdict(8, false), new(false)), false},
			{"a step whose version 9 owes its digest, told open", hash(9, verdict(8, true), new(true)), false},
			{"a step in version mode", hash(7, nil, nil), true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := migratedPool(t)
				admission := newAdmission(t, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
				seedCarry(t, pool, nil, tc.old)
				var old record.DesignGate
				inTx(t, pool, func(tx pgx.Tx) {
					gates, err := record.NewStore().GatesOf(context.Background(), tx, carryRoot)
					if err != nil || len(gates) != 1 {
						t.Fatalf("the old root's gates = %#v, %v; want its one spec step", gates, err)
					}
					old = gates[record.DesignStepSpec]
				})
				if !reflect.DeepEqual(old.Verdict, tc.old.Verdict) || !reflect.DeepEqual(old.ToldOpen, tc.old.ToldOpen) ||
					old.ApprovedDigest != tc.old.ApprovedDigest || old.Carried {
					t.Fatalf("the old root's step reads %#v, want it as written: %#v", old, tc.old)
				}

				handOver(t, pool, admission)

				got, ok := childGates(t, pool)[record.DesignStepSpec]
				if !ok {
					t.Fatalf("the child holds no spec step, want the old root's carried")
				}
				if !got.Carried {
					t.Fatalf("the carried step = %#v, want it marked a copy", got)
				}
				if !reflect.DeepEqual(got.Verdict, old.Verdict) || got.ApprovedDigest != old.ApprovedDigest || !reflect.DeepEqual(got.ToldOpen, old.ToldOpen) {
					t.Fatalf("the carried step's verdict %#v, approved digest %q and told standing %v, want the old root's: %#v, %q, %v",
						got.Verdict, got.ApprovedDigest, got.ToldOpen, old.Verdict, old.ApprovedDigest, old.ToldOpen)
				}
				want := old
				want.Issue, want.Carried = carryChild, true
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("the carried step = %#v, want the old root's row on %s, marked a copy: %#v", got, carryChild, want)
				}
				if record.GateOpen(got) != tc.open {
					t.Fatalf("the carried step reads open %t, want %t, as the old root's row read", record.GateOpen(got), tc.open)
				}
			})
		}
	})
}
