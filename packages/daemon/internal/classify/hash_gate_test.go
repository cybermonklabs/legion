package classify

//ci:reads packages/contracts/fixtures/hash-gate/** hashGateFact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The values the hash-gate fixtures carry (packages/contracts/fixtures/hash-gate): the document's
// id, and the digests artifact-digest.json names, of the record of the version it is for
// (content_sha256) and of the approved version's (approved_sha256), which match. The fixtures hold
// no digest whose approval is not current: notCurrent's is artifact-digest.json with
// approval_current false, no approved-version field and notCurrentDigest, a record's digest other
// than the approved one, for its content_sha256.
const (
	hashGateArtifact   = "00000000-0000-4000-8000-000000000001"
	currentDigest      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	currentApproved    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	notCurrentDigest   = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	hashGateIssue      = "ACME-7"
	hashGateProject    = "ACME"
	hashGateReviewText = "name the rollback path"
)

// hashGateBase is the time a mark's sequence counts from: the plan's mark mN is the sequence N at
// hashGateBase plus N seconds (hashGateMark), and tN that time alone (hashGateAt).
var hashGateBase = time.Date(2026, 11, 27, 18, 0, 0, 0, time.UTC)

func hashGateMark(n int64) record.EventMark {
	return record.EventMark{Seq: n, At: hashGateBase.Add(time.Duration(n) * time.Second)}
}

func hashGateAt(n int64) record.EventMark {
	return record.EventMark{At: hashGateBase.Add(time.Duration(n) * time.Second)}
}

// hashGateFact is the fact one event file of packages/contracts/fixtures/hash-gate, the wire
// contract of Dispatch's hash gate (R1 of dispatch://INFRA-6065), comes to. The file holds the
// event's type and payload alone: the payload, changed by change, is wrapped in the envelope the
// boot catch-up decodes (intake.DecodeDispatchEvent), at sequence seq and the time that goes with
// it (hashGateMark), made by Dispatch's fill (a system actor) for a digest and by the architect's
// session for a version, and the fact is taken as the workflow takes a live DispatchArtifact
// (gateFactOf): the verdict's digest is a digest's content_sha256, never an artifact.version's,
// which is the digest of the version's text. A row whose fact the files hold but for its version
// or its place is built from the file, so the file's shape is what the transition reads.
func hashGateFact(t *testing.T, name string, seq int64, change func(payload map[string]any)) GateFact {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "hash-gate", name))
	if err != nil {
		t.Fatalf("read the hash-gate fixture %s: %v", name, err)
	}
	var file map[string]any
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode the hash-gate fixture %s: %v", name, err)
	}
	eventType, _ := file["type"].(string)
	payload, ok := file["payload"].(map[string]any)
	if eventType == "" || !ok || len(file) != 2 {
		t.Fatalf("the hash-gate fixture %s is %s, want an event's type and payload alone", name, raw)
	}
	change(payload)
	actor := map[string]string{"kind": "session", "id": "session-architect"}
	if eventType == "artifact.digest" {
		actor = map[string]string{"kind": "system", "id": "digest-fill"}
	}
	encoded, err := json.Marshal(map[string]any{
		"id": 9000 + seq, "issue_key": hashGateIssue, "artifact_id": nil, "project": hashGateProject, "seq": seq, "type": eventType,
		"actor": actor, "notify": true, "created_at": hashGateMark(seq).At.Format(time.RFC3339Nano), "payload": payload,
	})
	if err != nil {
		t.Fatalf("encode the event of %s: %v", name, err)
	}
	decoded, _, err := intake.DecodeDispatchEvent(hashGateProject, hashGateIssue, encoded)
	if err != nil {
		t.Fatalf("decode the event of %s: %v", name, err)
	}
	fact, ok := decoded.(intake.DispatchArtifact)
	if !ok {
		t.Fatalf("the event of %s decoded to %#v, want an artifact fact", name, decoded)
	}
	digest := ""
	if fact.Kind == intake.DispatchArtifactDigest {
		digest = fact.ContentSHA256
	}
	return GateFact{Kind: DesignGateEventKind(fact.Kind), Version: fact.Version, Mark: fact.Mark, DigestOwed: fact.DigestOwed,
		Verdict: fact.Verdict, Digest: digest, ApprovedSHA256: fact.ApprovedSHA256}
}

// assertVerdictAfterReviews holds a row to the rule the store's reader rests on (record's
// digestAsWritten, which classify cannot call): no approval and no told changes request a row in
// hash mode holds is marked after its verdict (record.EventMark.After), or the reader takes the row
// for a move of a daemon from before the hash gate. A changes request the fact applied is recorded
// as told first, as the workflow records it (applyArtifact, gateRegistered: RecordChangesTold).
func assertVerdictAfterReviews(t *testing.T, gate record.DesignGate, fact GateFact, result GateFactResult) {
	t.Helper()
	if fact.Kind == DesignGateChangesRequested && result.Changed {
		gate = RecordChangesTold(gate, fact.Version, hashGateReviewText, fact.Mark)
	}
	if gate.Verdict == nil {
		return
	}
	for _, review := range []struct {
		name    string
		version *int
		mark    record.EventMark
	}{{"approval", gate.ApprovedVersion, gate.ApprovedMark}, {"told changes request", gate.ToldChangesVersion, gate.ToldChangesMark}} {
		if review.version != nil && review.mark != (record.EventMark{}) && review.mark.After(gate.Verdict.Mark) {
			t.Errorf("the %s of version %d is marked %#v, after the verdict's mark %#v: the store would read the row as an older daemon's move",
				review.name, *review.version, review.mark, gate.Verdict.Mark)
		}
	}
}

// TestHashGateAppliesFactsInOrder applies each fact of the plan's state table (rows C1 to C14 of
// dispatch://INFRA-6065's plan) to the row it names, through the one transition function
// (ApplyGateFact), by R3's rules: the entry into hash mode from a version, a digest and a read,
// with the entry verdict's mark and the narrowed tie-breaker (D4), and the changes-request guard
// (D5); the return to version mode; a version that owes its digest; a digest, stale, late, or
// above the latest version; a review at, above and below the latest version; a read that records
// its approval below the latest version and gives its verdict at its latest version, or none
// where the digest is owed. Each row asserts the row after, its open, owed and told standing, and
// what the caller must do. C3b/unmarked is the entry of a row whose review has no mark, C3e the
// read that shows the approval not standing (the architect's addition 1,
// dispatch://INFRA-6065/comment/6680734f-f581-444b-9c79-66e4a52e3e0a), C12b the person's approval
// D5 still takes, C15a to C15c the rows that hold the verdict's mark no earlier than each review
// the row records, and CB1 to CB6 the rows of a step on an ancestor's basis.
// C14 replays every classification fixture through the function with no digest field, which is
// ApplyDesignGateEvent exactly.
func TestHashGateAppliesFactsInOrder(t *testing.T) {
	m, at := hashGateMark, hashGateAt
	owed, notOwed := new(true), new(false)
	row := func(latest int) record.DesignGate {
		return record.DesignGate{Issue: hashGateIssue, Step: record.DesignStepSpec, ArtifactID: hashGateArtifact, LatestVersion: latest}
	}
	approved := func(gate record.DesignGate, version int, mark record.EventMark) record.DesignGate {
		gate.ApprovedVersion, gate.ApprovedMark = new(version), mark
		return gate
	}
	changes := func(gate record.DesignGate, version int, mark record.EventMark) record.DesignGate {
		return RecordChangesTold(gate, version, hashGateReviewText, mark)
	}
	hashed := func(gate record.DesignGate, version int, current bool, digest string, mark record.EventMark, told bool) record.DesignGate {
		gate.Verdict = &record.GateVerdict{Version: version, Current: current, Digest: digest, Mark: mark}
		gate.ToldOpen = new(told)
		return gate
	}
	withApprovedDigest := func(gate record.DesignGate, digest string) record.DesignGate {
		gate.ApprovedDigest = digest
		return gate
	}
	version := func(number int, owes *bool, seq int64) GateFact {
		return hashGateFact(t, "artifact-version.json", seq, func(payload map[string]any) {
			payload["version"].(map[string]any)["number"] = number
			if owes == nil {
				delete(payload, "digest_owed")
				delete(payload, "content_sha256")
				return
			}
			payload["digest_owed"] = *owes
		})
	}
	current := func(number int, seq int64) GateFact {
		return hashGateFact(t, "artifact-digest.json", seq, func(payload map[string]any) { payload["version"] = number })
	}
	notCurrent := func(number int, seq int64) GateFact {
		return hashGateFact(t, "artifact-digest.json", seq, func(payload map[string]any) {
			payload["version"], payload["approval_current"], payload["content_sha256"] = number, false, notCurrentDigest
			delete(payload, "approved_version")
			delete(payload, "approved_sha256")
		})
	}
	review := func(kind DesignGateEventKind, number int, mark record.EventMark) GateFact {
		return GateFact{Kind: kind, Version: number, Mark: mark}
	}
	// read is a read of Dispatch's approval, as artifact.json and artifact-owed.json show one
	// (built by hand: classify reads no GET body): the review it shows (kind, version, its mark),
	// its latest version, digest_owed and, where that is false, its verdict at the latest version
	// with the read's mark of it and the latest version's digest (current_sha256).
	read := func(kind DesignGateEventKind, number int, mark record.EventMark, latest int, owes *bool, verdict *bool, verdictMark record.EventMark) GateFact {
		fact := GateFact{Kind: kind, Version: number, Mark: mark, Read: true, Latest: latest, DigestOwed: owes, Verdict: verdict, VerdictMark: verdictMark}
		if owes != nil && !*owes {
			fact.Digest = currentDigest
		}
		return fact
	}

	// A step open on ancestor ACME-1's approval, registered at version 3 of the root's own
	// document, which a person approved at version 2 (m9) before the basis: the basis keeps that
	// approval apart (KeptPerson).
	basis := record.GateBasis{Issue: "ACME-1", ArtifactID: "artifact-1", Version: 4, Approval: m(5), Approver: "user:person",
		Reason: "the root's checks serve r1", Session: "ses_architect_7", RegisteredAt: hashGateBase}
	onBasis := record.OpenOnBasis(approved(row(3), 2, m(9)), basis)
	// CB1 to CB2b: version 4 of the root's own document owes its digest, then its digest settles it.
	basisOwed := onBasis
	basisOwed.LatestVersion = 4
	basisOwed = hashed(basisOwed, 3, true, "", m(9), true)
	basisClosed := hashed(approved(row(4), 2, m(9)), 4, false, notCurrentDigest, m(30), false)
	basisClosed.RestsOn = []string{"ACME-1"}
	preMigration := basisOwed
	preMigration.Basis = new(*basisOwed.Basis)
	preMigration.Basis.RootVersion = 0
	// CB3 and CB6: the step open on the basis in hash mode, its own verdict true at version 3.
	basisHashed := hashed(onBasis, 3, true, "", m(9), true)
	basisFalse := basisHashed
	basisFalse.Verdict = &record.GateVerdict{Version: 3, Current: false, Digest: notCurrentDigest, Mark: m(30)}
	reviewedOnBasis := hashed(approved(row(3), 3, m(40)), 3, true, "", m(40), true)
	reviewedOnBasis.RestsOn = []string{"ACME-1"}
	// CB4 and CB4b: a step pending on ACME-1's request, registered at root version 3 while the
	// root's digest of 3 was owed: the root's verdict at 2 is false (an edit of the requirements
	// after the person's approval of version 1, m9), so it was told closed.
	pending := record.PendOnBasis(hashed(approved(row(3), 1, m(9)), 2, false, "", m(20), false), basis, "ask-1")
	pendingOpened := withApprovedDigest(hashed(approved(row(3), 1, m(9)), 3, true, currentDigest, m(30), true), currentApproved)
	pendingFalse := pending
	pendingFalse.Verdict = &record.GateVerdict{Version: 3, Current: false, Digest: notCurrentDigest, Mark: m(30)}
	// CB5 and CB5b: a pending row written before the hash gate's migration (no root version), in
	// version mode at 3 with the person's approval of 2.
	pendingOld := record.PendOnBasis(approved(row(3), 2, m(9)), basis, "ask-1")
	pendingOld.Basis = new(*pendingOld.Basis)
	pendingOld.Basis.RootVersion = 0
	pendingOldOwed := hashed(pendingOld, 3, false, "", m(9), false)
	pendingOldOwed.LatestVersion = 4

	// C4 and C5: approved at 6 (m10), open, then versions 7 and 8 owe their digests.
	owedTwice := hashed(approved(row(8), 6, m(10)), 6, true, "", m(10), true)
	afterC4 := hashed(approved(row(8), 6, m(10)), 7, false, notCurrentDigest, m(30), true)
	hashOpen := hashed(approved(row(6), 6, m(10)), 6, true, "", m(10), true)
	changesAt6 := hashed(changes(row(6), 6, m(50)), 6, false, "", m(50), false)

	for _, tc := range []struct {
		id     string
		before record.DesignGate
		fact   GateFact
		want   record.DesignGate
		// open, owed and told are record.GateOpen, record.DigestOwed and record.ToldStanding of
		// the row after.
		open, owed, told bool
		seed, settled    bool
		changed          bool
	}{
		{id: "C1", before: approved(row(5), 5, m(10)), fact: version(6, owed, 11),
			want: hashed(approved(row(6), 5, m(10)), 5, true, "", m(10), true), owed: true, told: true, changed: true},
		{id: "C2", before: approved(row(6), 5, m(10)), fact: current(6, 20),
			want: withApprovedDigest(hashed(approved(row(6), 5, m(10)), 6, true, currentDigest, m(20), true), currentApproved),
			open: true, told: true, changed: true},
		{id: "C2b", before: approved(row(8), 8, m(20)), fact: notCurrent(7, 15),
			want: hashed(approved(row(8), 8, m(20)), 8, true, "", m(20), true), open: true, told: true, changed: true},
		{id: "C2c/digest", before: changes(row(8), 8, m(30)), fact: current(8, 25),
			want: hashed(changes(row(8), 8, m(30)), 8, false, "", m(30), false), changed: true},
		{id: "C2c/read", before: changes(row(8), 8, m(30)), fact: read(DesignGateApproved, 8, m(25), 8, notOwed, new(true), at(25)),
			want: hashed(changes(row(8), 8, m(30)), 8, false, "", m(30), false), changed: true},
		{id: "C2d/digest", before: approved(row(8), 8, m(30)), fact: notCurrent(8, 25),
			want: hashed(approved(row(8), 8, m(30)), 8, true, "", m(30), true), open: true, told: true, changed: true},
		{id: "C2d/read", before: approved(row(8), 8, m(30)), fact: read(DesignGateVersion, 8, record.EventMark{}, 8, notOwed, new(false), at(25)),
			want: hashed(approved(row(8), 8, m(30)), 8, true, "", m(30), true), open: true, told: true, changed: true},
		{id: "C3", before: approved(row(6), 6, m(10)), fact: read(DesignGateApproved, 6, m(10), 6, notOwed, new(true), at(15)),
			want: hashed(approved(row(6), 6, m(10)), 6, true, currentDigest, at(15), true), open: true, told: true, changed: true},
		{id: "C3b/later", before: approved(row(8), 7, m(10)), fact: read(DesignGateApproved, 7, m(10), 8, notOwed, new(true), at(20)),
			want: hashed(approved(row(8), 7, m(10)), 8, true, currentDigest, at(20), true), open: true, told: true, changed: true},
		{id: "C3b/equal", before: approved(row(8), 7, m(10)), fact: read(DesignGateApproved, 7, m(10), 8, notOwed, new(true), at(10)),
			want: hashed(approved(row(8), 7, m(10)), 8, true, currentDigest, at(10), true), open: true, told: true, changed: true},
		// C3b/unmarked: the row holds its approval with no mark (a read that gave none), so its entry
		// verdict has none, which EventMark.After would place after a read's time; no review stands
		// to be after, and the read's verdict is taken (D4: no mark where the row holds none).
		{id: "C3b/unmarked", before: approved(row(8), 7, record.EventMark{}),
			fact: read(DesignGateApproved, 7, record.EventMark{}, 8, notOwed, new(true), at(20)),
			want: hashed(approved(row(8), 7, record.EventMark{}), 8, true, currentDigest, at(20), true), open: true, told: true, changed: true},
		{id: "C3c", before: hashed(approved(row(8), 7, m(10)), 8, false, "", m(20), false),
			fact: read(DesignGateApproved, 7, m(10), 8, notOwed, new(true), at(15)),
			want: hashed(approved(row(8), 7, m(10)), 8, false, "", m(20), false)},
		{id: "C3d", before: hashed(approved(row(8), 7, m(10)), 8, false, "", m(20), false),
			fact: read(DesignGateApproved, 7, m(10), 8, notOwed, new(true), at(25)),
			want: hashed(approved(row(8), 7, m(10)), 8, true, currentDigest, at(25), true), open: true, told: true, changed: true},
		{id: "C3e", before: hashed(approved(row(7), 7, m(20)), 7, true, "", m(20), true),
			fact: read(DesignGateVersion, 8, record.EventMark{}, 8, notOwed, new(false), at(40)),
			want: hashed(approved(row(8), 7, m(20)), 8, false, currentDigest, at(40), false), settled: true, changed: true},
		{id: "C4", before: owedTwice, fact: notCurrent(7, 30), want: afterC4, owed: true, told: true, changed: true},
		{id: "C5", before: afterC4, fact: current(8, 31),
			want: withApprovedDigest(hashed(approved(row(8), 6, m(10)), 8, true, currentDigest, m(31), true), currentApproved),
			open: true, told: true, settled: true, changed: true},
		{id: "C6", before: hashed(changes(row(6), 6, m(25)), 6, false, "", m(25), false), fact: current(6, 22),
			want: hashed(changes(row(6), 6, m(25)), 6, false, "", m(25), false)},
		{id: "C7", before: hashOpen, fact: version(5, nil, 12), want: hashOpen, open: true, told: true},
		{id: "C8", before: hashOpen, fact: version(7, nil, 12), want: approved(row(7), 6, m(10)), changed: true},
		{id: "C9", before: hashOpen, fact: current(7, 30),
			want: withApprovedDigest(hashed(approved(row(7), 6, m(10)), 7, true, currentDigest, m(30), true), currentApproved),
			open: true, told: true, settled: true, changed: true},
		{id: "C10", before: hashed(approved(row(6), 5, m(10)), 6, false, notCurrentDigest, m(30), false), fact: review(DesignGateApproved, 6, m(40)),
			want: hashed(approved(row(6), 6, m(40)), 6, true, "", m(40), true), open: true, told: true, changed: true},
		{id: "C10b", before: withApprovedDigest(hashed(approved(row(6), 6, m(40)), 6, true, "", m(40), true), currentApproved),
			fact: review(DesignGateChangesRequested, 6, m(60)), want: hashed(row(6), 6, false, "", m(60), false), changed: true},
		{id: "C11/changes_requested", before: hashed(approved(row(7), 6, m(10)), 7, true, "", m(20), true),
			fact: review(DesignGateChangesRequested, 6, m(30)), want: hashed(approved(row(7), 6, m(10)), 7, true, "", m(20), true),
			open: true, told: true, seed: true},
		{id: "C11/approved", before: hashed(approved(row(7), 6, m(10)), 7, false, "", m(20), false),
			fact: review(DesignGateApproved, 6, m(30)), want: hashed(approved(row(7), 6, m(10)), 7, false, "", m(20), false), seed: true},
		{id: "C12", before: changesAt6, fact: current(6, 55), want: changesAt6},
		{id: "C12b", before: changesAt6, fact: review(DesignGateApproved, 6, m(60)),
			want: hashed(approved(row(6), 6, m(60)), 6, true, "", m(60), true), open: true, told: true, changed: true},
		{id: "C13", before: hashOpen, fact: review(DesignGateApproved, 8, m(40)),
			want: hashed(approved(row(8), 8, m(40)), 8, true, "", m(40), true), open: true, told: true, changed: true},
		// C15a: a read whose digest is owed records an approval below the latest version and gives
		// no verdict, so the verdict at 6 takes that approval's mark.
		{id: "C15a", before: hashed(approved(row(6), 6, m(20)), 6, true, "", m(20), true),
			fact: read(DesignGateApproved, 7, m(40), 8, owed, nil, record.EventMark{}),
			want: hashed(approved(row(8), 7, m(40)), 6, true, "", m(40), true), owed: true, told: true, changed: true},
		// C15b: a review marked by its sequence alone is after a read's verdict marked by its time
		// alone (EventMark.After), so the verdict takes the review's mark.
		{id: "C15b", before: hashed(row(8), 8, false, "", m(5), false),
			fact: read(DesignGateApproved, 8, record.EventMark{Seq: 30}, 8, notOwed, new(true), at(40)),
			want: hashed(approved(row(8), 8, record.EventMark{Seq: 30}), 8, true, currentDigest, record.EventMark{Seq: 30}, true),
			open: true, told: true, changed: true},
		// C15c: the verdict a row that held no review entered with has no mark, which every mark
		// is after; a read's changes request while the digest is owed, which the workflow records
		// as told, gives the verdict its mark.
		{id: "C15c", before: hashed(row(6), 5, false, "", record.EventMark{}, false),
			fact: read(DesignGateChangesRequested, 6, m(40), 6, owed, nil, record.EventMark{}),
			want: hashed(row(6), 5, false, "", m(40), false), owed: true, changed: true},
		{id: "CB1", before: onBasis, fact: version(4, owed, 20), want: basisOwed, owed: true, told: true, changed: true},
		{id: "CB2", before: basisOwed, fact: notCurrent(4, 30), want: basisClosed, settled: true, changed: true},
		{id: "CB2b", before: preMigration, fact: notCurrent(4, 30), want: basisClosed, settled: true, changed: true},
		{id: "CB3", before: basisHashed, fact: notCurrent(3, 30), want: basisFalse, open: true, told: true, changed: true},
		{id: "CB4", before: pending, fact: current(3, 30), want: pendingOpened, open: true, told: true, settled: true, changed: true},
		{id: "CB4b", before: pending, fact: notCurrent(3, 30), want: pendingFalse, settled: true, changed: true},
		{id: "CB5", before: pendingOld, fact: version(4, owed, 20), want: pendingOldOwed, owed: true, changed: true},
		{id: "CB5b", before: pendingOldOwed, fact: notCurrent(4, 30),
			want: hashed(approved(row(4), 2, m(9)), 4, false, notCurrentDigest, m(30), false), settled: true, changed: true},
		{id: "CB6", before: basisHashed, fact: review(DesignGateApproved, 3, m(40)), want: reviewedOnBasis, open: true, told: true, changed: true},
	} {
		t.Run(tc.id, func(t *testing.T) {
			got := ApplyGateFact(tc.before, tc.fact)
			if !reflect.DeepEqual(got.Gate, tc.want) {
				t.Fatalf("the row after %#v\n got: %s\nwant: %s", tc.fact, describeGate(got.Gate), describeGate(tc.want))
			}
			if open, owes, told := record.GateOpen(got.Gate), record.DigestOwed(got.Gate), record.ToldStanding(got.Gate); open != tc.open || owes != tc.owed || told != tc.told {
				t.Errorf("the row after reads open %t, owed %t, told open %t; want %t, %t, %t", open, owes, told, tc.open, tc.owed, tc.told)
			}
			if got.SeedWanted != tc.seed || got.Settled != tc.settled || got.Changed != tc.changed {
				t.Errorf("the fact asks the gate seed %t, settles the row %t, changes it %t; want %t, %t, %t",
					got.SeedWanted, got.Settled, got.Changed, tc.seed, tc.settled, tc.changed)
			}
			assertVerdictAfterReviews(t, got.Gate, tc.fact, got)
		})
	}

	t.Run("C14", func(t *testing.T) {
		replayed := 0
		for _, path := range fixtureFiles(t) {
			encoded, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var recorded fixture
			if err := json.Unmarshal(encoded, &recorded); err != nil {
				t.Fatalf("decode fixture %s: %v", path, err)
			}
			if recorded.Function != "reduceDispatchEvent" {
				continue
			}
			var input fixtureDispatchInput
			if err := json.Unmarshal(recorded.Input, &input); err != nil {
				t.Fatalf("decode the input of %s: %v", path, err)
			}
			// The cases replayDispatchDecision applies ApplyDesignGateEvent to, with its kind, its
			// version and its mark.
			held, found := input.State.Gates[input.Event.Key]
			if !found || input.State.Issues[input.Event.Key] == nil || held.ArtifactID != stringValue(input.Event.Payload, "artifact_id") {
				continue
			}
			before := held.record(input.Event.Key)
			fact := GateFact{Kind: DesignGateEventKind(strings.TrimPrefix(input.Event.Type, "artifact.")), Version: fixtureVersion(input.Event.Payload)}
			replayed++
			t.Run(strings.TrimSuffix(filepath.Base(path), ".json"), func(t *testing.T) {
				want := ApplyDesignGateEvent(before, fact.Kind, fact.Version, fact.Mark)
				got := ApplyGateFact(before, fact)
				if !reflect.DeepEqual(got, GateFactResult{Gate: want, Changed: !reflect.DeepEqual(before, want)}) {
					t.Fatalf("%s %d with no digest field = %#v, want ApplyDesignGateEvent's %s", fact.Kind, fact.Version, got, describeGate(want))
				}
				assertVerdictAfterReviews(t, got.Gate, fact, got)
			})
		}
		if replayed == 0 {
			t.Fatal("no classification fixture applies a Dispatch artifact event to a gate, want the cases replayDispatchDecision replays")
		}
	})
}

// describeGate is a row as JSON, which follows its pointers, for a failure to show.
func describeGate(gate record.DesignGate) string {
	encoded, err := json.Marshal(gate)
	if err != nil {
		return err.Error()
	}
	return string(encoded)
}
