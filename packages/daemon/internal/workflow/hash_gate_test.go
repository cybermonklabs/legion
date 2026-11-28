package workflow

//ci:reads packages/contracts/fixtures/hash-gate/** hashGateFact reads the event fixtures through ../../../contracts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The actors of the facts below: the root's architect, who writes each version; Dispatch's fill,
// which gives each verdict (artifact.digest); and the person who reviews.
var (
	hashArchitect = dispatch.Actor{Kind: "session", ID: "session-architect"}
	hashFill      = dispatch.Actor{Kind: "system", ID: "digest-fill"}
	hashPerson    = dispatch.Actor{Kind: "user", ID: "alice"}
)

// hashMark is the mark mN of the plan's state table: sequence seq in the issue's Dispatch event
// log, at the matching time tN.
func hashMark(seq int64) record.EventMark {
	return record.EventMark{Seq: seq, At: time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Minute)}
}

// hashGateFact is the fact intake decodes (intake.DecodeDispatchEvent) from one event file of
// packages/contracts/fixtures/hash-gate, the wire contract of Dispatch's hash gate (R1 of
// dispatch://INFRA-6065), with its payload changed by change: the file holds the event's type and
// payload alone, which this wraps in the envelope Dispatch's event log serves, as an event of key
// at seq (hashMark) made by actor.
func hashGateFact(t *testing.T, key, name string, seq int64, actor dispatch.Actor, change func(payload map[string]any)) intake.DispatchArtifact {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "hash-gate", name))
	if err != nil {
		t.Fatalf("read the hash-gate fixture %s: %v", name, err)
	}
	var file struct {
		Type    string         `json:"type"`
		Payload map[string]any `json:"payload"`
	}
	if err := json.Unmarshal(raw, &file); err != nil || file.Type == "" || file.Payload == nil {
		t.Fatalf("the hash-gate fixture %s is %s (%v), want an event's type and payload", name, raw, err)
	}
	change(file.Payload)
	event, err := json.Marshal(map[string]any{
		"id": 9000 + seq, "issue_key": key, "artifact_id": nil, "project": "LEGION", "seq": seq, "type": file.Type,
		"actor": map[string]string{"kind": actor.Kind, "id": actor.ID}, "notify": true,
		"created_at": hashMark(seq).At.Format(time.RFC3339Nano), "payload": file.Payload,
	})
	if err != nil {
		t.Fatalf("encode the event of %s: %v", name, err)
	}
	fact, unread, err := intake.DecodeDispatchEvent("LEGION", key, event)
	if err != nil || len(unread) != 0 {
		t.Fatalf("decode %s = %v, unread %v; want it decoded whole", event, err, unread)
	}
	artifact, ok := fact.(intake.DispatchArtifact)
	if !ok {
		t.Fatalf("decode %s = %#v, want an artifact fact", event, fact)
	}
	return artifact
}

// versionOwed is artifact-version.json as version of document, an event of key at seq: a version
// that owes its digest (digest_owed true).
func versionOwed(t *testing.T, key, document string, version int, seq int64) intake.DispatchArtifact {
	t.Helper()
	return hashGateFact(t, key, "artifact-version.json", seq, hashArchitect, func(payload map[string]any) {
		payload["artifact_id"], payload["version"] = document, map[string]any{"number": version}
	})
}

// versionFilled is artifact-version-filled.json as version of document, an event of key at seq:
// a version whose event says digest_owed false, which owes its digest all the same until the
// artifact.digest of it arrives (B10 of the plan of dispatch://INFRA-6065).
func versionFilled(t *testing.T, key, document string, version int, seq int64) intake.DispatchArtifact {
	t.Helper()
	return hashGateFact(t, key, "artifact-version-filled.json", seq, hashArchitect, func(payload map[string]any) {
		payload["artifact_id"], payload["version"] = document, map[string]any{"number": version}
	})
}

// digestOf is artifact-digest.json as Dispatch's verdict on version of document, an event of key
// at seq: current says whether the approval stands for it, and a verdict that it stands names
// approved as its approved_version; one that it does not names no approved version.
func digestOf(t *testing.T, key, document string, version int, seq int64, current bool, approved int) intake.DispatchArtifact {
	t.Helper()
	return hashGateFact(t, key, "artifact-digest.json", seq, hashFill, func(payload map[string]any) {
		payload["artifact_id"], payload["version"], payload["approval_current"] = document, version, current
		if current {
			payload["approved_version"] = approved
			return
		}
		delete(payload, "approved_version")
		delete(payload, "approved_sha256")
	})
}

// hashReview is a person's review of kind of version of document, an event of key at seq.
func hashReview(key, document string, kind intake.DispatchArtifactKind, version int, seq int64, reason string) intake.DispatchArtifact {
	return intake.DispatchArtifact{Key: key, ArtifactID: document, Kind: kind, Version: version, Mark: hashMark(seq), Actor: hashPerson, Reason: reason}
}

// gateSeedRead is the gate seed's read of document of key as the outbox's seed builds it (W8 of
// the plan): the approval Dispatch shows, of version at the review's mark (review), with the
// read's latest version, its digest_owed and, where no digest is owed, its verdict at the latest
// version (dispatch.Approval.Stands) at the read's own mark (read).
func gateSeedRead(key, document string, version, latest int, review record.EventMark, owed, stands bool, read record.EventMark) intake.DispatchArtifact {
	fact := intake.DispatchArtifact{Key: key, ArtifactID: document, Kind: intake.DispatchArtifactApproved, Version: version, Mark: review,
		Actor: hashPerson, Latest: latest, DigestOwed: new(owed), VerdictMark: read}
	if !owed {
		fact.Verdict = new(stands)
	}
	return fact
}

// hashTree records LEGION-208 at work in planning and its child LEGION-209 under test, each
// phase's holder and the root's architect having taken version took of the spec: a planner, a
// tester and the root's architect are each told a move of the root's gate.
func hashTree(t *testing.T, pool *pgxpool.Pool, took int) {
	t.Helper()
	seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "root", Phase: phase.Planning, Generation: 4, Status: "in_progress", Rank: "U"})
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: gatedRoot, Project: "LEGION", Title: "a child under test", Parent: new(gatedRoot), Phase: phase.Testing,
		Generation: 4, Status: "testing", Rank: "V"})
	seedSlot(t, pool, record.Slot{Issue: gatedRoot, Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	versions := map[record.DesignStep]int{record.DesignStepSpec: took}
	seedTold(t, pool, gatedRoot, record.AudienceHolder, versions)
	seedTold(t, pool, "LEGION-209", record.AudienceHolder, versions)
	seedTold(t, pool, gatedRoot, record.AudienceArchitect, versions)
}

// hashWaitingRoot records LEGION-208 waiting on a person's review of version waited of its spec,
// from the hand-back at since, as askRoot.
func hashWaitingRoot(t *testing.T, pool *pgxpool.Pool, waited int, since int64) {
	t.Helper()
	root := askRoot()
	root.Phase = phase.AwaitingPerson
	root.Awaiting = &record.Awaiting{GateVersion: waited, Artifact: gateDoc, Since: hashMark(since)}
	seedIssue(t, pool, root)
}

// editClosed is the row of LEGION-208's spec closed by a requirement edit: in hash mode, the person's
// approval of version 5 (m10) and Dispatch's verdict false at latest, given by the digest at
// verdictSeq, told closed.
func editClosed(latest int, verdictSeq int64) record.DesignGate {
	return record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: latest, ApprovedVersion: new(5), ApprovedMark: hashMark(10),
		Verdict: &record.GateVerdict{Version: latest, Current: false, Mark: hashMark(verdictSeq)}, ToldOpen: new(false), EverOpened: true}
}

// openAt is the row of LEGION-208's spec in hash mode open on the person's approval of approved
// (at approvedSeq), Dispatch's verdict true at latest at verdictSeq, told open.
func openAt(latest, approved int, approvedSeq, verdictSeq int64) record.DesignGate {
	return record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: latest, ApprovedVersion: new(approved), ApprovedMark: hashMark(approvedSeq),
		Verdict: &record.GateVerdict{Version: latest, Current: true, Mark: hashMark(verdictSeq)}, ToldOpen: new(true), EverOpened: true}
}

// onBasisAt is LEGION-208's spec step standing open on a person's approval of version basisVersion
// of LEGION-100's document, registered at version root of its own, in version mode.
func onBasisAt(root, basisVersion int) record.DesignGate {
	basis := approvedBasis()
	basis.Version, basis.RegisteredAt, basis.RootVersion = basisVersion, basisEngineNow, root
	return record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: root, ApprovedVersion: new(root), Basis: basis,
		RestsOn: []string{basisAncestor}, EverOpened: true}
}

// pendingAt is LEGION-208's spec step waiting, closed, on ask-100, the request of version 4 of
// LEGION-100's document, registered at version root of its own (rootVersion, 0 for a row written
// before the column that records it).
func pendingAt(latest, rootVersion int) record.DesignGate {
	basis := pendingBasis()
	basis.RegisteredAt, basis.RootVersion = basisEngineNow, rootVersion
	return record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: latest, Basis: basis}
}

// assertHashGate holds issue's spec row to open (record.GateOpen), told (record.ToldStanding) and
// the approved version (approved, 0 for none), and returns it.
func assertHashGate(t *testing.T, pool *pgxpool.Pool, issue string, open, told bool, approved int) record.DesignGate {
	t.Helper()
	gate, ok := gatesOf(t, pool, issue)[record.DesignStepSpec]
	if !ok {
		t.Fatalf("%s holds no spec row", issue)
	}
	got := 0
	if gate.ApprovedVersion != nil {
		got = *gate.ApprovedVersion
	}
	if record.GateOpen(gate) != open || record.ToldStanding(gate) != told || got != approved {
		t.Fatalf("the spec row of %s = %#v (verdict %+v): open %t, told open %t, approved %d; want %t, %t, %d",
			issue, gate, gate.Verdict, record.GateOpen(gate), record.ToldStanding(gate), got, open, told, approved)
	}
	return gate
}

// assertNoticeCount holds the outbox's notices of kind, for every issue, to want.
func assertNoticeCount(t *testing.T, pool *pgxpool.Pool, kind record.NoticeKind, want int) {
	t.Helper()
	var got int
	if err := pool.QueryRow(t.Context(), `select count(*) from outbox where kind = 'notice' and payload->>'kind' = $1`, string(kind)).Scan(&got); err != nil {
		t.Fatalf("count %s notices: %v", kind, err)
	}
	if got != want {
		t.Fatalf("%s notices = %d, want %d", kind, got, want)
	}
}

// assertNothingSent holds the outbox to no role message, notice, news reconcile or gate seed.
func assertNothingSent(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, kind := range []record.OutboxKind{record.OutboxKindRoleMessage, record.OutboxKindNotice, record.OutboxKindDesignChanges, record.OutboxKindGateSeed} {
		assertOutboxCount(t, pool, string(kind), 0)
	}
}

// assertTreeTold holds the role messages of LEGION-208's tree (hashTree) to bodies, in order: each
// is the move told to its planner, its architect and the tester of LEGION-209, in that order.
func assertTreeTold(t *testing.T, pool *pgxpool.Pool, bodies ...[3]string) {
	t.Helper()
	var root, child []record.RoleMessage
	for _, body := range bodies {
		root = append(root, record.RoleMessage{Role: treeRoleToken(t, gatedRoot, claim.RolePlanner), Body: body[0]},
			record.RoleMessage{Role: treeRoleToken(t, gatedRoot, claim.RoleArchitect), Body: body[1]})
		child = append(child, record.RoleMessage{Role: treeRoleToken(t, "LEGION-209", claim.RoleTester), Body: body[2]})
	}
	assertRoleMessages(t, pool, gatedRoot, root)
	assertRoleMessages(t, pool, "LEGION-209", child)
}

// toEveryone is body as told alike to the planner, the architect and the tester (assertTreeTold).
func toEveryone(body string) [3]string { return [3]string{body, body, body} }

// assertWait holds LEGION-208's wait to its gate version waited, answered by by ("" for
// unanswered), with detail and the version reviewed (0 for none) where answered.
func assertWait(t *testing.T, pool *pgxpool.Pool, waited int, by record.Answer, detail string, reviewed int) {
	t.Helper()
	waiting := recordedRoot(t, pool).Awaiting
	switch {
	case waiting == nil:
		t.Fatal("LEGION-208 waits on nothing, want its gate wait kept")
	case waiting.GateVersion != waited:
		t.Fatalf("wait = %+v, want it on version %d", waiting, waited)
	case by == "" && waiting.AnsweredAt != nil:
		t.Fatalf("wait = %+v, want it unanswered", waiting)
	case by == "":
	case waiting.AnsweredAt == nil || waiting.AnsweredBy != by || waiting.AnsweredDetail != detail:
		t.Fatalf("wait = %+v, want it answered by %s, %q", waiting, by, detail)
	case reviewed == 0 && waiting.AnsweredDocument != nil && waiting.AnsweredDocument.Version != 0,
		reviewed != 0 && (waiting.AnsweredDocument == nil || waiting.AnsweredDocument.Version != reviewed):
		t.Fatalf("wait answered of %+v, want version %d reviewed", waiting.AnsweredDocument, reviewed)
	}
}

// relayOfParked is what the outbox's relay of notice, queued for issue, comes to on a tree parked
// on the backlog (the daemon's parkDecision): the notice is asked first whether its decision is
// still the person's word, as the relay asks it, which it must be, and then handed to
// record.RelayParkedDecision as nobody in its tree hears it.
func relayOfParked(t *testing.T, pool *pgxpool.Pool, steps []record.DesignStep, issue string, notice record.Notice) record.RelayOutcome {
	t.Helper()
	decision, ok := record.SpecDecision(notice.Kind)
	if !ok {
		t.Fatalf("a %s notice is no spec decision", notice.Kind)
	}
	until := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	parked := record.Issue{Key: issue, Tree: issue, Project: "LEGION", Title: "parked", Phase: phase.Done, Generation: 4, Status: "backlog",
		DispatchStatus: "backlog", Rank: "U", LingerUntil: &until}
	var outcome record.RelayOutcome
	seedRecord(t, pool, func(tx pgx.Tx) error {
		gates, err := record.NewStore().GatesOf(t.Context(), tx, issue)
		if err != nil {
			return err
		}
		if superseded := NoticeSuperseded(gates, steps, issue, notice, decision); superseded != nil {
			t.Fatalf("the %s notice %+v reads superseded by %+v, want it the person's word", notice.Kind, notice, superseded)
		}
		outcome, err = record.RelayParkedDecision(t.Context(), tx, record.NewStore(), issue, parked, true,
			record.ParkedDecision{Ref: notice.Ref, Decision: decision, Actor: notice.Actor, At: notice.Review.At, Seq: notice.Review.Seq,
				Artifact: notice.Artifact, Version: notice.Version}, notice, steps)
		return err
	})
	return outcome
}

// openedWithVerdict is the opened message of LEGION-208's spec, the role message tellGateMoved
// sends and gateAtSend writes, where a person approved version approved and Dispatch reports that
// approval current for version current.
func openedWithVerdict(approved, current int) string {
	return fmt.Sprintf("The design gate of LEGION-208 opened: a person approved version %d of its spec document (artifact-208), "+
		"which Dispatch reports current for version %d, and every step the deployment gates now stands open, "+
		"so the design you are working to is the approved one. A merge the closed gate held is released.", approved, current)
}

// The workflow follows Dispatch's verdict on whether the approval of a gated document stands
// (dispatch://INFRA-6065, R3 and R5), one subtest per W row of the plan's state table, beside W6b
// (a review of the latest version while its digest is owed answers a wait once), W21 (a
// registration whose read owes the digest answers no wait for a person until the digest settles
// that version) and W22 (a rise of the approved version an owed digest held back queues the news
// at the settlement). Each row asserts the record it leaves (record.GateOpen, record.ToldStanding,
// the approved version, the root's wait) and the outbox rows it queues, exactly. Every event fact
// is decoded from the wire fixtures of packages/contracts/fixtures/hash-gate through
// intake.DecodeDispatchEvent; every read is the fact the gate seed builds of a read of Dispatch.
// W20 replays the rows' facts with no digest field and asserts today's results.
func TestHashGateFollowsDispatchApprovalVerdict(t *testing.T) {
	t.Parallel()
	engine := testEngine(config.DesignGateRootIssues, nil)
	steps := config.DesignGateRootIssues.Steps()
	// rig is a migrated database and the facts applied to it through engine.
	rig := func(t *testing.T) (*pgxpool.Pool, func(string, intake.Fact)) {
		t.Helper()
		pool := migratedPool(t)
		apply := applyFacts(t, pool, engine)
		return pool, func(id string, fact intake.Fact) {
			t.Helper()
			if result := apply(id, fact); result.Refusal != nil {
				t.Fatalf("%s refused: %#v", id, result.Refusal)
			}
		}
	}

	// A prose edit: the version owes its digest, which then reports the approval current. The
	// gate reads closed to the work while the digest is owed (the merger's start hold) and tells
	// nobody; once the digest settles it, it reads open again, and nobody was told anything.
	t.Run("W1", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 5)
		seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 5, ApprovedVersion: new(5), ApprovedMark: hashMark(10), EverOpened: true})

		apply("version-6", versionOwed(t, gatedRoot, gateDoc, 6, 20))
		if gate := assertHashGate(t, pool, gatedRoot, false, true, 5); !record.DigestOwed(gate) {
			t.Fatalf("the spec row = %+v, want version 6 owing its digest", gate)
		}
		if gates := gatesOf(t, pool, gatedRoot); engine.standingGateOpen(gates) || !engine.standingGateTold(gates) {
			t.Fatal("while the digest is owed, want the merger's start held and the tree's standing told open")
		}
		assertNothingSent(t, pool)

		apply("digest-6", digestOf(t, gatedRoot, gateDoc, 6, 21, true, 5))
		assertHashGate(t, pool, gatedRoot, true, true, 5)
		if !engine.standingGateOpen(gatesOf(t, pool, gatedRoot)) {
			t.Fatal("once the digest reports the approval current, want the merger's start released")
		}
		assertNothingSent(t, pool)
	})

	// A requirement edit: the digest reports the approval not current. The gate closes, and the
	// close is told once, at the digest: nothing at the version.
	t.Run("W2", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 5)
		seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 5, ApprovedVersion: new(5), ApprovedMark: hashMark(10), EverOpened: true})

		apply("version-6", versionOwed(t, gatedRoot, gateDoc, 6, 20))
		assertOutboxCount(t, pool, string(record.OutboxKindRoleMessage), 0)
		apply("digest-6", digestOf(t, gatedRoot, gateDoc, 6, 21, false, 0))

		assertHashGate(t, pool, gatedRoot, false, false, 5)
		assertTreeTold(t, pool, toEveryone(gateMovedBody(record.DesignStepSpec, gateDoc, 6, false)))
		assertOutboxCount(t, pool, string(record.OutboxKindNotice), 0)
		assertOutboxCount(t, pool, string(record.OutboxKindDesignChanges), 0)
	})

	// A restore of the approved text: after a requirement edit closed the gate, a version whose
	// digest reports the approval of version 5 current again opens it with no artifact.approved.
	// The opening is told with today's kinds, naming the approval the row holds and no actor
	// (Departure D6), and the opened message names the version Dispatch reports it current for.
	// ApprovedVersion did not rise, and every holder took 5, so no news is queued. A root waiting
	// on that gate is answered as an approval answers it. On a row that records no person's
	// approval the notice would name a version no person approved: the predicate finds it is not
	// the person's word (SupersedingApproval), and no architect is sent it.
	t.Run("W3", func(t *testing.T) {
		t.Parallel()
		t.Run("the tree is told the opening", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashTree(t, pool, 5)
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 5, ApprovedVersion: new(5), ApprovedMark: hashMark(10), EverOpened: true})
			apply("version-6", versionOwed(t, gatedRoot, gateDoc, 6, 20))
			apply("digest-6", digestOf(t, gatedRoot, gateDoc, 6, 21, false, 0))

			apply("version-7", versionOwed(t, gatedRoot, gateDoc, 7, 30))
			assertHashGate(t, pool, gatedRoot, false, false, 5)
			apply("digest-7", digestOf(t, gatedRoot, gateDoc, 7, 31, true, 5))

			assertHashGate(t, pool, gatedRoot, true, true, 5)
			opened := openedWithVerdict(5, 7)
			if !strings.Contains(opened, ", which Dispatch reports current for version 7,") {
				t.Fatalf("opened message %q names no verdict", opened)
			}
			assertTreeTold(t, pool, toEveryone(gateMovedBody(record.DesignStepSpec, gateDoc, 6, false)),
				[3]string{opened + " " + nothingChangedClause(5), opened + " " + treeNothingChangedClause(5), opened + " " + nothingChangedClause(5)})
			approved := noticesOfKind(t, pool, gatedRoot, "design-approved")
			if len(approved) != 1 || approved[0].Version != 5 || approved[0].Actor != "" || approved[0].Review != hashMark(10) || approved[0].Artifact != gateDoc {
				t.Fatalf("design-approved notices = %+v, want one of version 5, no actor, at the approval's mark m10", approved)
			}
			assertOutboxCount(t, pool, string(record.OutboxKindNotice), 1)
			assertOutboxCount(t, pool, string(record.OutboxKindDesignChanges), 0)
			if outcome := relayOfParked(t, pool, steps, gatedRoot, approved[0]); outcome != record.RelayNoPerson {
				t.Fatalf("the relay of the opening on a parked tree = %v, want RelayNoPerson", outcome)
			}
		})
		t.Run("a root waiting on the gate is answered as an approval answers it", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashWaitingRoot(t, pool, 6, 25)
			seedGate(t, pool, editClosed(6, 21))
			// The root's architect took version 5, the approval the opening names, so it is owed
			// no news.
			seedTold(t, pool, gatedRoot, record.AudienceArchitect, map[record.DesignStep]int{record.DesignStepSpec: 5})

			// Version 7 was written before the hand-back, and its event arrived late: the wait
			// moves to it with no answer, as today's rule moves it.
			apply("version-7", versionOwed(t, gatedRoot, gateDoc, 7, 24))
			assertWait(t, pool, 7, "", "", 0)
			apply("digest-7", digestOf(t, gatedRoot, gateDoc, 7, 31, true, 5))

			assertHashGate(t, pool, gatedRoot, true, true, 5)
			assertWait(t, pool, 7, record.AnsweredByApproval, "version 5", 5)
			assertNoticeCount(t, pool, "design-approved", 0)
			assertOutboxCount(t, pool, string(record.OutboxKindDesignChanges), 0)
		})
		t.Run("an opening on no approval of the person's sends no notice", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashTree(t, pool, 5)
			// Closed at 6 by Dispatch's verdict, with no person's approval recorded.
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 6,
				Verdict: &record.GateVerdict{Version: 6, Current: false, Mark: hashMark(21)}, ToldOpen: new(false)})

			apply("digest-7", digestOf(t, gatedRoot, gateDoc, 7, 31, true, 5))

			assertHashGate(t, pool, gatedRoot, true, true, 0)
			assertNoticeCount(t, pool, "design-approved", 0)
			assertOutboxCount(t, pool, string(record.OutboxKindNotice), 0)
		})
	})

	// A person's approval of the requirement edit opens the gate: today's design-approved, and the
	// news its rise of the approved version owes.
	t.Run("W4", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 5)
		seedGate(t, pool, editClosed(6, 21))

		apply("approved-6", hashReview(gatedRoot, gateDoc, intake.DispatchArtifactApproved, 6, 40, ""))

		assertHashGate(t, pool, gatedRoot, true, true, 6)
		approved := noticesOfKind(t, pool, gatedRoot, "design-approved")
		if len(approved) != 1 || approved[0].Version != 6 || approved[0].Actor != hashPerson.Name() {
			t.Fatalf("design-approved notices = %+v, want one of version 6 by %s", approved, hashPerson.Name())
		}
		assertOutboxCount(t, pool, string(record.OutboxKindNotice), 1)
		assertOutboxCount(t, pool, string(record.OutboxKindDesignChanges), 1)
		// Every holder took 5 and is owed 6, so each is told the opening by the task of its news.
		assertOutboxCount(t, pool, string(record.OutboxKindRoleMessage), 0)
		assertOpeningsOwed(t, pool, gatedRoot, gatedRoot+"/architect", gatedRoot+"/holder", "LEGION-209/holder")
	})

	// A person's changes request at the latest version of an open tree in hash mode closes it, as
	// today: the changes request is told, and so is the close.
	t.Run("W4b", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 6)
		seedGate(t, pool, openAt(6, 6, 40, 40))

		apply("changes-6", hashReview(gatedRoot, gateDoc, intake.DispatchArtifactChangesRequested, 6, 60, "name the rollback path"))

		gate := assertHashGate(t, pool, gatedRoot, false, false, 0)
		if gate.Verdict == nil || gate.Verdict.Current || gate.Verdict.Version != 6 {
			t.Fatalf("the verdict = %+v, want false at 6", gate.Verdict)
		}
		if changes := noticesOfKind(t, pool, gatedRoot, "design-changes-requested"); len(changes) != 1 || changes[0].Reason != "name the rollback path" {
			t.Fatalf("design-changes-requested notices = %+v, want the person's request", changes)
		}
		assertOutboxCount(t, pool, string(record.OutboxKindNotice), 1)
		assertTreeTold(t, pool, toEveryone(gateMovedBody(record.DesignStepSpec, gateDoc, 6, false)))
		assertOutboxCount(t, pool, string(record.OutboxKindDesignChanges), 0)
	})

	// A review of a version older than the latest gives no verdict in hash mode and moves no gate:
	// only a read of Dispatch can say whether that approval covers the latest version, so the gate
	// seed is queued.
	t.Run("W5", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 7)
		seedGate(t, pool, openAt(7, 7, 40, 40))

		apply("approved-6", hashReview(gatedRoot, gateDoc, intake.DispatchArtifactApproved, 6, 50, ""))

		if gate := assertHashGate(t, pool, gatedRoot, true, true, 7); gate.Verdict == nil || gate.Verdict.Mark != hashMark(40) {
			t.Fatalf("the verdict = %+v, want it as it was", gate.Verdict)
		}
		assertOutboxCount(t, pool, string(record.OutboxKindGateSeed), 1)
		for _, kind := range []record.OutboxKind{record.OutboxKindRoleMessage, record.OutboxKindNotice, record.OutboxKindDesignChanges} {
			assertOutboxCount(t, pool, string(kind), 0)
		}
	})

	// A version placed after a root's wait on its gate began answers the wait only when its
	// digest settles it, whichever way the verdict goes: as a newer version answers it today.
	t.Run("W6", func(t *testing.T) {
		t.Parallel()
		for _, current := range []bool{true, false} {
			t.Run(fmt.Sprintf("digest %t", current), func(t *testing.T) {
				t.Parallel()
				pool, apply := rig(t)
				hashWaitingRoot(t, pool, 5, 20)
				seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 5, ApprovedVersion: new(4), ApprovedMark: hashMark(5)})

				apply("version-6", versionOwed(t, gatedRoot, gateDoc, 6, 30))
				assertHashGate(t, pool, gatedRoot, false, false, 4)
				assertWait(t, pool, 5, "", "", 0)

				apply("digest-6", digestOf(t, gatedRoot, gateDoc, 6, 31, current, 4))
				assertHashGate(t, pool, gatedRoot, current, current, 4)
				assertWait(t, pool, 6, record.AnsweredByVersion, "version 6", 0)
			})
		}
	})

	// A person's approval of the latest version while its digest is owed gives the verdict and
	// answers the wait once, as the approval it is: by the approval of 6 and not also by version 6,
	// and an approval leaves the version the wait recorded as it was, as today.
	t.Run("W6b", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashWaitingRoot(t, pool, 5, 20)
		seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 5, ApprovedVersion: new(4), ApprovedMark: hashMark(5)})

		apply("version-6", versionOwed(t, gatedRoot, gateDoc, 6, 30))
		assertWait(t, pool, 5, "", "", 0)
		apply("approved-6", hashReview(gatedRoot, gateDoc, intake.DispatchArtifactApproved, 6, 31, ""))

		assertHashGate(t, pool, gatedRoot, true, true, 6)
		assertWait(t, pool, 5, record.AnsweredByApproval, "version 6", 6)
	})

	// A version placed before the wait began moves the wait to it with no answer at once (today's
	// rule), so its digest answers nothing.
	t.Run("W7", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashWaitingRoot(t, pool, 5, 20)
		seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 5, ApprovedVersion: new(4), ApprovedMark: hashMark(5)})

		apply("version-6", versionOwed(t, gatedRoot, gateDoc, 6, 15))
		assertWait(t, pool, 6, "", "", 0)
		apply("digest-6", digestOf(t, gatedRoot, gateDoc, 6, 31, false, 0))

		assertHashGate(t, pool, gatedRoot, false, false, 4)
		assertWait(t, pool, 6, "", "", 0)
	})

	// A step open on an ancestor's approval stays open on a verdict of the root's own document:
	// Dispatch's verdict there is on an approval of the root's own document, which the step does
	// not have.
	t.Run("W8", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 4)
		seedGate(t, pool, onBasisAt(4, 3))

		apply("digest-4", digestOf(t, gatedRoot, gateDoc, 4, 30, false, 0))

		if gate := assertHashGate(t, pool, gatedRoot, true, true, 4); !gate.OnBasis() || gate.Verdict == nil || gate.Verdict.Current {
			t.Fatalf("the spec row = %+v (verdict %+v), want it on its basis, tracking the root's own verdict false", gate, gate.Verdict)
		}
		assertNothingSent(t, pool)
	})

	// A prose edit of the ancestor's document, and its digest that reports the ancestor's approval
	// current, change nothing for a step open on that approval.
	t.Run("W9", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 4)
		seedGate(t, pool, onBasisAt(4, 3))

		apply("ancestor-version-4", versionOwed(t, basisAncestor, basisDoc, 4, 30))
		apply("ancestor-digest-4", digestOf(t, basisAncestor, basisDoc, 4, 31, true, 3))

		if gate := assertHashGate(t, pool, gatedRoot, true, true, 4); !gate.OnBasis() || gate.Verdict != nil {
			t.Fatalf("the spec row = %+v, want it on its basis, in version mode", gate)
		}
		assertNothingSent(t, pool)
		assertOutboxCount(t, pool, string(record.OutboxKindPendingBasisSeed), 0)
	})

	// A step open on an ancestor's approval closes on Dispatch's verdict that the approval no
	// longer stands: a digest of the basis version or a later one that reports it not current, or
	// a read of the ancestor's document that shows it not standing. The end is told to the root's
	// architect as basis-ended, naming the version and the verdict, and the close to the tree.
	t.Run("W10", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name, verdict string
			fact          func(t *testing.T) intake.DispatchArtifact
		}{
			{"the ancestor's digest", "Dispatch's verdict on version 4 of the primary document of LEGION-100 (dispatch://LEGION-100/spec) is that the approval that stood for its version 4 no longer stands.",
				func(t *testing.T) intake.DispatchArtifact {
					return digestOf(t, basisAncestor, basisDoc, 4, 30, false, 0)
				}},
			{"a read of the ancestor's document", "Dispatch reports that the approval of the primary document of LEGION-100 (dispatch://LEGION-100/spec) that stood for its version 4 does not stand for its latest version 5.",
				func(*testing.T) intake.DispatchArtifact {
					return gateSeedRead(basisAncestor, basisDoc, 4, 5, hashMark(20), false, false, hashMark(30))
				}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				pool, apply := rig(t)
				hashTree(t, pool, 4)
				seedGate(t, pool, onBasisAt(4, 4))

				apply("ancestor-verdict", tc.fact(t))

				if gate := assertHashGate(t, pool, gatedRoot, false, false, 0); gate.Basis != nil || len(gate.RestsOn) != 1 {
					t.Fatalf("the spec row = %+v, want its basis ended and its rest on %s kept", gate, basisAncestor)
				}
				ended := noticesOfKind(t, pool, gatedRoot, "basis-ended")
				if len(ended) != 1 || ended[0].Version != 4 || ended[0].Actor != "" || ended[0].Role != claim.RoleArchitect ||
					ended[0].Ref != "dispatch://LEGION-100/spec" || !strings.Contains(ended[0].Reason, tc.verdict) ||
					!strings.Contains(ended[0].Reason, "The step stays closed") {
					t.Fatalf("basis-ended notices = %+v, want one naming %q", ended, tc.verdict)
				}
				assertOutboxCount(t, pool, string(record.OutboxKindNotice), 1)
				assertTreeTold(t, pool, toEveryone(gateMovedBody(record.DesignStepSpec, gateDoc, 4, false)))
			})
		}
	})

	// A newer version of the root's own document ends the basis of a step open on an ancestor's
	// approval once its digest settles it, as today's newer version ends it, and not while it is
	// owed. A version whose event says digest_owed false owes its digest all the same until the
	// digest arrives. The verdict is false, so nothing else opens the gate.
	t.Run("W11", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 3)
		seedGate(t, pool, onBasisAt(3, 3))

		apply("version-4", versionFilled(t, gatedRoot, gateDoc, 4, 30))
		if gate := assertHashGate(t, pool, gatedRoot, false, true, 3); !gate.OnBasis() || !record.DigestOwed(gate) {
			t.Fatalf("the spec row = %+v, want it on its basis while version 4 owes its digest", gate)
		}
		assertNothingSent(t, pool)

		apply("digest-4", digestOf(t, gatedRoot, gateDoc, 4, 31, false, 0))
		if gate := assertHashGate(t, pool, gatedRoot, false, false, 0); gate.Basis != nil {
			t.Fatalf("the spec row = %+v, want its basis ended", gate)
		}
		assertTreeTold(t, pool, toEveryone(gateMovedBody(record.DesignStepSpec, gateDoc, 4, false)))
		assertNoticeCount(t, pool, "basis-ended", 0)
	})

	// A step waiting on an ancestor's request, registered at root version 3 while that version owed
	// its digest: the digest of 3 is no newer version (Departure D2), so a false one ends nothing.
	t.Run("W12", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 2)
		pending := pendingAt(3, 3)
		pending.ApprovedVersion, pending.ApprovedMark = new(2), hashMark(4)
		pending.Verdict, pending.ToldOpen = &record.GateVerdict{Version: 2, Current: true, Mark: hashMark(5)}, new(false)
		seedGate(t, pool, pending)

		apply("digest-3", digestOf(t, gatedRoot, gateDoc, 3, 31, false, 0))

		if gate := assertHashGate(t, pool, gatedRoot, false, false, 2); !gate.PendingOnBasis() {
			t.Fatalf("the spec row = %+v, want it still waiting on ask-100", gate)
		}
		assertNothingSent(t, pool)
	})

	// The same row, whose own verdict is true: the root's own approval stands at the version
	// registered, which ends the wait open, as a person's approval of the root's own document ends
	// it. The end is told as basis-ended, and the opening as design-approved with no actor (D6).
	t.Run("W12b", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 2)
		pending := pendingAt(3, 3)
		pending.ApprovedVersion, pending.ApprovedMark = new(2), hashMark(4)
		pending.Verdict, pending.ToldOpen = &record.GateVerdict{Version: 2, Current: true, Mark: hashMark(5)}, new(false)
		seedGate(t, pool, pending)

		apply("digest-3", digestOf(t, gatedRoot, gateDoc, 3, 31, true, 2))

		if gate := assertHashGate(t, pool, gatedRoot, true, true, 2); gate.Basis != nil {
			t.Fatalf("the spec row = %+v, want its wait ended", gate)
		}
		ended := noticesOfKind(t, pool, gatedRoot, "basis-ended")
		const own = "Dispatch reports the approval of version 2 of the primary document of LEGION-208 (dispatch://LEGION-208/spec), the root's own document, current for version 3."
		if len(ended) != 1 || ended[0].Actor != "" || !strings.Contains(ended[0].Reason, own) || !strings.Contains(ended[0].Reason, "no longer waits on approval request ask-100") {
			t.Fatalf("basis-ended notices = %+v, want one saying %q", ended, own)
		}
		approved := noticesOfKind(t, pool, gatedRoot, "design-approved")
		if len(approved) != 1 || approved[0].Version != 2 || approved[0].Actor != "" || approved[0].Review != hashMark(4) {
			t.Fatalf("design-approved notices = %+v, want one of version 2 with no actor", approved)
		}
	})

	// A pending-basis row written before the column that records its root version: it falls back
	// to the verdict's version before the fact (3), so the root's version 4 ends the wait once, at
	// its digest and not while it is owed.
	t.Run("W12c", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 2)
		seedGate(t, pool, pendingAt(3, 0))

		apply("version-4", versionOwed(t, gatedRoot, gateDoc, 4, 30))
		if gate := assertHashGate(t, pool, gatedRoot, false, false, 0); !gate.PendingOnBasis() {
			t.Fatalf("the spec row = %+v, want it still waiting while version 4 owes its digest", gate)
		}
		assertNoticeCount(t, pool, "basis-ended", 0)

		apply("digest-4", digestOf(t, gatedRoot, gateDoc, 4, 31, false, 0))
		if gate := assertHashGate(t, pool, gatedRoot, false, false, 0); gate.Basis != nil {
			t.Fatalf("the spec row = %+v, want its wait ended", gate)
		}
		const newer = "Version 4 of the primary document of LEGION-208 (dispatch://LEGION-208/spec), the root's own document, is newer than version 3, which the step was registered at."
		if ended := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(ended) != 1 || ended[0].Actor != "" || !strings.Contains(ended[0].Reason, newer) {
			t.Fatalf("basis-ended notices = %+v, want one saying %q", ended, newer)
		}
	})

	// carriedRig is LEGION-209, carried out of LEGION-208's tree as a root of its own, holding a
	// copy of LEGION-208's spec gate (copy), and the facts applied to it.
	carriedRig := func(t *testing.T, copy record.DesignGate) (*pgxpool.Pool, func(string, intake.Fact)) {
		t.Helper()
		pool, apply := rig(t)
		seedIssue(t, pool, record.Issue{Key: gatedRoot, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: gatedRoot, Project: "LEGION", Title: "the old root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U"})
		seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: "LEGION-209", Project: "LEGION", Title: "the carried root", Parent: new(gatedRoot), Phase: phase.Admitted, Generation: 2,
			Status: "in_progress", Rank: "V", HandedOver: true, HandedOverFrom: gatedRoot, LastDispatchSeq: 1})
		seedSlot(t, pool, record.Slot{Issue: "LEGION-209", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
		seedGate(t, pool, copy)
		return pool, apply
	}

	// A carried copy opens on the old root's digest that reports the approval current for the
	// copy's version: carried-gate-opened, with no actor and a reason that names the verdict,
	// which nobody relays on a parked tree, since no person made it (D6).
	t.Run("W13", func(t *testing.T) {
		t.Parallel()
		pool, apply := carriedRig(t, record.DesignGate{Issue: "LEGION-209", ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(3),
			ApprovedMark: hashMark(10), Carried: true})

		apply("old-root-digest-4", digestOf(t, gatedRoot, gateDoc, 4, 40, true, 3))

		if gate := assertHashGate(t, pool, "LEGION-209", true, true, 3); !gate.Carried {
			t.Fatalf("the copy = %+v, want it still a carried copy", gate)
		}
		opened := noticesOfKind(t, pool, "LEGION-209", "carried-gate-opened")
		const verdict = "Dispatch reports the approval of LEGION-208's spec document current for version 4, which opens the gate LEGION-209 carried"
		if len(opened) != 1 || opened[0].Actor != "" || opened[0].Version != 3 || !strings.Contains(opened[0].Reason, verdict) {
			t.Fatalf("carried-gate-opened notices = %+v, want one with no actor whose reason says %q", opened, verdict)
		}
		if outcome := relayOfParked(t, pool, steps, "LEGION-209", opened[0]); outcome != record.RelayNoPerson {
			t.Fatalf("the relay of the opening on a parked tree = %v, want RelayNoPerson", outcome)
		}
	})

	// Nothing on the old root's document closes a carried copy: a digest that reports the approval
	// not current is not taken (copyTakes).
	t.Run("W14", func(t *testing.T) {
		t.Parallel()
		pool, apply := carriedRig(t, record.DesignGate{Issue: "LEGION-209", ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(4),
			ApprovedMark: hashMark(10), Carried: true, EverOpened: true})

		apply("old-root-digest-5", digestOf(t, gatedRoot, gateDoc, 5, 40, false, 0))

		if gate := assertHashGate(t, pool, "LEGION-209", true, true, 4); gate.Verdict != nil || gate.LatestVersion != 4 {
			t.Fatalf("the copy = %+v, want it as it was", gate)
		}
		assertNothingSent(t, pool)
	})

	// standing is Dispatch's approval of version 4 of LEGION-100's document, the version a step of
	// LEGION-208 waits on (registeredPending), by a person at m60, with version 5 the latest: in
	// state, owing the digest of the latest version or not (owed, nil for a Dispatch that sends no
	// digest_owed).
	standing := func(state string, owed *bool) dispatch.Approval {
		approval := dispatch.Approval{IssueKey: basisAncestor, State: state, Version: new(4), LatestVersion: 5, DigestOwed: owed,
			By: basisPerson, Seq: 60, At: hashMark(60).At}
		if owed != nil && !*owed {
			approval.CurrentSHA256 = new(strings.Repeat("a", 64))
		}
		return approval
	}

	// The pending-basis judge opens a step on a person's approval of the basis version that
	// Dispatch reports standing, whatever the latest version: today's opening of a pending step.
	t.Run("W15", func(t *testing.T) {
		t.Parallel()
		pool, _, apply := registeredPending(t)

		apply("read", seedRead(standing("approved", new(false)), ancestorAtWork))

		gate := gatesOf(t, pool, gatedRoot)[record.DesignStepSpec]
		if !gate.OnBasis() || !record.GateOpen(gate) || !record.ToldStanding(gate) || gate.Basis.Version != 4 ||
			gate.Basis.Approval != (record.EventMark{Seq: 60, At: hashMark(60).At}) || gate.Basis.Approver != basisPerson.Name() {
			t.Fatalf("the spec step = %#v (basis %+v), want it open on the person's approval of version 4 at m60", gate, gate.Basis)
		}
		approved := noticesOfKind(t, pool, gatedRoot, "design-approved")
		if len(approved) != 1 || approved[0].Version != 4 || approved[0].Actor != basisPerson.Name() || approved[0].Ref != "dispatch://LEGION-100/spec" {
			t.Fatalf("design-approved notices = %+v, want the one that names the basis", approved)
		}
		assertNoticeCount(t, pool, "basis-ended", 0)
	})

	// A newer version ends the wait where no approval of the basis version stands.
	t.Run("W15b", func(t *testing.T) {
		t.Parallel()
		pool, _, apply := registeredPending(t)

		apply("read", seedRead(standing("stale", new(false)), ancestorAtWork))

		if gate := assertHashGate(t, pool, gatedRoot, false, false, 0); gate.Basis != nil {
			t.Fatalf("the spec step = %#v, want its wait ended", gate)
		}
		const newer = "Version 5 of the primary document of LEGION-100 (dispatch://LEGION-100/spec) is newer than version 4, which the request asks a person to approve."
		if ended := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(ended) != 1 || !strings.Contains(ended[0].Reason, newer) {
			t.Fatalf("basis-ended notices = %+v, want one naming %q", ended, newer)
		}
		assertNoticeCount(t, pool, "design-approved", 0)
	})

	// A read made while the digest is owed decides nothing.
	t.Run("W16", func(t *testing.T) {
		t.Parallel()
		pool, _, apply := registeredPending(t)
		told := basisTold(t, pool)

		apply("read", seedRead(standing("approved", new(true)), ancestorAtWork))

		if gate := assertHashGate(t, pool, gatedRoot, false, false, 0); !gate.PendingOnBasis() || gate.Basis.Pending.Ask != "ask-100" {
			t.Fatalf("the spec step = %#v, want it still waiting on ask-100", gate)
		}
		if now := basisTold(t, pool); now != told {
			t.Fatalf("notices of the step on the basis = %d, want the %d before the read", now, told)
		}
	})

	// Dispatch's verdict on a version of the basis document is a cue: it queues the pending-basis
	// seed, whose read decides, and changes nothing itself.
	t.Run("W17", func(t *testing.T) {
		t.Parallel()
		pool, _, apply := registeredPending(t)
		seeds, told := pendingSeeds(t, pool, gatedRoot), basisTold(t, pool)

		apply("ancestor-digest-5", digestOf(t, basisAncestor, basisDoc, 5, 70, true, 4))

		if queued := pendingSeeds(t, pool, gatedRoot); len(queued) != len(seeds)+1 || queued[len(queued)-1] != theSeed {
			t.Fatalf("pending-basis seeds = %#v, want the %d before the digest and the cue's %#v", queued, len(seeds), theSeed)
		}
		if gate := assertHashGate(t, pool, gatedRoot, false, false, 0); !gate.PendingOnBasis() {
			t.Fatalf("the spec step = %#v, want it still waiting", gate)
		}
		if now := basisTold(t, pool); now != told {
			t.Fatalf("notices of the step on the basis = %d, want the %d before the digest", now, told)
		}
	})

	// Under two gated steps, the standing told is the tree's: while the spec's version owes its
	// digest, the Verification document's version settling open on its own digest tells nothing,
	// and nothing is told until the spec's row settles, which leaves the standing as it was.
	t.Run("W18", func(t *testing.T) {
		t.Parallel()
		pool := migratedPool(t)
		twoStepped := stepsEngine(twoSteps(), nil)
		apply := applyFacts(t, pool, twoStepped)
		seedMergingRoot(t, pool)
		seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(4),
			ApprovedMark: hashMark(10), EverOpened: true})
		seedGate(t, pool, record.DesignGate{Issue: gatedRoot, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, LatestVersion: 2,
			ApprovedVersion: new(2), ApprovedMark: hashMark(11), EverOpened: true})
		seedTreeLifecycle(t, pool, gatedRoot)
		took := map[record.DesignStep]int{record.DesignStepCriteria: 2, record.DesignStepSpec: 4}
		seedTold(t, pool, gatedRoot, record.AudienceHolder, took)
		seedTold(t, pool, gatedRoot, record.AudienceArchitect, took)

		apply("spec-5", versionOwed(t, gatedRoot, gateDoc, 5, 30))
		apply("criteria-3", versionOwed(t, gatedRoot, criteriaDoc, 3, 31))
		apply("criteria-digest-3", digestOf(t, gatedRoot, criteriaDoc, 3, 32, true, 2))

		gates := gatesOf(t, pool, gatedRoot)
		if criteria := gates[record.DesignStepCriteria]; !record.GateOpen(criteria) || !record.ToldStanding(criteria) || record.DigestOwed(criteria) {
			t.Fatalf("the criteria row = %+v, want it settled open on its own digest", criteria)
		}
		if spec := gates[record.DesignStepSpec]; record.GateOpen(spec) || !record.ToldStanding(spec) || !record.DigestOwed(spec) {
			t.Fatalf("the spec row = %+v, want it owing its digest, told open", spec)
		}
		if twoStepped.standingGateOpen(gates) || !twoStepped.standingGateTold(gates) {
			t.Fatal("want the merger's start held and the tree's standing told open while the spec's digest is owed")
		}
		assertNothingSent(t, pool)

		apply("spec-digest-5", digestOf(t, gatedRoot, gateDoc, 5, 33, true, 4))
		assertHashGate(t, pool, gatedRoot, true, true, 4)
		assertNothingSent(t, pool)
	})

	// The texts that say how a gate stands read the standing told, and name a person's approval
	// below the latest version with the version Dispatch reports it current for: the resume task of
	// a root answered while its gate stood open there, the architect's start and the catch-up. None
	// says "which is not approved" of a version whose approval Dispatch reports current.
	t.Run("W19", func(t *testing.T) {
		t.Parallel()
		answered := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
		waiting := record.Awaiting{GateVersion: 7, Artifact: gateDoc, Since: hashMark(25), AnsweredAt: &answered, AnsweredBy: record.AnsweredByApproval,
			AnsweredDetail: "version 5", AnsweredDocument: &record.AnsweredDocument{Artifact: gateDoc, Step: record.DesignStepSpec, Version: 5}}
		open := openAt(7, 5, 10, 31)
		window := openAt(7, 5, 10, 31)
		window.LatestVersion = 8
		for _, tc := range []struct {
			name string
			gate record.DesignGate
			want string
		}{
			{"open on the approval of 5 at 7", open,
				" Your primary document is approved at version 5, which Dispatch reports current for version 7, and the design gate is open: " + DesignApprovedWake},
			{"told open while version 8 owes its digest", window,
				" Your primary document is approved at version 5, which Dispatch reports current for version 7, and the design gate is open: " + DesignApprovedWake},
			{"closed at 7 on a verdict that the approval does not stand", editClosed(7, 31),
				" The approval of version 5 stands, and the primary document is now at version 7, for which Dispatch does not report that approval current."},
		} {
			task := resumeTask(gatedRoot, waiting, record.DesignStepSpec, specGates(tc.gate), nil, true)
			if !strings.Contains(task, tc.want) || strings.Contains(task, "which is not approved") {
				t.Errorf("%s: resume task = %q, want it to say %q and never \"which is not approved\"", tc.name, task, tc.want)
			}
		}
		const standing = "its primary document is registered at version 7 and a person has approved it at version 5, which Dispatch reports current for version 7"
		gate := record.TreeGate{Steps: steps, Rows: specGates(open)}
		if said := gateStanding(gate); said != standing || !treeToldOpen(gate) {
			t.Errorf("the architect's start says %q (open %t), want %q, open", said, treeToldOpen(gate), standing)
		}
		if treeToldOpen(record.TreeGate{Steps: steps, Rows: specGates(editClosed(7, 31))}) {
			t.Error("the architect's start reads a gate told closed as open")
		}
		pool := migratedPool(t)
		seedIssue(t, pool, askRoot())
		seedGate(t, pool, window)
		seedRecord(t, pool, func(tx pgx.Tx) error {
			caught, _, err := TreeCatchUp(t.Context(), tx, record.NewStore(), steps, askRoot())
			if err == nil && (len(caught.Gates) != 1 || !caught.Gates[0].Open || caught.Gates[0].Approved != 5 || caught.Gates[0].Version != 8) {
				t.Errorf("the catch-up states %+v, want the spec open as told, approved 5 at version 8", caught.Gates)
			}
			return err
		})
	})

	// The same facts with no digest field, from a Dispatch that sends none, keep today's results:
	// each row in version mode, a version closing the gate and telling it at once, a version
	// answering or moving a wait at once, a newer version of the root's own document ending a basis
	// at once, and a read deciding a pending basis by its version.
	t.Run("W20", func(t *testing.T) {
		t.Parallel()
		version := func(key, document string, number int, seq int64) intake.DispatchArtifact {
			return intake.DispatchArtifact{Key: key, ArtifactID: document, Kind: intake.DispatchArtifactVersion, Version: number, Mark: hashMark(seq), Actor: hashArchitect}
		}
		openAtFive := record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 5, ApprovedVersion: new(5), ApprovedMark: hashMark(10), EverOpened: true}
		inVersionMode := func(t *testing.T, pool *pgxpool.Pool, issue string) {
			t.Helper()
			if gate := gatesOf(t, pool, issue)[record.DesignStepSpec]; gate.Verdict != nil || gate.ToldOpen != nil {
				t.Fatalf("the spec row of %s = %+v, want it in version mode", issue, gate)
			}
		}
		t.Run("W1 and W2", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashTree(t, pool, 5)
			seedGate(t, pool, openAtFive)
			apply("version-6", version(gatedRoot, gateDoc, 6, 20))
			assertHashGate(t, pool, gatedRoot, false, false, 5)
			inVersionMode(t, pool, gatedRoot)
			assertTreeTold(t, pool, toEveryone(gateMovedBody(record.DesignStepSpec, gateDoc, 6, false)))
			assertOutboxCount(t, pool, string(record.OutboxKindNotice), 0)
			assertOutboxCount(t, pool, string(record.OutboxKindDesignChanges), 0)
		})
		t.Run("W3", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashTree(t, pool, 5)
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 6, ApprovedVersion: new(5), ApprovedMark: hashMark(10), EverOpened: true})
			apply("version-7", version(gatedRoot, gateDoc, 7, 30))
			assertHashGate(t, pool, gatedRoot, false, false, 5)
			inVersionMode(t, pool, gatedRoot)
			assertNothingSent(t, pool)
		})
		t.Run("W4", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashTree(t, pool, 5)
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 6, ApprovedVersion: new(5), ApprovedMark: hashMark(10), EverOpened: true})
			apply("approved-6", hashReview(gatedRoot, gateDoc, intake.DispatchArtifactApproved, 6, 40, ""))
			assertHashGate(t, pool, gatedRoot, true, true, 6)
			inVersionMode(t, pool, gatedRoot)
			assertNoticeCount(t, pool, "design-approved", 1)
			assertOutboxCount(t, pool, string(record.OutboxKindDesignChanges), 1)
		})
		t.Run("W4b", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashTree(t, pool, 6)
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 6, ApprovedVersion: new(6), ApprovedMark: hashMark(40), EverOpened: true})
			apply("changes-6", hashReview(gatedRoot, gateDoc, intake.DispatchArtifactChangesRequested, 6, 60, "name the rollback path"))
			assertHashGate(t, pool, gatedRoot, false, false, 0)
			inVersionMode(t, pool, gatedRoot)
			assertNoticeCount(t, pool, "design-changes-requested", 1)
			assertTreeTold(t, pool, toEveryone(gateMovedBody(record.DesignStepSpec, gateDoc, 6, false)))
			assertOutboxCount(t, pool, string(record.OutboxKindDesignChanges), 0)
		})
		t.Run("W5", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashTree(t, pool, 7)
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 7, ApprovedVersion: new(7), ApprovedMark: hashMark(40), EverOpened: true})
			apply("approved-6", hashReview(gatedRoot, gateDoc, intake.DispatchArtifactApproved, 6, 50, ""))
			assertHashGate(t, pool, gatedRoot, true, true, 7)
			inVersionMode(t, pool, gatedRoot)
			assertNothingSent(t, pool)
		})
		for _, tc := range []struct {
			name   string
			seq    int64
			by     record.Answer
			detail string
		}{{"W6", 30, record.AnsweredByVersion, "version 6"}, {"W7", 15, "", ""}} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				pool, apply := rig(t)
				hashWaitingRoot(t, pool, 5, 20)
				seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 5, ApprovedVersion: new(4), ApprovedMark: hashMark(5)})
				apply("version-6", version(gatedRoot, gateDoc, 6, tc.seq))
				inVersionMode(t, pool, gatedRoot)
				assertWait(t, pool, 6, tc.by, tc.detail, 0)
			})
		}
		t.Run("W9", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashTree(t, pool, 4)
			seedGate(t, pool, onBasisAt(4, 3))
			apply("ancestor-version-4", version(basisAncestor, basisDoc, 4, 30))
			if gate := assertHashGate(t, pool, gatedRoot, true, true, 4); !gate.OnBasis() {
				t.Fatalf("the spec row = %+v, want it on its basis", gate)
			}
			assertNothingSent(t, pool)
		})
		t.Run("W11", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashTree(t, pool, 3)
			seedGate(t, pool, onBasisAt(3, 3))
			apply("version-4", version(gatedRoot, gateDoc, 4, 30))
			if gate := assertHashGate(t, pool, gatedRoot, false, false, 0); gate.Basis != nil {
				t.Fatalf("the spec row = %+v, want its basis ended at once", gate)
			}
			assertTreeTold(t, pool, toEveryone(gateMovedBody(record.DesignStepSpec, gateDoc, 4, false)))
		})
		t.Run("W12c", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashTree(t, pool, 2)
			seedGate(t, pool, pendingAt(3, 0))
			apply("version-4", version(gatedRoot, gateDoc, 4, 30))
			if gate := assertHashGate(t, pool, gatedRoot, false, false, 0); gate.Basis != nil {
				t.Fatalf("the spec row = %+v, want its wait ended at once", gate)
			}
			const newer = "Version 4 of the primary document of LEGION-208 (dispatch://LEGION-208/spec), the root's own document, is newer than version 3, which the step was registered at."
			if ended := noticesOfKind(t, pool, gatedRoot, "basis-ended"); len(ended) != 1 || !strings.Contains(ended[0].Reason, newer) {
				t.Fatalf("basis-ended notices = %+v, want one saying %q", ended, newer)
			}
		})
		t.Run("W13", func(t *testing.T) {
			t.Parallel()
			pool, apply := carriedRig(t, record.DesignGate{Issue: "LEGION-209", ArtifactID: gateDoc, LatestVersion: 4, ApprovedVersion: new(3),
				ApprovedMark: hashMark(10), Carried: true})
			apply("old-root-approved-4", hashReview(gatedRoot, gateDoc, intake.DispatchArtifactApproved, 4, 40, ""))
			assertHashGate(t, pool, "LEGION-209", true, true, 4)
			inVersionMode(t, pool, "LEGION-209")
			if opened := noticesOfKind(t, pool, "LEGION-209", "carried-gate-opened"); len(opened) != 1 || opened[0].Actor != hashPerson.Name() || opened[0].Version != 4 {
				t.Fatalf("carried-gate-opened notices = %+v, want today's, by %s", opened, hashPerson.Name())
			}
		})
		t.Run("W15", func(t *testing.T) {
			t.Parallel()
			pool, _, apply := registeredPending(t)
			apply("read", seedRead(standing("approved", nil), ancestorAtWork))
			if gate := assertHashGate(t, pool, gatedRoot, false, false, 0); gate.Basis != nil {
				t.Fatalf("the spec step = %#v, want its wait ended by the newer version, as today", gate)
			}
			assertNoticeCount(t, pool, "basis-ended", 1)
		})
		t.Run("W21", func(t *testing.T) {
			t.Parallel()
			pool, apply := rig(t)
			hashWaitingRoot(t, pool, 5, 20)
			seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 5, ApprovedVersion: new(4), ApprovedMark: hashMark(5)})
			apply("register-6", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, Version: 6, DispatchVersion: 6,
				Since: hashMark(25), RequestWaitsOnAgent: true})
			inVersionMode(t, pool, gatedRoot)
			assertWait(t, pool, 6, record.AnsweredByVersion, "version 6", 0)
		})
	})

	// A registration whose read shows a newer version owing its digest, with the approval request
	// on the agent's turn, answers no wait for a person: the answer comes when the digest settles
	// that version, as the newer version it is (the architect's addition, W21).
	t.Run("W21", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashWaitingRoot(t, pool, 5, 20)
		seedGate(t, pool, record.DesignGate{Issue: gatedRoot, ArtifactID: gateDoc, LatestVersion: 5, ApprovedVersion: new(4), ApprovedMark: hashMark(5)})

		apply("register-6", intake.GateRegistered{Issue: gatedRoot, Step: record.DesignStepSpec, ArtifactID: gateDoc, Version: 6, DispatchVersion: 6,
			Since: hashMark(25), RequestWaitsOnAgent: true, DigestOwed: new(true), VerdictMark: hashMark(25)})
		if gate := assertHashGate(t, pool, gatedRoot, false, false, 4); !record.DigestOwed(gate) || gate.LatestVersion != 6 {
			t.Fatalf("the spec row = %+v, want version 6 owing its digest", gate)
		}
		assertWait(t, pool, 5, "", "", 0)

		apply("digest-6", digestOf(t, gatedRoot, gateDoc, 6, 31, false, 0))
		assertHashGate(t, pool, gatedRoot, false, false, 4)
		assertWait(t, pool, 6, record.AnsweredByVersion, "version 6", 0)
	})

	// The gate is open and told open at the approval of 7, and every holder took 7. The person's
	// approval of 8 reaches the row by the gate seed's read while version 9 owes its digest, and
	// queues no news; version 9's digest keeps the gate open, so its standing does not move, but
	// the approved version rose from 7 to 8, and the settlement queues the one reconcile of the
	// news that rise owes, and tells no gate message (dispatch://INFRA-6065/comment/5ac0aafb).
	t.Run("W22", func(t *testing.T) {
		t.Parallel()
		pool, apply := rig(t)
		hashTree(t, pool, 7)
		seedGate(t, pool, openAt(7, 7, 10, 10))

		apply("seed-read", gateSeedRead(gatedRoot, gateDoc, 8, 9, hashMark(40), true, false, hashMark(41)))
		if gate := assertHashGate(t, pool, gatedRoot, false, true, 8); !record.DigestOwed(gate) || gate.LatestVersion != 9 {
			t.Fatalf("the spec row = %+v, want version 9 owing its digest", gate)
		}
		assertNothingSent(t, pool)

		apply("digest-9", digestOf(t, gatedRoot, gateDoc, 9, 50, true, 8))
		assertHashGate(t, pool, gatedRoot, true, true, 8)
		assertOutboxCount(t, pool, string(record.OutboxKindDesignChanges), 1)
		for _, kind := range []record.OutboxKind{record.OutboxKindRoleMessage, record.OutboxKindNotice, record.OutboxKindGateSeed} {
			assertOutboxCount(t, pool, string(kind), 0)
		}
	})
}
