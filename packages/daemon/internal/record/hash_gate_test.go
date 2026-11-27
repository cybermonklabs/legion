package record

import (
	"reflect"
	"testing"
	"time"
)

// hashVerdict is Dispatch's verdict at version, current or not, given by the fact at mark.
func hashVerdict(version int, current bool, mark EventMark) *GateVerdict {
	return &GateVerdict{Version: version, Current: current, Mark: mark}
}

// The gate's open rule (dispatch://INFRA-6065, R2). A row in version mode, and a step that stands
// open on an ancestor's approval (OnBasis) in either mode, open on today's rule: an approval of the
// latest version. Every other row in hash mode opens on Dispatch's verdict, which must stand for
// the latest version: a verdict for a version below it is owed its digest (DigestOwed), and the
// row reads closed whatever the verdict before it said, even where the approval it holds is of the
// latest version. A step pending on an ancestor's request is no OnBasis row, so in hash mode it
// reads the verdict on the root's own document. The told standing (ToldStanding) is the stored
// value in hash mode, which an owed digest does not move, so a row closed for the window still
// reads told open; in version mode, and where a hash-mode row stores none, it is GateOpen.
func TestHashGateOpenRule(t *testing.T) {
	registered := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	m40 := EventMark{Seq: 40, At: registered.Add(-time.Hour)}
	onBasis := OpenOnBasis(DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 4}, basisOf("LEGION-100", 3, registered))
	raised := onBasis
	raised.LatestVersion = 5
	hashOnBasis := onBasis
	hashOnBasis.Verdict, hashOnBasis.ToldOpen = hashVerdict(4, false, m40), new(true)
	hashOnBasisOwed := hashOnBasis
	hashOnBasisOwed.Verdict, hashOnBasisOwed.LatestVersion = hashVerdict(4, true, m40), 5
	pending := PendOnBasis(DesignGate{Issue: "LEGION-209", ArtifactID: "artifact-209", LatestVersion: 4}, basisOf("LEGION-100", 3, registered), "ask-1")
	pendingStands := pending
	pendingStands.Verdict, pendingStands.ToldOpen = hashVerdict(4, true, m40), new(true)
	pendingDoesNot := pending
	pendingDoesNot.Verdict, pendingDoesNot.ToldOpen = hashVerdict(4, false, m40), new(false)

	for _, tc := range []struct {
		name             string
		gate             DesignGate
		open, owed, told bool
	}{
		{"version mode, an approval of the latest version", DesignGate{LatestVersion: 4, ApprovedVersion: new(4)}, true, false, true},
		{"version mode, an approval below the latest version", DesignGate{LatestVersion: 5, ApprovedVersion: new(4)}, false, false, false},
		{"version mode, no approval", DesignGate{LatestVersion: 4}, false, false, false},
		{"hash mode, not owed, the approval of 7 stands for 8",
			DesignGate{LatestVersion: 8, ApprovedVersion: new(7), Verdict: hashVerdict(8, true, m40), ToldOpen: new(true)}, true, false, true},
		{"hash mode, not owed, the approval of 8 does not stand",
			DesignGate{LatestVersion: 8, ApprovedVersion: new(8), Verdict: hashVerdict(8, false, m40), ToldOpen: new(false)}, false, false, false},
		{"hash mode, owed, the verdict of 7 before it true",
			DesignGate{LatestVersion: 8, ApprovedVersion: new(7), Verdict: hashVerdict(7, true, m40), ToldOpen: new(true)}, false, true, true},
		{"hash mode, owed, with an approval of the latest version",
			DesignGate{LatestVersion: 8, ApprovedVersion: new(8), Verdict: hashVerdict(7, true, m40), ToldOpen: new(true)}, false, true, true},
		{"hash mode, not owed, no told standing stored",
			DesignGate{LatestVersion: 8, ApprovedVersion: new(7), Verdict: hashVerdict(8, true, m40)}, true, false, true},
		{"hash mode, owed, no told standing stored",
			DesignGate{LatestVersion: 8, ApprovedVersion: new(7), Verdict: hashVerdict(7, true, m40)}, false, true, false},
		{"a step open on a basis, version mode", onBasis, true, false, true},
		{"a step open on a basis, version mode, its latest version above the one registered", raised, false, false, false},
		{"a step open on a basis, hash mode, the root's own verdict false", hashOnBasis, true, false, true},
		{"a step open on a basis, hash mode, its latest version above the one registered and owed", hashOnBasisOwed, false, true, true},
		{"a pending step, version mode", pending, false, false, false},
		{"a pending step, hash mode, the root's own approval stands", pendingStands, true, false, true},
		{"a pending step, hash mode, the root's own approval does not stand", pendingDoesNot, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := GateOpen(tc.gate); got != tc.open {
				t.Errorf("GateOpen = %t, want %t", got, tc.open)
			}
			if got := DigestOwed(tc.gate); got != tc.owed {
				t.Errorf("DigestOwed = %t, want %t", got, tc.owed)
			}
			if got := ToldStanding(tc.gate); got != tc.told {
				t.Errorf("ToldStanding = %t, want %t", got, tc.told)
			}
		})
	}
}

// A basis records the version of the root's own document its registration stood at
// (GateBasis.RootVersion, Departure D2): the latest version the gate holds when the basis is set,
// open or pending, and, for a pending basis that a person's approval of the ancestor's request
// opens, the version that pending registration stood at. The two registrations leave a step a
// person's approval holds open as it is by the told standing, so a digest owed on the root's own
// document does not move that approval into KeptPerson. The store's reader closes a basis whose
// approved version stands below the latest as a release before the basis's move only where no
// digest is owed: in a fill window the latest version rose under this release, and the basis
// waits for the digest.
func TestHashGateBasisTransitionsReadTheToldStanding(t *testing.T) {
	registered := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	personal := EventMark{Seq: 9, At: registered.Add(-48 * time.Hour)}
	m40 := EventMark{Seq: 40, At: registered.Add(-time.Hour)}
	closed := DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4}

	t.Run("an opening records the latest version", func(t *testing.T) {
		if opened := OpenOnBasis(closed, basisOf("LEGION-100", 3, registered)); !opened.OnBasis() || opened.Basis.RootVersion != 4 {
			t.Fatalf("the step a basis opened = %#v, want it on the basis at root version 4", opened)
		}
	})
	t.Run("a pending registration records the latest version", func(t *testing.T) {
		if pending := PendOnBasis(closed, basisOf("LEGION-100", 3, registered), "ask-1"); !pending.PendingOnBasis() || pending.Basis.RootVersion != 4 {
			t.Fatalf("the pending step = %#v, want it pending at root version 4", pending)
		}
	})
	t.Run("a pending basis an approval opens keeps the version its registration stood at", func(t *testing.T) {
		atThree := closed
		atThree.LatestVersion, atThree.Verdict, atThree.ToldOpen = 3, hashVerdict(3, false, m40), new(false)
		pending := PendOnBasis(atThree, basisOf("LEGION-100", 3, registered), "ask-1")
		pending.LatestVersion = 4
		opened := OpenPendingBasis(pending, EventMark{Seq: 44, At: registered.Add(time.Hour)}, "user:bob")
		if !opened.OnBasis() || opened.Basis.RootVersion != 3 {
			t.Fatalf("the step the request's approval opened = %#v, want it on the basis at root version 3, as registered", opened)
		}
	})
	t.Run("an owed digest leaves the person's approval where it stands", func(t *testing.T) {
		standing := closed
		standing.ApprovedVersion, standing.ApprovedMark = new(4), personal
		standing.LatestVersion, standing.Verdict, standing.ToldOpen = 5, hashVerdict(4, true, m40), new(true)
		if GateOpen(standing) {
			t.Fatalf("the step in its fill window reads open, want it closed while the digest of 5 is owed")
		}
		if got := OpenOnBasis(standing, basisOf("LEGION-100", 3, registered)); !reflect.DeepEqual(got, standing) {
			t.Fatalf("a basis over a step whose person's approval stands through a fill = %#v, want the step left on that approval", got)
		}
		if got := PendOnBasis(standing, basisOf("LEGION-100", 3, registered), "ask-1"); !reflect.DeepEqual(got, standing) {
			t.Fatalf("a pending basis over a step whose person's approval stands through a fill = %#v, want the step left on that approval", got)
		}
	})
	t.Run("the reader keeps a basis through a fill and ends one an earlier release's newer version moved", func(t *testing.T) {
		earlier := closed
		earlier.ApprovedVersion, earlier.ApprovedMark = new(2), personal
		opened := OpenOnBasis(earlier, basisOf("LEGION-100", 3, registered))
		window := opened
		window.LatestVersion, window.Verdict, window.ToldOpen = 5, hashVerdict(4, true, m40), new(true)
		if got := basisAsWritten(window); !reflect.DeepEqual(got, window) {
			t.Fatalf("a step on a basis whose root version 5 owes its digest reads as %#v, want its basis standing", got)
		}
		moved := opened
		moved.LatestVersion = 5
		if got := basisAsWritten(moved); got.Basis != nil || got.ApprovedVersion == nil || *got.ApprovedVersion != 2 || got.ApprovedMark != personal {
			t.Fatalf("a step on a basis an earlier release's version 5 moved reads as %#v, want its basis ended on the person's approval of 2", got)
		}
	})
}

// A daemon from before the hash gate writes none of the verdict's columns, so a move it makes
// after a verdict leaves the verdict behind (R2). The reader takes a row whose approval or changes
// request is marked after the verdict as that daemon's move and reads it as the hash gate's rules
// would have made it (digestAsWritten): R-old-1, a changes request of the latest version, gives the
// verdict false; R-old-2, an approval of the latest version, the verdict true, each with that
// review's mark; R-old-3, a new version, leaves no mark and reads owed, so closed; R-old-4, any
// other move, here an approval below the latest version, reads in version mode. The told standing
// then reads as the row opens. A row this release wrote reads as it is: an approval a read
// recorded with a verdict marked no earlier than it, a step open on a basis, whose approval
// carries no mark, and a row in version mode.
func TestHashGateReadsAnOlderDaemonsMoveByTheMarks(t *testing.T) {
	at := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	m10 := EventMark{Seq: 10, At: at.Add(10 * time.Minute)}
	m40 := EventMark{Seq: 40, At: at.Add(40 * time.Minute)}
	m50 := EventMark{Seq: 50, At: at.Add(50 * time.Minute)}
	hash := func(latest int, approved *int, mark EventMark, verdict *GateVerdict, toldOpen bool) DesignGate {
		return DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: latest,
			ApprovedVersion: approved, ApprovedMark: mark, ApprovedDigest: "sha256:approved", Verdict: verdict, ToldOpen: new(toldOpen)}
	}
	named := func(verdict *GateVerdict) *GateVerdict {
		verdict.Digest = "sha256:content"
		return verdict
	}
	changes := hash(7, nil, EventMark{}, named(hashVerdict(7, true, m40)), true)
	changes.ApprovedDigest = ""
	changes.ToldChangesVersion, changes.ToldChangesReason, changes.ToldChangesMark = new(7), "name the rollback path", m50
	onBasis := OpenOnBasis(DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 4},
		basisOf("LEGION-100", 3, at))
	onBasis.Verdict, onBasis.ToldOpen = hashVerdict(4, true, EventMark{At: at}), new(true)
	unmarked := hash(7, new(7), m50, hashVerdict(7, false, EventMark{}), false)
	unmarked.ApprovedDigest = ""

	for _, tc := range []struct {
		name       string
		gate, want DesignGate
	}{
		{"R-old-1 a changes request of the latest version", changes, func() DesignGate {
			want := changes
			want.Verdict, want.ToldOpen = hashVerdict(7, false, m50), new(false)
			return want
		}()},
		{"R-old-2 an approval of the latest version", hash(7, new(7), m50, named(hashVerdict(7, false, m40)), false),
			DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 7,
				ApprovedVersion: new(7), ApprovedMark: m50, Verdict: hashVerdict(7, true, m50), ToldOpen: new(true)}},
		{"R-old-3 a new version", hash(8, new(7), m10, named(hashVerdict(7, true, m40)), true),
			hash(8, new(7), m10, named(hashVerdict(7, true, m40)), true)},
		{"R-old-4 an approval below the latest version", hash(8, new(7), m50, named(hashVerdict(7, true, m40)), true),
			DesignGate{Issue: "LEGION-208", Step: DesignStepSpec, ArtifactID: "artifact-208", LatestVersion: 8,
				ApprovedVersion: new(7), ApprovedMark: m50}},
		{"an approval a read recorded below the verdict it gave", hash(8, new(7), m10, hashVerdict(8, true, EventMark{At: at.Add(time.Hour)}), true),
			hash(8, new(7), m10, hashVerdict(8, true, EventMark{At: at.Add(time.Hour)}), true)},
		{"a step open on a basis, whose approval carries no mark", onBasis, onBasis},
		{"a verdict with no mark, and an approval of the latest version after it", unmarked, func() DesignGate {
			want := unmarked
			want.Verdict, want.ToldOpen = hashVerdict(7, true, m50), new(true)
			return want
		}()},
		{"version mode", DesignGate{Issue: "LEGION-208", LatestVersion: 7, ApprovedVersion: new(7), ApprovedMark: m50},
			DesignGate{Issue: "LEGION-208", LatestVersion: 7, ApprovedVersion: new(7), ApprovedMark: m50}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := digestAsWritten(tc.gate); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("the row reads as %#v (verdict %#v, told %v), want %#v (verdict %#v, told %v)",
					got, got.Verdict, got.ToldOpen, tc.want, tc.want.Verdict, tc.want.ToldOpen)
			}
		})
	}
}
