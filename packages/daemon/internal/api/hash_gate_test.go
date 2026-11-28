package api

//ci:reads packages/contracts/fixtures/hash-gate/** hashGateApproval reads the GET bodies through dispatch's own client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

const (
	// hashGateDocument is the document every GET body of the hash-gate fixtures reads.
	hashGateDocument = "00000000-0000-4000-8000-000000000001"
	// standingSHA256 is artifact.json's content_sha256 and current_sha256: the approved version's
	// digest, which the latest version's matches. owedSHA256 is artifact-owed.json's
	// content_sha256, the approved version's digest, beside a current_sha256 Dispatch still owes.
	standingSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	owedSHA256     = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	// hashGateSince is the event sequence Dispatch shows the root at, which register_gate reads
	// before the approval.
	hashGateSince = 7
)

// hashGateApproval is one GET body of packages/contracts/fixtures/hash-gate (R1 of
// dispatch://INFRA-6065) as dispatch's own client reads it, the daemon's reader of that contract.
// The fixtures hold the document's id, its name and its approval alone, so change, nil for none,
// adds to the approval what else a row's read shows (who reviewed, where and when) and moves the
// versions it needs, and the body names the issue carrying the document, as every read does.
func hashGateApproval(t *testing.T, name, issue string, change func(approval map[string]any)) dispatch.Approval {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "fixtures", "hash-gate", name))
	if err != nil {
		t.Fatalf("read the hash-gate fixture %s: %v", name, err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode the hash-gate fixture %s: %v", name, err)
	}
	approval, ok := body["approval"].(map[string]any)
	if !ok {
		t.Fatalf("the hash-gate fixture %s holds no approval", name)
	}
	if change != nil {
		change(approval)
	}
	body["issue_key"] = issue
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode the GET body of %s: %v", name, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write(encoded); err != nil {
			t.Errorf("write the GET body of %s: %v", name, err)
		}
	}))
	defer server.Close()
	read, err := dispatch.New(server.URL, nil).Approval(context.Background(), hashGateDocument)
	if err != nil {
		t.Fatalf("read the GET body of %s: %v", name, err)
	}
	return read
}

// reviewedBy is a change of a fixture's approval that names who gave the review it shows, and
// where Dispatch recorded it in the issue's event log and when.
func reviewedBy(by dispatch.Actor, seq int64, at time.Time) func(map[string]any) {
	return func(approval map[string]any) {
		approval["by"] = map[string]any{"kind": by.Kind, "id": by.ID}
		approval["seq"] = seq
		approval["at"] = at.Format(time.RFC3339Nano)
	}
}

// atVersions is a change of a fixture's approval that moves the version approved and the latest
// version, and then applies each of then.
func atVersions(approved, latest int, then ...func(map[string]any)) func(map[string]any) {
	return func(approval map[string]any) {
		approval["version"], approval["latest_version"] = approved, latest
		for _, change := range then {
			change(approval)
		}
	}
}

// hashGateRoot is the Dispatch of the register_gate rows: the tree root LEGION-208 at event
// sequence hashGateSince with no parent, so no view fact is read, carrying the fixtures' document
// with approval as its approval read.
func hashGateRoot(approval dispatch.Approval) *statusRecorder {
	return &statusRecorder{
		issues:    map[string]dispatch.Issue{basisR: {Key: basisR, LastSeq: hashGateSince, Status: "in_progress", Labels: []string{"legion"}}},
		documents: map[string]string{hashGateDocument: basisR},
		approvals: map[string]dispatch.Approval{hashGateDocument: approval},
	}
}

// registerHashGate registers version 2 of the fixtures' document as the spec step of LEGION-208
// and answers the facts the call added, and the moments just before and after it, between which
// the call's own read began.
func registerHashGate(t *testing.T, h *harness, facts *factRecorder, architect liveClaim) (added []intake.Fact, before, after time.Time) {
	t.Helper()
	known := len(facts.recorded())
	before = time.Now().UTC()
	recorder := h.request(http.MethodPost, "/legion/v1/gates/register", map[string]any{
		"grantId": architect.grant(t), "issue": basisR, "artifactId": hashGateDocument, "version": 2,
	}, nil)
	after = time.Now().UTC()
	if recorder.Code != http.StatusOK {
		t.Fatalf("register_gate = %d: %s", recorder.Code, recorder.Body)
	}
	return facts.recorded()[known:], before, after
}

// onlyRegistration is the one register_gate fact of added.
func onlyRegistration(t *testing.T, added []intake.Fact) intake.GateRegistered {
	t.Helper()
	if len(added) != 1 {
		t.Fatalf("register_gate facts = %#v, want one", added)
	}
	fact, ok := added[0].(intake.GateRegistered)
	if !ok {
		t.Fatalf("register_gate fact = %#v, want an intake.GateRegistered", added[0])
	}
	return fact
}

// withoutReadTime is fact with its verdict mark's time cleared, the one field of a registration
// the call's own clock gives, which a row asserts apart.
func withoutReadTime(fact intake.GateRegistered) intake.GateRegistered {
	fact.VerdictMark.At = time.Time{}
	return fact
}

// Each register_gate read and each basis registration asks Dispatch whether an approval stands
// through one predicate (dispatch.Approval.Stands, R5 of dispatch://INFRA-6065), read from the
// hash-gate fixtures through dispatch's own client, and the state answers a gated step by the
// standing the daemon last told (record.ToldStanding):
//
//   - register_gate on an approval of version 1 that Dispatch reports standing at its latest
//     version 2 carries that approval at version 1 and Dispatch's verdict true, marked by the read
//     (the root's sequence before the read, the read's own time), and the gate opens; with the
//     digest owed it carries no approval and no verdict, and the gate stays closed at version 2;
//     from a Dispatch that sends no digest field it is today's registration, approving nothing.
//     The fact's id names what the read found of the digest, so a read whose only change is the
//     filled digest is a fact of its own, while a version-mode read keeps today's id.
//   - A standing approval of the root's own document below its latest version opens the step as
//     today's approval of that version does, so the call registers no basis. A basis names the
//     ancestor's latest version whose approval Dispatch reports standing, and an older version is
//     refused with BASIS_VERSION_MISMATCH.
//   - Each refusal that names an ancestor's approval names the version a person approved and,
//     where it stands below the latest version, that Dispatch reports it current for that latest
//     version, never an approval of a latest version no person approved.
//   - GatedStepViewsOf answers `approved` with approvedVersion below currentVersion, and a step
//     whose newer version owes its digest keeps the word it was last told.
func TestHashGateRegistrationsTakeDispatchVerdict(t *testing.T) {
	person := dispatch.Actor{Kind: "user", ID: "person"}
	reviewedAt := time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC)
	standingID := fmt.Sprintf("gate:%s:1:spec:%s:2:%d:digest-owed:false:current:%s", basisR, hashGateDocument, hashGateSince, standingSHA256)

	t.Run("register_gate opens on an approval Dispatch reports standing below its latest version", func(t *testing.T) {
		approval := hashGateApproval(t, "artifact.json", basisR, reviewedBy(person, 5, reviewedAt))
		h, facts, architect := basisEngineHarness(t, hashGateRoot(approval), nil)
		added, before, after := registerHashGate(t, h, facts, architect)
		fact := onlyRegistration(t, added)
		want := intake.GateRegistered{Issue: basisR, Step: record.DesignStepSpec, ArtifactID: hashGateDocument, Version: 2, DispatchVersion: 2,
			ApprovedVersion: 1, ApprovedMark: record.EventMark{Seq: 5, At: reviewedAt}, ReviewedBy: person,
			Since: record.EventMark{Seq: hashGateSince}, DigestOwed: new(false), Verdict: new(true), VerdictMark: record.EventMark{Seq: hashGateSince},
			ContentSHA256: standingSHA256, CurrentSHA256: standingSHA256, By: architect.token, Generation: 1}
		if got := withoutReadTime(fact); !reflect.DeepEqual(got, want) {
			t.Fatalf("register_gate fact = %#v, want %#v", got, want)
		}
		if at := fact.VerdictMark.At; at.Before(before) || at.After(after) {
			t.Fatalf("the verdict's mark is at %s, want the read's own time, between %s and %s, after the review's %s", at, before, after, reviewedAt)
		}
		assertProcessedEvent(t, h, standingID)
		gate := specGateOf(t, h)
		if gate == nil || !record.GateOpen(*gate) || !record.ToldStanding(*gate) || gate.LatestVersion != 2 ||
			gate.ApprovedVersion == nil || *gate.ApprovedVersion != 1 || gate.Verdict == nil || gate.Verdict.Version != 2 || !gate.Verdict.Current {
			t.Fatalf("the spec step = %+v (verdict %+v), want it open at version 2 on the approval of version 1 with the verdict true at 2", gate, gate.Verdict)
		}
		views := GatedStepViewsOf(config.DesignGateRootIssues.Steps(), map[record.DesignStep]record.DesignGate{record.DesignStepSpec: *gate})
		wantViews := []GatedStepView{{Step: record.DesignStepSpec, ArtifactID: hashGateDocument, CurrentVersion: 2, ApprovedVersion: new(1), Status: GatedStepApproved}}
		if !reflect.DeepEqual(views, wantViews) {
			t.Fatalf("gatedSteps = %+v, want %+v", views, wantViews)
		}
	})

	t.Run("register_gate's verdict mark is the review's where Dispatch's sequence and clock run ahead of the read", func(t *testing.T) {
		ahead := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
		approval := hashGateApproval(t, "artifact.json", basisR, reviewedBy(person, 9, ahead))
		h, facts, architect := basisEngineHarness(t, hashGateRoot(approval), nil)
		added, _, _ := registerHashGate(t, h, facts, architect)
		if fact := onlyRegistration(t, added); fact.VerdictMark != (record.EventMark{Seq: 9, At: ahead}) || fact.ApprovedMark != fact.VerdictMark {
			t.Fatalf("the verdict's mark = %+v beside the review's %+v, want the review's own, sequence 9 at %s", fact.VerdictMark, fact.ApprovedMark, ahead)
		}
	})

	t.Run("register_gate with the digest owed carries no approval and no verdict", func(t *testing.T) {
		approval := hashGateApproval(t, "artifact-owed.json", basisR, reviewedBy(person, 5, reviewedAt))
		h, facts, architect := basisEngineHarness(t, hashGateRoot(approval), nil)
		added, _, _ := registerHashGate(t, h, facts, architect)
		want := intake.GateRegistered{Issue: basisR, Step: record.DesignStepSpec, ArtifactID: hashGateDocument, Version: 2, DispatchVersion: 2,
			Since: record.EventMark{Seq: hashGateSince}, DigestOwed: new(true), ContentSHA256: owedSHA256, By: architect.token, Generation: 1}
		if got := onlyRegistration(t, added); !reflect.DeepEqual(got, want) {
			t.Fatalf("register_gate fact = %#v, want %#v", got, want)
		}
		assertProcessedEvent(t, h, fmt.Sprintf("gate:%s:1:spec:%s:2:%d:digest-owed:true", basisR, hashGateDocument, hashGateSince))
		if gate := specGateOf(t, h); gate == nil || record.GateOpen(*gate) || gate.LatestVersion != 2 || gate.ApprovedVersion != nil {
			t.Fatalf("the spec step = %+v, want it closed at version 2 with no approval", gate)
		}
	})

	t.Run("register_gate from a Dispatch that sends no digest field is today's registration", func(t *testing.T) {
		approval := hashGateApproval(t, "artifact.json", basisR, func(approval map[string]any) {
			reviewedBy(person, 5, reviewedAt)(approval)
			delete(approval, "content_sha256")
			delete(approval, "current_sha256")
			delete(approval, "digest_owed")
		})
		h, facts, architect := basisEngineHarness(t, hashGateRoot(approval), nil)
		for range 2 {
			registerHashGate(t, h, facts, architect)
		}
		want := []intake.Fact{intake.GateRegistered{Issue: basisR, Step: record.DesignStepSpec, ArtifactID: hashGateDocument, Version: 2, DispatchVersion: 2,
			Since: record.EventMark{Seq: hashGateSince}, By: architect.token, Generation: 1}}
		if got := facts.recorded(); !reflect.DeepEqual(got, want) {
			t.Fatalf("register_gate facts of a read and its retry = %#v, want today's one fact, approving nothing: %#v", got, want)
		}
		assertProcessedEvent(t, h, fmt.Sprintf("gate:%s:1:spec:%s:2:%d", basisR, hashGateDocument, hashGateSince))
		if gate := specGateOf(t, h); gate == nil || record.GateOpen(*gate) || gate.Verdict != nil || gate.ApprovedVersion != nil {
			t.Fatalf("the spec step = %+v, want it closed in version mode with no approval", gate)
		}
	})

	t.Run("a read whose only change is the filled digest is a registration of its own", func(t *testing.T) {
		world := hashGateRoot(hashGateApproval(t, "artifact-owed.json", basisR, reviewedBy(person, 5, reviewedAt)))
		h, facts, architect := basisEngineHarness(t, world, nil)
		registerHashGate(t, h, facts, architect)
		world.approvals[hashGateDocument] = hashGateApproval(t, "artifact.json", basisR, reviewedBy(person, 5, reviewedAt))
		for range 2 {
			registerHashGate(t, h, facts, architect)
		}
		if got := facts.recorded(); len(got) != 2 {
			t.Fatalf("register_gate facts of an owed read, then a filled read and its retry = %#v, want two", got)
		}
		assertProcessedEvent(t, h, standingID)
		if gate := specGateOf(t, h); gate == nil || !record.GateOpen(*gate) || gate.ApprovedVersion == nil || *gate.ApprovedVersion != 1 {
			t.Fatalf("the spec step = %+v, want it open on the approval of version 1 the filled read carried", gate)
		}
	})

	t.Run("a standing approval of the root's own document below its latest version registers no basis", func(t *testing.T) {
		h, facts, statuses, architect := basisHarness(t, nil)
		const seq = 600
		*statuses = basisWorld(seq)
		statuses.approvals[rSpec] = hashGateApproval(t, "artifact.json", basisR, atVersions(3, 4, reviewedBy(basisAlice, 8, basisApprovedAt)))
		if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", basisBody(architect.grant(t)), nil); recorder.Code != http.StatusOK {
			t.Fatalf("register on the basis = %d: %s", recorder.Code, recorder.Body)
		}
		want := basisFact(seq, architect)
		want.Basis = nil
		want.ApprovedVersion, want.ApprovedMark, want.ReviewedBy = 3, record.EventMark{Seq: 8, At: basisApprovedAt}, basisAlice
		want.DigestOwed, want.Verdict, want.VerdictMark = new(false), new(true), record.EventMark{Seq: seq}
		want.ContentSHA256, want.CurrentSHA256 = standingSHA256, standingSHA256
		if got := facts.recorded(); len(got) != 1 || !reflect.DeepEqual(withoutReadTime(got[0].(intake.GateRegistered)), want) {
			t.Fatalf("registration facts = %#v, want the person's approval of version 3 and no basis: %#v", got, want)
		}
		assertProcessedEvent(t, h, fmt.Sprintf("gate:%s:0:spec:%s:4:%d:digest-owed:false:current:%s:view-of:%s", basisR, rSpec, seq, standingSHA256, basisP))
	})

	// pStanding is P's primary document approved by a person at version 2, which Dispatch reports
	// current for its latest version 3, the version every basis case names.
	pStanding := func(t *testing.T, by dispatch.Actor) dispatch.Approval {
		return hashGateApproval(t, "artifact.json", basisP, atVersions(2, 3, reviewedBy(by, 21, basisApprovedAt)))
	}

	t.Run("a basis at the ancestor's latest version on a standing approval of an older version is taken", func(t *testing.T) {
		h, facts, statuses, architect := basisHarness(t, nil)
		const seq = 610
		*statuses = basisWorld(seq)
		statuses.approvals[pSpec] = pStanding(t, basisAlice)
		if recorder := h.request(http.MethodPost, "/legion/v1/gates/register", basisBody(architect.grant(t)), nil); recorder.Code != http.StatusOK {
			t.Fatalf("register on the basis = %d: %s", recorder.Code, recorder.Body)
		}
		if got, want := facts.recorded(), basisFact(seq, architect); len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("registration facts = %#v, want the basis at P's latest version 3, on the approval Dispatch recorded at sequence 21: %#v", got, want)
		}
		assertProcessedEvent(t, h, fmt.Sprintf("gate:%s:0:spec:%s:4:%d:basis:%s:3:view-of:%s", basisR, rSpec, seq, pSpec, basisP))
	})

	// Each refusal that names an ancestor's approval, one row each: the world every basis case
	// starts from, with that one change put in.
	for index, tc := range []struct {
		name   string
		change func(t *testing.T, world *statusRecorder, body map[string]any)
		code   string
		says   string
		not    string
	}{
		{name: "the approved version older than the ancestor's latest is refused with BASIS_VERSION_MISMATCH",
			change: func(t *testing.T, world *statusRecorder, body map[string]any) {
				world.approvals[pSpec] = pStanding(t, basisAlice)
				body["basisVersion"] = 2
			}, code: "BASIS_VERSION_MISMATCH",
			says: "Dispatch shows LEGION-150's primary document (" + pSpec + ") approved at version 2, which Dispatch reports current for its latest version 3, " +
				"and the call names version 2; name version 3",
			not: "approved at its latest version"},
		{name: "a session's standing approval of an older version is refused with BASIS_NOT_BY_PERSON naming that version",
			change: func(t *testing.T, world *statusRecorder, _ map[string]any) {
				world.approvals[pSpec] = pStanding(t, dispatch.Actor{Kind: "session", ID: "ses-outsider"})
			}, code: "BASIS_NOT_BY_PERSON",
			says: "the approval of LEGION-150's primary document (" + pSpec + ") at version 2, which Dispatch reports current for its latest version 3, " +
				"was given by session:ses-outsider, not by a person",
			not: "at version 3 was given"},
		{name: "a nearer ancestor's standing approval of an older version is named by its version in BASIS_NOT_NEAREST",
			change: func(t *testing.T, world *statusRecorder, body map[string]any) {
				gUnderP(world)
				world.approvals[pSpec] = pStanding(t, basisAlice)
				body["basisArtifactId"], body["basisVersion"] = gSpec, 5
			}, code: "BASIS_NOT_NEAREST",
			says: "Dispatch shows on its primary document (" + pSpec + ") an approval of version 2 by user:alice, which Dispatch reports current for its latest version 3;",
			not:  "an approval of its latest version"},
		{name: "an approval whose digest Dispatch owes is refused with BASIS_NOT_APPROVED naming its version",
			change: func(t *testing.T, world *statusRecorder, _ map[string]any) {
				world.approvals[pSpec] = hashGateApproval(t, "artifact-owed.json", basisP, atVersions(2, 3, reviewedBy(basisAlice, 21, basisApprovedAt)))
			}, code: "BASIS_NOT_APPROVED",
			says: "Dispatch shows on LEGION-150's primary document (" + pSpec + ") an approval of version 2 by user:alice, " +
				"which Dispatch does not report current for its latest version 3 while it owes that version's digest; " +
				"a basis is a person's approval that Dispatch reports current for its latest version",
			not: "an approval of its latest version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, facts, statuses, architect := basisHarness(t, nil)
			*statuses = basisWorld(int64(620 + index))
			body := basisBody(architect.grant(t))
			tc.change(t, statuses, body)
			refusal := refused(t, h.request(http.MethodPost, "/legion/v1/gates/register", body, nil), http.StatusConflict, tc.code)
			if !strings.Contains(refusal, tc.says) || strings.Contains(refusal, tc.not) {
				t.Fatalf("refusal = %q, want it saying %q and never %q", refusal, tc.says, tc.not)
			}
			if got := facts.recorded(); len(got) != 0 {
				t.Fatalf("the refused registration applied %#v, want nothing recorded", got)
			}
		})
	}

	// The gated steps as GatedStepViewsOf answers them for one spec row in hash mode, at version 2
	// on a person's approval of version 1 that Dispatch reports current there, and on a basis.
	verdict := func(version int, current bool) *record.GateVerdict {
		return &record.GateVerdict{Version: version, Current: current, Mark: record.EventMark{Seq: 30}}
	}
	approvedOne := record.DesignGate{Issue: basisR, ArtifactID: rSpec, LatestVersion: 2, ApprovedVersion: new(1), ApprovedMark: record.EventMark{Seq: 20},
		Verdict: verdict(2, true), ToldOpen: new(true)}
	owedOpen := approvedOne
	owedOpen.LatestVersion = 3
	owedClosed := approvedOne
	owedClosed.LatestVersion, owedClosed.Verdict, owedClosed.ToldOpen = 3, verdict(2, false), new(false)
	basis := record.GateBasis{Issue: basisP, ArtifactID: pSpec, Version: 3, Approval: record.EventMark{Seq: 21, At: basisApprovedAt}, Approver: "user:alice",
		RegisteredAt: basisApprovedAt, RootVersion: 2}
	onBasisOwed := record.DesignGate{Issue: basisR, ArtifactID: rSpec, LatestVersion: 3, ApprovedVersion: new(2), Basis: &basis,
		Verdict: verdict(2, false), ToldOpen: new(true)}
	for _, tc := range []struct {
		name string
		gate record.DesignGate
		want GatedStepView
	}{
		{"a step open in hash mode on an approval below its current version reads approved", approvedOne,
			GatedStepView{Step: record.DesignStepSpec, ArtifactID: rSpec, CurrentVersion: 2, ApprovedVersion: new(1), Status: GatedStepApproved}},
		{"a step told open whose newer version owes its digest keeps approved", owedOpen,
			GatedStepView{Step: record.DesignStepSpec, ArtifactID: rSpec, CurrentVersion: 3, ApprovedVersion: new(1), Status: GatedStepApproved}},
		{"a step told closed whose newer version owes its digest keeps waiting", owedClosed,
			GatedStepView{Step: record.DesignStepSpec, ArtifactID: rSpec, CurrentVersion: 3, ApprovedVersion: new(1), Status: GatedStepWaiting}},
		{"a step told open on a basis whose root's newer version owes its digest keeps open_on_basis", onBasisOwed,
			GatedStepView{Step: record.DesignStepSpec, ArtifactID: rSpec, CurrentVersion: 3, ApprovedVersion: new(2), Status: GatedStepOpenOnBasis,
				Basis: &GatedStepBasis{Issue: basisP, ArtifactID: pSpec, Version: 3, Approver: "user:alice", ApprovedAt: &basisApprovedAt}}},
	} {
		t.Run("GatedStepViewsOf: "+tc.name, func(t *testing.T) {
			got := GatedStepViewsOf(config.DesignGateRootIssues.Steps(), map[record.DesignStep]record.DesignGate{record.DesignStepSpec: tc.gate})
			if want := []GatedStepView{tc.want}; !reflect.DeepEqual(got, want) {
				t.Fatalf("gatedSteps = %+v, want %+v", got, want)
			}
		})
	}
}
